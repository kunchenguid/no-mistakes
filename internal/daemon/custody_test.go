package daemon

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func custodyManagerFixture(t *testing.T) (*RunManager, *ipc.CustodyOperationParams, string) {
	t.Helper()
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	repo, base := setupTestGitRepo(t, p, d, "custody-repo")
	remote := filepath.Join(t.TempDir(), "published.git")
	gitCmd(t, "", "clone", "--bare", repo.WorkingPath, remote)
	repo, err = d.UpdateRepoMetadata(repo.ID, remote, "main")
	if err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo.WorkingPath, "checkout", "-b", "validation")
	gitCmd(t, repo.WorkingPath, "commit", "--allow-empty", "-m", "validated descendant")
	head := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
	gitCmd(t, repo.WorkingPath, "push", remote, base+":refs/heads/existing")
	gitCmd(t, repo.WorkingPath, "push", p.RepoDir(repo.ID), "HEAD:refs/heads/validation")
	run, err := d.InsertRun(repo.ID, "validation", head, base)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.SetRunWorktreeDir(run.ID, repo.WorkingPath); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatusWithVerifiedHead(run.ID, types.RunFailed, head); err != nil {
		t.Fatal(err)
	}
	m := NewRunManager(d, p, nil)
	m.publicationPR = func(_ context.Context, _ *db.Repo, _ *db.Run, _ string, branch string) (*scm.PR, error) {
		return &scm.PR{URL: "https://github.com/test/repo/pull/1", HeadSHA: base}, nil
	}
	return m, &ipc.CustodyOperationParams{Action: "rebind", RepoID: repo.ID, RunID: run.ID, WorkDir: repo.WorkingPath, HeadSHA: head, PublicationBranch: "existing"}, remote
}

func parkCustodyRun(t *testing.T, m *RunManager, p *ipc.CustodyOperationParams) func() {
	t.Helper()
	run, _ := m.db.GetRun(p.RunID)
	repo, _ := m.db.GetRepo(p.RepoID)
	ctx, cancel := context.WithCancel(context.Background())
	e := pipeline.NewExecutor(m.db, m.paths, config.Merge(config.DefaultGlobalConfig(), &config.RepoConfig{}), nil, []pipeline.Step{&mockApprovalStep{name: types.StepReview}}, nil)
	m.executors[run.ID] = e
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = e.Execute(ctx, run, repo, p.WorkDir)
	}()
	stop := func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("executor did not stop")
		}
	}
	t.Cleanup(stop)
	deadline := time.Now().Add(5 * time.Second)
	for {
		r, _ := m.db.GetRun(run.ID)
		if r.AwaitingAgentSince != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("run did not park")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return stop
}

func TestCustodyManagerReboundPublicationCanReleaseAndReconcile(t *testing.T) {
	for _, action := range []string{"release", "reconcile"} {
		for _, scenario := range []string{"recorded-pr", "replacement-pr", "replacement-after-archive"} {
			t.Run(action+"/"+scenario, func(t *testing.T) {
				m, p, remote := custodyManagerFixture(t)
				stop := parkCustodyRun(t, m, p)
				if _, err := m.HandleCustodyOperation(context.Background(), p); err != nil {
					t.Fatal(err)
				}
				// Publish the rebound head, then finish the executor before release.
				gitCmd(t, p.WorkDir, "push", remote, p.HeadSHA+":refs/heads/existing")
				if err := m.db.UpdateRunPublication(p.RunID, db.PushBinding{
					HeadSHA: p.HeadSHA, TargetKind: "upstream", TargetFingerprint: branchsync.TargetFingerprint(remote), Ref: "refs/heads/existing",
				}); err != nil {
					t.Fatal(err)
				}
				stop()
				delete(m.executors, p.RunID)
				status := types.RunCompleted
				message := ""
				if action == "reconcile" {
					status, message = types.RunFailed, "daemon crashed during execution"
				}
				if err := m.db.UpdateRunErrorStatusWithVerifiedHead(p.RunID, message, status, p.HeadSHA); err != nil {
					t.Fatal(err)
				}
				before, _ := m.db.GetRun(p.RunID)
				// The source branch has no public ref or PR. Both provider reads must
				// use the rebound destination recorded by the supported rebind call.
				calls := 0
				m.publicationPR = func(_ context.Context, _ *db.Repo, _ *db.Run, _ string, branch string) (*scm.PR, error) {
					calls++
					if branch != "existing" {
						return nil, os.ErrNotExist
					}
					url := *before.PRURL
					if scenario == "replacement-pr" || scenario == "replacement-after-archive" && calls > 1 {
						url = "https://github.com/test/repo/pull/2"
					}
					return &scm.PR{URL: url, HeadSHA: p.HeadSHA}, nil
				}
				// Restore a stale custody lane while retaining its old head in an archive.
				gitCmd(t, m.paths.RepoDir(p.RepoID), "update-ref", "refs/heads/validation", before.BaseSHA)
				p.Action = action
				result, err := m.HandleCustodyOperation(context.Background(), p)
				if scenario != "recorded-pr" {
					if err == nil || result != nil {
						t.Fatalf("replacement PR accepted: result=%+v err=%v", result, err)
					}
					after, readErr := m.db.GetRun(p.RunID)
					if readErr != nil || !reflect.DeepEqual(before, after) {
						t.Fatalf("refusal changed run: before=%+v after=%+v err=%v", before, after, readErr)
					}
					if got := gitOutput(t, m.paths.RepoDir(p.RepoID), "rev-parse", "refs/heads/validation"); got != before.BaseSHA {
						t.Fatal("refusal changed the custody lane")
					}
					if got := gitOutput(t, remote, "rev-parse", "refs/heads/existing"); got != p.HeadSHA {
						t.Fatal("refusal changed the published destination")
					}
					if got := gitOutput(t, p.WorkDir, "rev-parse", "HEAD"); got != p.HeadSHA {
						t.Fatal("refusal moved the caller")
					}
					if scenario == "replacement-pr" {
						archives, listErr := git.Run(context.Background(), m.paths.RepoDir(p.RepoID), "for-each-ref", "--format=%(refname)", "refs/no-mistakes/release/")
						if calls != 1 || listErr != nil || archives != "" {
							t.Fatalf("replacement PR was not refused before archiving: calls=%d archives=%q err=%v", calls, archives, listErr)
						}
					} else {
						if calls != 2 {
							t.Fatalf("provider calls=%d, want initial proof and archive recheck", calls)
						}
						for _, head := range []string{p.HeadSHA, before.BaseSHA} {
							if got := gitOutput(t, m.paths.RepoDir(p.RepoID), "rev-parse", "refs/no-mistakes/release/"+p.RunID+"/"+head); got != head {
								t.Fatal("refusal lost an archived head")
							}
						}
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				after, _ := m.db.GetRun(p.RunID)
				if result.State != "released" || result.Branch != "validation" || calls != 2 || after.CustodyReturnedAt == nil {
					t.Fatalf("release result=%+v run=%+v provider calls=%d", result, after, calls)
				}
				if after.Branch != before.Branch || after.PublishBranch() != before.PublishBranch() || after.HeadSHA != before.HeadSHA || *after.LastPushedSHA != *before.LastPushedSHA || *after.PushRef != *before.PushRef || *after.PushGeneration != *before.PushGeneration || after.Status != before.Status {
					t.Fatalf("release changed provenance: before=%+v after=%+v", before, after)
				}
				for _, head := range []string{p.HeadSHA, before.BaseSHA} {
					if got := gitOutput(t, m.paths.RepoDir(p.RepoID), "rev-parse", "refs/no-mistakes/release/"+p.RunID+"/"+head); got != head {
						t.Fatal("owned head was not archived")
					}
				}
				if got := gitOutput(t, m.paths.RepoDir(p.RepoID), "rev-parse", "refs/heads/validation"); got != p.HeadSHA {
					t.Fatal("source custody lane was not restored")
				}
				if got := gitOutput(t, remote, "rev-parse", "refs/heads/existing"); got != p.HeadSHA {
					t.Fatal("published destination changed")
				}
				if got := gitOutput(t, p.WorkDir, "rev-parse", "HEAD"); got != p.HeadSHA {
					t.Fatal("caller moved")
				}
				if _, err := m.HandleCustodyOperation(context.Background(), p); err != nil {
					t.Fatalf("idempotent release: %v", err)
				}
			})
		}
	}
}

func TestCustodyManagerRebindsParkedLiveRunWithoutRenamingCustody(t *testing.T) {
	m, p, remote := custodyManagerFixture(t)
	parkCustodyRun(t, m, p)
	run, _ := m.db.GetRun(p.RunID)
	result, err := m.HandleCustodyOperation(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != "confirmed" || result.Branch != "existing" {
		t.Fatalf("result=%+v", result)
	}
	r, _ := m.db.GetRun(run.ID)
	if r.Branch != "validation" || r.PublishBranch() != "existing" || r.LastPushedSHA != nil {
		t.Fatalf("binding=%+v", r)
	}
	if got := gitOutput(t, p.WorkDir, "rev-parse", "HEAD"); got != p.HeadSHA {
		t.Fatal("caller was moved")
	}
	if got := gitOutput(t, remote, "rev-parse", "refs/heads/existing"); got != result.HeadSHA {
		t.Fatal("rebind published or rewrote history")
	}
}

func TestCustodyManagerPublicationRefusals(t *testing.T) {
	for _, scenario := range []string{"missing-branch", "missing-pr", "rewrite", "default-branch", "wrong-head", "wrong-run", "active-unparked", "terminal", "dirty-release"} {
		t.Run(scenario, func(t *testing.T) {
			m, p, remote := custodyManagerFixture(t)
			if scenario != "active-unparked" && scenario != "dirty-release" && scenario != "terminal" {
				parkCustodyRun(t, m, p)
			}
			switch scenario {
			case "missing-branch":
				gitCmd(t, remote, "update-ref", "-d", "refs/heads/existing")
			case "missing-pr":
				m.publicationPR = func(context.Context, *db.Repo, *db.Run, string, string) (*scm.PR, error) { return nil, os.ErrNotExist }
			case "rewrite":
				gitCmd(t, p.WorkDir, "checkout", "--orphan", "different")
				gitCmd(t, p.WorkDir, "commit", "--allow-empty", "-m", "different history")
				other := gitOutput(t, p.WorkDir, "rev-parse", "HEAD")
				gitCmd(t, p.WorkDir, "push", "--force", remote, "HEAD:refs/heads/existing")
				gitCmd(t, p.WorkDir, "checkout", "validation")
				m.publicationPR = func(context.Context, *db.Repo, *db.Run, string, string) (*scm.PR, error) {
					return &scm.PR{URL: "https://github.com/test/repo/pull/1", HeadSHA: other}, nil
				}
			case "default-branch":
				p.PublicationBranch = "main"
			case "wrong-head":
				p.HeadSHA = strings.Repeat("a", 40)
			case "wrong-run":
				p.RunID = "not-owned"
			case "active-unparked":
				_ = m.db.UpdateRunStatus(p.RunID, types.RunRunning)
				m.executors[p.RunID] = pipeline.NewExecutor(m.db, m.paths, nil, nil, nil, nil)
			case "dirty-release":
				p.Action = "release"
				if err := m.db.UpdateRunPRURL(p.RunID, "https://github.com/test/repo/pull/1"); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(p.WorkDir, "uncommitted"), []byte("work"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			before := gitOutput(t, m.paths.RepoDir(p.RepoID), "rev-parse", "refs/heads/validation")
			if _, err := m.HandleCustodyOperation(context.Background(), p); err == nil {
				t.Fatal("unsafe operation accepted")
			}
			if got := gitOutput(t, m.paths.RepoDir(p.RepoID), "rev-parse", "refs/heads/validation"); got != before {
				t.Fatal("gate changed on refusal")
			}
			runs, _ := m.db.GetRunsByRepo(p.RepoID)
			for _, r := range runs {
				if r.PublicationBranch != nil || r.CustodyReturnedAt != nil {
					t.Fatalf("ownership changed on refusal: %+v", r)
				}
			}
		})
	}
}

func TestCustodyManagerReleaseAndReconcilePublishedRestart(t *testing.T) {
	for _, action := range []string{"release", "reconcile"} {
		for _, scenario := range []string{"recorded-pr", "replacement-pr", "missing-recorded-pr", "empty-recorded-pr"} {
			t.Run(action+"/"+scenario, func(t *testing.T) {
				m, p, remote := custodyManagerFixture(t)
				p.Action = action
				gitCmd(t, p.WorkDir, "push", remote, p.HeadSHA+":refs/heads/validation")
				run, _ := m.db.GetRun(p.RunID)
				if err := m.db.UpdateRunErrorStatusWithVerifiedHead(run.ID, "daemon crashed during execution", types.RunFailed, p.HeadSHA); err != nil {
					t.Fatal(err)
				}
				if scenario != "missing-recorded-pr" {
					url := "https://github.com/test/repo/pull/1"
					if scenario == "empty-recorded-pr" {
						url = ""
					}
					if err := m.db.UpdateRunPRURL(run.ID, url); err != nil {
						t.Fatal(err)
					}
				}
				before, _ := m.db.GetRun(run.ID)
				m.publicationPR = func(context.Context, *db.Repo, *db.Run, string, string) (*scm.PR, error) {
					url := "https://github.com/test/repo/pull/1"
					if scenario == "replacement-pr" {
						url = "https://github.com/test/repo/pull/2"
					}
					return &scm.PR{URL: url, HeadSHA: p.HeadSHA}, nil
				}
				result, err := m.HandleCustodyOperation(context.Background(), p)
				if scenario != "recorded-pr" {
					if err == nil || result != nil {
						t.Fatalf("unbound PR accepted: result=%+v err=%v", result, err)
					}
					after, readErr := m.db.GetRun(run.ID)
					if readErr != nil || !reflect.DeepEqual(before, after) {
						t.Fatalf("refusal changed run: before=%+v after=%+v err=%v", before, after, readErr)
					}
					for _, dir := range []string{p.WorkDir, remote, m.paths.RepoDir(p.RepoID)} {
						if got := gitOutput(t, dir, "rev-parse", "refs/heads/validation"); got != p.HeadSHA {
							t.Fatal("refusal moved a branch")
						}
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				r, _ := m.db.GetRun(p.RunID)
				if result.State != "released" || r.CustodyReturnedAt == nil || r.Error == nil || *r.Error != "daemon crashed during execution" || r.HeadSHA != p.HeadSHA {
					t.Fatalf("lost restart history: %+v", r)
				}
				if got := gitOutput(t, remote, "rev-parse", "refs/heads/validation"); got != p.HeadSHA {
					t.Fatal("published history changed")
				}
				if got := gitOutput(t, m.paths.RepoDir(p.RepoID), "rev-parse", "refs/no-mistakes/release/"+r.ID+"/"+p.HeadSHA); got != p.HeadSHA {
					t.Fatal("archive missing")
				}
			})
		}
	}
}

func TestCustodyManagerRebindRefusesChangedGenerationOrProvider(t *testing.T) {
	for _, scenario := range []string{"head", "target", "default", "registration", "pr", "other-publisher"} {
		t.Run(scenario, func(t *testing.T) {
			m, p, _ := custodyManagerFixture(t)
			parkCustodyRun(t, m, p)
			calls := 0
			original := m.publicationPR
			m.publicationPR = func(ctx context.Context, repo *db.Repo, r *db.Run, dir, branch string) (*scm.PR, error) {
				calls++
				pr, err := original(ctx, repo, r, dir, branch)
				if calls == 2 {
					switch scenario {
					case "head":
						_ = m.db.UpdateRunHeadSHA(r.ID, r.BaseSHA)
					case "default":
						_, err = m.db.UpdateRepoMetadata(repo.ID, repo.UpstreamURL, "existing")
					case "registration":
						_, err = m.db.UpdateRepoWorkingPath(repo.ID, filepath.Join(t.TempDir(), "different"))
					case "target":
						_, err = m.db.UpdateRepoMetadata(repo.ID, "https://github.com/other/repo", "main")
					case "pr":
						pr.URL = "https://github.com/test/repo/pull/2"
					case "other-publisher":
						_, err = m.db.InsertRun(repo.ID, "existing", r.HeadSHA, r.BaseSHA)
					}
				}
				return pr, err
			}
			if _, err := m.HandleCustodyOperation(context.Background(), p); err == nil {
				t.Fatal("stale publication binding accepted")
			}
			r, _ := m.db.GetRun(p.RunID)
			if r.PublicationBranch != nil || r.LastPushedSHA != nil {
				t.Fatal("refusal changed provenance")
			}
		})
	}
}

func TestCustodyReleaseRefusesPRChangedWhileArchiving(t *testing.T) {
	m, p, remote := custodyManagerFixture(t)
	p.Action = "release"
	if err := m.db.UpdateRunPRURL(p.RunID, "https://github.com/test/repo/pull/1"); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, p.WorkDir, "push", remote, p.HeadSHA+":refs/heads/validation")
	calls := 0
	m.publicationPR = func(context.Context, *db.Repo, *db.Run, string, string) (*scm.PR, error) {
		calls++
		url := "https://github.com/test/repo/pull/1"
		if calls > 1 {
			url = "https://github.com/test/repo/pull/2"
		}
		return &scm.PR{URL: url, HeadSHA: p.HeadSHA}, nil
	}
	before := gitOutput(t, m.paths.RepoDir(p.RepoID), "rev-parse", "refs/heads/validation")
	if _, err := m.HandleCustodyOperation(context.Background(), p); err == nil {
		t.Fatal("changed PR identity accepted")
	}
	if calls != 2 {
		t.Fatalf("provider calls=%d, want initial proof and archive recheck", calls)
	}
	r, _ := m.db.GetRun(p.RunID)
	if r.CustodyReturnedAt != nil {
		t.Fatal("stale PR returned custody")
	}
	if got := gitOutput(t, m.paths.RepoDir(p.RepoID), "rev-parse", "refs/heads/validation"); got != before {
		t.Fatal("gate moved after PR changed")
	}
}

// recordedPublicationFinder answers a branch search with whatever the provider
// would surface today, which is how a sibling open PR on the same source branch
// reaches a custody operation ahead of the PR the run actually published to.
type recordedPublicationFinder struct {
	discovered *scm.PR
	err        error
	calls      int
}

func (f *recordedPublicationFinder) FindPR(context.Context, string, string) (*scm.PR, error) {
	f.calls++
	return f.discovered, f.err
}

func TestPublicationPRIdentityPrefersTheRunRecordedPR(t *testing.T) {
	const recorded = "https://github.com/owner/fork/pull/7"
	recordedRun := func() *db.Run {
		url := recorded
		// The recorded URL belongs to `feature`, which is the branch release and
		// reconcile ask about; a rebind asks about a different one.
		return &db.Run{Branch: "feature", PRURL: &url}
	}
	for _, tc := range []struct {
		name    string
		run     *db.Run
		branch  string
		finder  *recordedPublicationFinder
		wantURL string
		wantErr string
	}{
		{
			// The regression: a second open PR from the same source branch, at a
			// different base, used to be the PR release and reconcile verified,
			// and its URL check refused custody even though the recorded PR was
			// open at the exact published head.
			name:    "sibling does not block the recorded identity",
			run:     recordedRun(),
			branch:  "feature",
			finder:  &recordedPublicationFinder{discovered: &scm.PR{URL: "https://github.com/owner/fork/pull/9", HeadSHA: "head"}},
			wantURL: recorded,
		},
		{
			name:    "the discovered PR is used when it is the recorded one",
			run:     recordedRun(),
			branch:  "feature",
			finder:  &recordedPublicationFinder{discovered: &scm.PR{URL: recorded, HeadSHA: "head"}},
			wantURL: recorded,
		},
		{
			// A failed listing is not a reason to refuse a run that can name its
			// own PR: the caller still proves that PR open and at the head.
			name:    "a failed lookup does not outrank the recorded identity",
			run:     recordedRun(),
			branch:  "feature",
			finder:  &recordedPublicationFinder{err: context.DeadlineExceeded},
			wantURL: recorded,
		},
		{
			// The other regression: a parked run that already has a PR, rebound
			// to a different existing PR branch, must be given that branch's PR.
			// Preference for a recorded identity that belongs to another branch
			// makes the head proof compare the old PR against the new branch and
			// refuses a rebind an operator is entitled to make.
			name:    "a rebind to another branch follows discovery",
			run:     recordedRun(),
			branch:  "rebound",
			finder:  &recordedPublicationFinder{discovered: &scm.PR{URL: "https://github.com/owner/fork/pull/11", HeadSHA: "head"}},
			wantURL: "https://github.com/owner/fork/pull/11",
		},
		{
			// A run that never rebound has nothing recorded, so discovery is all
			// there is and a failed lookup still refuses rather than guessing.
			name:    "discovery stands with nothing recorded",
			run:     &db.Run{Branch: "feature"},
			branch:  "feature",
			finder:  &recordedPublicationFinder{discovered: &scm.PR{URL: "https://github.com/owner/fork/pull/3", HeadSHA: "head"}},
			wantURL: "https://github.com/owner/fork/pull/3",
		},
		{
			name:    "a failed lookup refuses with nothing recorded",
			run:     &db.Run{Branch: "feature"},
			branch:  "feature",
			finder:  &recordedPublicationFinder{err: context.DeadlineExceeded},
			wantErr: "publication requires an existing open PR",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := publicationPRIdentity(context.Background(), tc.finder, tc.run, tc.branch)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("got err %v, want a refusal containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("choose publication PR: %v", err)
			}
			if got.URL != tc.wantURL {
				t.Fatalf("identity = %q, want %q", got.URL, tc.wantURL)
			}
			if tc.finder.calls != 1 {
				t.Fatalf("FindPR calls = %d, want one branch lookup", tc.finder.calls)
			}
		})
	}
}

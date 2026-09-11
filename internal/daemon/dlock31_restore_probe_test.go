package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type dlock31Recovery struct {
	d                         *db.DB
	p                         *paths.Paths
	m                         *RunManager
	run                       *db.Run
	work, published, retained string
	review, ci                *mockPassStep
}

func newDLOCK31Recovery(t *testing.T) *dlock31Recovery {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_ALLOW_PROTOCOL", "file")
	f := &dlock31Recovery{p: paths.WithRoot(t.TempDir()), review: &mockPassStep{name: types.StepReview}, ci: &mockPassStep{name: types.StepCI}}
	if err := f.p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(f.p.DB())
	if err != nil {
		t.Fatal(err)
	}
	f.d = d
	t.Cleanup(func() { d.Close() })
	work := t.TempDir()
	gitCmd(t, work, "init", "--initial-branch=main")
	gitCmd(t, work, "config", "user.name", "test")
	gitCmd(t, work, "config", "user.email", "test@example.invalid")
	gitCmd(t, work, "commit", "--allow-empty", "-m", "published")
	f.published = gitOutput(t, work, "rev-parse", "HEAD")
	gitCmd(t, work, "commit", "--allow-empty", "-m", "retained repair")
	f.retained = gitOutput(t, work, "rev-parse", "HEAD")
	repo, err := d.InsertRepo(work, "https://github.com/test/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	gate := f.p.RepoDir(repo.ID)
	gitCmd(t, work, "clone", "--bare", "--no-hardlinks", work, gate)
	f.run, err = d.InsertRun(repo.ID, "feature", f.published, f.published)
	if err != nil {
		t.Fatal(err)
	}
	f.work = filepath.Join(t.TempDir(), "retained")
	gitCmd(t, gate, "worktree", "add", "--detach", f.work, f.retained)
	prepareDLOCK31RecoveryRows(t, f)
	return f
}

func prepareDLOCK31RecoveryRows(t *testing.T, f *dlock31Recovery) {
	t.Helper()
	review, err := f.d.InsertStepResult(f.run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	ci, err := f.d.InsertStepResult(f.run.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[{"id":"quota","severity":"error","action":"ask-user","description":"retained repair"}],"summary":"CI repair attestation is unsettled"}`
	for _, err := range []error{
		f.d.SetRunWorktreeDir(f.run.ID, f.work), f.d.UpdateRunStatus(f.run.ID, types.RunRunning), f.d.SetRunAwaitingAgent(f.run.ID),
		f.d.UpdateRunReviewApprovedHeadSHA(f.run.ID, f.published), f.d.CompleteStepWithStatus(review.ID, types.StepStatusCompleted, 0, 1, ""),
		f.d.StartStep(ci.ID), f.d.SetStepFindings(ci.ID, findings), f.d.UpdateStepStatusWithDuration(ci.ID, types.StepStatusFixReview, 1),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.d.InsertStepRound(ci.ID, 1, "auto_fix", &findings, nil, 1); err != nil {
		t.Fatal(err)
	}
	f.run, err = f.d.GetRun(f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := pipeline.BindRetainedCIRepair(context.Background(), f.d, f.run, ci.ID, f.p.RepoDir(f.run.RepoID), f.work, f.retained); err != nil {
		t.Fatal(err)
	}
	// Config/agent construction is exercised, but all fetches remain local and
	// no agent process is invoked. The mock pipeline steps own execution below.
	mockClaude := writeMockClaude(t, t.TempDir())
	if err := os.WriteFile(f.p.ConfigFile(), []byte("agent: claude\nagent_path_override:\n  claude: "+mockClaude+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	originalFetch := fetchRecoveredRemoteBranch
	fetchRecoveredRemoteBranch = func(ctx context.Context, dir, _, branch string) error {
		_, err := git.Run(ctx, dir, "update-ref", "refs/remotes/origin/"+branch, f.published)
		return err
	}
	t.Cleanup(func() { fetchRecoveredRemoteBranch = originalFetch })
	f.m = NewRunManager(f.d, f.p, func() []pipeline.Step { return []pipeline.Step{f.review, f.ci} })
}

func TestDLOCK31RecoveryRestoresOriginalGateWithoutReview(t *testing.T) {
	f := newDLOCK31Recovery(t)
	plan, err := f.m.prepareRecoveredRun(context.Background(), f.run)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.agent.Close()
	admitted, err := f.d.GetRun(f.run.ID)
	if err != nil || admitted.HeadSHA != f.published || plan.run.HeadSHA != f.published {
		t.Fatalf("recovery hid head mismatch: %+v %v", admitted, err)
	}
	executor := pipeline.NewExecutor(f.d, f.p, plan.cfg, plan.agent, plan.steps, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- executor.Resume(ctx, plan.run, plan.repo, plan.workDir) }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if err := executor.Respond(types.StepCI, types.ActionFix, []string{"quota"}); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("original gate was not restored")
		}
		time.Sleep(time.Millisecond)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	after, err := f.d.GetRun(f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.ID != f.run.ID || after.RepoID != f.run.RepoID || after.Branch != f.run.Branch {
		t.Fatalf("restoration rewrote owner/head: %+v", after)
	}
	if f.review.execCnt.Load() != 0 || f.ci.execCnt.Load() != 1 {
		t.Fatalf("review=%d ci=%d", f.review.execCnt.Load(), f.ci.execCnt.Load())
	}
	if gitOutput(t, f.work, "rev-parse", "HEAD") != f.retained {
		t.Fatal("restoration lost correction")
	}
	binding, err := f.d.RetainedCIRepair(f.run.ID)
	if err != nil || binding == nil {
		t.Fatalf("recovery lost pending publication: %+v %v", binding, err)
	}
	rounds, err := f.d.GetRoundsByStep(binding.StepID)
	if err != nil || len(rounds) != 2 {
		t.Fatalf("original round history not preserved: rounds=%d error=%v", len(rounds), err)
	}
}

func TestDLOCK31RecoveryFailsClosed(t *testing.T) {
	for _, mutation := range []string{"unbound", "dirty", "moved", "divergent", "rewound_to_recorded", "moved_anchor", "symbolic_anchor"} {
		t.Run(mutation, func(t *testing.T) {
			f := newDLOCK31Recovery(t)
			ref := "refs/no-mistakes/ci-repair/" + f.run.ID
			switch mutation {
			case "unbound":
				if err := f.d.ClearRetainedCIRepair(f.run.ID, f.retained); err != nil {
					t.Fatal(err)
				}
			case "dirty":
				if err := os.WriteFile(filepath.Join(f.work, "dirty"), []byte("keep"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "moved":
				gitCmd(t, f.work, "commit", "--allow-empty", "-m", "later unrelated work")
			case "divergent":
				gitCmd(t, f.work, "checkout", "--detach", f.published)
				gitCmd(t, f.work, "commit", "--allow-empty", "-m", "different repair")
			case "rewound_to_recorded":
				gitCmd(t, f.work, "checkout", "--detach", f.published)
			case "moved_anchor":
				gitCmd(t, f.work, "update-ref", ref, f.published)
			case "symbolic_anchor":
				gitCmd(t, f.work, "symbolic-ref", ref, "refs/heads/main")
			}
			before := gitOutput(t, f.work, "rev-parse", "HEAD")
			if plan, err := f.m.prepareRecoveredRun(context.Background(), f.run); err == nil {
				plan.agent.Close()
				t.Fatal("unsafe recovery accepted")
			}
			if gitOutput(t, f.work, "rev-parse", "HEAD") != before || f.review.execCnt.Load() != 0 || f.ci.execCnt.Load() != 0 {
				t.Fatal("refusal mutated or executed pipeline")
			}
		})
	}
}

func TestDLOCK31RecoveryEqualHeadControl(t *testing.T) {
	f := newDLOCK31Recovery(t)
	// A normally settled run has matching recorded/worktree heads and no
	// pending repair. It must retain the original supported recovery path.
	if err := f.d.UpdateRunPublication(f.run.ID, db.PushBinding{HeadSHA: f.retained}); err != nil {
		t.Fatal(err)
	}
	run, err := f.d.GetRun(f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := f.m.prepareRecoveredRun(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	plan.agent.Close()
}

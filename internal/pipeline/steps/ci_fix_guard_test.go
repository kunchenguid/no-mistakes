package steps

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func runGit(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=test",
		"GIT_AUTHOR_EMAIL=test@test.com",
		"GIT_COMMITTER_NAME=test",
		"GIT_COMMITTER_EMAIL=test@test.com",
		"GIT_EDITOR=true",
	)
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func newCIGuardFixture(t *testing.T, conflict bool, act func(dir string), conclusion string) *ciRepairFixture {
	t.Helper()
	f := newCIRepairFixture(t, false, nil)
	gitCmd(t, f.dir, "checkout", "main")
	name := "main-only.txt"
	if conflict {
		name = "feature.txt"
	}
	if err := os.WriteFile(filepath.Join(f.dir, name), []byte("main side\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, f.dir, "add", "-A")
	gitCmd(t, f.dir, "commit", "-m", "advance main")
	gitCmd(t, f.dir, "checkout", "feature")
	f.sctx.Agent = &mockAgent{name: "native", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		act(opts.CWD)
		return &agent.Result{Output: json.RawMessage(conclusion)}, nil
	}}
	f.sctx.Ctx = context.Background()
	f.sctx.PreviousFindings = `{"findings":[{"id":"ci-1","severity":"error","description":"failed","action":"auto-fix","category":"ci-check","check":"test","check_id":"github-check-run:42"}]}`
	return f
}

func (f *ciRepairFixture) repairRound(t *testing.T) *pipeline.StepOutcome {
	t.Helper()
	host := &completionSnapshotHost{checks: []scm.Check{{Name: "test", ProviderID: "github-check-run:42", Bucket: scm.CheckBucketFail, CompletedAt: time.Now()}}}
	outcome, err := (&CIStep{}).repairFromFindings(f.sctx, host, &scm.PR{Number: "42"})
	if err != nil {
		t.Fatalf("repair round error: %v\nlog:\n%s", err, f.log())
	}
	return outcome
}

func gitPathPresent(t *testing.T, dir, name string) bool {
	t.Helper()
	path := gitCmd(t, dir, "rev-parse", "--git-path", name)
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	_, err := os.Stat(path)
	return err == nil
}

func TestCIRepair_UnfinishedGitOperationIsNotConcluded(t *testing.T) {
	for _, tc := range []struct {
		name    string
		start   func(dir string)
		present string
		want    string
	}{
		{
			name: "merge",
			start: func(dir string) {
				_, _ = runGit(t, dir, "merge", "--no-edit", "main")
			},
			present: "MERGE_HEAD",
			want:    "finish or abort",
		},
		{
			name: "rebase",
			start: func(dir string) {
				_, _ = runGit(t, dir, "rebase", "main")
			},
			present: "rebase-merge",
			want:    "git rebase --abort",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCIGuardFixture(t, true, tc.start, `{"summary":"left the operation unfinished","code_change_needed":true}`)
			outcome := f.repairRound(t)
			if got := f.remoteHead(t); got != f.headSHA {
				t.Errorf("remote moved to %s", got)
			}
			if !gitPathPresent(t, f.dir, tc.present) {
				t.Errorf("%s was removed", tc.present)
			}
			if after := gitCmd(t, f.dir, "diff", "--name-only", "--diff-filter=U"); after != "feature.txt" {
				t.Errorf("unmerged paths = %q, want feature.txt", after)
			}
			if outcome == nil || !outcome.NeedsApproval {
				t.Fatalf("outcome = %#v, want the repair parked", outcome)
			}
			if !strings.Contains(outcome.Findings, tc.want) {
				t.Fatalf("findings = %s, want %q", outcome.Findings, tc.want)
			}
		})
	}
}

func TestStagePipelineChanges_RefusesUnfinishedMerge(t *testing.T) {
	f := newCIGuardFixture(t, true, func(string) {}, `{}`)
	if _, err := runGit(t, f.dir, "merge", "--no-edit", "main"); err == nil || !gitPathPresent(t, f.dir, "MERGE_HEAD") {
		t.Fatal("fixture error: no unfinished merge")
	}
	head := gitCmd(t, f.dir, "rev-parse", "HEAD")
	if err := stagePipelineChanges(f.sctx); err == nil {
		t.Fatal("staging succeeded over an unfinished merge")
	}
	if unmerged := gitCmd(t, f.dir, "diff", "--name-only", "--diff-filter=U"); unmerged != "feature.txt" {
		t.Errorf("unmerged paths = %q, want feature.txt", unmerged)
	}
	if got := gitCmd(t, f.dir, "rev-parse", "HEAD"); got != head {
		t.Errorf("HEAD moved from %s to %s", head, got)
	}
	if !gitPathPresent(t, f.dir, "MERGE_HEAD") {
		t.Error("the unfinished merge was removed")
	}
}

func TestCIRepair_DeclinedDirtyWorkIsNotCommitted(t *testing.T) {
	f := newCIGuardFixture(t, false, func(dir string) {
		if err := os.WriteFile(filepath.Join(dir, "dirty.txt"), []byte("keep these bytes\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}, `{"summary":"external failure","code_change_needed":false}`)
	outcome := f.repairRound(t)
	if got := f.remoteHead(t); got != f.headSHA {
		t.Errorf("remote moved to %s", got)
	}
	if f.localHead(t) != f.headSHA {
		t.Error("declined work was committed")
	}
	if staged := gitCmd(t, f.dir, "diff", "--cached", "--name-only"); staged != "" {
		t.Errorf("staged paths = %q, want nothing staged", staged)
	}
	got, err := os.ReadFile(filepath.Join(f.dir, "dirty.txt"))
	if err != nil || string(got) != "keep these bytes\n" {
		t.Fatalf("dirty bytes = %q, %v", got, err)
	}
	if outcome == nil || !outcome.NeedsApproval {
		t.Fatalf("outcome = %#v, want the declined repair parked", outcome)
	}
	parsed, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if !types.HasAskUserFindings(parsed) {
		t.Fatalf("findings = %+v, want an ask-user park", parsed.Items)
	}
}

func TestCIRepair_DeclinedAgentCommitIsNotPublished(t *testing.T) {
	f := newCIGuardFixture(t, false, func(dir string) {
		if err := os.WriteFile(filepath.Join(dir, "agent.txt"), []byte("agent commit\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if out, err := runGit(t, dir, "add", "agent.txt"); err != nil {
			t.Fatalf("stage: %v: %s", err, out)
		}
		if out, err := runGit(t, dir, "commit", "-m", "agent commit"); err != nil {
			t.Fatalf("commit: %v: %s", err, out)
		}
	}, `{"summary":"external failure","code_change_needed":false}`)
	outcome := f.repairRound(t)
	if got := f.remoteHead(t); got != f.headSHA {
		t.Errorf("remote moved to %s", got)
	}
	if outcome == nil || !outcome.NeedsApproval {
		t.Fatalf("outcome = %#v, want the declined repair parked", outcome)
	}
}

func TestCIRepair_ResolvedMergeStillPublishes(t *testing.T) {
	f := newCIGuardFixture(t, true, func(dir string) {
		out, err := runGit(t, dir, "merge", "--squash", "main")
		if gitPathPresent(t, dir, "MERGE_HEAD") {
			t.Fatalf("squash merge left MERGE_HEAD: %s", out)
		}
		if err != nil && !gitPathPresent(t, dir, "SQUASH_MSG") {
			t.Fatalf("squash merge failed: %v: %s", err, out)
		}
		if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("main side\nfeature\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if out, err := runGit(t, dir, "add", "-A"); err != nil {
			t.Fatalf("stage resolved merge: %v: %s", err, out)
		}
		if out, err := runGit(t, dir, "commit", "-m", "resolve the merge"); err != nil {
			t.Fatalf("commit resolved merge: %v: %s", err, out)
		}
	}, `{"summary":"merged main and resolved the conflict","code_change_needed":true}`)
	if gitPathPresent(t, f.dir, "MERGE_HEAD") {
		t.Fatal("fixture error: the merge is still unfinished")
	}
	outcome := f.repairRound(t)
	local := f.localHead(t)
	if local == f.headSHA || f.remoteHead(t) != local {
		t.Fatalf("remote = %s, local = %s, original = %s\nlog:\n%s", f.remoteHead(t), local, f.headSHA, f.log())
	}
	published := gitCmd(t, f.upstream, "show", "refs/heads/feature:feature.txt")
	if strings.Contains(published, "<<<<<<<") {
		t.Fatalf("published content carries conflict markers:\n%s", published)
	}
	if outcome != nil && outcome.NeedsApproval {
		t.Fatalf("outcome = %#v, want monitoring to resume after publication", outcome)
	}
}

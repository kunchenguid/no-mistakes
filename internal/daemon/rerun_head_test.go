package daemon

import (
	"context"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestRerunChecksCallerHeadAgainstSelectedHead(t *testing.T) {
	for _, preserved := range []bool{false, true} {
		name := "gate"
		if preserved {
			name = "preserved"
		}
		for _, matches := range []bool{false, true} {
			relation := "differs"
			if matches {
				relation = "matches"
			}
			t.Run(name+"/"+relation, func(t *testing.T) {
				step := &mockPassStep{name: types.StepReview}
				p, d := startTestDaemonWithSteps(t, func() []pipeline.Step { return []pipeline.Step{step} })
				repo, submitted := setupTestGitRepo(t, p, d, "rerun-head-repo")
				prior, err := d.InsertRun(repo.ID, "main", submitted, submitted)
				if err != nil {
					t.Fatal(err)
				}
				selected := submitted
				if preserved {
					gitCmd(t, repo.WorkingPath, "commit", "--allow-empty", "-m", "pipeline rewrite")
					selected = gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
					gitCmd(t, repo.WorkingPath, "push", "gate", selected+":"+custody.RecoveryRef(prior.ID))
				}
				if err := d.UpdateRunStatusWithVerifiedHead(prior.ID, types.RunFailed, selected); err != nil {
					t.Fatal(err)
				}
				if !matches {
					gitCmd(t, repo.WorkingPath, "commit", "--allow-empty", "--amend", "-m", "corrected local head")
				}
				callerHead := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
				if dirty, err := git.HasUncommittedChanges(context.Background(), repo.WorkingPath); err != nil || dirty {
					t.Fatalf("caller worktree is dirty: %v, err=%v", dirty, err)
				}
				client, err := ipc.Dial(p.Socket())
				if err != nil {
					t.Fatal(err)
				}
				defer client.Close()
				var result ipc.RerunResult
				// Use the wire shape so the regression runs on the pre-fix protocol.
				err = client.Call(ipc.MethodRerun, map[string]string{
					"repo_id": repo.ID, "branch": "main", "caller_head_sha": callerHead,
				}, &result)
				if matches {
					if err != nil {
						t.Fatal(err)
					}
					run := waitForRunTerminalState(t, d, result.RunID)
					if run.SubmittedHeadSHA == nil || *run.SubmittedHeadSHA != selected || run.Status != types.RunCompleted {
						t.Fatalf("matching rerun did not complete at selected head %s: %+v", selected, run)
					}
				} else {
					if err == nil {
						t.Fatalf("rerun started %s at selected head %s despite clean caller head %s", result.RunID, selected, callerHead)
					}
					for _, want := range []string{selected, callerHead, "no-mistakes axi status", "no-mistakes axi run"} {
						if !strings.Contains(err.Error(), want) {
							t.Errorf("refusal %q missing %q", err, want)
						}
					}
					runs, err := d.GetRunsByRepo(repo.ID)
					if err != nil || len(runs) != 1 || step.execCnt.Load() != 0 {
						t.Fatalf("refused rerun performed work: runs=%d executions=%d err=%v", len(runs), step.execCnt.Load(), err)
					}
				}
				if got := gitOutput(t, p.RepoDir(repo.ID), "rev-parse", "refs/heads/main"); got != submitted {
					t.Fatalf("gate branch moved to %s, want %s", got, submitted)
				}
				if got := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD"); got != callerHead {
					t.Fatalf("caller head moved to %s, want %s", got, callerHead)
				}
			})
		}
	}
}

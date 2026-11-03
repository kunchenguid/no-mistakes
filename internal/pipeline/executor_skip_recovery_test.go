package pipeline

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func skipRecoverySteps(t *testing.T, callsPath string, gate types.StepName) []Step {
	t.Helper()
	var steps []Step
	for _, name := range types.AllSteps() {
		steps = append(steps, &adaptiveCallStep{name: name, fn: func(sctx *StepContext) (*StepOutcome, error) {
			f, err := os.OpenFile(callsPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
			if err != nil {
				return nil, err
			}
			_, err = f.WriteString(string(name) + "\n")
			closeErr := f.Close()
			if err != nil {
				return nil, err
			}
			if closeErr != nil {
				return nil, closeErr
			}
			outcome := &StepOutcome{NeedsApproval: name == gate, ReviewApprovedHeadSHA: sctx.Run.HeadSHA}
			if name == gate {
				outcome.Findings = `{"findings":[],"summary":"acknowledge validation"}`
			}
			return outcome, nil
		}})
	}
	return steps
}

func TestExecutor_SkipPersistenceFailureExecutesNoSteps(t *testing.T) {
	database, p, run, repo := setupTest(t)
	control, err := sql.Open("sqlite", p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	if _, err := control.Exec(`CREATE TRIGGER deny_skip BEFORE UPDATE OF status ON step_results
		WHEN NEW.status = 'skipped' BEGIN SELECT RAISE(FAIL, 'skip write failed'); END`); err != nil {
		t.Fatal(err)
	}
	review, test, push := newPassStep(types.StepReview), newPassStep(types.StepTest), newPassStep(types.StepPush)
	executor := NewExecutor(database, p, nil, nil, []Step{review, test, push}, nil)
	executor.SetSkippedSteps([]types.StepName{types.StepPush})
	if err := executor.Execute(context.Background(), run, repo, t.TempDir()); err == nil || !strings.Contains(err.Error(), "skip write failed") {
		t.Fatalf("Execute() error = %v, want the skip persistence failure", err)
	}
	if review.callCount() != 0 || test.callCount() != 0 || push.callCount() != 0 {
		t.Fatal("a step ran before requested skips were durable")
	}
}

// Exit at the durably emitted park, without cancelling the run. Recovery must
// use the database alone, exactly as after loss of the original daemon process.
func TestExecutorSkipRecoveryProcess(t *testing.T) {
	root := os.Getenv("NM_SKIP_FIXTURE_ROOT")
	if root == "" {
		return
	}
	p := paths.WithRoot(root)
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.GetRun(os.Getenv("NM_SKIP_FIXTURE_RUN"))
	if err != nil {
		t.Fatal(err)
	}
	repo, err := database.GetRepo(run.RepoID)
	if err != nil {
		t.Fatal(err)
	}
	gate := types.StepName(os.Getenv("NM_SKIP_FIXTURE_GATE"))
	executor := NewExecutor(database, p, nil, nil, skipRecoverySteps(t, filepath.Join(root, "calls"), gate), func(e ipc.Event) {
		if e.StepName != nil && *e.StepName == gate && e.Status != nil && *e.Status == string(types.StepStatusAwaitingApproval) {
			os.Exit(0)
		}
	})
	if os.Getenv("NM_SKIP_FIXTURE_SKIPS") == "push" {
		executor.SetSkippedSteps([]types.StepName{types.StepPush})
	}
	if os.Getenv("NM_SKIP_FIXTURE_SKIPS") == "true" {
		executor.SetSkippedSteps([]types.StepName{types.StepRebase, types.StepPush, types.StepPR, types.StepCI})
	}
	if err := executor.Execute(context.Background(), run, repo, filepath.Join(root, "checkout")); err != nil {
		t.Fatal(err)
	}
	t.Fatal("fixture completed without its required gate")
}

func TestExecutor_RequestedSkipsSurviveParkAndRecovery(t *testing.T) {
	for _, gate := range []types.StepName{types.StepReview, types.StepTest} {
		for _, recover := range []bool{false, true} {
			for _, skipped := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/recovery=%t/skips=%t", gate, recover, skipped), func(t *testing.T) {
					database, p, run, repo := setupTest(t)
					workDir := filepath.Join(p.Root(), "checkout")
					if err := os.Mkdir(workDir, 0o700); err != nil {
						t.Fatal(err)
					}
					initGitRepo(t, workDir)
					head, err := git.HeadSHA(context.Background(), workDir)
					if err != nil {
						t.Fatal(err)
					}
					if err := database.UpdateRunHeadSHA(run.ID, head); err != nil {
						t.Fatal(err)
					}
					run.HeadSHA = head
					ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
					defer cancel()
					var executor *Executor
					done := make(chan error, 1)
					if recover {
						if err := database.Close(); err != nil {
							t.Fatal(err)
						}
						cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestExecutorSkipRecoveryProcess$")
						cmd.Env = append(os.Environ(), "NM_SKIP_FIXTURE_ROOT="+p.Root(), "NM_SKIP_FIXTURE_RUN="+run.ID, "NM_SKIP_FIXTURE_GATE="+string(gate), fmt.Sprintf("NM_SKIP_FIXTURE_SKIPS=%t", skipped))
						if out, err := cmd.CombinedOutput(); err != nil {
							t.Fatalf("parked process: %v\n%s", err, out)
						}
						database, err = db.Open(p.DB())
						if err != nil {
							t.Fatal(err)
						}
						t.Cleanup(func() { database.Close() })
						run, err = database.GetRun(run.ID)
						if err != nil {
							t.Fatal(err)
						}
						executor = NewExecutor(database, p, nil, nil, skipRecoverySteps(t, filepath.Join(p.Root(), "calls"), ""), nil)
						if err := ValidateRecoveredRun(database, run, executor.steps); err != nil {
							t.Fatalf("parked fixture is not recoverable: %v", err)
						}
						go func() { done <- executor.Resume(ctx, run, repo, workDir) }()
					} else {
						executor = NewExecutor(database, p, nil, nil, skipRecoverySteps(t, filepath.Join(p.Root(), "calls"), gate), nil)
						if skipped {
							executor.SetSkippedSteps([]types.StepName{types.StepRebase, types.StepPush, types.StepPR, types.StepCI})
						}
						go func() { done <- executor.Execute(ctx, run, repo, workDir) }()
						waitForStepStatus(t, database, run.ID, gate, types.StepStatusAwaitingApproval)
					}
					finished := false
					defer func() {
						cancel()
						if !finished {
							select {
							case <-done:
							case <-time.After(5 * time.Second):
								t.Error("fixture executor did not stop")
							}
						}
					}()
					deadline := time.Now().Add(5 * time.Second)
					for {
						select {
						case err := <-done:
							finished = true
							t.Fatalf("executor ended before approval: %v", err)
						default:
						}
						if err := executor.Respond(gate, types.ActionApprove, nil); err == nil {
							break
						} else if time.Now().After(deadline) {
							t.Fatalf("gate refused approval: %v", err)
						}
						time.Sleep(10 * time.Millisecond)
					}
					waitExecutorDone(t, done)
					finished = true
					calls, err := os.ReadFile(filepath.Join(p.Root(), "calls"))
					if err != nil {
						t.Fatal(err)
					}
					want := "intent\nrebase\nreview\ntest\ndocument\nlint\npush\npr\nci\n"
					if skipped {
						want = "intent\nreview\ntest\ndocument\nlint\n"
					}
					if string(calls) != want {
						t.Fatalf("executed steps = %q, want %q", calls, want)
					}
					results, err := database.GetStepsByRun(run.ID)
					if err != nil {
						t.Fatal(err)
					}
					for _, result := range results {
						wantStatus := types.StepStatusCompleted
						if skipped && slices.Contains([]types.StepName{types.StepRebase, types.StepPush, types.StepPR, types.StepCI}, result.StepName) {
							wantStatus = types.StepStatusSkipped
						}
						if result.Status != wantStatus {
							t.Errorf("%s = %s, want %s", result.StepName, result.Status, wantStatus)
						}
					}
					completed, err := database.GetRun(run.ID)
					if err != nil || completed.Status != types.RunCompleted {
						t.Fatalf("completed run = %+v, error = %v", completed, err)
					}
				})
			}
		}
	}
}

func TestExecutor_SkipRemainingPreservesRequestedSkipsAfterRecovery(t *testing.T) {
	for _, gate := range []types.StepName{types.StepRebase, types.StepIntent} {
		t.Run(string(gate), func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			workDir := filepath.Join(p.Root(), "checkout")
			if err := os.Mkdir(workDir, 0o700); err != nil {
				t.Fatal(err)
			}
			initGitRepo(t, workDir)
			head, err := git.HeadSHA(context.Background(), workDir)
			if err != nil {
				t.Fatal(err)
			}
			if err := database.UpdateRunHeadSHA(run.ID, head); err != nil {
				t.Fatal(err)
			}
			if err := database.Close(); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestExecutorSkipRecoveryProcess$")
			cmd.Env = append(os.Environ(), "NM_SKIP_FIXTURE_ROOT="+p.Root(), "NM_SKIP_FIXTURE_RUN="+run.ID, "NM_SKIP_FIXTURE_GATE="+string(gate), "NM_SKIP_FIXTURE_SKIPS=push")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("parked process: %v\n%s", err, out)
			}
			database, err = db.Open(p.DB())
			if err != nil {
				t.Fatal(err)
			}
			defer database.Close()
			run, err = database.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			before, err := database.GetStepsByRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			var skippedPush *db.StepResult
			for _, result := range before {
				if result.StepName == types.StepPush {
					skippedPush = result
				}
			}
			if skippedPush == nil || skippedPush.Status != types.StepStatusSkipped {
				t.Fatal("requested push skip was not durable before recovery")
			}
			control, err := sql.Open("sqlite", p.DB())
			if err != nil {
				t.Fatal(err)
			}
			defer control.Close()
			if _, err := control.Exec(`CREATE TRIGGER deny_reskip BEFORE UPDATE OF status ON step_results
				WHEN OLD.status = 'skipped' BEGIN SELECT RAISE(FAIL, 'existing skip rewritten'); END`); err != nil {
				t.Fatal(err)
			}
			steps := skipRecoverySteps(t, filepath.Join(p.Root(), "calls"), "")
			for _, step := range steps {
				if step.Name() == types.StepRebase {
					rebase := step.(*adaptiveCallStep)
					original := rebase.fn
					rebase.fn = func(sctx *StepContext) (*StepOutcome, error) {
						outcome, err := original(sctx)
						if err == nil {
							outcome.SkipRemaining = true
						}
						return outcome, err
					}
				}
			}
			executor := NewExecutor(database, p, nil, nil, steps, nil)
			if err := ValidateRecoveredRun(database, run, steps); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- executor.Resume(ctx, run, repo, workDir) }()
			action := types.ActionApprove
			if gate == types.StepRebase {
				action = types.ActionFix
			}
			deadline := time.Now().Add(5 * time.Second)
			for {
				if err := executor.Respond(gate, action, nil); err == nil {
					break
				} else if time.Now().After(deadline) {
					cancel()
					waitExecutorDone(t, done)
					t.Fatalf("recovered gate refused %s: %v", action, err)
				}
				time.Sleep(10 * time.Millisecond)
			}
			waitExecutorDone(t, done)
			calls, err := os.ReadFile(filepath.Join(p.Root(), "calls"))
			if err != nil {
				t.Fatal(err)
			}
			want := "intent\nrebase\n"
			if gate == types.StepRebase {
				want += "rebase\n"
			}
			if string(calls) != want {
				t.Fatalf("executed steps = %q, want %q", calls, want)
			}
			results, err := database.GetStepsByRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, result := range results {
				wantStatus := types.StepStatusSkipped
				if result.StepName == types.StepIntent || result.StepName == types.StepRebase {
					wantStatus = types.StepStatusCompleted
				}
				if result.Status != wantStatus {
					t.Errorf("%s status = %s, want %s", result.StepName, result.Status, wantStatus)
				}
				if result.StepName == types.StepPush && (result.ID != skippedPush.ID || result.CompletedAt == nil || skippedPush.CompletedAt == nil || *result.CompletedAt != *skippedPush.CompletedAt) {
					t.Error("recovery rewrote the existing push skip")
				}
			}
			completed, err := database.GetRun(run.ID)
			if err != nil || completed.Status != types.RunCompleted {
				t.Fatalf("completed run = %+v, error = %v", completed, err)
			}
		})
	}
}

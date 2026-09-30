package pipeline

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestExecutorTerminalCIObservationWaitsForPostStepGuard(t *testing.T) {
	for _, state := range []string{"merged", "closed"} {
		t.Run(state, func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			guardSawTerminal := false
			executor := NewExecutor(database, p, nil, nil, []Step{
				&adaptiveCallStep{name: types.StepCI, fn: func(sctx *StepContext) (*StepOutcome, error) {
					if err := database.UpdateRunPRState(run.ID, state); err != nil {
						return nil, err
					}
					sctx.Run.PRState = &state
					return &StepOutcome{}, nil
				}},
			}, nil)
			executor.SetPRContextGuard(func(sctx *StepContext, _ types.StepName) (PRContextDecision, error) {
				if sctx.Run.PRState == nil {
					return PRContextDecision{Target: PRTargetSelection{TargetBranch: "main"}}, nil
				}
				guardSawTerminal = true
				current, err := database.GetRun(run.ID)
				if err != nil {
					return PRContextDecision{}, err
				}
				ci, err := database.GetStepsByRun(run.ID)
				if err != nil {
					return PRContextDecision{}, err
				}
				if current.Status != types.RunRunning || ci[0].Status != types.StepStatusRunning {
					t.Errorf("before live comparison: run=%s ci=%s, want both running", current.Status, ci[0].Status)
				}
				return PRContextDecision{}, fmt.Errorf("live comparison failed")
			})
			if err := executor.Execute(context.Background(), run, repo, t.TempDir()); err == nil {
				t.Fatal("failed comparison completed run")
			}
			if !guardSawTerminal {
				t.Fatal("guard never checked terminal observation")
			}
			current, _ := database.GetRun(run.ID)
			if current.Status == types.RunCompleted {
				t.Fatal("failed comparison left durable completion")
			}
		})
	}
}

type terminalObservationReconciler struct{ calls int }

func (*terminalObservationReconciler) Name() types.StepName { return types.StepCI }
func (s *terminalObservationReconciler) Execute(*StepContext) (*StepOutcome, error) {
	s.calls++
	if s.calls == 1 {
		return &StepOutcome{NeedsApproval: true, Findings: `{"findings":[{"id":"ci-old","severity":"warning","description":"old CI observation","action":"ask-user"}]}`}, nil
	}
	return &StepOutcome{Findings: `{"findings":[{"id":"ci-current","severity":"info","description":"current support proof","action":"no-op"}]}`}, nil
}
func (*terminalObservationReconciler) ReconcileApprovalGate(sctx *StepContext) (bool, error) {
	state := "closed"
	if err := sctx.DB.UpdateRunPRState(sctx.Run.ID, state); err != nil {
		return false, err
	}
	sctx.Run.PRState = &state
	return true, nil
}

func TestExecutorReconciledTerminalCICollectsCurrentSupport(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := completionFixture(t, database, run)
	step := &terminalObservationReconciler{}
	executor := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	executor.SetGateReconcileTimings(time.Millisecond, time.Second)
	if err := executor.Execute(context.Background(), run, repo, workDir); err != nil {
		t.Fatal(err)
	}
	if step.calls != 2 {
		t.Fatalf("CI executions=%d, want fresh support execution after terminal reconciliation", step.calls)
	}
	ci, _ := database.GetStepsByRun(run.ID)
	if ci[0].FindingsJSON == nil || *ci[0].FindingsJSON == "" {
		t.Fatal("current support was not recorded")
	}
}

func TestExecutorRecoveredTerminalCIGateCollectsCurrentSupport(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := completionFixture(t, database, run)
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	ci, err := database.InsertStepResult(run.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.StartStep(ci.ID); err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[{"id":"ci-old","severity":"warning","description":"old CI observation","action":"ask-user"}]}`
	if err := database.SetStepFindings(ci.ID, findings); err != nil {
		t.Fatal(err)
	}
	if _, err := database.InsertStepRound(ci.ID, 1, "initial", &findings, nil, 1); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateStepStatusWithDuration(ci.ID, types.StepStatusAwaitingApproval, 1); err != nil {
		t.Fatal(err)
	}
	if err := database.SetRunAwaitingAgent(run.ID); err != nil {
		t.Fatal(err)
	}
	run, err = database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	step := &terminalObservationReconciler{calls: 1}
	executor := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	if err := executor.Resume(context.Background(), run, repo, workDir); err != nil {
		t.Fatal(err)
	}
	if step.calls != 2 {
		t.Fatalf("CI executions=%d, want recovered gate to collect current support", step.calls)
	}
}

func TestExecutorTerminalReconciliationPreservesUnprovenCISupportGate(t *testing.T) {
	database, p, run, repo := setupTest(t)
	step := &terminalObservationReconciler{}
	executor := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	for _, disposition := range []string{types.FindingSupportDispositionSupported, types.FindingSupportDispositionUnresolved} {
		findings := fmt.Sprintf(`{"findings":[{"id":"support","severity":"error","action":"ask-user","category":"review-support-unresolved","support":{"claim_type":"ci","owner_result":{"disposition":%q}}}]}`, disposition)
		reconciled, err := executor.reconcileApprovalGate(context.Background(), step, &StepContext{Run: run, Repo: repo, DB: database}, findings)
		if err != nil || reconciled {
			t.Fatalf("%s support gate reconciled=%t err=%v; terminal state cannot resolve its support", disposition, reconciled, err)
		}
	}
}

func TestExecutorTerminalCIRequiresOwnerProofBeforeCompletion(t *testing.T) {
	for _, state := range []string{"merged", "closed"} {
		t.Run(state, func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			const head = "1111111111111111111111111111111111111111"
			const target = "2222222222222222222222222222222222222222"
			const digest = "3333333333333333333333333333333333333333333333333333333333333333"
			run.HeadSHA = head
			if err := database.UpdateRunHeadSHA(run.ID, head); err != nil {
				t.Fatal(err)
			}
			if _, err := database.BindRunPRContext(run.ID, db.PRContextCandidate{LocalHeadSHA: head, TargetBranch: "main", TargetSHA: target, MergeBaseSHA: target, DiffDigest: digest}, types.StepRebase); err != nil {
				t.Fatal(err)
			}
			const pending = `{"findings":[{"id":"review-ci","severity":"warning","description":"historical CI failure needs proof","action":"no-op","category":"review-support-pending","support":{"claim_type":"ci","ci":{"check_id":"ci-old","head_sha":"1111111111111111111111111111111111111111"}}}]}`
			executor := NewExecutor(database, p, nil, nil, []Step{
				&adaptiveCallStep{name: types.StepReview, fn: func(*StepContext) (*StepOutcome, error) {
					return &StepOutcome{Findings: pending, ReviewApprovedHeadSHA: head}, nil
				}},
				&adaptiveCallStep{name: types.StepCI, fn: func(c *StepContext) (*StepOutcome, error) {
					if err := database.UpdateRunPRState(run.ID, state); err != nil {
						return nil, err
					}
					c.Run.PRState = &state
					return &StepOutcome{}, nil
				}},
			}, nil)
			if err := executor.Execute(context.Background(), run, repo, t.TempDir()); err == nil {
				t.Fatal("terminal CI completed without current support")
			}
			results, err := database.GetStepsByRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if results[1].Status != types.StepStatusFailed {
				t.Fatalf("unproven CI status=%s, want failed without durable completion", results[1].Status)
			}
			current, _ := database.GetRun(run.ID)
			if current.Status != types.RunFailed {
				t.Fatalf("unproven run status=%s", current.Status)
			}
		})
	}
}

func TestExecutorCIRestartDoesNotRecordCompletionBeforeFreshReview(t *testing.T) {
	database, p, run, repo := setupTest(t)
	step := &adaptiveCallStep{name: types.StepCI, fn: func(*StepContext) (*StepOutcome, error) {
		return &StepOutcome{RestartFrom: types.StepReview}, nil
	}}
	executor := NewExecutor(database, p, nil, nil, []Step{newPassStep(types.StepReview), step}, nil)
	executor.initializeRunScopes(run.ID)
	ci, err := database.InsertStepResult(run.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	_, restart, err := executor.executeStep(context.Background(), step, ci, run, repo, t.TempDir(), t.TempDir(), stepExecutionState{})
	if err != nil || restart != types.StepReview {
		t.Fatalf("restart=%s err=%v", restart, err)
	}
	result, err := database.GetStepResult(ci.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != types.StepStatusRunning {
		t.Fatalf("CI restart status=%s, want running until fresh comparison/support", result.Status)
	}
}

func TestExecutorMergedHookRunsOnlyAfterDurableCompletion(t *testing.T) {
	for _, reject := range []bool{false, true} {
		name := "complete"
		if reject {
			name = "comparison-failed"
		}
		t.Run(name, func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			workDir := completionFixture(t, database, run)
			hookCalls := 0
			executor := NewExecutor(database, p, nil, nil, []Step{
				&adaptiveCallStep{name: types.StepCI, fn: func(c *StepContext) (*StepOutcome, error) {
					state := "merged"
					if err := database.UpdateRunPRState(run.ID, state); err != nil {
						return nil, err
					}
					c.Run.PRState = &state
					return &StepOutcome{}, nil
				}},
			}, nil)
			executor.SetOnPRMerged(func(ctx context.Context, id string) {
				hookCalls++
				current, err := database.GetRun(id)
				if err != nil || current.Status != types.RunCompleted {
					t.Errorf("merged hook before durable completion: run=%+v err=%v", current, err)
				}
				steps, err := database.GetStepsByRun(id)
				if err != nil || steps[0].Status != types.StepStatusCompleted {
					t.Errorf("merged hook before CI completion: steps=%+v err=%v", steps, err)
				}
			})
			executor.SetPRContextGuard(func(c *StepContext, _ types.StepName) (PRContextDecision, error) {
				if reject && terminalPRObserved(c.Run) {
					return PRContextDecision{}, fmt.Errorf("comparison failed")
				}
				return PRContextDecision{Target: PRTargetSelection{TargetBranch: "main"}}, nil
			})
			err := executor.Execute(context.Background(), run, repo, workDir)
			expected := 1
			if reject {
				expected = 0
				if err == nil {
					t.Fatal("failed comparison completed")
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if hookCalls != expected {
				t.Fatalf("merged hook calls=%d, want %d", hookCalls, expected)
			}
		})
	}
}

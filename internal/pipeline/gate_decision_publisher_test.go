package pipeline

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// publishingStep is a CI-shaped gate that implements GateDecisionPublisher and
// records the step row as the publisher saw it, so a test can prove the
// decision was durable before the publication ran.
type publishingStep struct {
	*overrideVerifyingStep
	db      *db.DB
	err     error
	mu      sync.Mutex
	visible []db.StepResult
}

func (p *publishingStep) PublishGateDecision(sctx *StepContext) error {
	row, err := p.db.GetStepResult(sctx.StepResultID)
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.visible = append(p.visible, *row)
	p.mu.Unlock()
	return p.err
}

func (p *publishingStep) seen() []db.StepResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]db.StepResult(nil), p.visible...)
}

const publisherFindings = `{"findings":[{"id":"c1","severity":"error","description":"required-check failing","action":"auto-fix"}]}`

func newPublishingStep(database *db.DB, override string) *publishingStep {
	return &publishingStep{
		overrideVerifyingStep: newOverrideVerifyingApprovalStep(types.StepCI, publisherFindings, func(*StepContext) (string, error) {
			return override, nil
		}),
		db: database,
	}
}

func TestExecutor_GateDecisionPublisher_LiveGate(t *testing.T) {
	const override = "required-check is failing at the current PR head"
	cases := []struct {
		name      string
		override  string
		respond   func(t *testing.T, exec *Executor, database *db.DB, runID string)
		wantCalls int
		check     func(t *testing.T, seen db.StepResult)
	}{
		{
			name:     "approve with override",
			override: override,
			respond: func(t *testing.T, exec *Executor, _ *db.DB, _ string) {
				if err := exec.Respond(types.StepCI, types.ActionApprove, nil); err != nil {
					t.Fatal(err)
				}
			},
			wantCalls: 1,
			check: func(t *testing.T, seen db.StepResult) {
				if seen.OverrideReason == nil || *seen.OverrideReason != override {
					t.Fatalf("publisher saw override %v, want the recorded override", seen.OverrideReason)
				}
			},
		},
		{
			name: "approve without override",
			respond: func(t *testing.T, exec *Executor, _ *db.DB, _ string) {
				if err := exec.Respond(types.StepCI, types.ActionApprove, nil); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "skip",
			respond: func(t *testing.T, exec *Executor, _ *db.DB, _ string) {
				if err := exec.Respond(types.StepCI, types.ActionSkip, nil); err != nil {
					t.Fatal(err)
				}
			},
			wantCalls: 1,
			check: func(t *testing.T, seen db.StepResult) {
				if seen.Status != types.StepStatusSkipped || seen.SkipReason == nil || *seen.SkipReason != types.OperatorSkipReason {
					t.Fatalf("publisher saw %s %v, want the operator skip already recorded", seen.Status, seen.SkipReason)
				}
			},
		},
		{
			name: "abort",
			respond: func(t *testing.T, exec *Executor, _ *db.DB, _ string) {
				if _, err := exec.RespondWithOverrides(types.StepCI, types.ActionAbort, nil, nil, nil, nil, "not shipping this"); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name:     "fix then abort",
			override: override,
			respond: func(t *testing.T, exec *Executor, database *db.DB, runID string) {
				if err := respondFixPartial(t, exec, types.StepCI, "c1"); err != nil {
					t.Fatal(err)
				}
				waitForStepStatus(t, database, runID, types.StepCI, types.StepStatusFixReview)
				if _, err := exec.RespondWithOverrides(types.StepCI, types.ActionAbort, nil, nil, nil, nil, "not shipping this"); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			step := newPublishingStep(database, tc.override)
			exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
			done, _ := startExecutor(t, exec, run, repo, t.TempDir())
			waitForStepStatus(t, database, run.ID, types.StepCI, types.StepStatusAwaitingApproval)
			tc.respond(t, exec, database, run.ID)
			select {
			case <-done:
			case <-time.After(30 * time.Second):
				t.Fatal("executor did not finish")
			}
			seen := step.seen()
			if len(seen) != tc.wantCalls {
				t.Fatalf("publisher calls = %d, want %d", len(seen), tc.wantCalls)
			}
			if tc.check != nil {
				tc.check(t, seen[0])
			}
		})
	}
}

func TestExecutor_GateDecisionPublisher_FailureFailsTheStep(t *testing.T) {
	database, p, run, repo := setupTest(t)
	step := newPublishingStep(database, "")
	step.err = errors.New("PR body changed while preparing the refresh")
	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	done, _ := startExecutor(t, exec, run, repo, t.TempDir())
	waitForStepStatus(t, database, run.ID, types.StepCI, types.StepStatusAwaitingApproval)
	if err := exec.Respond(types.StepCI, types.ActionSkip, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "publish gate decision") {
			t.Fatalf("Execute() error = %v, want the publication failure", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("executor did not finish")
	}
	got, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != types.RunFailed {
		t.Fatalf("run status = %s, want failed", got.Status)
	}
	rows, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rows[0].Status != types.StepStatusFailed || rows[0].Error == nil || !strings.Contains(*rows[0].Error, "PR body changed") {
		t.Fatalf("CI step = %s %v, want failed with the publication error", rows[0].Status, rows[0].Error)
	}
}

// parkRecoveredCIGate leaves a CI step parked at awaiting_approval as if the
// daemon restarted mid-gate (see TestExecutor_ApprovalOverride_RecoveredPathStillFailing).
func parkRecoveredCIGate(t *testing.T, database *db.DB, run *db.Run) (*db.Run, *db.StepResult) {
	t.Helper()
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	stepResult, err := database.InsertStepResult(run.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.StartStep(stepResult.ID); err != nil {
		t.Fatal(err)
	}
	findings := publisherFindings
	if err := database.SetStepFindings(stepResult.ID, findings); err != nil {
		t.Fatal(err)
	}
	if _, err := database.InsertStepRound(stepResult.ID, 1, "initial", &findings, nil, 10); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateStepStatusWithDuration(stepResult.ID, types.StepStatusAwaitingApproval, 10); err != nil {
		t.Fatal(err)
	}
	if err := database.SetRunAwaitingAgent(run.ID); err != nil {
		t.Fatal(err)
	}
	run, err = database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	return run, stepResult
}

func TestExecutor_GateDecisionPublisher_RecoveredGate(t *testing.T) {
	const override = "required-check is failing at the current PR head"
	cases := []struct {
		name   string
		action types.ApprovalAction
		check  func(t *testing.T, seen db.StepResult)
	}{
		{
			name:   "approve with override",
			action: types.ActionApprove,
			check: func(t *testing.T, seen db.StepResult) {
				if seen.OverrideReason == nil || *seen.OverrideReason != override {
					t.Fatalf("publisher saw override %v, want the recorded override", seen.OverrideReason)
				}
			},
		},
		{
			name:   "skip",
			action: types.ActionSkip,
			check: func(t *testing.T, seen db.StepResult) {
				if seen.Status != types.StepStatusSkipped {
					t.Fatalf("publisher saw %s, want the skip already recorded", seen.Status)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			run, stepResult := parkRecoveredCIGate(t, database, run)
			step := newPublishingStep(database, override)
			exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
			done := make(chan error, 1)
			go func() { done <- exec.Resume(context.Background(), run, repo, t.TempDir()) }()
			deadline := time.Now().Add(5 * time.Second)
			var respondErr error
			for time.Now().Before(deadline) {
				if respondErr = exec.Respond(types.StepCI, tc.action, nil); respondErr == nil {
					break
				}
				time.Sleep(20 * time.Millisecond)
			}
			if respondErr != nil {
				t.Fatalf("Respond(%s) error = %v", tc.action, respondErr)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("Resume() error = %v", err)
				}
			case <-time.After(30 * time.Second):
				t.Fatal("resumed executor did not finish")
			}
			seen := step.seen()
			if len(seen) != 1 || seen[0].ID != stepResult.ID {
				t.Fatalf("publisher calls = %+v, want one for the recovered CI row", seen)
			}
			tc.check(t, seen[0])
		})
	}
}

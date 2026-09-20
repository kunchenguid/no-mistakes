package pipeline

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestExecutor_RespondRefusesUnrecognizedActionAndKeepsGateParked pins the
// P2 that an ApprovalAction outside the approve/fix/skip/abort vocabulary
// used to be delivered to the gate loop, match no case, and re-park forever.
// Respond now refuses it up front with an error while the gate stays parked,
// so the same gate still accepts a valid response afterwards.
func TestExecutor_RespondRefusesUnrecognizedActionAndKeepsGateParked(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			return &StepOutcome{
				NeedsApproval: true,
				Findings:      `{"findings":[{"severity":"warning","description":"needs a human","action":"ask-user"}],"summary":"1 issue"}`,
			}, nil
		},
	}
	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)

	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(context.Background(), run, repo, workDir)
	}()
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)

	err := exec.Respond(types.StepReview, types.ApprovalAction("approv"), nil)
	if err == nil {
		t.Fatal("Respond with an unrecognized action returned nil, want an error")
	}
	if !strings.Contains(err.Error(), `unrecognized approval action "approv"`) {
		t.Fatalf("Respond error = %q, want it to name the unrecognized action", err)
	}

	// The refusal must not consume the gate: the executor is still waiting
	// and a valid response completes the run.
	select {
	case err := <-done:
		t.Fatalf("executor returned %v after a refused action, want it still parked", err)
	case <-time.After(200 * time.Millisecond):
	}
	if err := exec.Respond(types.StepReview, types.ActionApprove, nil); err != nil {
		t.Fatalf("valid respond after refusal: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("executor error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor timed out")
	}
}

// TestExecutor_GateLoopFailsStepOnUnrecognizedAction covers the switch's own
// default branch for a producer that bypasses Respond: the step fails with an
// error naming the action rather than looping back into the park.
func TestExecutor_GateLoopFailsStepOnUnrecognizedAction(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	calls := 0
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			calls++
			return &StepOutcome{
				NeedsApproval: true,
				Findings:      `{"findings":[{"severity":"warning","description":"needs a human","action":"ask-user"}],"summary":"1 issue"}`,
			}, nil
		},
	}
	exec := NewExecutor(database, p, nil, nil, []Step{step, newPassStep(types.StepTest)}, nil)

	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(context.Background(), run, repo, workDir)
	}()
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)

	// Bypass Respond's vocabulary check and hand the loop a response it
	// cannot dispatch.
	exec.approvalCh <- approvalResponse{action: types.ApprovalAction("bogus")}

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("executor returned nil for an unrecognized action, want an error")
		}
		if !strings.Contains(err.Error(), `unrecognized approval action "bogus"`) {
			t.Fatalf("executor error = %q, want it to name the unrecognized action", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor timed out: an unrecognized action re-parked instead of failing")
	}

	dbSteps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatalf("get steps: %v", err)
	}
	if dbSteps[0].Status != types.StepStatusFailed {
		t.Fatalf("review status = %q, want %q", dbSteps[0].Status, types.StepStatusFailed)
	}
	if dbSteps[0].Error == nil || !strings.Contains(*dbSteps[0].Error, "unrecognized approval action") {
		t.Fatalf("review error = %v, want the unrecognized-action message", dbSteps[0].Error)
	}
	if calls != 1 {
		t.Fatalf("step executed %d times, want 1 (no re-park round)", calls)
	}
}

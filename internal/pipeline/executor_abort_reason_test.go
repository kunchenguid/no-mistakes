package pipeline

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const abortReason = "unsafe to land"

// parkReviewGate runs a review gate that parks on findings, followed by a test
// step that must never run when the gate is rejected. executed counts how many
// times the review step ran, so a rejection that started a fix round shows up.
func parkReviewGate(t *testing.T) (exec *Executor, database *db.DB, run *db.Run, done chan error, executed, testRan *atomic.Int32) {
	t.Helper()
	database, p, run, repo := setupTest(t)
	executed, testRan = new(atomic.Int32), new(atomic.Int32)
	review := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(*StepContext) (*StepOutcome, error) {
			executed.Add(1)
			return &StepOutcome{NeedsApproval: true, Findings: declinedTestFindings}, nil
		},
	}
	after := &adaptiveCallStep{
		name: types.StepTest,
		fn: func(*StepContext) (*StepOutcome, error) {
			testRan.Add(1)
			return &StepOutcome{}, nil
		},
	}
	exec = NewExecutor(database, p, nil, nil, []Step{review, after}, nil)
	done = make(chan error, 1)
	workDir := t.TempDir()
	go func() { done <- exec.Execute(context.Background(), run, repo, workDir) }()
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	return exec, database, run, done, executed, testRan
}

func waitDone(t *testing.T, done chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("executor timed out")
		return nil
	}
}

// Rejecting a parked gate records the reason on the step and on the run, fails
// the run, starts no fix round, and keeps the round's findings.
func TestExecutor_AbortAtGateRecordsTheReasonAndStartsNoFixRound(t *testing.T) {
	exec, database, run, done, executed, testRan := parkReviewGate(t)

	if _, err := exec.RespondWithOverrides(types.StepReview, types.ActionAbort, nil, nil, nil, nil, abortReason); err != nil {
		t.Fatalf("RespondWithOverrides(abort) = %v", err)
	}
	wantStep := "aborted by user: " + abortReason
	if err := waitDone(t, done); err == nil || err.Error() != "step review: "+wantStep {
		t.Fatalf("Execute() error = %v, want the rejection with its reason", err)
	}

	got, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != types.RunFailed || got.Error == nil || *got.Error != "step review: "+wantStep {
		t.Fatalf("run = %s / %v, want failed with the reason in its error", got.Status, got.Error)
	}
	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var review *db.StepResult
	for _, s := range steps {
		if s.StepName == types.StepReview {
			review = s
		}
	}
	if review == nil || review.Status != types.StepStatusFailed || review.Error == nil || *review.Error != wantStep {
		t.Fatalf("review step = %+v, want failed with %q", review, wantStep)
	}
	if !types.IsUserAbortStepError(*review.Error) {
		t.Fatalf("%q is not recognised as an operator abort", *review.Error)
	}
	if review.FindingsJSON == nil || !strings.Contains(*review.FindingsJSON, "journal-version-deduplication") {
		t.Fatalf("the rejected gate's findings were not kept: %v", review.FindingsJSON)
	}
	rounds, err := database.GetRoundsByStep(review.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rounds) != 1 {
		t.Fatalf("rounds = %d, want the one the gate parked on (no fix round)", len(rounds))
	}
	if rounds[0].SelectionSource == nil || *rounds[0].SelectionSource != db.RoundSelectionSourceUserDeclined {
		t.Fatalf("selection_source = %v, want the round recorded as declined", rounds[0].SelectionSource)
	}
	if n := executed.Load(); n != 1 {
		t.Fatalf("review ran %d times, want 1: a rejection must not start a fix round", n)
	}
	if n := testRan.Load(); n != 0 {
		t.Fatalf("a later step ran %d times after the gate was rejected", n)
	}
}

// An abort sent without a reason keeps the existing error text exactly.
func TestExecutor_AbortAtGateWithoutAReasonKeepsTheExistingError(t *testing.T) {
	exec, database, run, done, _, _ := parkReviewGate(t)
	if err := exec.Respond(types.StepReview, types.ActionAbort, nil); err != nil {
		t.Fatal(err)
	}
	if err := waitDone(t, done); err == nil || err.Error() != "step review: aborted by user" {
		t.Fatalf("Execute() error = %v, want the reasonless abort", err)
	}
	got, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Error == nil || *got.Error != "step review: aborted by user" {
		t.Fatalf("run error = %v", got.Error)
	}
}

// A reason belongs to a Test approval exception or an abort, nothing else, and
// a refused response leaves the gate parked for a valid one.
func TestExecutor_ReasonIsRefusedForOtherResponsesAndLeavesTheGateParked(t *testing.T) {
	exec, database, run, done, _, _ := parkReviewGate(t)

	for _, action := range []types.ApprovalAction{types.ActionApprove, types.ActionSkip, types.ActionFix} {
		if _, err := exec.RespondWithOverrides(types.StepReview, action, []string{"journal-version-deduplication"}, nil, nil, nil, "because"); err == nil {
			t.Fatalf("a reason with %s was accepted", action)
		}
	}
	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range steps {
		if s.StepName == types.StepReview && s.Status != types.StepStatusAwaitingApproval {
			t.Fatalf("a refused response moved the gate to %s", s.Status)
		}
	}
	if _, err := exec.RespondWithOverrides(types.StepReview, types.ActionAbort, nil, nil, nil, nil, abortReason); err != nil {
		t.Fatalf("the gate no longer accepted a valid abort: %v", err)
	}
	waitDone(t, done)
}

// A rejection aimed at the wrong place has no effect: no gate, or another step.
func TestExecutor_AbortIsRefusedWithoutAMatchingParkedGate(t *testing.T) {
	database, p, _, _ := setupTest(t)
	idle := NewExecutor(database, p, nil, nil, []Step{newPassStep(types.StepReview)}, nil)
	if _, err := idle.RespondWithOverrides(types.StepReview, types.ActionAbort, nil, nil, nil, nil, abortReason); err == nil || !strings.Contains(err.Error(), "no step awaiting approval") {
		t.Fatalf("abort with nothing parked = %v, want the no-gate refusal", err)
	}

	exec, _, _, done, executed, _ := parkReviewGate(t)
	if _, err := exec.RespondWithOverrides(types.StepTest, types.ActionAbort, nil, nil, nil, nil, abortReason); err == nil || !strings.Contains(err.Error(), "step mismatch") {
		t.Fatalf("abort naming another step = %v, want the step mismatch refusal", err)
	}
	if n := executed.Load(); n != 1 {
		t.Fatalf("a refused abort re-ran the step (%d runs)", n)
	}
	if err := exec.Respond(types.StepReview, types.ActionAbort, nil); err != nil {
		t.Fatal(err)
	}
	waitDone(t, done)
}

// A gate recovered after a daemon restart records the same rejection.
func TestExecutor_RecoveredGateAbortRecordsTheReason(t *testing.T) {
	database, p, run, repo := setupTest(t)
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	stepResult, err := database.InsertStepResult(run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.StartStep(stepResult.ID); err != nil {
		t.Fatal(err)
	}
	if err := database.SetStepFindings(stepResult.ID, declinedTestFindings); err != nil {
		t.Fatal(err)
	}
	findings := declinedTestFindings
	if _, err := database.InsertStepRound(stepResult.ID, 1, "initial", &findings, nil, 25); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateStepStatusWithDuration(stepResult.ID, types.StepStatusAwaitingApproval, 25); err != nil {
		t.Fatal(err)
	}
	if err := database.SetRunAwaitingAgent(run.ID); err != nil {
		t.Fatal(err)
	}
	run, err = database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}

	exec := NewExecutor(database, p, nil, nil, []Step{newApprovalStep(types.StepTest, declinedTestFindings)}, nil)
	done := make(chan error, 1)
	go func() { done <- exec.Resume(context.Background(), run, repo, t.TempDir()) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := exec.RespondWithOverrides(types.StepTest, types.ActionAbort, nil, nil, nil, nil, abortReason); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("recovered gate never accepted the abort")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := waitDone(t, done); err == nil || err.Error() != "step test: aborted by user: "+abortReason {
		t.Fatalf("Resume() error = %v, want the rejection with its reason", err)
	}
	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range steps {
		if s.StepName == types.StepTest && (s.Status != types.StepStatusFailed || s.Error == nil || *s.Error != "aborted by user: "+abortReason) {
			t.Fatalf("recovered step = %s / %v", s.Status, s.Error)
		}
	}
	got, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != types.RunFailed || got.Error == nil || !strings.Contains(*got.Error, abortReason) {
		t.Fatalf("recovered run = %s / %v", got.Status, got.Error)
	}
}

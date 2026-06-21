package pipeline

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestExecutor_StartWriteFailureMarksRunFailed ensures that when the initial
// UpdateRunStatus(RunRunning) write fails, the run is routed through failRun
// (terminal event emitted, run marked failed) rather than returning a raw
// error that leaves the run zombieed in RunPending.
func TestExecutor_StartWriteFailureMarksRunFailed(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	exec := NewExecutor(database, p, nil, nil, []Step{newPassStep(types.StepReview)}, nil)
	events := collectEvents(exec)

	// Simulate a transient SQLite failure by closing the connection before
	// Execute runs. The start status write is the first DB operation Execute
	// performs, so this targets exactly that write.
	if err := database.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}

	err := exec.Execute(context.Background(), run, repo, workDir)
	if err == nil {
		t.Fatal("expected error from failed start write, got nil")
	}

	// failRun must have emitted the terminal event (the bug: raw-error return
	// emits nothing, leaving subscribers waiting forever).
	if e := events.findRunEvent(ipc.EventRunCompleted); e == nil {
		t.Fatal("expected run_completed event after start-write failure")
	}

	// The in-memory run must be terminal (failed), not still pending/running.
	if run.Status != types.RunFailed {
		t.Errorf("run status = %q, want %q", run.Status, types.RunFailed)
	}
}

// TestExecutor_FinalWriteFailureMarksRunFailed ensures that when the final
// UpdateRunStatus(RunCompleted) write fails (after every step succeeded), the
// run is routed through failRun instead of returning a raw error that leaves
// it zombieed in RunRunning with EventRunCompleted never firing.
func TestExecutor_FinalWriteFailureMarksRunFailed(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	// Empty steps: Execute does the start write, skips the loop, then performs
	// the final completed write. Closing the DB right after the start write
	// (observed via the "running" run_updated event) isolates the final write.
	ec := &eventCollector{}
	closed := false
	exec := NewExecutor(database, p, nil, nil, nil, func(e ipc.Event) {
		ec.handler(e)
		if !closed && e.Type == ipc.EventRunUpdated && e.StepName == nil &&
			e.Status != nil && *e.Status == string(types.RunRunning) {
			closed = true
			if err := database.Close(); err != nil {
				t.Errorf("close db: %v", err)
			}
		}
	})

	err := exec.Execute(context.Background(), run, repo, workDir)
	if err == nil {
		t.Fatal("expected error from failed final write, got nil")
	}
	if !closed {
		t.Fatal("expected DB to be closed after the running event")
	}

	completed := ec.findRunEvent(ipc.EventRunCompleted)
	if completed == nil {
		t.Fatal("expected run_completed event after final-write failure (the bug never emitted one)")
	}
	if completed.Status == nil || *completed.Status != string(types.RunFailed) {
		got := "<nil>"
		if completed.Status != nil {
			got = *completed.Status
		}
		t.Errorf("run_completed status = %q, want %q", got, types.RunFailed)
	}

	// The run must not be left in the zombie RunRunning state.
	if run.Status != types.RunFailed {
		t.Errorf("run status = %q, want %q", run.Status, types.RunFailed)
	}
}

// TestExecutor_FailedStepMarksSubsequentSkipped ensures that a mid-pipeline
// step failure marks unstarted subsequent steps as skipped, not pending.
// Pending rows render as "⏳ pending" in the PR body, implying work that will
// still happen on a run that has already failed.
func TestExecutor_FailedStepMarksSubsequentSkipped(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	steps := []Step{
		newPassStep(types.StepReview),
		newFailStep(types.StepTest, errors.New("tests crashed")),
		newPassStep(types.StepLint), // unstarted after failure
		newPassStep(types.StepPush), // unstarted after failure
	}

	exec := NewExecutor(database, p, nil, nil, steps, nil)
	events := collectEvents(exec)

	if err := exec.Execute(context.Background(), run, repo, workDir); err == nil {
		t.Fatal("expected error from failing step, got nil")
	}

	dbSteps, _ := database.GetStepsByRun(run.ID)
	if len(dbSteps) != 4 {
		t.Fatalf("expected 4 step rows, got %d", len(dbSteps))
	}
	if dbSteps[0].Status != types.StepStatusCompleted {
		t.Errorf("review: expected %q, got %q", types.StepStatusCompleted, dbSteps[0].Status)
	}
	if dbSteps[1].Status != types.StepStatusFailed {
		t.Errorf("test: expected %q, got %q", types.StepStatusFailed, dbSteps[1].Status)
	}
	// Subsequent unstarted steps must be skipped, never pending.
	for _, s := range dbSteps[2:] {
		if s.Status != types.StepStatusSkipped {
			t.Errorf("step %s: expected %q, got %q", s.StepName, types.StepStatusSkipped, s.Status)
		}
	}

	// A skipped completion event should be emitted for each subsequent step.
	if e := events.find(ipc.EventStepCompleted, types.StepLint); e == nil || e.Status == nil || *e.Status != string(types.StepStatusSkipped) {
		t.Errorf("expected skipped event for lint, got %v", e)
	}
	if e := events.find(ipc.EventStepCompleted, types.StepPush); e == nil || e.Status == nil || *e.Status != string(types.StepStatusSkipped) {
		t.Errorf("expected skipped event for push, got %v", e)
	}
}

// TestExecutor_AwaitingApprovalWriteFailureFailsStep ensures that a failure of
// the awaiting_approval DB write fails the step instead of desyncing the DB
// (prior status) from the event stream (awaiting) and blocking on
// waitForApproval with no durable record of the wait.
func TestExecutor_AwaitingApprovalWriteFailureFailsStep(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	// The step returns a NeedsApproval outcome and, before doing so, breaks the
	// DB so the subsequent awaiting_approval status write fails. StartStep runs
	// before step.Execute, so the connection is still open for it.
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			if err := sctx.DB.Close(); err != nil {
				t.Errorf("close db: %v", err)
			}
			return &StepOutcome{
				NeedsApproval: true,
				Findings:      `{"findings":[{"severity":"error","description":"bug","action":"fix"}],"summary":"1 issue"}`,
			}, nil
		},
	}

	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	events := collectEvents(exec)

	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(context.Background(), run, repo, workDir)
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error from awaiting_approval write failure, got nil")
		}
	case <-time.After(5 * time.Second):
		// The bug: the executor logs the DB error, emits the awaiting event,
		// and blocks forever on waitForApproval. The fix returns promptly.
		t.Fatal("executor blocked on waitForApproval after awaiting_approval write failure")
	}

	// The step must be reported failed, not awaiting_approval.
	failedEvent := events.find(ipc.EventStepCompleted, types.StepReview)
	if failedEvent == nil || failedEvent.Status == nil {
		t.Fatal("expected step_completed event for review")
	}
	if *failedEvent.Status != string(types.StepStatusFailed) {
		t.Errorf("review status = %q, want %q", *failedEvent.Status, types.StepStatusFailed)
	}

	// No awaiting_approval event must have been emitted (that would desync the
	// event stream from the DB, which still holds the prior status).
	for _, e := range events.all() {
		if e.Type == ipc.EventStepCompleted && e.Status != nil && *e.Status == string(types.StepStatusAwaitingApproval) {
			t.Errorf("did not expect awaiting_approval event after write failure: %+v", e)
		}
	}

	// The run must be terminal.
	if run.Status != types.RunFailed {
		t.Errorf("run status = %q, want %q", run.Status, types.RunFailed)
	}
}

// TestExecutor_SkipRemainingDBErrorFailsRun ensures that a DB error while
// marking subsequent steps skipped (on the skip-remaining path) is treated as
// fatal rather than logged-and-swallowed, which previously left the remaining
// step row pending.
func TestExecutor_SkipRemainingDBErrorFailsRun(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	// First step signals skip-remaining; the callback closes the DB once the
	// first step has completed so that marking the second step skipped fails.
	ec := &eventCollector{}
	closed := false
	exec := NewExecutor(database, p, nil, nil, []Step{
		&mockStep{name: types.StepRebase, outcome: &StepOutcome{ExitCode: 0, SkipRemaining: true}},
		newPassStep(types.StepReview),
	}, func(e ipc.Event) {
		ec.handler(e)
		if !closed && e.Type == ipc.EventStepCompleted && e.StepName != nil &&
			*e.StepName == types.StepRebase && e.Status != nil && *e.Status == string(types.StepStatusCompleted) {
			closed = true
			if err := database.Close(); err != nil {
				t.Errorf("close db: %v", err)
			}
		}
	})

	err := exec.Execute(context.Background(), run, repo, workDir)
	if err == nil {
		t.Fatal("expected error from skip-remaining DB write failure, got nil")
	}
	if !closed {
		t.Fatal("expected DB to be closed after the rebase step completed")
	}
	if run.Status != types.RunFailed {
		t.Errorf("run status = %q, want %q", run.Status, types.RunFailed)
	}
	if !strings.Contains(err.Error(), "skip step") {
		t.Errorf("expected error to mention skip step, got %q", err)
	}
}

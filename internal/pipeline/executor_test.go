package pipeline

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestExecutor_AllStepsPass(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	steps := []Step{
		newPassStep(types.StepReview),
		newPassStep(types.StepTest),
		newPassStep(types.StepLint),
	}

	exec := NewExecutor(database, p, nil, nil, steps, nil)
	events := collectEvents(exec)

	err := exec.Execute(context.Background(), run, repo, workDir)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	// Run should be completed
	updated, _ := database.GetRun(run.ID)
	if updated.Status != types.RunCompleted {
		t.Errorf("expected run status %q, got %q", types.RunCompleted, updated.Status)
	}

	// All steps should be completed
	dbSteps, _ := database.GetStepsByRun(run.ID)
	if len(dbSteps) != 3 {
		t.Fatalf("expected 3 steps, got %d", len(dbSteps))
	}
	for _, s := range dbSteps {
		if s.Status != types.StepStatusCompleted {
			t.Errorf("step %s: expected status %q, got %q", s.StepName, types.StepStatusCompleted, s.Status)
		}
		if s.StartedAt == nil {
			t.Errorf("step %s: started_at should be set", s.StepName)
		}
		if s.CompletedAt == nil {
			t.Errorf("step %s: completed_at should be set", s.StepName)
		}
	}

	// Should have step_started + step_completed events for each step
	for _, name := range []types.StepName{types.StepReview, types.StepTest, types.StepLint} {
		if e := events.find(ipc.EventStepStarted, name); e == nil {
			t.Errorf("missing step_started event for %s", name)
		}
		if e := events.find(ipc.EventStepCompleted, name); e == nil {
			t.Errorf("missing step_completed event for %s", name)
		}
	}
}

func TestExecutor_RunEventStatusCorrectOnSuccess(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	exec := NewExecutor(database, p, nil, nil, []Step{newPassStep(types.StepReview)}, nil)
	events := collectEvents(exec)

	err := exec.Execute(context.Background(), run, repo, workDir)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	// run_updated event should carry "running" status (not stale "pending")
	updatedEvent := events.findRunEvent(ipc.EventRunUpdated)
	if updatedEvent == nil {
		t.Fatal("expected run_updated event")
	}
	if updatedEvent.Status == nil || *updatedEvent.Status != string(types.RunRunning) {
		got := "<nil>"
		if updatedEvent.Status != nil {
			got = *updatedEvent.Status
		}
		t.Errorf("run_updated event: expected status %q, got %q", types.RunRunning, got)
	}

	// run_completed event should carry "completed" status (not stale "running")
	completedEvent := events.findRunEvent(ipc.EventRunCompleted)
	if completedEvent == nil {
		t.Fatal("expected run_completed event")
	}
	if completedEvent.Status == nil || *completedEvent.Status != string(types.RunCompleted) {
		got := "<nil>"
		if completedEvent.Status != nil {
			got = *completedEvent.Status
		}
		t.Errorf("run_completed event: expected status %q, got %q", types.RunCompleted, got)
	}
}

func TestExecutor_RunEventStatusCorrectOnFailure(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	exec := NewExecutor(database, p, nil, nil, []Step{newFailStep(types.StepReview, fmt.Errorf("boom"))}, nil)
	events := collectEvents(exec)

	err := exec.Execute(context.Background(), run, repo, workDir)
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	// run_completed event should carry "failed" status (not stale "running")
	completedEvent := events.findRunEvent(ipc.EventRunCompleted)
	if completedEvent == nil {
		t.Fatal("expected run_completed event")
	}
	if completedEvent.Status == nil || *completedEvent.Status != string(types.RunFailed) {
		got := "<nil>"
		if completedEvent.Status != nil {
			got = *completedEvent.Status
		}
		t.Errorf("run_completed event: expected status %q, got %q", types.RunFailed, got)
	}
}

func TestExecutor_StepError_FailsRun(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	steps := []Step{
		newPassStep(types.StepReview),
		newFailStep(types.StepTest, fmt.Errorf("tests crashed")),
		newPassStep(types.StepLint), // should not run
	}

	exec := NewExecutor(database, p, nil, nil, steps, nil)

	err := exec.Execute(context.Background(), run, repo, workDir)
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	// Run should be failed
	updated, _ := database.GetRun(run.ID)
	if updated.Status != types.RunFailed {
		t.Errorf("expected run status %q, got %q", types.RunFailed, updated.Status)
	}

	// Second step should be failed, third should be pending
	dbSteps, _ := database.GetStepsByRun(run.ID)
	if dbSteps[1].Status != types.StepStatusFailed {
		t.Errorf("step test: expected %q, got %q", types.StepStatusFailed, dbSteps[1].Status)
	}
	if dbSteps[2].Status != types.StepStatusPending {
		t.Errorf("step lint: expected %q, got %q", types.StepStatusPending, dbSteps[2].Status)
	}
}

func TestExecutor_FailedStepRecordsDuration(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	steps := []Step{
		newFailStep(types.StepReview, fmt.Errorf("review crashed")),
	}

	exec := NewExecutor(database, p, nil, nil, steps, nil)

	err := exec.Execute(context.Background(), run, repo, workDir)
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	// Failed step should still have duration_ms recorded.
	dbSteps, _ := database.GetStepsByRun(run.ID)
	if dbSteps[0].DurationMS == nil {
		t.Error("expected failed step to have duration_ms recorded, got nil")
	}
}

func TestExecutor_EmptySteps(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	exec := NewExecutor(database, p, nil, nil, nil, nil)

	err := exec.Execute(context.Background(), run, repo, workDir)
	if err != nil {
		t.Fatalf("expected no error for empty steps, got: %v", err)
	}

	updated, _ := database.GetRun(run.ID)
	if updated.Status != types.RunCompleted {
		t.Errorf("expected run status %q, got %q", types.RunCompleted, updated.Status)
	}
}

func TestExecutor_RunMarkedRunning(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	var runStatusDuringExec types.RunStatus
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			r, _ := sctx.DB.GetRun(run.ID)
			runStatusDuringExec = r.Status
			return &StepOutcome{ExitCode: 0}, nil
		},
	}

	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	exec.Execute(context.Background(), run, repo, workDir)

	if runStatusDuringExec != types.RunRunning {
		t.Errorf("expected run status during execution to be %q, got %q", types.RunRunning, runStatusDuringExec)
	}
}

func TestExecutor_StepResultHasDuration(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			time.Sleep(10 * time.Millisecond)
			return &StepOutcome{ExitCode: 0}, nil
		},
	}

	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	exec.Execute(context.Background(), run, repo, workDir)

	dbSteps, _ := database.GetStepsByRun(run.ID)
	if len(dbSteps) != 1 {
		t.Fatalf("expected 1 step, got %d", len(dbSteps))
	}
	if dbSteps[0].DurationMS == nil {
		t.Fatal("expected duration_ms to be set")
	}
	if *dbSteps[0].DurationMS < 10 {
		t.Errorf("expected duration >= 10ms, got %dms", *dbSteps[0].DurationMS)
	}
}

func TestExecutor_StepResultUsesDurationOverride(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	step := &mockStep{
		name: types.StepReview,
		outcome: &StepOutcome{
			ExitCode:           0,
			DurationOverrideMS: 45000,
		},
	}

	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	exec.Execute(context.Background(), run, repo, workDir)

	dbSteps, _ := database.GetStepsByRun(run.ID)
	if len(dbSteps) != 1 {
		t.Fatalf("expected 1 step, got %d", len(dbSteps))
	}
	if dbSteps[0].DurationMS == nil {
		t.Fatal("expected duration_ms to be set")
	}
	if got := *dbSteps[0].DurationMS; got != 45000 {
		t.Fatalf("duration_ms = %d, want %d", got, 45000)
	}
}

func TestExecutor_SkipRemaining_SkipsSubsequentSteps(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	skipStep := &mockStep{
		name:    types.StepRebase,
		outcome: &StepOutcome{ExitCode: 0, SkipRemaining: true},
	}
	reviewStep := newPassStep(types.StepReview)
	testStep := newPassStep(types.StepTest)

	steps := []Step{skipStep, reviewStep, testStep}
	exec := NewExecutor(database, p, nil, nil, steps, nil)
	events := collectEvents(exec)

	err := exec.Execute(context.Background(), run, repo, workDir)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	// Run should be completed
	updated, _ := database.GetRun(run.ID)
	if updated.Status != types.RunCompleted {
		t.Errorf("expected run status %q, got %q", types.RunCompleted, updated.Status)
	}

	// The rebase step should be completed
	dbSteps, _ := database.GetStepsByRun(run.ID)
	if len(dbSteps) != 3 {
		t.Fatalf("expected 3 steps, got %d", len(dbSteps))
	}
	if dbSteps[0].Status != types.StepStatusCompleted {
		t.Errorf("rebase step: expected status %q, got %q", types.StepStatusCompleted, dbSteps[0].Status)
	}

	// Subsequent steps should be skipped
	for _, s := range dbSteps[1:] {
		if s.Status != types.StepStatusSkipped {
			t.Errorf("step %s: expected status %q, got %q", s.StepName, types.StepStatusSkipped, s.Status)
		}
		if s.CompletedAt == nil {
			t.Errorf("step %s: completed_at should be set for skipped steps", s.StepName)
		}
	}

	// Subsequent steps should NOT have been executed
	if reviewStep.callCount() != 0 {
		t.Errorf("review step was called %d times, expected 0", reviewStep.callCount())
	}
	if testStep.callCount() != 0 {
		t.Errorf("test step was called %d times, expected 0", testStep.callCount())
	}

	// Should have completed events for all steps
	for _, name := range []types.StepName{types.StepRebase, types.StepReview, types.StepTest} {
		if e := events.find(ipc.EventStepCompleted, name); e == nil {
			t.Errorf("missing step_completed event for %s", name)
		}
	}
}

func TestExecutor_StepOutcomePRURL_EmitsRunUpdated(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	prURL := "https://github.com/test/repo/pull/99"
	prStep := &mockStep{
		name:    types.StepPR,
		outcome: &StepOutcome{ExitCode: 0, PRURL: prURL},
	}
	steps := []Step{newPassStep(types.StepReview), prStep}

	exec := NewExecutor(database, p, nil, nil, steps, nil)
	events := collectEvents(exec)

	err := exec.Execute(context.Background(), run, repo, workDir)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	// Should have a run_updated event with the PRURL after the PR step.
	found := false
	for _, e := range events.all() {
		if e.Type == ipc.EventRunUpdated && e.PRURL != nil && *e.PRURL == prURL {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected a run_updated event with PRURL after PR step")
	}

	// The run_completed event should also carry the PRURL.
	completedEvent := events.findRunEvent(ipc.EventRunCompleted)
	if completedEvent == nil {
		t.Fatal("expected run_completed event")
	}
	if completedEvent.PRURL == nil || *completedEvent.PRURL != prURL {
		t.Errorf("expected run_completed PRURL %q, got %v", prURL, completedEvent.PRURL)
	}
}

func TestExecutor_SkippedOutcome_MarksStepSkipped(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	step := &mockStep{
		name:    types.StepPR,
		outcome: &StepOutcome{Skipped: true},
	}

	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	events := collectEvents(exec)

	if err := exec.Execute(context.Background(), run, repo, workDir); err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	dbSteps, _ := database.GetStepsByRun(run.ID)
	if len(dbSteps) != 1 {
		t.Fatalf("expected 1 step, got %d", len(dbSteps))
	}
	if dbSteps[0].Status != types.StepStatusSkipped {
		t.Fatalf("expected skipped status, got %q", dbSteps[0].Status)
	}

	event := events.find(ipc.EventStepCompleted, types.StepPR)
	if event == nil {
		t.Fatal("expected step_completed event")
	}
	if event.Status == nil || *event.Status != string(types.StepStatusSkipped) {
		got := "<nil>"
		if event.Status != nil {
			got = *event.Status
		}
		t.Fatalf("expected skipped event status, got %q", got)
	}
}

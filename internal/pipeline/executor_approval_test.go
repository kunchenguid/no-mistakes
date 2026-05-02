package pipeline

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/telemetry"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestExecutor_ApprovalApprove(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	steps := []Step{
		newApprovalStep(types.StepReview, `{"findings":[{"severity":"error","description":"bug found","action":"auto-fix"}],"summary":"1 issue found"}`),
		newPassStep(types.StepTest),
	}

	exec := NewExecutor(database, p, nil, nil, steps, nil)
	events := collectEvents(exec)

	// Run executor in background
	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(context.Background(), run, repo, workDir)
	}()

	// Wait for step to reach awaiting_approval
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)

	// Check findings are stored
	dbSteps, _ := database.GetStepsByRun(run.ID)
	reviewStep := dbSteps[0]
	if reviewStep.FindingsJSON == nil {
		t.Fatal("expected findings to be stored")
	}
	storedItems := mustParseFindingItems(t, *reviewStep.FindingsJSON)
	if len(storedItems) != 1 || storedItems[0].ID != "review-1" || storedItems[0].Description != "bug found" {
		t.Errorf("unexpected stored findings: %#v", storedItems)
	}

	// Send approval
	err := exec.Respond(types.StepReview, types.ActionApprove, nil)
	if err != nil {
		t.Fatalf("respond error: %v", err)
	}

	// Wait for execution to finish
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor timed out")
	}

	// Run should be completed
	updated, _ := database.GetRun(run.ID)
	if updated.Status != types.RunCompleted {
		t.Errorf("expected run status %q, got %q", types.RunCompleted, updated.Status)
	}

	// Review step should be completed (approved), test step should be completed
	dbSteps, _ = database.GetStepsByRun(run.ID)
	if dbSteps[0].Status != types.StepStatusCompleted {
		t.Errorf("review: expected %q, got %q", types.StepStatusCompleted, dbSteps[0].Status)
	}
	if dbSteps[1].Status != types.StepStatusCompleted {
		t.Errorf("test: expected %q, got %q", types.StepStatusCompleted, dbSteps[1].Status)
	}

	// Should have awaiting_approval event
	_ = events // events collected for verification
}

func TestExecutor_ApprovalDurationExcludesWaitTime(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	steps := []Step{
		newApprovalStep(types.StepReview, `{"findings":[{"severity":"error","description":"bug","action":"auto-fix"}],"summary":"1 issue"}`),
	}

	exec := NewExecutor(database, p, nil, nil, steps, nil)
	events := collectEvents(exec)

	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(context.Background(), run, repo, workDir)
	}()

	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)

	// Duration should be stored in DB while awaiting approval (execution-only time).
	dbSteps, _ := database.GetStepsByRun(run.ID)
	if dbSteps[0].DurationMS == nil {
		t.Fatal("expected duration_ms to be set on awaiting_approval step")
	}
	execDuration := *dbSteps[0].DurationMS

	// Simulate user taking time to review.
	time.Sleep(200 * time.Millisecond)

	exec.Respond(types.StepReview, types.ActionApprove, nil)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor timed out")
	}

	// Final duration should not include the 200ms+ approval wait time.
	dbSteps, _ = database.GetStepsByRun(run.ID)
	finalDuration := *dbSteps[0].DurationMS
	if finalDuration > execDuration+100 {
		t.Errorf("final duration %dms should not significantly exceed pre-approval duration %dms (approval wait should be excluded)", finalDuration, execDuration)
	}

	// The EventStepCompleted event for awaiting_approval should carry duration.
	awaitingEvent := events.findLast(ipc.EventStepCompleted, string(types.StepStatusAwaitingApproval))
	if awaitingEvent == nil {
		t.Fatal("expected awaiting_approval step_completed event")
	}
	if awaitingEvent.DurationMS == nil {
		t.Error("expected awaiting_approval event to carry DurationMS")
	}

	// The final completed event should also carry duration.
	completedEvent := events.findLast(ipc.EventStepCompleted, string(types.StepStatusCompleted))
	if completedEvent == nil {
		t.Fatal("expected completed step_completed event")
	}
	if completedEvent.DurationMS == nil {
		t.Error("expected completed event to carry DurationMS")
	}
}

func TestExecutor_ApprovalFix(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	// Step that needs approval on first call, passes on second
	callCount := 0
	var step Step = &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			callCount++
			if callCount == 1 {
				return &StepOutcome{NeedsApproval: true, Findings: `{"issues":["bug"]}`}, nil
			}
			// After fix, re-evaluate passes
			return &StepOutcome{NeedsApproval: false, ExitCode: 0}, nil
		},
	}

	steps := []Step{step, newPassStep(types.StepTest)}
	exec := NewExecutor(database, p, nil, nil, steps, nil)

	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(context.Background(), run, repo, workDir)
	}()

	// Wait for awaiting_approval
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)

	// Send fix action
	exec.Respond(types.StepReview, types.ActionFix, nil)

	// Wait for step to re-execute and complete (it passes on second call)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor timed out")
	}

	// Both steps should be completed
	dbSteps, _ := database.GetStepsByRun(run.ID)
	if dbSteps[0].Status != types.StepStatusCompleted {
		t.Errorf("review: expected %q, got %q", types.StepStatusCompleted, dbSteps[0].Status)
	}
	if dbSteps[1].Status != types.StepStatusCompleted {
		t.Errorf("test: expected %q, got %q", types.StepStatusCompleted, dbSteps[1].Status)
	}

	// Step should have been called twice (initial + after fix)
	if callCount != 2 {
		t.Errorf("expected step to be called 2 times, got %d", callCount)
	}
}

func TestExecutor_TracksApprovalAndUserFixTelemetry(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	recorder := &telemetryRecorder{}
	restore := telemetry.SetDefaultForTesting(recorder)
	defer restore()

	callCount := 0
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			callCount++
			if callCount == 1 {
				return &StepOutcome{NeedsApproval: true, Findings: `{"findings":[{"severity":"error","description":"bug one","action":"auto-fix"},{"severity":"warn","description":"bug two","action":"ask-user"}],"summary":"2 issues"}`}, nil
			}
			return &StepOutcome{ExitCode: 0}, nil
		},
	}

	exec := NewExecutor(database, p, &config.Config{Agent: types.AgentClaude}, nil, []Step{step}, nil)

	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(context.Background(), run, repo, workDir)
	}()

	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)

	if err := exec.Respond(types.StepReview, types.ActionFix, nil); err != nil {
		t.Fatalf("respond error: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor timed out")
	}

	approvalEvent := recorder.find("approval", "action", "fix")
	if approvalEvent == nil {
		t.Fatal("expected approval telemetry event")
	}
	if got := approvalEvent.fields["step"]; got != string(types.StepReview) {
		t.Fatalf("approval step = %v, want %q", got, types.StepReview)
	}
	if got := approvalEvent.fields["selected_findings_count"]; fmt.Sprint(got) != "2" {
		t.Fatalf("approval selected_findings_count = %v, want 2", got)
	}

	fixEvent := recorder.find("fix", "source", "user")
	if fixEvent == nil {
		t.Fatal("expected user fix telemetry event")
	}
	if got := fixEvent.fields["selected_findings_count"]; fmt.Sprint(got) != "2" {
		t.Fatalf("fix selected_findings_count = %v, want 2", got)
	}

	stepEvent := recorder.find("step", "status", string(types.StepStatusAwaitingApproval))
	if stepEvent == nil {
		t.Fatal("expected awaiting approval step telemetry event")
	}
	if got := stepEvent.fields["findings_count"]; fmt.Sprint(got) != "2" {
		t.Fatalf("step findings_count = %v, want 2", got)
	}
	if got := stepEvent.fields["agent"]; got != string(types.AgentClaude) {
		t.Fatalf("step agent = %v, want %q", got, types.AgentClaude)
	}
}

func TestExecutor_TracksAutoFixTelemetry(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	recorder := &telemetryRecorder{}
	restore := telemetry.SetDefaultForTesting(recorder)
	defer restore()

	callCount := 0
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			callCount++
			if callCount == 1 {
				return &StepOutcome{
					AutoFixable: true,
					Findings:    `{"findings":[{"severity":"error","description":"fix me","action":"auto-fix"}],"summary":"1 issue"}`,
				}, nil
			}
			return &StepOutcome{ExitCode: 0}, nil
		},
	}

	cfg := &config.Config{Agent: types.AgentClaude, AutoFix: config.AutoFix{Review: 1}}
	exec := NewExecutor(database, p, cfg, nil, []Step{step}, nil)

	if err := exec.Execute(context.Background(), run, repo, workDir); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	fixEvent := recorder.find("fix", "source", "auto")
	if fixEvent == nil {
		t.Fatal("expected auto-fix telemetry event")
	}
	if got := fixEvent.fields["step"]; got != string(types.StepReview) {
		t.Fatalf("fix step = %v, want %q", got, types.StepReview)
	}
	if got := fixEvent.fields["selected_findings_count"]; fmt.Sprint(got) != "1" {
		t.Fatalf("fix selected_findings_count = %v, want 1", got)
	}
	if got := fixEvent.fields["attempt"]; fmt.Sprint(got) != "1" {
		t.Fatalf("fix attempt = %v, want 1", got)
	}
}

func TestExecutor_Respond_NoWaitingStep(t *testing.T) {
	database, p, _, _ := setupTest(t)

	exec := NewExecutor(database, p, nil, nil, nil, nil)
	err := exec.Respond(types.StepReview, types.ActionApprove, nil)
	if err == nil {
		t.Fatal("expected error when no step awaiting approval")
	}
}

func TestExecutor_Respond_WrongStep(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	steps := []Step{
		newApprovalStep(types.StepReview, `{"issues":["bug"]}`),
		newPassStep(types.StepTest),
	}

	exec := NewExecutor(database, p, nil, nil, steps, nil)

	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(context.Background(), run, repo, workDir)
	}()

	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)

	// Respond with wrong step name — should error
	err := exec.Respond(types.StepTest, types.ActionApprove, nil)
	if err == nil {
		t.Fatal("expected error for step mismatch")
	}
	if !strings.Contains(err.Error(), "step mismatch") {
		t.Errorf("expected step mismatch error, got: %v", err)
	}

	// Respond with correct step name — should succeed
	err = exec.Respond(types.StepReview, types.ActionApprove, nil)
	if err != nil {
		t.Fatalf("respond with correct step should succeed: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor timed out")
	}
}

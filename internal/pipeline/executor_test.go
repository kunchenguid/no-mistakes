package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// --- mock step helpers ---

// mockStep is a test step that returns a configurable outcome.
type mockStep struct {
	name    types.StepName
	outcome *StepOutcome
	err     error
	calls   int
	mu      sync.Mutex
}

func (m *mockStep) Name() types.StepName { return m.name }

func (m *mockStep) Execute(sctx *StepContext) (*StepOutcome, error) {
	m.mu.Lock()
	m.calls++
	m.mu.Unlock()
	if sctx.Log != nil {
		sctx.Log(fmt.Sprintf("executing %s", m.name))
	}
	return m.outcome, m.err
}

func (m *mockStep) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls
}

func newPassStep(name types.StepName) *mockStep {
	return &mockStep{name: name, outcome: &StepOutcome{ExitCode: 0}}
}

func newApprovalStep(name types.StepName, findings string) *mockStep {
	return &mockStep{name: name, outcome: &StepOutcome{NeedsApproval: true, Findings: findings}}
}

func newFailStep(name types.StepName, err error) *mockStep {
	return &mockStep{name: name, err: err}
}

// --- test helpers ---

func setupTest(t *testing.T) (*db.DB, *paths.Paths, *db.Run, *db.Repo) {
	t.Helper()
	dir := t.TempDir()
	p := paths.WithRoot(dir)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	repo, err := database.InsertRepoWithID("testrepo", "/tmp/test-repo", "https://github.com/test/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "feature", "abc123", "def456")
	if err != nil {
		t.Fatal(err)
	}
	return database, p, run, repo
}

// eventCollector is a thread-safe event accumulator for tests.
type eventCollector struct {
	mu     sync.Mutex
	events []ipc.Event
}

func (ec *eventCollector) handler(e ipc.Event) {
	ec.mu.Lock()
	ec.events = append(ec.events, e)
	ec.mu.Unlock()
}

func (ec *eventCollector) all() []ipc.Event {
	ec.mu.Lock()
	defer ec.mu.Unlock()
	out := make([]ipc.Event, len(ec.events))
	copy(out, ec.events)
	return out
}

func (ec *eventCollector) find(eventType ipc.EventType, stepName types.StepName) *ipc.Event {
	ec.mu.Lock()
	defer ec.mu.Unlock()
	for _, e := range ec.events {
		if e.Type == eventType && e.StepName != nil && *e.StepName == stepName {
			cp := e
			return &cp
		}
	}
	return nil
}

func (ec *eventCollector) findLast(eventType ipc.EventType, status string) *ipc.Event {
	ec.mu.Lock()
	defer ec.mu.Unlock()
	for i := len(ec.events) - 1; i >= 0; i-- {
		e := ec.events[i]
		if e.Type == eventType && e.Status != nil && *e.Status == status {
			cp := e
			return &cp
		}
	}
	return nil
}

func (ec *eventCollector) findRunEvent(eventType ipc.EventType) *ipc.Event {
	ec.mu.Lock()
	defer ec.mu.Unlock()
	for _, e := range ec.events {
		if e.Type == eventType && e.StepName == nil {
			cp := e
			return &cp
		}
	}
	return nil
}

func collectEvents(exec *Executor) *eventCollector {
	ec := &eventCollector{}
	exec.onEvent = ec.handler
	return ec
}

// --- tests ---

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

func TestExecutor_ApprovalApprove(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	steps := []Step{
		newApprovalStep(types.StepReview, `{"findings":[{"severity":"error","description":"bug found"}],"summary":"1 issue found"}`),
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
		newApprovalStep(types.StepReview, `{"findings":[{"severity":"error","description":"bug"}],"summary":"1 issue"}`),
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

func TestExecutor_ApprovalApprovePreservesExitCode(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	steps := []Step{
		&mockStep{
			name: types.StepTest,
			outcome: &StepOutcome{
				NeedsApproval: true,
				Findings:      `{"findings":[{"severity":"error","description":"tests failed"}],"summary":"failing test output"}`,
				ExitCode:      42,
			},
		},
	}

	exec := NewExecutor(database, p, nil, nil, steps, nil)

	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(context.Background(), run, repo, workDir)
	}()

	waitForStepStatus(t, database, run.ID, types.StepTest, types.StepStatusAwaitingApproval)
	if err := exec.Respond(types.StepTest, types.ActionApprove, nil); err != nil {
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

	dbSteps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dbSteps[0].ExitCode == nil || *dbSteps[0].ExitCode != 42 {
		t.Fatalf("exit code = %v, want 42", dbSteps[0].ExitCode)
	}
}

func TestExecutor_ApprovalSkip(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	steps := []Step{
		newApprovalStep(types.StepReview, ""),
		newPassStep(types.StepTest),
	}

	exec := NewExecutor(database, p, nil, nil, steps, nil)

	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(context.Background(), run, repo, workDir)
	}()

	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	exec.Respond(types.StepReview, types.ActionSkip, nil)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor timed out")
	}

	// Review should be skipped, test should be completed
	dbSteps, _ := database.GetStepsByRun(run.ID)
	if dbSteps[0].Status != types.StepStatusSkipped {
		t.Errorf("review: expected %q, got %q", types.StepStatusSkipped, dbSteps[0].Status)
	}
	if dbSteps[1].Status != types.StepStatusCompleted {
		t.Errorf("test: expected %q, got %q", types.StepStatusCompleted, dbSteps[1].Status)
	}
}

func TestExecutor_ApprovalAbort(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	steps := []Step{
		newApprovalStep(types.StepReview, ""),
		newPassStep(types.StepTest),
	}

	exec := NewExecutor(database, p, nil, nil, steps, nil)

	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(context.Background(), run, repo, workDir)
	}()

	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	exec.Respond(types.StepReview, types.ActionAbort, nil)

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error from abort, got nil")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor timed out")
	}

	// Run should be failed
	updated, _ := database.GetRun(run.ID)
	if updated.Status != types.RunFailed {
		t.Errorf("expected run status %q, got %q", types.RunFailed, updated.Status)
	}

	// Review should be failed, test should be pending
	dbSteps, _ := database.GetStepsByRun(run.ID)
	if dbSteps[0].Status != types.StepStatusFailed {
		t.Errorf("review: expected %q, got %q", types.StepStatusFailed, dbSteps[0].Status)
	}
	if dbSteps[1].Status != types.StepStatusPending {
		t.Errorf("test: expected %q, got %q", types.StepStatusPending, dbSteps[1].Status)
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

func TestExecutor_FixEmitsDiffAndFixReviewStatus(t *testing.T) {
	database, p, run, repo := setupTest(t)

	// Create a real git repo as workDir so DiffHead works
	workDir := t.TempDir()
	initGitRepo(t, workDir)

	// Step that needs approval on first call and after fix
	callCount := 0
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			callCount++
			if sctx.Fixing {
				// Simulate agent making changes in the worktree
				writeTestFile(t, workDir, "fix.txt", "agent fix\n")
				execGit(t, workDir, "add", "fix.txt")
			}
			return &StepOutcome{NeedsApproval: true, Findings: `{"items":[]}`}, nil
		},
	}

	steps := []Step{step}
	exec := NewExecutor(database, p, nil, nil, steps, nil)
	events := collectEvents(exec)

	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(context.Background(), run, repo, workDir)
	}()

	// First: step reaches awaiting_approval (not fix_review)
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)

	// Verify initial event has awaiting_approval status
	initialEvent := events.find(ipc.EventStepCompleted, types.StepReview)
	if initialEvent == nil {
		t.Fatal("expected step_completed event for review")
	}
	if initialEvent.Status == nil || *initialEvent.Status != string(types.StepStatusAwaitingApproval) {
		t.Errorf("expected awaiting_approval status, got %v", initialEvent.Status)
	}
	if initialEvent.Diff != nil {
		t.Error("expected no diff on initial approval")
	}

	// Send fix action
	exec.Respond(types.StepReview, types.ActionFix, nil)

	// Find the fix_review event
	fixEvent := waitForEvent(t, events, ipc.EventStepCompleted, string(types.StepStatusFixReview))

	// Verify diff is included in the event
	if fixEvent.Diff == nil || *fixEvent.Diff == "" {
		t.Error("expected diff in fix_review event")
	} else if !strings.Contains(*fixEvent.Diff, "fix.txt") {
		t.Errorf("expected diff to mention fix.txt, got: %s", *fixEvent.Diff)
	}

	// Approve to end
	exec.Respond(types.StepReview, types.ActionApprove, nil)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor timed out")
	}
}

func TestExecutor_FixEmitsFixingStatusImmediately(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	fixStarted := make(chan struct{})
	releaseFix := make(chan struct{})
	callCount := 0
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			callCount++
			if callCount == 1 {
				return &StepOutcome{NeedsApproval: true, Findings: `{"issues":["bug"]}`}, nil
			}
			close(fixStarted)
			<-releaseFix
			return &StepOutcome{ExitCode: 0}, nil
		},
	}

	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	events := collectEvents(exec)

	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(context.Background(), run, repo, workDir)
	}()

	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	if err := exec.Respond(types.StepReview, types.ActionFix, nil); err != nil {
		t.Fatal(err)
	}

	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusFixing)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if event := events.findLast(ipc.EventStepCompleted, string(types.StepStatusFixing)); event != nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if event := events.findLast(ipc.EventStepCompleted, string(types.StepStatusFixing)); event == nil {
		close(releaseFix)
		<-done
		t.Fatal("expected step_completed event with fixing status after fix was accepted")
	}

	<-fixStarted
	close(releaseFix)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor timed out")
	}
}

func TestExecutor_FixReviewNoChanges(t *testing.T) {
	database, p, run, repo := setupTest(t)

	// Create a real git repo as workDir
	workDir := t.TempDir()
	initGitRepo(t, workDir)

	// Step that needs approval both times but agent makes no changes on fix
	callCount := 0
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			callCount++
			return &StepOutcome{NeedsApproval: true, Findings: `{"items":[]}`}, nil
		},
	}

	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	events := collectEvents(exec)

	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(context.Background(), run, repo, workDir)
	}()

	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	exec.Respond(types.StepReview, types.ActionFix, nil)

	// No changes made — diff should not be in event
	fixEvent := waitForEvent(t, events, ipc.EventStepCompleted, string(types.StepStatusFixReview))
	if fixEvent.Diff != nil {
		t.Error("expected no diff when agent made no changes")
	}

	exec.Respond(types.StepReview, types.ActionApprove, nil)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor timed out")
	}
}

func TestExecutor_FixSetsPreviousFindings(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	findings := `{"findings":[{"severity":"error","file":"main.go","line":42,"description":"nil pointer dereference"}],"summary":"1 error found"}`
	var capturedFindings string

	callCount := 0
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			callCount++
			if callCount == 1 {
				// First call: return findings that need approval
				return &StepOutcome{NeedsApproval: true, Findings: findings}, nil
			}
			// Second call (fix): capture PreviousFindings and pass
			capturedFindings = sctx.PreviousFindings
			return &StepOutcome{}, nil
		},
	}

	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)

	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(context.Background(), run, repo, workDir)
	}()

	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	exec.Respond(types.StepReview, types.ActionFix, nil)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor timed out")
	}

	items := mustParseFindingItems(t, capturedFindings)
	if len(items) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(items))
	}
	if items[0].ID != "review-1" || items[0].Description != "nil pointer dereference" {
		t.Errorf("unexpected PreviousFindings: %#v", items)
	}
}

func TestExecutor_AssignsFindingIDsBeforePersistingAndEmitting(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			return &StepOutcome{
				NeedsApproval: true,
				Findings:      `{"findings":[{"severity":"error","description":"first"},{"severity":"warning","description":"second"}],"summary":"2 findings"}`,
			}, nil
		},
	}

	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	events := collectEvents(exec)

	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(context.Background(), run, repo, workDir)
	}()

	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)

	paused := events.find(ipc.EventStepCompleted, types.StepReview)
	if paused == nil || paused.Findings == nil {
		t.Fatal("expected paused step event with findings")
	}

	items := mustParseFindingItems(t, *paused.Findings)
	if len(items) != 2 {
		t.Fatalf("expected 2 findings, got %d", len(items))
	}
	if items[0].ID != "review-1" || items[1].ID != "review-2" {
		t.Fatalf("unexpected finding IDs: %#v", items)
	}

	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if steps[0].FindingsJSON == nil {
		t.Fatal("expected findings stored in DB")
	}
	storedItems := mustParseFindingItems(t, *steps[0].FindingsJSON)
	if len(storedItems) != 2 {
		t.Fatalf("expected 2 stored findings, got %d", len(storedItems))
	}
	if storedItems[0].ID != "review-1" || storedItems[1].ID != "review-2" {
		t.Fatalf("unexpected stored finding IDs: %#v", storedItems)
	}

	if err := exec.Respond(types.StepReview, types.ActionAbort, nil); err != nil {
		t.Fatal(err)
	}
	<-done
}

func TestExecutor_FixUsesSelectedFindingIDsOnly(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	var capturedFindings string
	callCount := 0
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			callCount++
			if callCount == 1 {
				return &StepOutcome{
					NeedsApproval: true,
					Findings:      `{"findings":[{"id":"review-1","severity":"error","description":"first"},{"id":"review-2","severity":"warning","description":"second"}],"summary":"2 findings"}`,
				}, nil
			}
			capturedFindings = sctx.PreviousFindings
			return &StepOutcome{}, nil
		},
	}

	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)

	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(context.Background(), run, repo, workDir)
	}()

	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	if err := exec.Respond(types.StepReview, types.ActionFix, []string{"review-2"}); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor timed out")
	}

	items := mustParseFindingItems(t, capturedFindings)
	if len(items) != 1 {
		t.Fatalf("expected 1 selected finding, got %d", len(items))
	}
	if items[0].ID != "review-2" || items[0].Description != "second" {
		t.Fatalf("unexpected selected finding: %#v", items[0])
	}
}

func TestExecutor_FixClearsStoredFindingsAfterSuccessfulReRun(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	callCount := 0
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			callCount++
			if callCount == 1 {
				return &StepOutcome{
					NeedsApproval: true,
					Findings:      `{"findings":[{"severity":"error","description":"first pass issue"}],"summary":"1 issue"}`,
				}, nil
			}
			return &StepOutcome{}, nil
		},
	}

	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)

	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(context.Background(), run, repo, workDir)
	}()

	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	if err := exec.Respond(types.StepReview, types.ActionFix, nil); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor timed out")
	}

	dbSteps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dbSteps[0].FindingsJSON != nil {
		t.Fatalf("expected findings to be cleared, got %q", *dbSteps[0].FindingsJSON)
	}
}

func TestExecutor_FixSelectedFindingsRewritesSummary(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	var capturedFindings string
	callCount := 0
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			callCount++
			if callCount == 1 {
				return &StepOutcome{
					NeedsApproval: true,
					Findings:      `{"findings":[{"id":"review-1","severity":"error","description":"first"},{"id":"review-2","severity":"warning","description":"second"}],"summary":"2 findings"}`,
				}, nil
			}
			capturedFindings = sctx.PreviousFindings
			return &StepOutcome{}, nil
		},
	}

	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)

	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(context.Background(), run, repo, workDir)
	}()

	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	if err := exec.Respond(types.StepReview, types.ActionFix, []string{"review-2"}); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor timed out")
	}

	var payload struct {
		Findings []findingJSON `json:"findings"`
		Summary  string        `json:"summary"`
	}
	if err := json.Unmarshal([]byte(capturedFindings), &payload); err != nil {
		t.Fatalf("parse findings JSON: %v", err)
	}
	if len(payload.Findings) != 1 || payload.Findings[0].ID != "review-2" {
		t.Fatalf("unexpected selected findings payload: %#v", payload.Findings)
	}
	if payload.Summary != "1 selected finding" {
		t.Fatalf("summary = %q, want %q", payload.Summary, "1 selected finding")
	}
}

func TestExecutor_PreviousFindingsEmptyOnFirstExecution(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	var capturedFindings string
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			capturedFindings = sctx.PreviousFindings
			return &StepOutcome{}, nil
		},
	}

	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	err := exec.Execute(context.Background(), run, repo, workDir)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if capturedFindings != "" {
		t.Errorf("PreviousFindings should be empty on first execution, got: %s", capturedFindings)
	}
}

func TestExecutor_ContextCancellation(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	// Step that blocks until context is cancelled
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			<-sctx.Ctx.Done()
			return nil, sctx.Ctx.Err()
		},
	}

	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(ctx, run, repo, workDir)
	}()

	// Give executor time to start
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error from cancellation, got nil")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor timed out")
	}

	updated, _ := database.GetRun(run.ID)
	if updated.Status != types.RunFailed {
		t.Errorf("expected run status %q, got %q", types.RunFailed, updated.Status)
	}
}

func TestExecutor_ContextCancelCause(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	// Two steps: first passes, second blocks until context is cancelled.
	// This tests that the cause propagates even when detected between steps.
	step1 := newPassStep(types.StepReview)
	step2 := &adaptiveCallStep{
		name: types.StepTest,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			<-sctx.Ctx.Done()
			return nil, context.Cause(sctx.Ctx)
		},
	}

	exec := NewExecutor(database, p, nil, nil, []Step{step1, step2}, nil)

	cancelReason := fmt.Errorf("cancelled: superseded by new push")
	ctx, cancel := context.WithCancelCause(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(ctx, run, repo, workDir)
	}()

	// Give executor time to start step2
	time.Sleep(50 * time.Millisecond)
	cancel(cancelReason)

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error from cancellation, got nil")
		}
		if !strings.Contains(err.Error(), "superseded by new push") {
			t.Errorf("expected error to contain 'superseded by new push', got %q", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor timed out")
	}

	updated, _ := database.GetRun(run.ID)
	if updated.Status != types.RunCancelled {
		t.Errorf("expected run status %q, got %q", types.RunCancelled, updated.Status)
	}
	if updated.Error == nil || !strings.Contains(*updated.Error, "superseded by new push") {
		var got string
		if updated.Error != nil {
			got = *updated.Error
		}
		t.Errorf("expected run error to contain 'superseded by new push', got %q", got)
	}
}

func TestExecutor_ContextCancelCauseBetweenSteps(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	// First step passes and signals, cancel fires before second step starts.
	started := make(chan struct{})
	step1 := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			close(started)
			// Small delay so cancel can fire before step2 starts
			time.Sleep(50 * time.Millisecond)
			return &StepOutcome{ExitCode: 0}, nil
		},
	}
	step2 := newPassStep(types.StepTest)

	exec := NewExecutor(database, p, nil, nil, []Step{step1, step2}, nil)

	cancelReason := fmt.Errorf("cancelled: superseded by new push")
	ctx, cancel := context.WithCancelCause(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(ctx, run, repo, workDir)
	}()

	<-started
	cancel(cancelReason)

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error from cancellation, got nil")
		}
		if !strings.Contains(err.Error(), "superseded by new push") {
			t.Errorf("expected error to contain 'superseded by new push', got %q", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor timed out")
	}

	updated, _ := database.GetRun(run.ID)
	if updated.Status != types.RunCancelled {
		t.Errorf("expected run status %q, got %q", types.RunCancelled, updated.Status)
	}
	if updated.Error == nil || !strings.Contains(*updated.Error, "superseded by new push") {
		var got string
		if updated.Error != nil {
			got = *updated.Error
		}
		t.Errorf("expected run error to contain 'superseded by new push', got %q", got)
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

func TestExecutor_LogCallback(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	var logMessages []string
	var mu sync.Mutex

	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			if sctx.Log != nil {
				sctx.Log("hello from review")
			}
			return &StepOutcome{ExitCode: 0}, nil
		},
	}

	onEvent := func(e ipc.Event) {
		if e.Type == ipc.EventLogChunk && e.Content != nil {
			mu.Lock()
			logMessages = append(logMessages, *e.Content)
			mu.Unlock()
		}
	}

	exec := NewExecutor(database, p, nil, nil, []Step{step}, onEvent)
	exec.Execute(context.Background(), run, repo, workDir)

	mu.Lock()
	defer mu.Unlock()
	found := false
	for _, msg := range logMessages {
		if msg == "hello from review" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected log message 'hello from review' in events, got: %v", logMessages)
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

func TestExecutor_RunLogDir(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	exec := NewExecutor(database, p, nil, nil, []Step{newPassStep(types.StepReview)}, nil)
	exec.Execute(context.Background(), run, repo, workDir)

	// Verify log dir was created
	logDir := p.RunLogDir(run.ID)
	if !dirExists(logDir) {
		t.Errorf("expected run log dir to exist: %s", logDir)
	}

	// Verify step log_path is set
	dbSteps, _ := database.GetStepsByRun(run.ID)
	if dbSteps[0].LogPath == nil {
		t.Fatal("expected log_path to be set")
	}
	expected := filepath.Join(logDir, "review.log")
	if *dbSteps[0].LogPath != expected {
		t.Errorf("expected log_path %q, got %q", expected, *dbSteps[0].LogPath)
	}
}

func TestExecutor_LogFileWritten(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			sctx.Log("first log line")
			sctx.Log("second log line")
			return &StepOutcome{}, nil
		},
	}

	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	exec.Execute(context.Background(), run, repo, workDir)

	// Verify log file exists and contains the log messages
	logPath := filepath.Join(p.RunLogDir(run.ID), "review.log")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("expected log file at %s: %v", logPath, err)
	}
	content := string(data)
	if !strings.Contains(content, "first log line") {
		t.Errorf("expected log file to contain 'first log line', got: %s", content)
	}
	if !strings.Contains(content, "second log line") {
		t.Errorf("expected log file to contain 'second log line', got: %s", content)
	}
}

func TestExecutor_LogFileMultipleSteps(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	step1 := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			sctx.Log("review message")
			return &StepOutcome{}, nil
		},
	}
	step2 := &adaptiveCallStep{
		name: types.StepTest,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			sctx.Log("test message")
			return &StepOutcome{}, nil
		},
	}

	exec := NewExecutor(database, p, nil, nil, []Step{step1, step2}, nil)
	exec.Execute(context.Background(), run, repo, workDir)

	// Each step should have its own log file
	reviewLog, err := os.ReadFile(filepath.Join(p.RunLogDir(run.ID), "review.log"))
	if err != nil {
		t.Fatalf("expected review log file: %v", err)
	}
	if !strings.Contains(string(reviewLog), "review message") {
		t.Errorf("review log missing message, got: %s", reviewLog)
	}

	testLog, err := os.ReadFile(filepath.Join(p.RunLogDir(run.ID), "test.log"))
	if err != nil {
		t.Fatalf("expected test log file: %v", err)
	}
	if !strings.Contains(string(testLog), "test message") {
		t.Errorf("test log missing message, got: %s", testLog)
	}

	// Review log should NOT contain test message
	if strings.Contains(string(reviewLog), "test message") {
		t.Error("review log should not contain test message")
	}
}

func TestExecutor_AutoFixTriggersWithoutApproval(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	// Config with auto-fix enabled for review (max 3 attempts)
	cfg := &config.Config{AutoFix: config.AutoFix{Review: 3}}

	callCount := 0
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			callCount++
			if callCount == 1 {
				return &StepOutcome{
					NeedsApproval: true,
					AutoFixable:   true,
					Findings:      `{"findings":[{"severity":"error","description":"bug"}],"summary":"1 issue"}`,
				}, nil
			}
			// After auto-fix, verify Fixing is set
			if !sctx.Fixing {
				t.Error("expected Fixing to be true on auto-fix re-execution")
			}
			if sctx.PreviousFindings == "" {
				t.Error("expected PreviousFindings to be set on auto-fix")
			}
			return &StepOutcome{}, nil
		},
	}

	exec := NewExecutor(database, p, cfg, nil, []Step{step}, nil)

	err := exec.Execute(context.Background(), run, repo, workDir)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	if callCount != 2 {
		t.Errorf("expected step called 2 times (initial + auto-fix), got %d", callCount)
	}

	updated, _ := database.GetRun(run.ID)
	if updated.Status != types.RunCompleted {
		t.Errorf("expected run status %q, got %q", types.RunCompleted, updated.Status)
	}
}

func TestExecutor_AutoFixRespectsMaxAttempts(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	// Config with auto-fix limited to 2 attempts for lint
	cfg := &config.Config{AutoFix: config.AutoFix{Lint: 2}}

	callCount := 0
	step := &adaptiveCallStep{
		name: types.StepLint,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			callCount++
			// Always return NeedsApproval to exhaust auto-fix attempts
			return &StepOutcome{
				NeedsApproval: true,
				AutoFixable:   true,
				Findings:      `{"findings":[{"severity":"warning","description":"style issue"}],"summary":"lint issue"}`,
			}, nil
		},
	}

	exec := NewExecutor(database, p, cfg, nil, []Step{step}, nil)

	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(context.Background(), run, repo, workDir)
	}()

	// After 2 auto-fix attempts fail, should fall back to manual approval
	// 1 initial + 2 auto-fix = 3 calls, then waits for approval
	// Status is fix_review since auto-fix cycles ran (sctx.Fixing was true)
	waitForStepStatus(t, database, run.ID, types.StepLint, types.StepStatusFixReview)

	if callCount != 3 {
		t.Errorf("expected 3 calls (1 initial + 2 auto-fix), got %d", callCount)
	}

	// Now approve manually to finish
	exec.Respond(types.StepLint, types.ActionApprove, nil)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor timed out")
	}
}

func TestExecutor_AutoFixDisabledWithZero(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	// Config with auto-fix disabled for review
	cfg := &config.Config{AutoFix: config.AutoFix{Review: 0}}

	callCount := 0
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			callCount++
			return &StepOutcome{
				NeedsApproval: true,
				Findings:      `{"findings":[{"severity":"error","description":"bug"}],"summary":"1 issue"}`,
			}, nil
		},
	}

	exec := NewExecutor(database, p, cfg, nil, []Step{step}, nil)

	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(context.Background(), run, repo, workDir)
	}()

	// Should immediately wait for approval (no auto-fix)
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)

	if callCount != 1 {
		t.Errorf("expected 1 call (no auto-fix), got %d", callCount)
	}

	exec.Respond(types.StepReview, types.ActionApprove, nil)

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected no error, got: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor timed out")
	}
}

func TestExecutor_AutoFixNilConfigUsesDefaults(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	// nil config - executor should not panic and should use no auto-fix (backwards compat)
	callCount := 0
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			callCount++
			return &StepOutcome{
				NeedsApproval: true,
				Findings:      `{"findings":[{"severity":"error","description":"bug"}],"summary":"1 issue"}`,
			}, nil
		},
	}

	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)

	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(context.Background(), run, repo, workDir)
	}()

	// With nil config, should wait for approval
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)

	if callCount != 1 {
		t.Errorf("expected 1 call (nil config, no auto-fix), got %d", callCount)
	}

	exec.Respond(types.StepReview, types.ActionAbort, nil)
	<-done
}

func TestExecutor_AutoFixEmitsEvents(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	cfg := &config.Config{AutoFix: config.AutoFix{Lint: 1}}

	callCount := 0
	step := &adaptiveCallStep{
		name: types.StepLint,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			callCount++
			if callCount == 1 {
				return &StepOutcome{
					NeedsApproval: true,
					AutoFixable:   true,
					Findings:      `{"findings":[{"severity":"warning","description":"issue"}],"summary":"1 issue"}`,
				}, nil
			}
			return &StepOutcome{}, nil
		},
	}

	exec := NewExecutor(database, p, cfg, nil, []Step{step}, nil)
	events := collectEvents(exec)

	err := exec.Execute(context.Background(), run, repo, workDir)
	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	// Should have a fixing status event from auto-fix
	fixingEvent := events.findLast(ipc.EventStepCompleted, string(types.StepStatusFixing))
	if fixingEvent == nil {
		t.Error("expected step_completed event with fixing status during auto-fix")
	}
}

func TestExecutor_DoesNotAutoFixManualApprovalOutcome(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	cfg := &config.Config{AutoFix: config.AutoFix{Test: 3}}

	callCount := 0
	step := &adaptiveCallStep{
		name: types.StepTest,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			callCount++
			return &StepOutcome{
				NeedsApproval: true,
				Findings:      `{"findings":[{"severity":"info","description":"new test file written by agent: agent_test.go"}],"summary":"tests passed, but agent wrote new test files"}`,
			}, nil
		},
	}

	exec := NewExecutor(database, p, cfg, nil, []Step{step}, nil)

	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(context.Background(), run, repo, workDir)
	}()

	waitForStepStatus(t, database, run.ID, types.StepTest, types.StepStatusAwaitingApproval)

	if callCount != 1 {
		t.Fatalf("expected 1 call for manual approval outcome, got %d", callCount)
	}

	if err := exec.Respond(types.StepTest, types.ActionApprove, nil); err != nil {
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
}

// --- helper types ---

// adaptiveCallStep allows custom Execute logic via a function.
type adaptiveCallStep struct {
	name types.StepName
	fn   func(sctx *StepContext) (*StepOutcome, error)
}

func (a *adaptiveCallStep) Name() types.StepName { return a.name }
func (a *adaptiveCallStep) Execute(sctx *StepContext) (*StepOutcome, error) {
	return a.fn(sctx)
}

// waitForEvent polls the event collector until an event with the given type and status appears.
func waitForEvent(t *testing.T, ec *eventCollector, eventType ipc.EventType, status string) *ipc.Event {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if e := ec.findLast(eventType, status); e != nil {
			return e
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("event %s with status %q not found within timeout", eventType, status)
	return nil
}

// waitForStepStatus polls the DB until a step reaches the expected status.
func waitForStepStatus(t *testing.T, database *db.DB, runID string, stepName types.StepName, expected types.StepStatus) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		steps, err := database.GetStepsByRun(runID)
		if err == nil {
			for _, s := range steps {
				if s.StepName == stepName && s.Status == expected {
					return
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("step %s did not reach status %q within timeout", stepName, expected)
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.IsDir()
}

type findingJSON struct {
	ID          string `json:"id"`
	Severity    string `json:"severity"`
	Description string `json:"description"`
}

func mustParseFindingItems(t *testing.T, raw string) []findingJSON {
	t.Helper()
	var payload struct {
		Findings []findingJSON `json:"findings"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatalf("parse findings JSON: %v", err)
	}
	return payload.Findings
}

// initGitRepo creates a git repo with an initial commit.
func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	execGit(t, dir, "init")
	execGit(t, dir, "config", "user.email", "test@test.com")
	execGit(t, dir, "config", "user.name", "Test")
	writeTestFile(t, dir, "README.md", "# test\n")
	execGit(t, dir, "add", ".")
	execGit(t, dir, "commit", "-m", "initial")
}

func execGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
}

func writeTestFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

package pipeline

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// overrideVerifyingStep is a mockStep that also implements
// pipeline.ApprovalOverrideVerifier, so these tests can exercise the
// executor's ActionApprove override-recording path without a real CI/SCM
// host. It never implements ApprovalGateReconciler: these tests are about
// what happens when a human explicitly answers approve, not about
// auto-reconciliation while parked.
type overrideVerifyingStep struct {
	*mockStep
	verify func(sctx *StepContext) (string, error)
}

func (o *overrideVerifyingStep) VerifyApprovalOverride(sctx *StepContext) (string, error) {
	return o.verify(sctx)
}

func newOverrideVerifyingApprovalStep(name types.StepName, findings string, verify func(sctx *StepContext) (string, error)) *overrideVerifyingStep {
	return &overrideVerifyingStep{
		mockStep: newApprovalStep(name, findings),
		verify:   verify,
	}
}

// runApprovalOverrideCase drives a single-step run through NeedsApproval,
// answers ActionApprove once parked, and returns the completed step's
// OverrideReason column (nil when never set).
func runApprovalOverrideCase(t *testing.T, step Step) *string {
	t.Helper()
	database, p, run, repo := setupTest(t)
	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)

	done := make(chan error, 1)
	go func() { done <- exec.Execute(context.Background(), run, repo, t.TempDir()) }()

	waitForStepStatus(t, database, run.ID, step.Name(), types.StepStatusAwaitingApproval)
	if err := exec.Respond(step.Name(), types.ActionApprove, nil); err != nil {
		t.Fatalf("Respond(approve) error = %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Execute() error = %v, want approval to complete the run", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor did not complete after approval")
	}

	got, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != types.RunCompleted {
		t.Fatalf("run status = %s, want %s (approval must still proceed)", got.Status, types.RunCompleted)
	}

	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 || steps[0].Status != types.StepStatusCompleted {
		t.Fatalf("steps after approval = %+v, want one completed step", steps)
	}
	return steps[0].OverrideReason
}

// TestExecutor_ApprovalOverride_StillFailingIsRecorded is the core regression
// for the "no-mistakes must never self-report a passing terminal outcome when
// the live required-check view still contains an unresolved failure" defect:
// a human approves a CI-shaped gate while the live condition is still
// unresolved. The approval must still proceed (never block a deliberate
// operator decision) but the completion must carry a durable override marker
// instead of reading identically to a genuinely green run.
func TestExecutor_ApprovalOverride_StillFailingIsRecorded(t *testing.T) {
	step := newOverrideVerifyingApprovalStep(types.StepCI, `{"summary":"CI check failing: required-check"}`,
		func(*StepContext) (string, error) {
			return "live checks for https://example/pr/1 still failing: required-check", nil
		})
	reason := runApprovalOverrideCase(t, step)
	if reason == nil || *reason == "" {
		t.Fatal("OverrideReason = nil, want the still-unresolved reason recorded")
	}
	if want := "required-check"; !strings.Contains(*reason, want) {
		t.Errorf("OverrideReason = %q, want it to name %q", *reason, want)
	}
}

// TestExecutor_ApprovalOverride_BecameGreenLeavesNoMark proves the opposite
// direction: when the live condition has cleared by the time the human
// approves (e.g. it turned green moments before they clicked), the completion
// is the ordinary, unqualified pass with no override marker - this is not a
// license to mark every CI approval as an override.
func TestExecutor_ApprovalOverride_BecameGreenLeavesNoMark(t *testing.T) {
	step := newOverrideVerifyingApprovalStep(types.StepCI, `{"summary":"CI check failing: required-check"}`,
		func(*StepContext) (string, error) { return "", nil })
	reason := runApprovalOverrideCase(t, step)
	if reason != nil {
		t.Fatalf("OverrideReason = %q, want nil once the live condition cleared", *reason)
	}
}

// TestExecutor_ApprovalOverride_VerificationErrorFailsClosed covers a live
// verification error (e.g. the provider could not be reached): the interface
// contract fails closed, recording an override rather than silently
// completing as if the condition had cleared.
func TestExecutor_ApprovalOverride_VerificationErrorFailsClosed(t *testing.T) {
	step := newOverrideVerifyingApprovalStep(types.StepCI, `{"summary":"CI check failing: required-check"}`,
		func(*StepContext) (string, error) { return "", errors.New("provider unreachable") })
	reason := runApprovalOverrideCase(t, step)
	if reason == nil || *reason == "" {
		t.Fatal("OverrideReason = nil, want a verification error to fail closed and record an override")
	}
	if !strings.Contains(*reason, "provider unreachable") {
		t.Errorf("OverrideReason = %q, want it to surface the verification error", *reason)
	}
}

// TestExecutor_ApprovalOverride_OrdinaryStepUnaffected pins that a step which
// does not implement ApprovalOverrideVerifier (every step but CI today, e.g.
// review/test/document) behaves exactly as before ActionApprove is answered:
// no override column is ever touched, and the plain completion path used for
// every existing approval test is unchanged.
func TestExecutor_ApprovalOverride_OrdinaryStepUnaffected(t *testing.T) {
	step := newApprovalStep(types.StepReview, `{"issues":["nit"]}`)
	reason := runApprovalOverrideCase(t, step)
	if reason != nil {
		t.Fatalf("OverrideReason = %q, want nil: %s does not implement ApprovalOverrideVerifier", *reason, step.Name())
	}
}

// TestExecutor_ApprovalOverride_RecoveredPathStillFailing exercises the OTHER
// ActionApprove site - the daemon-restart recovery path in Resume - proving
// both callers of the interface route through the same override-recording
// behavior. Setup mirrors TestExecutor_ResumeFatalReconcileErrorFailsRun: a
// step parked at awaiting_approval as if the daemon restarted mid-gate.
func TestExecutor_ApprovalOverride_RecoveredPathStillFailing(t *testing.T) {
	database, p, run, repo := setupTest(t)
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
	findings := `{"summary":"CI check failing: required-check"}`
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

	step := newOverrideVerifyingApprovalStep(types.StepCI, findings, func(*StepContext) (string, error) {
		return "live checks for https://example/pr/1 still failing: required-check", nil
	})
	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)

	done := make(chan error, 1)
	go func() { done <- exec.Resume(context.Background(), run, repo, t.TempDir()) }()

	// The step is already parked at awaiting_approval in the DB before Resume
	// even starts (that is what "recovered" means), so waitForStepStatus
	// resolves immediately and proves nothing about whether Resume's goroutine
	// has reached e.waiting = true yet. Retry Respond instead of gating on a
	// step-local channel: this step deliberately implements no
	// ApprovalGateReconciler hook to observe.
	deadline := time.Now().Add(5 * time.Second)
	var respondErr error
	for time.Now().Before(deadline) {
		if respondErr = exec.Respond(types.StepCI, types.ActionApprove, nil); respondErr == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if respondErr != nil {
		t.Fatalf("Respond(approve) error = %v", respondErr)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Resume() error = %v, want approval to complete the run", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("resumed executor did not complete after approval")
	}

	got, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != types.RunCompleted {
		t.Fatalf("run status = %s, want %s", got.Status, types.RunCompleted)
	}

	// Serialized durable state: re-read the step fresh from the DB (not the
	// in-memory value from before Resume) so this proves the override marker
	// actually persisted, not merely that the in-process struct carries it.
	reread, err := database.GetStepResult(stepResult.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reread.OverrideReason == nil || *reread.OverrideReason == "" {
		t.Fatal("recovered-path OverrideReason = nil, want the still-unresolved reason recorded and durable")
	}
}

// TestExecutor_ApprovalOverride_RunCompletedEventCarriesReason is the producer
// half of the "TUI hides persisted CI overrides" P1: recording the override on
// the step row (tested above) is not enough - the live run_completed delta must
// carry the reason so an attached TUI can show the passed-with-override banner
// without a snapshot read. Before the fix emitRunEvent dropped the reason and
// the banner read as a plain green pass on the event path.
func TestExecutor_ApprovalOverride_RunCompletedEventCarriesReason(t *testing.T) {
	database, p, run, repo := setupTest(t)

	var mu sync.Mutex
	var completedReason *string
	var sawCompleted bool
	onEvent := func(e ipc.Event) {
		if e.Type != ipc.EventRunCompleted {
			return
		}
		mu.Lock()
		sawCompleted = true
		completedReason = e.CIOverrideReason
		mu.Unlock()
	}

	step := newOverrideVerifyingApprovalStep(types.StepCI, `{"summary":"CI check failing: required-check"}`,
		func(*StepContext) (string, error) {
			return "live checks for https://example/pr/1 still failing: required-check", nil
		})
	exec := NewExecutor(database, p, nil, nil, []Step{step}, onEvent)

	done := make(chan error, 1)
	go func() { done <- exec.Execute(context.Background(), run, repo, t.TempDir()) }()

	waitForStepStatus(t, database, run.ID, step.Name(), types.StepStatusAwaitingApproval)
	if err := exec.Respond(step.Name(), types.ActionApprove, nil); err != nil {
		t.Fatalf("Respond(approve) error = %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Execute() error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor did not complete after approval")
	}

	mu.Lock()
	defer mu.Unlock()
	if !sawCompleted {
		t.Fatal("no run_completed event emitted")
	}
	if completedReason == nil || *completedReason == "" {
		t.Fatal("run_completed event CIOverrideReason = nil, want the override reason carried on the delta")
	}
	if !strings.Contains(*completedReason, "required-check") {
		t.Errorf("run_completed CIOverrideReason = %q, want it to name %q", *completedReason, "required-check")
	}
}

package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestModel_View_RunningStepShowsElapsedTime(t *testing.T) {
	configureTUIColors()
	run := testRun()
	run.Steps[0].Status = types.StepStatusCompleted
	run.Steps[0].DurationMS = ptr(int64(1200))
	run.Steps[1].Status = types.StepStatusRunning

	m := NewModel("", nil, run)
	m.width = 80
	m.height = 40
	// Simulate step started 5 seconds ago.
	m.stepStartTimes = map[types.StepName]time.Time{
		types.StepTest: time.Now().Add(-5 * time.Second),
	}

	view := stripANSI(m.View())

	// Running step should show an elapsed time (approximately 5.0s).
	// The completed step shows "1.2s", and the running step should show ~"5.0s".
	if !strings.Contains(view, "5.0s") && !strings.Contains(view, "5.1s") && !strings.Contains(view, "4.9s") {
		t.Errorf("expected running step to show ~5.0s elapsed time, got:\n%s", view)
	}
}

func TestModel_Update_StepStartedRecordsStartTime(t *testing.T) {
	configureTUIColors()
	run := testRun()
	m := NewModel("", nil, run)

	before := time.Now()
	stepName := types.StepReview
	m.Update(eventMsg{event: ipc.Event{
		Type:     ipc.EventStepStarted,
		StepName: &stepName,
	}, subscriptionID: m.subscriptionID})
	after := time.Now()

	startTime, ok := m.stepStartTimes[types.StepReview]
	if !ok {
		t.Fatal("expected stepStartTimes to contain entry for Review step")
	}
	if startTime.Before(before) || startTime.After(after) {
		t.Errorf("expected start time between %v and %v, got %v", before, after, startTime)
	}
}

func TestModel_View_RunningStepNoElapsedWithoutStartTime(t *testing.T) {
	configureTUIColors()
	run := testRun()
	run.Steps[0].Status = types.StepStatusCompleted
	run.Steps[0].DurationMS = ptr(int64(1200))
	run.Steps[1].Status = types.StepStatusRunning

	m := NewModel("", nil, run)
	m.width = 80
	m.height = 40
	// No stepStartTimes set - should not show elapsed time for running step.

	view := stripANSI(m.View())

	// The completed step shows "1.2s", but the running Test step should NOT show any duration.
	// Find the Test line and verify it doesn't have a duration.
	lines := strings.Split(view, "\n")
	for _, line := range lines {
		if strings.Contains(line, "Test") && !strings.Contains(line, "test-") {
			// This is the Test step line - should not contain any "s" duration pattern
			// other than the step name itself.
			content := strings.TrimSpace(line)
			content = strings.ReplaceAll(content, "│", "")
			content = strings.TrimSpace(content)
			if strings.Contains(content, "Test") && strings.Contains(content, ".") && strings.HasSuffix(strings.TrimSpace(content), "s") {
				t.Errorf("expected no elapsed time for running step without start time, but found duration-like text in: %q", content)
			}
		}
	}
}

func TestOutcomeBanner_CancelledShowsBanner(t *testing.T) {
	run := testRun()
	run.Status = types.RunCancelled
	steps := []ipc.StepResultInfo{
		{StepName: types.StepReview, Status: types.StepStatusCompleted},
		{StepName: types.StepTest, Status: types.StepStatusFailed},
	}
	banner := stripANSI(renderOutcomeBanner(run, steps))
	if !strings.Contains(banner, "Pipeline cancelled") {
		t.Errorf("expected 'Pipeline cancelled' in banner, got: %s", banner)
	}
	if !strings.Contains(banner, "✗") {
		t.Errorf("expected ✗ in cancelled banner, got: %s", banner)
	}
}

func TestOutcomeBanner_CancelledShowsElapsedTime(t *testing.T) {
	run := testRun()
	run.Status = types.RunCancelled
	d1 := int64(2000)
	d2 := int64(3500)
	steps := []ipc.StepResultInfo{
		{StepName: types.StepReview, Status: types.StepStatusCompleted, DurationMS: &d1},
		{StepName: types.StepTest, Status: types.StepStatusFailed, DurationMS: &d2},
	}
	banner := stripANSI(renderOutcomeBanner(run, steps))
	if !strings.Contains(banner, "5.5s") {
		t.Errorf("expected elapsed time '5.5s' in cancelled banner, got: %s", banner)
	}
}

func TestOutcomeBanner_CancelledInView(t *testing.T) {
	run := testRun()
	run.Status = types.RunCancelled
	m := NewModel("/tmp/sock", nil, run)
	m.width = 80
	m.height = 40
	m.done = true
	view := stripANSI(m.View())
	if !strings.Contains(view, "Pipeline cancelled") {
		t.Errorf("expected 'Pipeline cancelled' in view when run is cancelled, got: %s", view)
	}
}

// Test: When a step completes via EventStepCompleted and we had a start time
// recorded for it, the completed step should show its final duration in the view.
func TestModel_View_CompletedStepPreservesDuration(t *testing.T) {
	configureTUIColors()
	run := testRun()
	run.Steps[0].Status = types.StepStatusRunning
	run.Status = types.RunRunning

	m := NewModel("", nil, run)
	m.width = 80
	m.height = 40

	// Record a start time 5 seconds ago for the running step.
	m.stepStartTimes[types.StepReview] = time.Now().Add(-5 * time.Second)

	// Step completes via event.
	completedStatus := string(types.StepStatusCompleted)
	stepName := types.StepReview
	m.applyEvent(ipc.Event{
		Type:     ipc.EventStepCompleted,
		StepName: &stepName,
		Status:   &completedStatus,
	})

	view := stripANSI(m.View())

	// The completed Review step should show a duration around 5s.
	lines := strings.Split(view, "\n")
	found := false
	for _, line := range lines {
		if strings.Contains(line, "Review") && strings.Contains(line, "5.") && strings.Contains(line, "s") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected completed Review step to show ~5.0s duration, but it was not found in view:\n%s", view)
	}
}

// Test: When EventStepCompleted arrives with a tracked start time, the DurationMS
// on the step is populated from the elapsed time.
func TestModel_ApplyEvent_StepCompletedSetsDuration(t *testing.T) {
	configureTUIColors()
	run := testRun()
	run.Steps[0].Status = types.StepStatusRunning

	m := NewModel("", nil, run)

	// Record a start time 3 seconds ago.
	m.stepStartTimes[types.StepReview] = time.Now().Add(-3 * time.Second)

	// Step completes via event.
	completedStatus := string(types.StepStatusCompleted)
	stepName := types.StepReview
	m.applyEvent(ipc.Event{
		Type:     ipc.EventStepCompleted,
		StepName: &stepName,
		Status:   &completedStatus,
	})

	// The step should have DurationMS set.
	for _, s := range m.steps {
		if s.StepName == types.StepReview {
			if s.DurationMS == nil {
				t.Fatal("expected DurationMS to be set on completed step with tracked start time")
			}
			// Should be approximately 3000ms (allow 2800-3500 for timing variance).
			if *s.DurationMS < 2800 || *s.DurationMS > 3500 {
				t.Errorf("expected DurationMS ~3000ms, got %d", *s.DurationMS)
			}
			return
		}
	}
	t.Fatal("Review step not found in model steps")
}

// Test: When EventStepCompleted arrives without a tracked start time, DurationMS
// remains nil (no crash, no bogus data).
func TestModel_ApplyEvent_StepCompletedNoDurationWithoutStartTime(t *testing.T) {
	configureTUIColors()
	run := testRun()
	run.Steps[0].Status = types.StepStatusRunning

	m := NewModel("", nil, run)
	// No stepStartTimes entry for Review.

	completedStatus := string(types.StepStatusCompleted)
	stepName := types.StepReview
	m.applyEvent(ipc.Event{
		Type:     ipc.EventStepCompleted,
		StepName: &stepName,
		Status:   &completedStatus,
	})

	for _, s := range m.steps {
		if s.StepName == types.StepReview {
			if s.DurationMS != nil {
				t.Errorf("expected DurationMS to remain nil without start time, got %d", *s.DurationMS)
			}
			return
		}
	}
	t.Fatal("Review step not found in model steps")
}

// Test: When re-attaching, steps with StartedAt but no DurationMS get their
// start times seeded into stepStartTimes so elapsed time can be computed.
func TestNewModel_SeedsStartTimesFromStartedAt(t *testing.T) {
	configureTUIColors()
	startedAt := time.Now().Add(-10 * time.Second).Unix()
	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval
	run.Steps[0].StartedAt = &startedAt

	m := NewModel("", nil, run)

	st, ok := m.stepStartTimes[types.StepReview]
	if !ok {
		t.Fatal("expected stepStartTimes to contain entry for Review step on re-attach")
	}
	// The seeded time should be approximately 10 seconds ago.
	elapsed := time.Since(st)
	if elapsed < 9*time.Second || elapsed > 12*time.Second {
		t.Errorf("expected start time ~10s ago, got %v ago", elapsed)
	}
}

// Test: stepsWithRunningElapsed computes elapsed time for AwaitingApproval steps.
func TestModel_View_AwaitingApprovalShowsElapsedTime(t *testing.T) {
	configureTUIColors()
	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval

	m := NewModel("", nil, run)
	m.width = 80
	m.height = 40
	m.stepStartTimes[types.StepReview] = time.Now().Add(-7 * time.Second)

	view := stripANSI(m.View())

	if !strings.Contains(view, "7.0s") && !strings.Contains(view, "7.1s") && !strings.Contains(view, "6.9s") {
		t.Errorf("expected awaiting approval step to show ~7.0s elapsed time, got:\n%s", view)
	}
}

// Test: stepsWithRunningElapsed computes elapsed time for FixReview steps.
func TestModel_View_FixReviewShowsElapsedTime(t *testing.T) {
	configureTUIColors()
	run := testRun()
	run.Steps[0].Status = types.StepStatusFixReview

	m := NewModel("", nil, run)
	m.width = 80
	m.height = 40
	m.stepStartTimes[types.StepReview] = time.Now().Add(-4 * time.Second)

	view := stripANSI(m.View())

	if !strings.Contains(view, "4.0s") && !strings.Contains(view, "4.1s") && !strings.Contains(view, "3.9s") {
		t.Errorf("expected fix review step to show ~4.0s elapsed time, got:\n%s", view)
	}
}

// Test: Re-attach scenario - step is awaiting approval, TUI connects and shows duration
// computed from StartedAt in the initial run data.
func TestModel_View_ReattachAwaitingApprovalShowsDuration(t *testing.T) {
	configureTUIColors()
	startedAt := time.Now().Add(-15 * time.Second).Unix()
	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval
	run.Steps[0].StartedAt = &startedAt

	m := NewModel("", nil, run)
	m.width = 80
	m.height = 40

	view := stripANSI(m.View())

	// StartedAt is seeded from Unix seconds, so sub-second truncation plus render delay
	// can round this re-attach duration up to 16.0s on slower CI runners.
	if !strings.Contains(view, "15.") && !strings.Contains(view, "14.9") && !strings.Contains(view, "15.0") && !strings.Contains(view, "15.1") && !strings.Contains(view, "16.0") {
		t.Errorf("expected re-attached awaiting approval step to show ~15s elapsed, got:\n%s", view)
	}
}

// Test: When EventStepCompleted carries DurationMS, it takes precedence over
// the computed elapsed time from stepStartTimes.
func TestModel_ApplyEvent_StepCompletedPrefersEventDuration(t *testing.T) {
	configureTUIColors()
	run := testRun()
	run.Steps[0].Status = types.StepStatusRunning

	m := NewModel("", nil, run)
	// Record a start time 10 seconds ago.
	m.stepStartTimes[types.StepReview] = time.Now().Add(-10 * time.Second)

	// Event carries execution-only duration of 2 seconds (excluding approval wait).
	completedStatus := string(types.StepStatusCompleted)
	stepName := types.StepReview
	eventDuration := int64(2000)
	m.applyEvent(ipc.Event{
		Type:       ipc.EventStepCompleted,
		StepName:   &stepName,
		Status:     &completedStatus,
		DurationMS: &eventDuration,
	})

	for _, s := range m.steps {
		if s.StepName == types.StepReview {
			if s.DurationMS == nil {
				t.Fatal("expected DurationMS to be set")
			}
			// Should use event's 2000ms, not the computed ~10000ms.
			if *s.DurationMS != 2000 {
				t.Errorf("expected DurationMS = 2000 (from event), got %d", *s.DurationMS)
			}
			return
		}
	}
	t.Fatal("Review step not found")
}

func TestModel_FixingEventDoesNotFreezeDuration(t *testing.T) {
	configureTUIColors()
	run := testRun()
	run.Steps[0].Status = types.StepStatusRunning

	m := NewModel("", nil, run)
	m.stepStartTimes[types.StepReview] = time.Now().Add(-5 * time.Second)

	fixingStatus := string(types.StepStatusFixing)
	stepName := types.StepReview
	m.applyEvent(ipc.Event{
		Type:     ipc.EventStepCompleted,
		StepName: &stepName,
		Status:   &fixingStatus,
	})

	for _, s := range m.steps {
		if s.StepName == types.StepReview {
			if s.DurationMS != nil {
				t.Errorf("expected DurationMS to remain nil during fixing so timer keeps ticking, got %d", *s.DurationMS)
			}
			return
		}
	}
	t.Fatal("Review step not found")
}

// Test: When a running step auto-fixes (no approval in between), the live timer
// must continue accumulating from the original start time, not reset to 0.
func TestModel_AutoFixPreservesAccumulatedElapsed(t *testing.T) {
	configureTUIColors()
	run := testRun()
	run.Steps[0].Status = types.StepStatusRunning

	m := NewModel("", nil, run)
	m.stepStartTimes[types.StepReview] = time.Now().Add(-5 * time.Second)

	fixingStatus := string(types.StepStatusFixing)
	stepName := types.StepReview
	m.applyEvent(ipc.Event{
		Type:     ipc.EventStepCompleted,
		StepName: &stepName,
		Status:   &fixingStatus,
	})

	// stepsWithRunningElapsed should report ~5s of accumulated time, not 0.
	elapsed := m.stepsWithRunningElapsed()
	for _, s := range elapsed {
		if s.StepName == types.StepReview {
			if s.DurationMS == nil {
				t.Fatal("expected stepsWithRunningElapsed to compute live elapsed for Fixing step")
			}
			if *s.DurationMS < 4500 {
				t.Errorf("expected accumulated elapsed >= 4500ms, got %dms (timer reset to 0)", *s.DurationMS)
			}
			return
		}
	}
	t.Fatal("Review step not found")
}

// Test: When a step already has DurationMS persisted (e.g. from AwaitingApproval)
// and then transitions to Fixing, the stale DurationMS must be cleared and the
// live timer must accumulate from the previous execution time.
func TestModel_FixingEventClearsStaleDuration(t *testing.T) {
	configureTUIColors()
	run := testRun()
	run.Steps[0].Status = types.StepStatusRunning

	m := NewModel("", nil, run)
	m.stepStartTimes[types.StepReview] = time.Now().Add(-10 * time.Second)

	// Simulate step entering AwaitingApproval with 10s of persisted execution time.
	awaitingStatus := string(types.StepStatusAwaitingApproval)
	stepName := types.StepReview
	dur := int64(10000)
	m.applyEvent(ipc.Event{
		Type:       ipc.EventStepCompleted,
		StepName:   &stepName,
		Status:     &awaitingStatus,
		DurationMS: &dur,
	})

	// Verify duration was persisted.
	for _, s := range m.steps {
		if s.StepName == types.StepReview && s.DurationMS == nil {
			t.Fatal("expected DurationMS to be set after AwaitingApproval event")
		}
	}

	// Now simulate user pressing fix - step transitions to Fixing.
	fixingStatus := string(types.StepStatusFixing)
	m.applyEvent(ipc.Event{
		Type:     ipc.EventStepCompleted,
		StepName: &stepName,
		Status:   &fixingStatus,
	})

	// DurationMS must be nil so stepsWithRunningElapsed computes live elapsed.
	for _, s := range m.steps {
		if s.StepName == types.StepReview {
			if s.DurationMS != nil {
				t.Errorf("expected DurationMS to be cleared when entering Fixing, got %d", *s.DurationMS)
			}
			break
		}
	}

	// stepsWithRunningElapsed should accumulate: the 10s of prior execution
	// plus a small amount of wall time since the Fixing event.
	elapsed := m.stepsWithRunningElapsed()
	for _, s := range elapsed {
		if s.StepName == types.StepReview {
			if s.DurationMS == nil {
				t.Fatal("expected stepsWithRunningElapsed to compute live elapsed for Fixing step")
			}
			if *s.DurationMS < 9500 || *s.DurationMS > 11000 {
				t.Errorf("expected accumulated elapsed ~10000ms, got %dms", *s.DurationMS)
			}
			return
		}
	}
	t.Fatal("Review step not found")
}

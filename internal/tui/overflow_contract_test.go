package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The TUI is the only consumer that applies event payloads as deltas against a
// locally-held model, so it is where a coalesced transition would otherwise
// leave the display permanently wrong. These cases cover its half of the
// bounded subscription overflow contract.

func ciRunningModel(ready bool) Model {
	run := &ipc.RunInfo{
		ID:      "run-1",
		Branch:  "feature/foo",
		Status:  types.RunRunning,
		CIReady: ready,
		Steps: []ipc.StepResultInfo{
			{RunID: "run-1", StepName: types.StepCI, Status: types.StepStatusRunning},
		},
	}
	m := NewModel("/tmp/sock", nil, run)
	m.steps = run.Steps
	m.width, m.height = 120, 40
	return m
}

func snapshot(rev int64, ciReady bool, ciStatus types.StepStatus) *ipc.RunInfo {
	return &ipc.RunInfo{
		ID:       "run-1",
		Branch:   "feature/foo",
		Status:   types.RunRunning,
		CIReady:  ciReady,
		StateRev: rev,
		Steps: []ipc.StepResultInfo{
			{RunID: "run-1", StepName: types.StepCI, Status: ciStatus},
		},
	}
}

// firstMsgOfType runs a command (unwrapping a batch) and returns the first
// message of the requested type.
func reconciledFrom(t *testing.T, cmd tea.Cmd) runReconciledMsg {
	t.Helper()
	if cmd == nil {
		t.Fatal("expected a command")
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, c := range batch {
			if c == nil {
				continue
			}
			if sub, ok := c().(runReconciledMsg); ok {
				return sub
			}
		}
		t.Fatal("batch contained no runReconciledMsg")
	}
	got, ok := msg.(runReconciledMsg)
	if !ok {
		t.Fatalf("message = %T, want runReconciledMsg", msg)
	}
	return got
}

// A live "checks passed" title must not survive an authoritative readiness
// invalidation that the daemon had to coalesce away under buffer pressure.
func TestTUIOverflow_GreenTitleClearsFromAuthoritativeSnapshot(t *testing.T) {
	m := ciRunningModel(true)
	m.stateRev = 10
	if !strings.Contains(m.terminalTitle(), "Checks passed") {
		t.Fatalf("precondition: title = %q, want the green state", m.terminalTitle())
	}

	m.reconcile = func(context.Context) (*ipc.RunInfo, error) {
		return snapshot(42, false, types.StepStatusRunning), nil
	}
	updated, cmd := m.Update(eventMsg{
		event:          ipc.Event{Type: ipc.EventStreamGap, RunID: "run-1", StateRev: 42},
		subscriptionID: m.subscriptionID,
	})
	m = updated.(Model)
	if cmd == nil {
		t.Fatal("a stream gap must schedule an authoritative reconciliation")
	}
	updated, _ = m.Update(reconciledFrom(t, cmd))
	m = updated.(Model)

	if got := m.terminalTitle(); strings.Contains(got, "Checks passed") {
		t.Fatalf("title = %q, want the green state cleared by the snapshot", got)
	}
	if m.stateRev != 42 {
		t.Fatalf("stateRev = %d, want 42", m.stateRev)
	}
}

// A delta that was queued before the snapshot must not regress state after it,
// even though it arrives later in the stream.
func TestTUIOverflow_StaleQueuedDeltaCannotRegressAfterSnapshot(t *testing.T) {
	m := ciRunningModel(true)
	m.reconcile = func(context.Context) (*ipc.RunInfo, error) {
		return snapshot(42, false, types.StepStatusCompleted), nil
	}
	updated, cmd := m.Update(eventMsg{
		event:          ipc.Event{Type: ipc.EventStreamGap, RunID: "run-1", StateRev: 42},
		subscriptionID: m.subscriptionID,
	})
	m = updated.(Model)
	updated, _ = m.Update(reconciledFrom(t, cmd))
	m = updated.(Model)

	// Two stale frames drained from behind the gap.
	running := string(types.StepStatusRunning)
	name := types.StepCI
	stale := []ipc.Event{
		{Type: ipc.EventStepStarted, RunID: "run-1", StepName: &name, Status: &running, StateRev: 7},
		{Type: ipc.EventStepCompleted, RunID: "run-1", StepName: &name, Status: &running, StateRev: 11},
	}
	for _, e := range stale {
		updated, _ = m.Update(eventMsg{event: e, subscriptionID: m.subscriptionID})
		m = updated.(Model)
	}

	for _, s := range m.steps {
		if s.StepName == types.StepCI && s.Status != types.StepStatusCompleted {
			t.Fatalf("CI step status = %q, want it not regressed from %q", s.Status, types.StepStatusCompleted)
		}
	}
	if m.stateRev != 42 {
		t.Fatalf("stateRev = %d, want the snapshot revision to stand", m.stateRev)
	}
}

// A newer delta arriving after the snapshot must still apply.
func TestTUIOverflow_NewerDeltaAfterSnapshotStillApplies(t *testing.T) {
	m := ciRunningModel(false)
	m.reconcile = func(context.Context) (*ipc.RunInfo, error) {
		return snapshot(42, false, types.StepStatusRunning), nil
	}
	updated, cmd := m.Update(eventMsg{
		event:          ipc.Event{Type: ipc.EventStreamGap, RunID: "run-1", StateRev: 42},
		subscriptionID: m.subscriptionID,
	})
	m = updated.(Model)
	updated, _ = m.Update(reconciledFrom(t, cmd))
	m = updated.(Model)

	completed := string(types.StepStatusCompleted)
	name := types.StepCI
	updated, _ = m.Update(eventMsg{
		event:          ipc.Event{Type: ipc.EventStepCompleted, RunID: "run-1", StepName: &name, Status: &completed, StateRev: 43},
		subscriptionID: m.subscriptionID,
	})
	m = updated.(Model)

	for _, s := range m.steps {
		if s.StepName == types.StepCI && s.Status != types.StepStatusCompleted {
			t.Fatalf("CI step status = %q, want the newer delta applied", s.Status)
		}
	}
}

// A snapshot older than what the model already holds is ignored, so two
// reconciliations racing cannot move state backwards.
func TestTUIOverflow_OlderSnapshotIsIgnored(t *testing.T) {
	m := ciRunningModel(false)
	m.stateRev = 90
	m.applySnapshot(snapshot(42, true, types.StepStatusRunning))
	if m.run.CIReady {
		t.Fatal("an older snapshot overwrote newer state")
	}
	if m.stateRev != 90 {
		t.Fatalf("stateRev = %d, want 90", m.stateRev)
	}
}

// A terminal snapshot delivered through a gap must end the run view even if
// the run_completed frame itself was coalesced away.
func TestTUIOverflow_TerminalSnapshotThroughGapCompletesTheView(t *testing.T) {
	m := ciRunningModel(false)
	m.reconcile = func(context.Context) (*ipc.RunInfo, error) {
		return &ipc.RunInfo{ID: "run-1", Status: types.RunFailed, StateRev: 50}, nil
	}
	updated, cmd := m.Update(eventMsg{
		event:          ipc.Event{Type: ipc.EventStreamGap, RunID: "run-1", StateRev: 50},
		subscriptionID: m.subscriptionID,
	})
	m = updated.(Model)
	updated, _ = m.Update(reconciledFrom(t, cmd))
	m = updated.(Model)

	if !m.done {
		t.Fatal("terminal snapshot delivered through a gap did not complete the view")
	}
	if m.run.Status != types.RunFailed {
		t.Fatalf("run status = %q, want failed", m.run.Status)
	}
}

// A dropped stream must resubscribe and converge rather than freezing the live
// view on an error. The new subscription opens gapped, so the model resets its
// applied revision and adopts the daemon's numbering.
func TestTUIOverflow_DroppedStreamResubscribesAndResetsRevision(t *testing.T) {
	m := ciRunningModel(true)
	m.stateRev = 10
	updated, cmd := m.Update(subscriptionErrMsg{err: fmt.Errorf("event stream closed"), subscriptionID: m.subscriptionID})
	m = updated.(Model)
	if cmd == nil {
		t.Fatal("a dropped stream must schedule a resubscribe")
	}
	if m.stateRev != 0 {
		t.Fatalf("stateRev = %d, want 0 so the new subscription adopts the daemon's numbering", m.stateRev)
	}
	if m.subscriptionID == 1 {
		t.Fatal("subscription generation did not advance")
	}
	if m.resubscribeTries != 1 {
		t.Fatalf("resubscribeTries = %d, want 1", m.resubscribeTries)
	}
}

// Reconnect attempts are bounded and delayed, so a daemon that is genuinely
// gone surfaces as a visible error instead of a reconnect spin.
func TestTUIOverflow_ReconnectAttemptsAreBounded(t *testing.T) {
	m := ciRunningModel(false)
	var last tea.Cmd
	for i := 0; i < maxResubscribeTries+5; i++ {
		updated, cmd := m.Update(subscriptionErrMsg{err: fmt.Errorf("dial: no daemon"), subscriptionID: m.subscriptionID})
		m = updated.(Model)
		last = cmd
	}
	if m.resubscribeTries != maxResubscribeTries {
		t.Fatalf("resubscribeTries = %d, want it capped at %d", m.resubscribeTries, maxResubscribeTries)
	}
	if last != nil {
		t.Fatal("expected reconnect attempts to stop once the bound is reached")
	}
	if m.err == nil {
		t.Fatal("expected the stream error to remain visible")
	}
}

// A successful reconnect clears the retry budget so a later drop gets a fresh
// set of attempts.
func TestTUIOverflow_SuccessfulReconnectResetsTheRetryBudget(t *testing.T) {
	m := ciRunningModel(false)
	updated, _ := m.Update(subscriptionErrMsg{err: fmt.Errorf("dial: no daemon"), subscriptionID: m.subscriptionID})
	m = updated.(Model)
	if m.resubscribeTries == 0 {
		t.Fatal("precondition: expected a consumed retry")
	}
	events := make(chan ipc.Event)
	updated, _ = m.Update(connectedMsg{events: events, cancelSub: func() {}, subscriptionID: m.subscriptionID})
	m = updated.(Model)
	if m.resubscribeTries != 0 {
		t.Fatalf("resubscribeTries = %d, want 0 after a successful reconnect", m.resubscribeTries)
	}
}

// A completed run must not keep resubscribing.
func TestTUIOverflow_DroppedStreamOnDoneRunDoesNotResubscribe(t *testing.T) {
	m := ciRunningModel(false)
	m.done = true
	_, cmd := m.Update(subscriptionErrMsg{err: fmt.Errorf("event stream closed"), subscriptionID: m.subscriptionID})
	if cmd != nil {
		t.Fatal("a finished run must not resubscribe")
	}
}

// Concurrent gaps coalesce into one in-flight authoritative read plus exactly
// one follow-up, so overflow can never turn into a reconciliation storm.
func TestTUIOverflow_ReconciliationRequestsCoalesce(t *testing.T) {
	m := ciRunningModel(true)
	calls := 0
	m.reconcile = func(context.Context) (*ipc.RunInfo, error) {
		calls++
		return snapshot(int64(50+calls), false, types.StepStatusRunning), nil
	}
	gap := eventMsg{event: ipc.Event{Type: ipc.EventStreamGap, RunID: "run-1", StateRev: 42}, subscriptionID: m.subscriptionID}

	updated, first := m.Update(gap)
	m = updated.(Model)
	if first == nil {
		t.Fatal("first gap should start a reconciliation")
	}
	updated, second := m.Update(gap)
	m = updated.(Model)
	if !m.reconcileAgain {
		t.Fatal("a gap arriving during a reconciliation should schedule exactly one follow-up")
	}
	if second != nil {
		if batch, ok := second().(tea.BatchMsg); ok {
			for _, c := range batch {
				if c == nil {
					continue
				}
				if _, ok := c().(runReconciledMsg); ok {
					t.Fatal("second gap started a concurrent reconciliation instead of coalescing")
				}
			}
		}
	}

	updated, follow := m.Update(reconciledFrom(t, first))
	m = updated.(Model)
	if m.reconcileAgain {
		t.Fatal("follow-up flag was not consumed")
	}
	if follow == nil {
		t.Fatal("the deferred follow-up reconciliation was never issued")
	}
}

// Activity frames carry no revision and must never advance the applied
// revision or trigger reconciliation work.
func TestTUIOverflow_ActivityDoesNotTouchRevisionOrReconcile(t *testing.T) {
	m := ciRunningModel(false)
	m.stateRev = 12
	content := "some agent output\n"
	updated, _ := m.Update(eventMsg{
		event:          ipc.Event{Type: ipc.EventLogChunk, RunID: "run-1", Content: &content},
		subscriptionID: m.subscriptionID,
	})
	m = updated.(Model)
	if m.stateRev != 12 {
		t.Fatalf("stateRev = %d, want activity to leave it untouched", m.stateRev)
	}
	if m.reconcilePending {
		t.Fatal("activity must not trigger an authoritative read")
	}
	if len(m.logs) == 0 {
		t.Fatal("log content was dropped")
	}
}

// Events without a revision (an older daemon) must keep applying, so a new
// client against an old daemon is no worse off than before.
func TestTUIOverflow_UnversionedDeltasStillApply(t *testing.T) {
	m := ciRunningModel(false)
	m.stateRev = 12
	completed := string(types.StepStatusCompleted)
	name := types.StepCI
	updated, _ := m.Update(eventMsg{
		event:          ipc.Event{Type: ipc.EventStepCompleted, RunID: "run-1", StepName: &name, Status: &completed},
		subscriptionID: m.subscriptionID,
	})
	m = updated.(Model)
	for _, s := range m.steps {
		if s.StepName == types.StepCI && s.Status != types.StepStatusCompleted {
			t.Fatalf("CI step status = %q, want an unversioned delta to apply", s.Status)
		}
	}
}

func TestTUIOverflow_UnknownStateEventRequestsAuthoritativeReconciliation(t *testing.T) {
	m := ciRunningModel(false)
	m.stateRev = 8
	m.reconcile = func(context.Context) (*ipc.RunInfo, error) {
		return &ipc.RunInfo{ID: "run-1", Status: types.RunRunning, StateRev: 9}, nil
	}

	updated, cmd := m.Update(eventMsg{
		event:          ipc.Event{Type: "future_state_transition", RunID: "run-1", StateRev: 9},
		subscriptionID: m.subscriptionID,
	})
	m = updated.(Model)
	if !m.reconcilePending || cmd == nil {
		t.Fatal("unknown state event did not request authoritative reconciliation")
	}
	if m.stateRev != 9 {
		t.Fatalf("stateRev = %d, want future event revision 9 retained as the monotonic floor", m.stateRev)
	}
}

// The fix-review diff is fetched on demand and lands in the model, so removing
// it from the event stream costs the user nothing.
func TestTUIOverflow_FixReviewDiffIsFetchedOnDemand(t *testing.T) {
	m := ciRunningModel(false)
	m.fetchStepDiff = func(step types.StepName) (string, error) {
		if step != types.StepReview {
			return "", fmt.Errorf("unexpected step %s", step)
		}
		return "--- a/x\n+++ b/x\n+agent fix\n", nil
	}

	gate := string(types.StepStatusFixReview)
	name := types.StepReview
	updated, cmd := m.Update(eventMsg{
		event:          ipc.Event{Type: ipc.EventStepCompleted, RunID: "run-1", StepName: &name, Status: &gate, StateRev: 5},
		subscriptionID: m.subscriptionID,
	})
	m = updated.(Model)
	if !m.stepDiffFetching[types.StepReview] {
		t.Fatal("gate entry did not request the diff")
	}

	msg := firstMsgOfType[stepDiffMsg](t, cmd)
	updated, _ = m.Update(msg)
	m = updated.(Model)

	if got := m.stepDiffs[types.StepReview]; !strings.Contains(got, "agent fix") {
		t.Fatalf("stepDiffs = %q, want the fetched diff", got)
	}
	if m.stepDiffFetching[types.StepReview] {
		t.Fatal("in-flight marker was not cleared")
	}
}

// A gate reached while the model was out of sync still gets its diff from the
// reconciled snapshot path.
func TestTUIOverflow_SnapshotGateAlsoFetchesItsDiff(t *testing.T) {
	m := ciRunningModel(false)
	m.fetchStepDiff = func(types.StepName) (string, error) { return "diff body\n", nil }
	m.reconcile = func(context.Context) (*ipc.RunInfo, error) {
		return &ipc.RunInfo{
			ID: "run-1", Status: types.RunRunning, StateRev: 30,
			Steps: []ipc.StepResultInfo{
				{RunID: "run-1", StepName: types.StepReview, Status: types.StepStatusFixReview},
			},
		}, nil
	}
	updated, cmd := m.Update(eventMsg{
		event:          ipc.Event{Type: ipc.EventStreamGap, RunID: "run-1", StateRev: 30},
		subscriptionID: m.subscriptionID,
	})
	m = updated.(Model)
	updated, follow := m.Update(reconciledFrom(t, cmd))
	m = updated.(Model)

	if !m.stepDiffFetching[types.StepReview] {
		t.Fatal("reconciled fix-review gate did not request its diff")
	}
	msg := firstMsgOfType[stepDiffMsg](t, follow)
	updated, _ = m.Update(msg)
	m = updated.(Model)
	if m.stepDiffs[types.StepReview] != "diff body\n" {
		t.Fatalf("stepDiffs = %q, want the fetched diff", m.stepDiffs[types.StepReview])
	}
}

func TestTUIOverflow_SnapshotGateReplacesPreviousRoundDiffAndInflightFetch(t *testing.T) {
	m := ciRunningModel(false)
	m.stepDiffs[types.StepReview] = "old round\n"
	m.stepDiffTruncated[types.StepReview] = true
	m.requestStepDiff(types.StepReview, false)
	oldRequestID := m.stepDiffRequestID[types.StepReview]
	m.pendingDiffFetch = nil

	m.applySnapshot(&ipc.RunInfo{
		ID: "run-1", Status: types.RunRunning, StateRev: 20,
		Steps: []ipc.StepResultInfo{
			{RunID: "run-1", StepName: types.StepReview, Status: types.StepStatusFixReview},
		},
	})

	if _, ok := m.stepDiffs[types.StepReview]; ok {
		t.Fatal("snapshot retained the previous fix round diff")
	}
	if m.stepDiffTruncated[types.StepReview] {
		t.Fatal("snapshot retained the previous fix round truncation state")
	}
	if m.stepDiffRequestID[types.StepReview] <= oldRequestID || len(m.pendingDiffFetch) != 1 {
		t.Fatal("snapshot did not supersede the in-flight previous-round diff request")
	}

	stale := stepDiffMsg{
		step: types.StepReview, diff: "stale response\n", requestID: oldRequestID,
		subscriptionID: m.subscriptionID,
	}
	updated, _ := m.Update(stale)
	m = updated.(Model)
	if _, ok := m.stepDiffs[types.StepReview]; ok {
		t.Fatal("stale previous-round diff response mutated the current gate")
	}
}

func TestTUIOverflow_ReconnectClearsGenerationScopedAsyncState(t *testing.T) {
	m := ciRunningModel(false)
	m.reconcilePending = true
	m.reconcileAgain = true
	m.stepDiffFetching[types.StepReview] = true
	m.pendingDiffFetch = []stepDiffRequest{{step: types.StepReview, requestID: 1}}
	oldSubscriptionID := m.subscriptionID

	updated, _ := m.Update(subscriptionErrMsg{
		err: fmt.Errorf("stream closed"), subscriptionID: oldSubscriptionID,
	})
	m = updated.(Model)
	if m.subscriptionID == oldSubscriptionID {
		t.Fatal("subscription generation did not advance")
	}
	if m.reconcilePending || m.reconcileAgain || len(m.stepDiffFetching) != 0 || len(m.pendingDiffFetch) != 0 {
		t.Fatal("reconnect retained generation-scoped reconcile or diff work")
	}

	updated, _ = m.Update(runReconciledMsg{
		run:            &ipc.RunInfo{ID: "run-1", Status: types.RunCompleted, StateRev: 99},
		subscriptionID: oldSubscriptionID,
	})
	m = updated.(Model)
	updated, _ = m.Update(stepDiffMsg{
		step: types.StepReview, diff: "stale\n", requestID: 1,
		subscriptionID: oldSubscriptionID,
	})
	m = updated.(Model)
	if m.done || m.stepDiffs[types.StepReview] == "stale\n" {
		t.Fatal("stale asynchronous result mutated the new subscription generation")
	}

	m.reconcile = func(context.Context) (*ipc.RunInfo, error) {
		return &ipc.RunInfo{ID: "run-1", Status: types.RunRunning, StateRev: 40}, nil
	}
	updated, cmd := m.Update(eventMsg{
		event:          ipc.Event{Type: ipc.EventStreamGap, RunID: "run-1", StateRev: 40},
		subscriptionID: m.subscriptionID,
	})
	m = updated.(Model)
	if !m.reconcilePending || cmd == nil {
		t.Fatal("new generation gap could not start reconciliation")
	}
}

func TestTUIOverflow_TruncatedDiffStateRendersBeforeApprovalActions(t *testing.T) {
	m := ciRunningModel(false)
	m.steps = []ipc.StepResultInfo{
		{RunID: "run-1", StepName: types.StepReview, Status: types.StepStatusFixReview},
	}
	m.stepDiffs[types.StepReview] = "diff body\n"
	m.stepDiffTruncated[types.StepReview] = true
	m.width = 100

	view := stripANSI(m.View())
	warning := "Diff truncated at 512 KiB."
	warningAt := strings.Index(view, warning)
	approveAt := strings.Index(view, "approve")
	if warningAt < 0 {
		t.Fatalf("truncation warning is not visible:\n%s", view)
	}
	if !strings.Contains(view, "Approval applies to the full") {
		t.Fatalf("truncation warning does not disclose approval scope:\n%s", view)
	}
	if approveAt < 0 || warningAt > approveAt {
		t.Fatalf("truncation warning must appear before approval actions:\n%s", view)
	}
}

// A failed diff read is not fatal: the marker clears so a later gate entry can
// retry, and no stale diff is invented.
func TestTUIOverflow_FailedDiffFetchClearsInFlightMarker(t *testing.T) {
	m := ciRunningModel(false)
	m.fetchStepDiff = func(types.StepName) (string, error) { return "", fmt.Errorf("daemon gone") }

	gate := string(types.StepStatusFixReview)
	name := types.StepReview
	updated, cmd := m.Update(eventMsg{
		event:          ipc.Event{Type: ipc.EventStepCompleted, RunID: "run-1", StepName: &name, Status: &gate, StateRev: 5},
		subscriptionID: m.subscriptionID,
	})
	m = updated.(Model)
	updated, _ = m.Update(firstMsgOfType[stepDiffMsg](t, cmd))
	m = updated.(Model)

	if m.stepDiffFetching[types.StepReview] {
		t.Fatal("failed fetch left the step permanently marked in flight")
	}
	if _, ok := m.stepDiffs[types.StepReview]; ok {
		t.Fatal("failed fetch invented a diff")
	}
}

func firstMsgOfType[T tea.Msg](t *testing.T, cmd tea.Cmd) T {
	t.Helper()
	var zero T
	if cmd == nil {
		t.Fatal("expected a command")
	}
	msg := cmd()
	if got, ok := msg.(T); ok {
		return got
	}
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, c := range batch {
			if c == nil {
				continue
			}
			if got, ok := c().(T); ok {
				return got
			}
		}
	}
	t.Fatalf("no message of type %T in command output", zero)
	return zero
}

package daemon

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The acceptance matrix for the bounded subscription overflow contract. Every
// case first fills the ordinary 64-slot path, because the contract only has to
// hold once the buffer is saturated.

func logEvent(runID string) ipc.Event {
	c := "some agent output line"
	return ipc.Event{Type: ipc.EventLogChunk, RunID: runID, Content: &c}
}

func stepEvent(runID string, t ipc.EventType, name types.StepName, status string) ipc.Event {
	s := status
	n := name
	return ipc.Event{Type: t, RunID: runID, StepName: &n, Status: &s}
}

func fillActivity(m *RunManager, runID string) {
	for i := 0; i < mailboxMaxEvents; i++ {
		m.broadcast(logEvent(runID))
	}
}

// subscribeDrained subscribes and consumes the mandatory opening gap so a case
// starts from an empty mailbox. The opening gap itself is asserted in A1.
func subscribeDrained(t *testing.T, m *RunManager, runID string) *Subscription {
	t.Helper()
	sub, err := m.Subscribe(runID)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	first, ok := sub.Next(ctx)
	if !ok || first.Type != ipc.EventStreamGap {
		t.Fatalf("first frame = %#v ok=%v, want stream_gap", first, ok)
	}
	return sub
}

// drainReady consumes everything currently available without blocking on an
// empty mailbox.
func drainReady(t *testing.T, sub *Subscription) []ipc.Event {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var out []ipc.Event
	for {
		queued, _, gap, _, _ := sub.mb.stats()
		if queued == 0 && !gap {
			return out
		}
		e, ok := sub.Next(ctx)
		if !ok {
			return out
		}
		out = append(out, e)
	}
}

// coversStateUpTo reports whether the drained frames either delivered the named
// event type or raised a gap whose revision covers everything up to wantRev.
func coversStateUpTo(events []ipc.Event, want ipc.EventType, wantRev int64) bool {
	var gapRev int64
	for _, e := range events {
		if e.Type == want {
			return true
		}
		if e.Type == ipc.EventStreamGap && e.StateRev > gapRev {
			gapRev = e.StateRev
		}
	}
	return gapRev >= wantRev
}

// A1: every subscription opens with a reconcile trigger, so "subscribe then
// read authoritative state" is a server-side invariant rather than a rule each
// consumer has to remember.
func TestMailbox_SubscriptionOpensWithGap(t *testing.T) {
	m := NewRunManager(nil, nil, nil)
	sub, err := m.Subscribe("run-1")
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	e, ok := sub.Next(ctx)
	if !ok || e.Type != ipc.EventStreamGap {
		t.Fatalf("first frame = %#v ok=%v, want stream_gap", e, ok)
	}
	if e.RunID != "run-1" {
		t.Fatalf("gap RunID = %q, want run-1", e.RunID)
	}
}

// A2: a lifecycle transition behind a full activity buffer survives, because
// activity is the only class that is ever evicted.
func TestMailbox_LifecycleSurvivesFullActivityBuffer(t *testing.T) {
	m := NewRunManager(nil, nil, nil)
	sub := subscribeDrained(t, m, "run-1")
	defer sub.Close()

	fillActivity(m, "run-1")
	m.broadcast(stepEvent("run-1", ipc.EventStepCompleted, types.StepReview, string(types.StepStatusAwaitingApproval)))
	rev := m.StateRev("run-1")

	events := drainReady(t, sub)
	if !coversStateUpTo(events, ipc.EventStepCompleted, rev) {
		t.Fatalf("step completion neither delivered nor gapped across %d frames", len(events))
	}
	for _, e := range events {
		if e.Type == ipc.EventStepCompleted {
			return // delivered outright by evicting one log line
		}
	}
	t.Fatal("expected the state event to be delivered by evicting activity, not folded")
}

// A3: state pressure never evicts state. When the queue holds nothing
// droppable, the transition folds into the gap instead of displacing a peer.
func TestMailbox_StatePressureFoldsInsteadOfEvictingState(t *testing.T) {
	m := NewRunManager(nil, nil, nil)
	sub := subscribeDrained(t, m, "run-1")
	defer sub.Close()

	for i := 0; i < mailboxMaxEvents; i++ {
		m.broadcast(stepEvent("run-1", ipc.EventStepStarted, types.StepTest, fmt.Sprintf("s%d", i)))
	}
	m.broadcast(ipc.Event{Type: ipc.EventRunUpdated, RunID: "run-1"})
	finalRev := m.StateRev("run-1")

	events := drainReady(t, sub)
	starts := 0
	var gapRev int64
	for _, e := range events {
		switch e.Type {
		case ipc.EventStepStarted:
			starts++
		case ipc.EventStreamGap:
			gapRev = e.StateRev
		}
	}
	if starts != mailboxMaxEvents {
		t.Fatalf("queued state events delivered = %d, want all %d preserved", starts, mailboxMaxEvents)
	}
	if gapRev < finalRev {
		t.Fatalf("gap rev %d does not cover the folded transition at rev %d", gapRev, finalRev)
	}
}

// A4: a terminal completion cannot be hidden, even when it folds into the gap
// and the run is closed immediately afterwards. Close is soft: queued frames
// and the pending gap still drain first.
func TestMailbox_TerminalCompletionCannotBeHidden(t *testing.T) {
	m := NewRunManager(nil, nil, nil)
	sub := subscribeDrained(t, m, "run-1")
	defer sub.Close()

	for i := 0; i < mailboxMaxEvents; i++ {
		m.broadcast(stepEvent("run-1", ipc.EventStepCompleted, types.StepTest, "completed"))
	}
	status := string(types.RunFailed)
	m.broadcast(ipc.Event{Type: ipc.EventRunCompleted, RunID: "run-1", Status: &status})
	terminalRev := m.StateRev("run-1")
	m.closeSubscribers("run-1")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var covered bool
	for {
		e, ok := sub.Next(ctx)
		if !ok {
			break
		}
		if e.Type == ipc.EventRunCompleted {
			covered = true
		}
		if e.Type == ipc.EventStreamGap && e.StateRev >= terminalRev {
			covered = true
		}
	}
	if !covered {
		t.Fatal("terminal completion was neither delivered nor gapped before the stream finished")
	}
}

// A5: approval/finding state cannot be hidden.
func TestMailbox_FindingsAndApprovalCannotBeHidden(t *testing.T) {
	m := NewRunManager(nil, nil, nil)
	sub := subscribeDrained(t, m, "run-1")
	defer sub.Close()

	fillActivity(m, "run-1")
	findings := `{"findings":[{"id":"x","action":"ask-user"}]}`
	e := stepEvent("run-1", ipc.EventStepCompleted, types.StepReview, string(types.StepStatusAwaitingApproval))
	e.Findings = &findings
	m.broadcast(e)
	rev := m.StateRev("run-1")

	if !coversStateUpTo(drainReady(t, sub), ipc.EventStepCompleted, rev) {
		t.Fatal("awaiting-approval findings neither delivered nor gapped")
	}
}

// A6: ordinary logs stay bounded, are droppable, and never force a consumer to
// do reconciliation work.
func TestMailbox_ActivityIsBoundedDroppableAndRaisesNoGap(t *testing.T) {
	m := NewRunManager(nil, nil, nil)
	sub := subscribeDrained(t, m, "run-1")
	defer sub.Close()

	for i := 0; i < 10_000; i++ {
		m.broadcast(logEvent("run-1"))
	}
	queued, bytes, gap, dropped, coalesced := sub.mb.stats()
	if queued > mailboxMaxEvents {
		t.Fatalf("queued = %d, want <= %d", queued, mailboxMaxEvents)
	}
	if bytes > mailboxMaxBytes {
		t.Fatalf("queued bytes = %d, want <= %d", bytes, mailboxMaxBytes)
	}
	if dropped == 0 {
		t.Fatal("expected activity drops under a 10k log burst")
	}
	if gap || coalesced != 0 {
		t.Fatalf("activity pressure must not raise a state gap (gap=%v coalesced=%d)", gap, coalesced)
	}
}

// The byte ceiling binds independently of the count ceiling, so a burst of
// large frames cannot inflate a subscriber past its budget.
func TestMailbox_ByteCeilingBindsBeforeCountCeiling(t *testing.T) {
	m := NewRunManager(nil, nil, nil)
	sub := subscribeDrained(t, m, "run-1")
	defer sub.Close()

	big := strings.Repeat("x", 64*1024)
	for i := 0; i < 1_000; i++ {
		c := big
		m.broadcast(ipc.Event{Type: ipc.EventLogChunk, RunID: "run-1", Content: &c})
	}
	queued, bytes, _, dropped, _ := sub.mb.stats()
	if bytes > mailboxMaxBytes {
		t.Fatalf("queued bytes = %d, want <= %d", bytes, mailboxMaxBytes)
	}
	if queued >= mailboxMaxEvents {
		t.Fatalf("queued = %d, want the byte ceiling to bind first", queued)
	}
	if dropped == 0 {
		t.Fatal("expected large-frame drops")
	}
}

// A7: overflow notices coalesce. The signal is a scalar, so it never grows
// with the number of losses.
func TestMailbox_GapCoalescesAndNeverGrows(t *testing.T) {
	m := NewRunManager(nil, nil, nil)
	sub := subscribeDrained(t, m, "run-1")
	defer sub.Close()

	for i := 0; i < mailboxMaxEvents; i++ {
		m.broadcast(stepEvent("run-1", ipc.EventStepCompleted, types.StepTest, "completed"))
	}
	for i := 0; i < 50_000; i++ {
		m.broadcast(ipc.Event{Type: ipc.EventRunUpdated, RunID: "run-1"})
	}
	queued, bytes, _, _, coalesced := sub.mb.stats()
	if queued > mailboxMaxEvents || bytes > mailboxMaxBytes {
		t.Fatalf("unbounded under state pressure: queued=%d bytes=%d", queued, bytes)
	}
	gaps := 0
	for _, e := range drainReady(t, sub) {
		if e.Type == ipc.EventStreamGap {
			gaps++
		}
	}
	if gaps != 1 {
		t.Fatalf("gap frames = %d after %d coalesced losses, want exactly 1", gaps, coalesced)
	}
}

// The gap drains ahead of queued payload so a coalesced invalidation reaches
// the consumer before it renders more stale frames.
func TestMailbox_GapDrainsAheadOfQueuedPayload(t *testing.T) {
	m := NewRunManager(nil, nil, nil)
	sub := subscribeDrained(t, m, "run-1")
	defer sub.Close()

	for i := 0; i < mailboxMaxEvents; i++ {
		m.broadcast(stepEvent("run-1", ipc.EventStepStarted, types.StepTest, "running"))
	}
	m.broadcast(ipc.Event{Type: ipc.EventRunCompleted, RunID: "run-1"})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	first, ok := sub.Next(ctx)
	if !ok || first.Type != ipc.EventStreamGap {
		t.Fatalf("first drained frame = %#v, want the gap at priority", first)
	}
}

// A8: no publisher ever blocks, including on subscribers that never read.
func TestMailbox_PublisherNeverBlocksOnWedgedSubscribers(t *testing.T) {
	m := NewRunManager(nil, nil, nil)
	for i := 0; i < 4; i++ {
		sub, err := m.Subscribe("run-1")
		if err != nil {
			t.Fatal(err)
		}
		defer sub.Close()
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100_000; i++ {
			m.broadcast(logEvent("run-1"))
			m.broadcast(stepEvent("run-1", ipc.EventStepCompleted, types.StepTest, "completed"))
		}
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("broadcast blocked on wedged subscribers")
	}
}

// A9: a delta queued before an authoritative snapshot cannot regress state
// after it. This exercises the consumer-side revision guard the TUI and AXI
// both apply.
func TestMailbox_StaleDeltasCannotRegressAfterSnapshot(t *testing.T) {
	m := NewRunManager(nil, nil, nil)
	sub := subscribeDrained(t, m, "run-1")
	defer sub.Close()

	m.broadcast(stepEvent("run-1", ipc.EventStepStarted, types.StepCI, string(types.StepStatusRunning)))
	for i := 0; i < mailboxMaxEvents; i++ {
		m.broadcast(logEvent("run-1"))
	}
	m.broadcast(stepEvent("run-1", ipc.EventStepCompleted, types.StepCI, string(types.StepStatusCompleted)))
	for i := 0; i < mailboxMaxEvents; i++ {
		m.broadcast(stepEvent("run-1", ipc.EventStepStarted, types.StepTest, "running"))
	}
	m.broadcast(ipc.Event{Type: ipc.EventRunCompleted, RunID: "run-1"})

	authoritativeRev := m.StateRev("run-1")
	appliedRev := int64(0)
	ciStatus := ""
	for _, e := range drainReady(t, sub) {
		switch {
		case e.Type == ipc.EventStreamGap:
			if authoritativeRev >= appliedRev {
				ciStatus = string(types.StepStatusCompleted) // what get_run reports now
				appliedRev = authoritativeRev
			}
		case ipc.ClassOf(e.Type) == ipc.ClassState:
			if e.StateRev <= appliedRev {
				continue // stale: already covered by a newer snapshot
			}
			appliedRev = e.StateRev
			if e.StepName != nil && *e.StepName == types.StepCI && e.Status != nil {
				ciStatus = *e.Status
			}
		}
	}
	if ciStatus != string(types.StepStatusCompleted) {
		t.Fatalf("CI status = %q after reconciliation, want it not regressed to %q", ciStatus, types.StepStatusRunning)
	}
}

// A10: unsubscribe removes the registration, releases retained payload, is
// idempotent, and leaves later broadcasts harmless.
func TestMailbox_UnsubscribeReleasesEverything(t *testing.T) {
	m := NewRunManager(nil, nil, nil)
	sub, err := m.Subscribe("run-1")
	if err != nil {
		t.Fatal(err)
	}
	fillActivity(m, "run-1")
	sub.Close()
	sub.Close() // idempotent

	m.subMu.Lock()
	registered := len(m.subscribers["run-1"])
	m.subMu.Unlock()
	if registered != 0 {
		t.Fatalf("subscribers after Close = %d, want 0", registered)
	}
	queued, bytes, _, _, _ := sub.mb.stats()
	if queued != 0 || bytes != 0 {
		t.Fatalf("payload retained after Close: queued=%d bytes=%d", queued, bytes)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, ok := sub.Next(ctx); ok {
		t.Fatal("Next after Close should report the stream finished")
	}
	m.broadcast(logEvent("run-1")) // must not panic or re-queue
	if queued, _, _, _, _ := sub.mb.stats(); queued != 0 {
		t.Fatal("broadcast after Close re-queued into a released mailbox")
	}
}

// A11: a reconnecting consumer converges, because the new subscription opens
// gapped at the current revision.
func TestMailbox_ReconnectConvergesAtCurrentRevision(t *testing.T) {
	m := NewRunManager(nil, nil, nil)
	first := subscribeDrained(t, m, "run-1")
	m.broadcast(stepEvent("run-1", ipc.EventStepStarted, types.StepCI, "running"))
	first.Close() // stream dropped

	m.broadcast(stepEvent("run-1", ipc.EventStepCompleted, types.StepCI, "completed"))
	second, err := m.Subscribe("run-1")
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	e, ok := second.Next(ctx)
	if !ok || e.Type != ipc.EventStreamGap {
		t.Fatalf("reconnect first frame = %#v, want stream_gap", e)
	}
	if e.StateRev < m.StateRev("run-1") {
		t.Fatalf("reconnect gap rev %d < current %d", e.StateRev, m.StateRev("run-1"))
	}
}

// A12: cancellation releases a waiting reader.
func TestMailbox_CancellationUnblocksWaitingReader(t *testing.T) {
	m := NewRunManager(nil, nil, nil)
	sub := subscribeDrained(t, m, "run-1")
	defer sub.Close()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() {
		_, ok := sub.Next(ctx)
		done <- ok
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case ok := <-done:
		if ok {
			t.Fatal("cancelled Next returned an event")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled Next did not return")
	}
}

// A13: publish, drain, subscribe, and unsubscribe are race-free under churn.
func TestMailbox_ConcurrentChurnIsRaceFree(t *testing.T) {
	m := NewRunManager(nil, nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 3000; i++ {
				m.broadcast(logEvent("run-1"))
				m.broadcast(stepEvent("run-1", ipc.EventStepStarted, types.StepTest, fmt.Sprintf("s%d", w)))
			}
		}(w)
	}
	for c := 0; c < 4; c++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 40; i++ {
				sub, err := m.Subscribe("run-1")
				if err != nil {
					continue
				}
				rctx, rcancel := context.WithTimeout(ctx, 10*time.Millisecond)
				for j := 0; j < 20; j++ {
					if _, ok := sub.Next(rctx); !ok {
						break
					}
				}
				rcancel()
				sub.Close()
			}
		}()
	}
	wg.Wait()
}

// Many simultaneous transitions collapse into exactly one repair signal whose
// revision covers all of them. A single reserved slot would be overrun by the
// second concurrent transition; a coalescing scalar is not.
func TestMailbox_ManySimultaneousTransitionsCollapseToOneGap(t *testing.T) {
	m := NewRunManager(nil, nil, nil)
	sub := subscribeDrained(t, m, "run-1")
	defer sub.Close()

	for i := 0; i < mailboxMaxEvents; i++ {
		m.broadcast(stepEvent("run-1", ipc.EventStepCompleted, types.StepTest, "completed"))
	}
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				m.broadcast(ipc.Event{Type: ipc.EventRunUpdated, RunID: "run-1"})
			}
		}()
	}
	wg.Wait()
	finalRev := m.StateRev("run-1")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	first, ok := sub.Next(ctx)
	if !ok || first.Type != ipc.EventStreamGap {
		t.Fatalf("first frame = %#v, want the coalesced gap", first)
	}
	if first.StateRev < finalRev {
		t.Fatalf("gap rev %d does not cover final rev %d", first.StateRev, finalRev)
	}
	if _, _, gapPending, _, _ := sub.mb.stats(); gapPending {
		t.Fatal("a second gap remained pending after the first was delivered")
	}
}

// The subscriber cap bounds the global mailbox footprint. Refusing is an
// ordinary error, never unbounded growth.
func TestMailbox_SubscriberCapIsEnforced(t *testing.T) {
	m := NewRunManager(nil, nil, nil)
	for i := 0; i < maxSubscribersPerRun; i++ {
		sub, err := m.Subscribe("run-1")
		if err != nil {
			t.Fatalf("subscribe %d: %v", i, err)
		}
		defer sub.Close()
	}
	if _, err := m.Subscribe("run-1"); err == nil {
		t.Fatalf("subscribe beyond %d succeeded, want a refusal", maxSubscribersPerRun)
	}
}

// State revisions are assigned only to state events and advance monotonically
// in enqueue order, which is what lets a consumer's monotonic guard discard a
// stale delta without discarding a newer one's payload.
func TestMailbox_StateRevisionsAdvanceInEnqueueOrder(t *testing.T) {
	m := NewRunManager(nil, nil, nil)
	sub := subscribeDrained(t, m, "run-1")
	defer sub.Close()

	m.broadcast(logEvent("run-1"))
	m.broadcast(stepEvent("run-1", ipc.EventStepStarted, types.StepTest, "running"))
	m.broadcast(logEvent("run-1"))
	m.broadcast(stepEvent("run-1", ipc.EventStepCompleted, types.StepTest, "completed"))

	var last int64
	for _, e := range drainReady(t, sub) {
		switch ipc.ClassOf(e.Type) {
		case ipc.ClassActivity:
			if e.StateRev != 0 {
				t.Fatalf("activity frame carries StateRev %d, want 0", e.StateRev)
			}
		case ipc.ClassState:
			if e.StateRev <= last {
				t.Fatalf("state revisions out of order: %d after %d", e.StateRev, last)
			}
			last = e.StateRev
		}
	}
	if last != m.StateRev("run-1") {
		t.Fatalf("last delivered rev %d != manager rev %d", last, m.StateRev("run-1"))
	}
}

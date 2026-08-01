package daemon

import (
	"fmt"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// D6: an authoritative snapshot samples the state revision BEFORE it reads the
// database.
//
// The read hook below stands in for a state transition that lands while the
// snapshot is being assembled. Because the revision was sampled first, the
// snapshot is stamped older than that transition, so a consumer's monotonic
// guard still applies the delta instead of discarding it as stale. Sampling
// after the read would stamp the snapshot as newer than content it never saw,
// and the repairing delta would be silently skipped.
func TestRunSnapshot_SamplesRevisionBeforeReadingSoConcurrentChangesStillApply(t *testing.T) {
	m := NewRunManager(nil, nil, nil)
	m.broadcast(stepEvent("run-1", ipc.EventStepStarted, types.StepCI, "running"))
	revBeforeRead := m.StateRev("run-1")

	var revDuringRead int64
	info, err := runSnapshot(m, "run-1", func(runID string) (*ipc.RunInfo, error) {
		// A transition lands between the sample and the read.
		m.broadcast(stepEvent(runID, ipc.EventStepCompleted, types.StepCI, "completed"))
		revDuringRead = m.StateRev(runID)
		return &ipc.RunInfo{ID: runID}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if info.StateRev != revBeforeRead {
		t.Fatalf("snapshot StateRev = %d, want the revision sampled before the read (%d)", info.StateRev, revBeforeRead)
	}
	if info.StateRev >= revDuringRead {
		t.Fatalf("snapshot StateRev %d is not older than the concurrent transition at %d; that delta would be discarded as stale",
			info.StateRev, revDuringRead)
	}
}

// A failing read must not produce a stamped snapshot.
func TestRunSnapshot_PropagatesReadFailure(t *testing.T) {
	m := NewRunManager(nil, nil, nil)
	info, err := runSnapshot(m, "run-1", func(string) (*ipc.RunInfo, error) {
		return nil, fmt.Errorf("run not found: run-1")
	})
	if err == nil {
		t.Fatal("expected the read failure to propagate")
	}
	if info != nil {
		t.Fatalf("snapshot = %#v, want nil on failure", info)
	}
}

// Revisions are per-run: pressure on one run cannot make another run's
// snapshot look newer than it is.
func TestRunSnapshot_RevisionsAreScopedPerRun(t *testing.T) {
	m := NewRunManager(nil, nil, nil)
	for i := 0; i < 5; i++ {
		m.broadcast(ipc.Event{Type: ipc.EventRunUpdated, RunID: "run-1"})
	}
	info, err := runSnapshot(m, "run-2", func(runID string) (*ipc.RunInfo, error) {
		return &ipc.RunInfo{ID: runID}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if info.StateRev != 0 {
		t.Fatalf("run-2 snapshot StateRev = %d, want 0", info.StateRev)
	}
}

func TestRunSnapshot_CompletedRunRetainsTerminalRevisionUntilEviction(t *testing.T) {
	m := NewRunManager(nil, nil, nil)
	m.broadcast(ipc.Event{Type: ipc.EventRunCompleted, RunID: "terminal"})
	terminalRev := m.StateRev("terminal")
	m.closeSubscribers("terminal")

	info, err := runSnapshot(m, "terminal", func(runID string) (*ipc.RunInfo, error) {
		return &ipc.RunInfo{ID: runID, Status: types.RunCompleted}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if info.StateRev != terminalRev || info.StateRev == 0 {
		t.Fatalf("completed snapshot StateRev = %d, want terminal revision %d", info.StateRev, terminalRev)
	}

	sub, err := m.Subscribe("terminal")
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Close()
	event, ok := sub.Next(t.Context())
	if !ok || event.Type != ipc.EventStreamGap || event.StateRev != terminalRev {
		t.Fatalf("completed subscription first event = %#v, ok=%v, want terminal gap revision %d", event, ok, terminalRev)
	}
}

func TestRunSnapshot_CompletedRevisionEvictsWithCompletedRecord(t *testing.T) {
	m := NewRunManager(nil, nil, nil)
	for i := 0; i <= 1000; i++ {
		runID := fmt.Sprintf("run-%04d", i)
		m.broadcast(ipc.Event{Type: ipc.EventRunCompleted, RunID: runID})
		m.closeSubscribers(runID)
	}

	if len(m.completedRuns) != 501 || len(m.stateRevs) != 501 || len(m.completedOrder) != 501 {
		t.Fatalf("retained completed records/revisions/order = %d/%d/%d, want 501/501/501",
			len(m.completedRuns), len(m.stateRevs), len(m.completedOrder))
	}
	if m.completedRuns["run-0000"] || m.StateRev("run-0000") != 0 {
		t.Fatal("oldest completed run and revision were not evicted together")
	}
	if !m.completedRuns["run-1000"] || m.StateRev("run-1000") == 0 {
		t.Fatal("newest completed run lost its terminal revision")
	}
}

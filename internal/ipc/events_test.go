package ipc_test

import (
	"encoding/json"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
)

// The taxonomy is the single owner of "may this event be dropped?". Every
// broker and consumer reads it from here instead of re-deriving it from names.
func TestClassOfPartitionsEventsByLossTolerance(t *testing.T) {
	tests := []struct {
		event ipc.EventType
		want  ipc.EventClass
	}{
		{ipc.EventLogChunk, ipc.ClassActivity},
		{ipc.EventStreamGap, ipc.ClassControl},
		{ipc.EventRunCreated, ipc.ClassState},
		{ipc.EventRunUpdated, ipc.ClassState},
		{ipc.EventRunScheduleFinalized, ipc.ClassState},
		{ipc.EventRunCompleted, ipc.ClassState},
		{ipc.EventStepStarted, ipc.ClassState},
		{ipc.EventStepCompleted, ipc.ClassState},
	}
	for _, tt := range tests {
		if got := ipc.ClassOf(tt.event); got != tt.want {
			t.Errorf("ClassOf(%s) = %v, want %v", tt.event, got, tt.want)
		}
	}
}

// D7: an event type this build does not recognise must never be treated as
// droppable. A future producer's state transition has to fail safe.
func TestClassOfUnknownEventFailsSafeToState(t *testing.T) {
	if got := ipc.ClassOf(ipc.EventType("some_future_event")); got != ipc.ClassState {
		t.Fatalf("ClassOf(unknown) = %v, want ClassState", got)
	}
	if got := ipc.ClassOf(""); got != ipc.ClassState {
		t.Fatalf("ClassOf(empty) = %v, want ClassState", got)
	}
}

// The gap frame and the revision must survive the wire, and both stay absent
// from frames that do not carry them so older peers see no change.
func TestStreamGapAndStateRevRoundTrip(t *testing.T) {
	gap := ipc.Event{Type: ipc.EventStreamGap, RunID: "run-1", RepoID: "repo-1", StateRev: 71}
	raw, err := json.Marshal(gap)
	if err != nil {
		t.Fatal(err)
	}
	var back ipc.Event
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	if back.Type != ipc.EventStreamGap || back.StateRev != 71 || back.RunID != "run-1" {
		t.Fatalf("round trip = %#v", back)
	}

	plain, err := json.Marshal(ipc.Event{Type: ipc.EventLogChunk, RunID: "run-1"})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(plain); jsonHasKey(got, "state_rev") {
		t.Fatalf("activity frame carries state_rev: %s", got)
	}

	snap, err := json.Marshal(ipc.RunInfo{ID: "run-1", StateRev: 9})
	if err != nil {
		t.Fatal(err)
	}
	var info ipc.RunInfo
	if err := json.Unmarshal(snap, &info); err != nil {
		t.Fatal(err)
	}
	if info.StateRev != 9 {
		t.Fatalf("snapshot StateRev = %d, want 9", info.StateRev)
	}
}

func jsonHasKey(doc, key string) bool {
	var m map[string]any
	if err := json.Unmarshal([]byte(doc), &m); err != nil {
		return false
	}
	_, ok := m[key]
	return ok
}

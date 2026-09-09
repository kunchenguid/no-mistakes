package daemon

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/telemetry"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type recordedTelemetryEvent struct {
	name   string
	fields telemetry.Fields
}

type telemetryRecorder struct {
	mu     sync.Mutex
	events []recordedTelemetryEvent
}

func (r *telemetryRecorder) Track(name string, fields telemetry.Fields) {
	r.mu.Lock()
	defer r.mu.Unlock()

	clone := make(telemetry.Fields, len(fields))
	for k, v := range fields {
		clone[k] = v
	}
	r.events = append(r.events, recordedTelemetryEvent{name: name, fields: clone})
}

func (r *telemetryRecorder) Pageview(path string, fields telemetry.Fields) {
	r.Track("pageview", fields)
}

func (r *telemetryRecorder) Close(context.Context) error { return nil }

func (r *telemetryRecorder) find(name, field string, want any) *recordedTelemetryEvent {
	r.mu.Lock()
	defer r.mu.Unlock()

	for i := len(r.events) - 1; i >= 0; i-- {
		e := r.events[i]
		if e.name != name {
			continue
		}
		if field == "" || fmt.Sprint(e.fields[field]) == fmt.Sprint(want) {
			cp := e
			return &cp
		}
	}
	return nil
}

func waitForTelemetryEvent(t *testing.T, recorder *telemetryRecorder, name, field string, want any) *recordedTelemetryEvent {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if event := recorder.find(name, field, want); event != nil {
			return event
		}
		time.Sleep(10 * time.Millisecond)
	}
	return nil
}

func TestTelemetryFailedStepNameRedactsCustomGateLabel(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	repo, err := database.InsertRepo("/repo", "https://example.com/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	step, err := database.InsertStepResult(run.ID, types.CustomGateStepName(types.StepTest, "private-policy"))
	if err != nil {
		t.Fatal(err)
	}
	if err := database.FailStep(step.ID, "failed", 1); err != nil {
		t.Fatal(err)
	}
	if got := telemetryFailedStepName(database, run.ID); got != "gate" {
		t.Fatalf("telemetryFailedStepName() = %q, want gate", got)
	}
}

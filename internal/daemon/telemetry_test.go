package daemon

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/telemetry"
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

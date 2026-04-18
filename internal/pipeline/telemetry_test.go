package pipeline

import (
	"context"
	"fmt"
	"sync"

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

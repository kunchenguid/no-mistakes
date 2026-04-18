package cli

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

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

func (r *telemetryRecorder) Close(context.Context) error { return nil }

func (r *telemetryRecorder) find(name, field string, want any) *recordedTelemetryEvent {
	r.mu.Lock()
	defer r.mu.Unlock()

	for i := len(r.events) - 1; i >= 0; i-- {
		e := r.events[i]
		if e.name != name {
			continue
		}
		if field == "" {
			cp := e
			return &cp
		}
		if fmt.Sprint(e.fields[field]) == fmt.Sprint(want) {
			cp := e
			return &cp
		}
	}
	return nil
}

func TestInitTracksCommandTelemetry(t *testing.T) {
	setupTestRepo(t)

	recorder := &telemetryRecorder{}
	restore := telemetry.SetDefaultForTesting(recorder)
	defer restore()

	if _, err := executeCmd("init"); err != nil {
		t.Fatalf("init failed: %v", err)
	}

	event := recorder.find("command", "command", "init")
	if event == nil {
		t.Fatal("expected command telemetry for init")
	}
	if got := event.fields["status"]; got != "success" {
		t.Fatalf("status = %v, want success", got)
	}
	if _, ok := event.fields["duration_ms"]; !ok {
		t.Fatal("expected duration_ms in command telemetry")
	}
}

func TestStatusTracksSoftFailureAsError(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("NM_HOME", t.TempDir())
	chdir(t, tmpDir)

	recorder := &telemetryRecorder{}
	restore := telemetry.SetDefaultForTesting(recorder)
	defer restore()

	if _, err := executeCmd("status"); err != nil {
		t.Fatalf("status failed: %v", err)
	}

	event := recorder.find("command", "command", "status")
	if event == nil {
		t.Fatal("expected command telemetry for status")
	}
	if got := event.fields["status"]; got != "error" {
		t.Fatalf("status = %v, want error", got)
	}
}

func TestDoctorTracksFailedChecksAsError(t *testing.T) {
	nmHome := filepath.Join(t.TempDir(), "missing-nm-home")
	t.Setenv("NM_HOME", nmHome)
	t.Setenv("PATH", "/nonexistent")

	recorder := &telemetryRecorder{}
	restore := telemetry.SetDefaultForTesting(recorder)
	defer restore()

	if _, err := executeCmd("doctor"); err != nil {
		t.Fatalf("doctor failed: %v", err)
	}

	event := recorder.find("command", "command", "doctor")
	if event == nil {
		t.Fatal("expected command telemetry for doctor")
	}
	if got := event.fields["status"]; got != "error" {
		t.Fatalf("status = %v, want error", got)
	}
}

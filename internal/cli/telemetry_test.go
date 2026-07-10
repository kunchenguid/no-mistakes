package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
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
	r.record(name, fields)
}

func (r *telemetryRecorder) Pageview(path string, fields telemetry.Fields) {
	clone := make(telemetry.Fields, len(fields)+1)
	for k, v := range fields {
		clone[k] = v
	}
	clone["path"] = path
	r.record("pageview", clone)
}

func (r *telemetryRecorder) record(name string, fields telemetry.Fields) {
	r.mu.Lock()
	defer r.mu.Unlock()

	clone := make(telemetry.Fields, len(fields))
	for k, v := range fields {
		clone[k] = v
	}
	r.events = append(r.events, recordedTelemetryEvent{name: name, fields: clone})
}

func (r *telemetryRecorder) Close(context.Context) error { return nil }

func (r *telemetryRecorder) count(name string) int {
	r.mu.Lock()
	defer r.mu.Unlock()

	total := 0
	for _, e := range r.events {
		if e.name == name {
			total++
		}
	}
	return total
}

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

func TestReadSurfaceTelemetryOptOutDoesNotPersistGate(t *testing.T) {
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)
	t.Setenv("NO_MISTAKES_TELEMETRY", "off")

	if err := trackReadSurface("status", nil, func() (string, string, error) {
		return "repo|idle", "success", nil
	}); err != nil {
		t.Fatalf("track read surface: %v", err)
	}

	p := paths.WithRoot(nmHome)
	for _, path := range []string{p.TelemetryGateFile(), p.TelemetryGateFile() + ".lock"} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("telemetry opt-out created %q: %v", path, err)
		}
	}
}

func TestStatusFingerprintIncludesDisplayedRunHead(t *testing.T) {
	run := &db.Run{ID: "run-1", Branch: "feature/test", Status: "running", HeadSHA: "head-one"}
	before := statusFingerprint("repo", "running", run)
	run.HeadSHA = "head-two"
	if after := statusFingerprint("repo", "running", run); before == after {
		t.Fatal("changing the displayed head must change the status fingerprint")
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

func TestAttachTracksTUIPageview(t *testing.T) {
	nmHome := makeSocketSafeTempDir(t)
	t.Setenv("NM_HOME", nmHome)
	p := paths.WithRoot(nmHome)

	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	repo, err := d.InsertRepoWithID("repo-1", "/tmp/repo", "https://github.com/test/repo", "main")
	if err != nil {
		t.Fatal(err)
	}

	startTestDaemon(t, p, d)

	run, err := d.InsertRun(repo.ID, "feature/test", "abc123", "def456")
	if err != nil {
		t.Fatal(err)
	}

	recorder := &telemetryRecorder{}
	restore := telemetry.SetDefaultForTesting(recorder)
	defer restore()

	prevRunTUI := runTUI
	runTUI = func(string, *ipc.Client, *ipc.RunInfo, string) error { return nil }
	defer func() { runTUI = prevRunTUI }()

	if err := attachRun(context.Background(), io.Discard, run.ID, false, false, nil); err != nil {
		t.Fatalf("attachRun() error = %v", err)
	}

	event := recorder.find("pageview", "path", "/tui")
	if event == nil {
		t.Fatal("expected TUI pageview telemetry")
	}
	if got := event.fields["entrypoint"]; got != "attach" {
		t.Fatalf("entrypoint = %v, want attach", got)
	}
	if got := fmt.Sprint(event.fields["run_status"]); got != "pending" {
		t.Fatalf("run_status = %v, want pending", got)
	}
}

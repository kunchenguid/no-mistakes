package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/telemetry"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestAxiMutationCommandsEmitPageviews verifies that state-changing axi
// commands keep full-fidelity telemetry: a pageview at entry (agent usage
// parity with the human /tui and /wizard surfaces) plus the command event.
// The commands fail fast here because the repo is uninitialized, but the
// pageview fires at command entry before any of that, so it is still recorded.
func TestAxiMutationCommandsEmitPageviews(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		path    string
		command string
	}{
		{"run", []string{"axi", "run", "--intent", "ship the thing"}, "/axi/run", "axi-run"},
		{"respond", []string{"axi", "respond", "--action", "approve"}, "/axi/respond", "axi-respond"},
		{"abort", []string{"axi", "abort"}, "/axi/abort", "axi-abort"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			t.Setenv("NM_HOME", t.TempDir())
			chdir(t, tmpDir)

			recorder := &telemetryRecorder{}
			restore := telemetry.SetDefaultForTesting(recorder)
			defer restore()

			// The command may fail (uninitialized repo); we only assert telemetry.
			_, _ = executeCmd(tc.args...)

			if event := recorder.find("pageview", "path", tc.path); event == nil {
				t.Fatalf("expected %s pageview for %v", tc.path, tc.args)
			}
			// The pageview is added alongside the existing command event, not in
			// place of it, so per-command status/duration is still recorded.
			if event := recorder.find("command", "command", tc.command); event == nil {
				t.Fatalf("expected %s command event alongside the pageview", tc.command)
			}
		})
	}
}

// TestAxiReadSurfacesEmitNoPageview verifies the high-frequency read-only axi
// surfaces no longer double-emit: no pageview at all, and a single command
// event that carries the surface's segmentation fields.
func TestAxiReadSurfacesEmitNoPageview(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		path    string
		command string
	}{
		{"home", []string{"axi"}, "/axi", "axi-home"},
		{"status", []string{"axi", "status"}, "/axi/status", "axi-status"},
		{"logs", []string{"axi", "logs", "--step", "review"}, "/axi/logs", "axi-logs"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			t.Setenv("NM_HOME", t.TempDir())
			chdir(t, tmpDir)

			recorder := &telemetryRecorder{}
			restore := telemetry.SetDefaultForTesting(recorder)
			defer restore()

			_, _ = executeCmd(tc.args...)

			if event := recorder.find("pageview", "path", tc.path); event != nil {
				t.Fatalf("read surface %v must not emit a pageview, got %v", tc.args, event.fields)
			}
			if event := recorder.find("command", "command", tc.command); event == nil {
				t.Fatalf("expected a single %s command event", tc.command)
			}
		})
	}
}

// TestReadSurfaceTelemetryStaysBoundedUnderPolling reproduces the telemetry
// firehose: an agent polling axi status/home (and a human looping status/runs)
// every few seconds. Before the diet each poll emitted a pageview plus a
// command event without bound; now an unchanged state must emit exactly once
// until the heartbeat interval elapses.
func TestReadSurfaceTelemetryStaysBoundedUnderPolling(t *testing.T) {
	cases := [][]string{
		{"axi"},
		{"axi", "status"},
		{"status"},
		{"runs"},
	}

	for _, args := range cases {
		t.Run(strings.Join(args, "-"), func(t *testing.T) {
			tmpDir := t.TempDir()
			t.Setenv("NM_HOME", t.TempDir())
			chdir(t, tmpDir)

			now := time.Unix(1_700_000_000, 0)
			readSurfaceNow = func() time.Time { return now }
			defer func() { readSurfaceNow = nil }()

			recorder := &telemetryRecorder{}
			restore := telemetry.SetDefaultForTesting(recorder)
			defer restore()

			// A 5-second polling loop for 5 minutes: 60 invocations.
			for i := 0; i < 60; i++ {
				_, _ = executeCmd(args...)
				now = now.Add(5 * time.Second)
			}

			if got := recorder.count("command"); got != 1 {
				t.Fatalf("60 unchanged polls emitted %d command events, want 1", got)
			}
			if got := recorder.count("pageview"); got != 0 {
				t.Fatalf("read surface polling emitted %d pageviews, want 0", got)
			}

			// After the heartbeat interval the surface reports once more.
			now = now.Add(readSurfaceHeartbeat)
			_, _ = executeCmd(args...)
			if got := recorder.count("command"); got != 2 {
				t.Fatalf("heartbeat emit count = %d command events, want 2", got)
			}
		})
	}
}

// TestAxiRunPageviewCarriesFlags verifies the run pageview includes the
// flag-derived context an analytics surface can segment on, mirroring how the
// TUI pageview carries entrypoint/run_status.
func TestAxiRunPageviewCarriesFlags(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("NM_HOME", t.TempDir())
	chdir(t, tmpDir)

	recorder := &telemetryRecorder{}
	restore := telemetry.SetDefaultForTesting(recorder)
	defer restore()

	_, _ = executeCmd("axi", "run", "--intent", "ship it", "--yes", "--skip", "lint")

	event := recorder.find("pageview", "path", "/axi/run")
	if event == nil {
		t.Fatal("expected /axi/run pageview")
	}
	if got := event.fields["auto_yes"]; got != true {
		t.Fatalf("auto_yes = %v, want true", got)
	}
	if got := event.fields["has_intent"]; got != true {
		t.Fatalf("has_intent = %v, want true", got)
	}
	if got := event.fields["has_skip"]; got != true {
		t.Fatalf("has_skip = %v, want true", got)
	}
}

// TestAxiLogsCommandEventCarriesStep verifies the logs command event records
// which step and whether a specific run was requested (fields that used to
// ride on the now-removed pageview).
func TestAxiLogsCommandEventCarriesStep(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("NM_HOME", t.TempDir())
	chdir(t, tmpDir)

	recorder := &telemetryRecorder{}
	restore := telemetry.SetDefaultForTesting(recorder)
	defer restore()

	_, _ = executeCmd("axi", "logs", "--step", "test", "--run", "run-123")

	event := recorder.find("command", "command", "axi-logs")
	if event == nil {
		t.Fatal("expected axi-logs command event")
	}
	if got := event.fields["step"]; got != "test" {
		t.Fatalf("step = %v, want test", got)
	}
	if got := event.fields["explicit_run_id"]; got != true {
		t.Fatalf("explicit_run_id = %v, want true", got)
	}
}

func TestRunStateFingerprintIncludesStepStatuses(t *testing.T) {
	rv := runView{
		ID:      "run-1",
		Branch:  "feature/test",
		Status:  string(types.RunRunning),
		HeadSHA: "head-one",
		PRURL:   "https://example.test/pr/1",
		Steps:   []stepView{{Name: "review", Status: string(types.StepStatusRunning)}},
	}
	before := runStateFingerprint(rv)
	rv.Steps[0].Status = string(types.StepStatusCompleted)
	if after := runStateFingerprint(rv); before == after {
		t.Fatal("changing a step status must change the logs state fingerprint")
	}
	rv.HeadSHA = "head-two"
	if after := runStateFingerprint(rv); before == after {
		t.Fatal("changing the displayed head must change the status fingerprint")
	}
	rv.PRURL = "https://example.test/pr/2"
	if after := runStateFingerprint(rv); before == after {
		t.Fatal("changing the displayed PR must change the status fingerprint")
	}
}

func TestAxiLogsCommandEventSanitizesInvalidStep(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("NM_HOME", t.TempDir())
	chdir(t, tmpDir)

	recorder := &telemetryRecorder{}
	restore := telemetry.SetDefaultForTesting(recorder)
	defer restore()

	_, _ = executeCmd("axi", "logs", "--step", "secret user text")

	event := recorder.find("command", "command", "axi-logs")
	if event == nil {
		t.Fatal("expected axi-logs command event")
	}
	if got := event.fields["step"]; got != "invalid" {
		t.Fatalf("step = %v, want invalid", got)
	}
}

func TestAxiRespondPageviewSanitizesInvalidAction(t *testing.T) {
	tmpDir := t.TempDir()
	t.Setenv("NM_HOME", t.TempDir())
	chdir(t, tmpDir)

	recorder := &telemetryRecorder{}
	restore := telemetry.SetDefaultForTesting(recorder)
	defer restore()

	_, _ = executeCmd("axi", "respond", "--action", "secret user text")

	event := recorder.find("pageview", "path", "/axi/respond")
	if event == nil {
		t.Fatal("expected /axi/respond pageview")
	}
	if got := event.fields["action"]; got != "invalid" {
		t.Fatalf("action = %v, want invalid", got)
	}
}

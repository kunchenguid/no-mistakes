package cli

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/telemetry"
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

// TestAxiReadSurfacesEmitNoTelemetry verifies high-frequency read-only axi
// surfaces emit neither a pageview nor a command event. Agent polling of
// these commands was the dominant remote telemetry volume.
func TestAxiReadSurfacesEmitNoTelemetry(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"home", []string{"axi"}},
		{"status", []string{"axi", "status"}},
		{"logs", []string{"axi", "logs", "--step", "review"}},
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

			if got := recorder.count("pageview") + recorder.count("command"); got != 0 {
				t.Fatalf("read surface %v emitted %d telemetry events, want 0", tc.args, got)
			}
		})
	}
}

// TestReadSurfacesEmitZeroTelemetryUnderPolling reproduces the telemetry
// firehose: an agent polling axi status/home (and a human looping status/runs)
// every few seconds. These read-only commands must emit nothing, even across
// dozens of unchanged polls.
func TestReadSurfacesEmitZeroTelemetryUnderPolling(t *testing.T) {
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

			recorder := &telemetryRecorder{}
			restore := telemetry.SetDefaultForTesting(recorder)
			defer restore()

			for i := 0; i < 60; i++ {
				_, _ = executeCmd(args...)
			}

			if got := recorder.count("command") + recorder.count("pageview"); got != 0 {
				t.Fatalf("60 unchanged polls emitted %d telemetry events, want 0", got)
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

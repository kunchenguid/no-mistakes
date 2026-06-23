package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/telemetry"
)

// TestDoctorListsCopilotAgent exercises the user-facing `no-mistakes doctor`
// report and verifies the GitHub Copilot CLI now appears in the Agents section
// and is detected (LookPath) when its binary is on PATH, just like the other
// first-class agents. The full rendered report is logged so it can be captured
// as reviewer-visible evidence.
func TestDoctorListsCopilotAgent(t *testing.T) {
	restore := telemetry.SetDefaultForTesting(&telemetryRecorder{})
	defer restore()

	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)

	binDir := t.TempDir()
	copilotPath := writeFakeCopilotBinary(t, binDir)

	// Prepend our fake bin dir so doctor's LookPath("copilot") resolves here,
	// while git/gh still resolve from the inherited PATH.
	sep := string(os.PathListSeparator)
	t.Setenv("PATH", binDir+sep+os.Getenv("PATH"))

	out, err := executeCmd("doctor")
	if err != nil {
		t.Fatalf("doctor failed: %v\n%s", err, out)
	}

	t.Logf("rendered `no-mistakes doctor` report:\n%s", out)

	if !strings.Contains(out, "copilot") {
		t.Fatalf("doctor report missing copilot agent entry:\n%s", out)
	}
	// The copilot line must show the detected fake binary path, proving doctor
	// probes the copilot binary alongside claude/codex/rovodev/opencode/pi.
	if !strings.Contains(out, copilotPath) {
		t.Fatalf("doctor did not detect copilot at %q:\n%s", copilotPath, out)
	}
}

// writeFakeCopilotBinary writes a stub `copilot` executable that doctor's
// LookPath can resolve. doctor never executes it, so an empty no-op script is
// enough.
func writeFakeCopilotBinary(t *testing.T, dir string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		dst := filepath.Join(dir, "copilot.cmd")
		if err := os.WriteFile(dst, []byte("@echo off\r\nexit /b 0\r\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return dst
	}
	dst := filepath.Join(dir, "copilot")
	if err := os.WriteFile(dst, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dst
}

package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/telemetry"
)

// TestDoctorValidatesRoutingContract exercises `no-mistakes doctor` and verifies
// it validates the routing contract and probes each routing runner's
// executable for provider availability, instead of recommending a legacy
// single agent.
func TestDoctorValidatesRoutingContract(t *testing.T) {
	restore := telemetry.SetDefaultForTesting(&telemetryRecorder{})
	defer restore()

	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)

	binDir := t.TempDir()
	codexPath := writeFakeDoctorBinary(t, binDir, "codex")
	claudePath := writeFakeDoctorBinary(t, binDir, "claude")

	sep := string(os.PathListSeparator)
	t.Setenv("PATH", binDir+sep+os.Getenv("PATH"))

	out, err := executeCmd("doctor")
	if err != nil {
		t.Fatalf("doctor failed: %v\n%s", err, out)
	}
	t.Logf("rendered `no-mistakes doctor` report:\n%s", out)

	// The routing contract is validated and each runner executable is probed.
	for _, want := range []string{"Routing", "contract", "codex", "claude", codexPath, claudePath} {
		if !strings.Contains(out, want) {
			t.Fatalf("doctor routing report missing %q:\n%s", want, out)
		}
	}
	// The legacy single-agent "Agents" section and its non-runner agents are gone.
	for _, gone := range []string{"Agents", "rovodev", "opencode", "copilot"} {
		if strings.Contains(out, gone) {
			t.Fatalf("doctor should no longer recommend legacy agent %q:\n%s", gone, out)
		}
	}
	if strings.Contains(out, "some checks failed") {
		t.Fatalf("healthy routing contract should not report failed checks:\n%s", out)
	}
}

func writeFakeDoctorBinary(t *testing.T, dir, name string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		dst := filepath.Join(dir, name+".cmd")
		if err := os.WriteFile(dst, []byte("@echo off\r\nexit /b 0\r\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return dst
	}
	dst := filepath.Join(dir, name)
	if err := os.WriteFile(dst, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dst
}

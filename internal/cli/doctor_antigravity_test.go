package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/telemetry"
)

// TestDoctorListsAntigravityAgent verifies that `no-mistakes doctor` includes
// the Antigravity agent in the Agents section and detects `agy` on PATH.
func TestDoctorListsAntigravityAgent(t *testing.T) {
	restore := telemetry.SetDefaultForTesting(&telemetryRecorder{})
	defer restore()

	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)

	binDir := t.TempDir()
	agyPath := writeFakeAgyBinary(t, binDir)

	sep := string(os.PathListSeparator)
	t.Setenv("PATH", binDir+sep+os.Getenv("PATH"))

	out, err := executeCmd("doctor")
	if err != nil {
		t.Fatalf("doctor failed: %v\n%s", err, out)
	}

	t.Logf("rendered `no-mistakes doctor` report:\n%s", out)

	if !strings.Contains(out, "antigravity") {
		t.Fatalf("doctor report missing antigravity agent entry:\n%s", out)
	}
	if !strings.Contains(out, agyPath) {
		t.Fatalf("doctor did not detect antigravity at %q:\n%s", agyPath, out)
	}
}

func writeFakeAgyBinary(t *testing.T, dir string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		dst := filepath.Join(dir, "agy.cmd")
		if err := os.WriteFile(dst, []byte("@echo off\r\nexit /b 0\r\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return dst
	}
	dst := filepath.Join(dir, "agy")
	if err := os.WriteFile(dst, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dst
}

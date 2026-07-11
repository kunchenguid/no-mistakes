package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/telemetry"
)

// TestDoctorListsGrokAgent exercises the user-facing `no-mistakes doctor`
// report and verifies the Grok CLI appears in the Agents section and is
// detected when its binary is on PATH.
func TestDoctorListsGrokAgent(t *testing.T) {
	restore := telemetry.SetDefaultForTesting(&telemetryRecorder{})
	defer restore()

	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)

	binDir := t.TempDir()
	grokPath := writeFakeGrokBinary(t, binDir)

	sep := string(os.PathListSeparator)
	t.Setenv("PATH", binDir+sep+os.Getenv("PATH"))

	out, err := executeCmd("doctor")
	if err != nil {
		t.Fatalf("doctor failed: %v\n%s", err, out)
	}

	t.Logf("rendered `no-mistakes doctor` report:\n%s", out)

	if !strings.Contains(out, "grok") {
		t.Fatalf("doctor report missing grok agent entry:\n%s", out)
	}
	if !strings.Contains(out, grokPath) {
		t.Fatalf("doctor did not detect grok at %q:\n%s", grokPath, out)
	}
}

func writeFakeGrokBinary(t *testing.T, dir string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		dst := filepath.Join(dir, "grok.cmd")
		if err := os.WriteFile(dst, []byte("@echo off\r\nexit /b 0\r\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return dst
	}
	dst := filepath.Join(dir, "grok")
	if err := os.WriteFile(dst, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return dst
}

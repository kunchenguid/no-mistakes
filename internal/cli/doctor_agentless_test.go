package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/telemetry"
)

func TestDoctorReportsGateCannotValidateWithoutAgent(t *testing.T) {
	restore := telemetry.SetDefaultForTesting(&telemetryRecorder{})
	defer restore()

	t.Setenv("NM_HOME", t.TempDir())
	binDir := t.TempDir()
	writeDoctorGitBinary(t, binDir)
	t.Setenv("PATH", binDir)

	out, err := executeCmd("doctor")
	if err != nil {
		t.Fatalf("doctor failed: %v\n%s", err, out)
	}
	for _, want := range []string{"gate validation", "no runnable agent", "gate cannot validate", "some checks failed"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor output should contain %q, got:\n%s", want, out)
		}
	}
}

func TestDoctorAcceptsConfiguredACPBridge(t *testing.T) {
	restore := telemetry.SetDefaultForTesting(&telemetryRecorder{})
	defer restore()

	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)
	if err := os.WriteFile(filepath.Join(nmHome, "config.yaml"), []byte("agent: acp:gemini\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	binDir := t.TempDir()
	writeDoctorGitBinary(t, binDir)
	writeDoctorStubBinary(t, binDir, "acpx")
	t.Setenv("PATH", binDir)

	out, err := executeCmd("doctor")
	if err != nil {
		t.Fatalf("doctor failed: %v\n%s", err, out)
	}
	for _, want := range []string{"acpx", "gate validation", "acp:gemini is runnable"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor output should contain %q, got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "some checks failed") {
		t.Errorf("doctor should accept configured ACP bridge, got:\n%s", out)
	}
}

func writeDoctorGitBinary(t *testing.T, dir string) {
	t.Helper()
	name := "git"
	contents := "#!/bin/sh\necho 'git version test'\n"
	if runtime.GOOS == "windows" {
		name = "git.cmd"
		contents = "@echo off\r\necho git version test\r\n"
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0o755); err != nil {
		t.Fatalf("write fake git: %v", err)
	}
}

func writeDoctorStubBinary(t *testing.T, dir, base string) {
	t.Helper()
	name := base
	contents := "#!/bin/sh\nexit 0\n"
	if runtime.GOOS == "windows" {
		name += ".cmd"
		contents = "@echo off\r\nexit /b 0\r\n"
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0o755); err != nil {
		t.Fatalf("write fake %s: %v", base, err)
	}
}

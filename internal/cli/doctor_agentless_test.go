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
	for _, want := range []string{"gate validation", "omp binary", "not found", "some checks failed"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor output should contain %q, got:\n%s", want, out)
		}
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

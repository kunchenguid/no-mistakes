package cli

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/telemetry"
)

var doctorACPAliasRows = []struct {
	row        string
	commandBin string
}{
	{row: "cursor", commandBin: "cursor-agent"},
	{row: "devin", commandBin: "devin"},
}

func TestDoctorACPAliasRequiresBothBinaries(t *testing.T) {
	for _, tt := range doctorACPAliasRows {
		t.Run(tt.row, func(t *testing.T) {
			restore := telemetry.SetDefaultForTesting(&telemetryRecorder{})
			defer restore()

			t.Setenv("NM_HOME", t.TempDir())

			binDir := t.TempDir()
			writeFakeBinary(t, binDir, tt.commandBin)
			t.Setenv("PATH", binDir)

			out, err := executeCmd("doctor")
			if err != nil {
				t.Fatalf("doctor failed: %v\n%s", err, out)
			}

			line := doctorAgentLine(t, out, tt.row)
			if !strings.Contains(line, "acpx") {
				t.Fatalf("%s alias row should name the missing acpx binary:\n%s", tt.row, line)
			}
		})
	}
}

func TestDoctorACPAliasDetectedWithBothBinaries(t *testing.T) {
	for _, tt := range doctorACPAliasRows {
		t.Run(tt.row, func(t *testing.T) {
			restore := telemetry.SetDefaultForTesting(&telemetryRecorder{})
			defer restore()

			t.Setenv("NM_HOME", t.TempDir())

			binDir := t.TempDir()
			commandPath := writeFakeBinary(t, binDir, tt.commandBin)
			acpxPath := writeFakeBinary(t, binDir, "acpx")
			t.Setenv("PATH", binDir)

			out, err := executeCmd("doctor")
			if err != nil {
				t.Fatalf("doctor failed: %v\n%s", err, out)
			}

			line := doctorAgentLine(t, out, tt.row)
			for _, want := range []string{commandPath, acpxPath} {
				if !strings.Contains(line, want) {
					t.Fatalf("%s alias row did not report binary %q:\n%s", tt.row, want, line)
				}
			}
		})
	}
}

func doctorAgentLine(t *testing.T, out, agent string) string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		for i, f := range strings.Fields(line) {
			if i >= 1 && f == agent {
				return line
			}
		}
	}
	t.Fatalf("no %q row in doctor output:\n%s", agent, out)
	return ""
}

func writeFakeBinary(t *testing.T, dir, name string) string {
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

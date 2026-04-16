//go:build windows

package steps

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunShellCommandWithEnv_UsesCmdAndIgnoresUserShell_EnableWindowsCI(t *testing.T) {
	workDir := t.TempDir()
	marker := filepath.Join(t.TempDir(), "user-shell-used")
	shellPath := filepath.Join(t.TempDir(), "fake-shell.cmd")
	script := "@echo off\r\n> \"%USER_SHELL_MARKER%\" echo used\r\nexit /b 99\r\n"
	if err := os.WriteFile(shellPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHELL", shellPath)
	t.Setenv("USER_SHELL_MARKER", marker)

	output, exitCode, err := runShellCommandWithEnv(context.Background(), workDir, []string{"STEP_SPECIAL=from-step"}, `echo %STEP_SPECIAL%`)
	if err != nil {
		t.Fatal(err)
	}
	if exitCode != 0 {
		t.Fatalf("exitCode = %d", exitCode)
	}
	if strings.TrimSpace(output) != "from-step" {
		t.Fatalf("output = %q", output)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("expected custom user shell to be ignored")
	}
}

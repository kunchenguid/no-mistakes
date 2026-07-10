//go:build windows

package agent

import (
	"os"
	"path/filepath"
	"testing"
)

// npmClaudeShim mirrors the real C:\Users\...\AppData\Roaming\npm\claude.cmd
// that npm generates on Windows. The launcher line references the wrapped
// executable relative to the shim directory via a %dp0% variable.
const npmClaudeShim = `@ECHO off
GOTO start
:find_dp0
SET dp0=%~dp0
EXIT /b
:start
SETLOCAL
CALL :find_dp0
"%dp0%\node_modules\@anthropic-ai\claude-code\bin\claude.exe"   %*
`

// writeShim writes shim content plus, when target is non-empty, an empty file
// at the shim-relative target path so os.Stat succeeds. It returns the shim path.
func writeShim(t *testing.T, content, target string) string {
	t.Helper()
	dir := t.TempDir()
	shim := filepath.Join(dir, "claude.cmd")
	if err := os.WriteFile(shim, []byte(content), 0o644); err != nil {
		t.Fatalf("write shim: %v", err)
	}
	if target != "" {
		full := filepath.Join(dir, target)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir target: %v", err)
		}
		if err := os.WriteFile(full, []byte("MZ"), 0o644); err != nil {
			t.Fatalf("write target exe: %v", err)
		}
	}
	return shim
}

func TestShimNativeTarget_NpmClaudeShim(t *testing.T) {
	target := filepath.Join("node_modules", "@anthropic-ai", "claude-code", "bin", "claude.exe")
	shim := writeShim(t, npmClaudeShim, target)

	got := shimNativeTarget(shim)
	want := filepath.Clean(filepath.Join(filepath.Dir(shim), target))
	if got != want {
		t.Fatalf("shimNativeTarget = %q, want %q", got, want)
	}
}

func TestShimNativeTarget_MissingTargetFallsBack(t *testing.T) {
	// The shim references a claude.exe, but we do not create it on disk.
	shim := writeShim(t, npmClaudeShim, "")
	if got := shimNativeTarget(shim); got != "" {
		t.Fatalf("shimNativeTarget = %q, want empty when target is absent", got)
	}
}

func TestShimNativeTarget_NonShimReturnsEmpty(t *testing.T) {
	shim := writeShim(t, "@ECHO off\necho hello\n", "")
	if got := shimNativeTarget(shim); got != "" {
		t.Fatalf("shimNativeTarget = %q, want empty for a non-launcher .cmd", got)
	}
}

func TestResolveAgentBinary_ResolvesShimToExe(t *testing.T) {
	target := filepath.Join("node_modules", "@anthropic-ai", "claude-code", "bin", "claude.exe")
	shim := writeShim(t, npmClaudeShim, target)

	got := resolveAgentBinary(shim)
	want := filepath.Clean(filepath.Join(filepath.Dir(shim), target))
	if got != want {
		t.Fatalf("resolveAgentBinary(%q) = %q, want %q", shim, got, want)
	}
}

func TestResolveAgentBinary_NonShimUnchanged(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "claude.exe")
	if err := os.WriteFile(exe, []byte("MZ"), 0o644); err != nil {
		t.Fatalf("write exe: %v", err)
	}
	if got := resolveAgentBinary(exe); got != exe {
		t.Fatalf("resolveAgentBinary(%q) = %q, want unchanged", exe, got)
	}
}

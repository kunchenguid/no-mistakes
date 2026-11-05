package runenv

import (
	"path/filepath"
	"testing"
)

func TestCaptureTranscriptHomes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CODEX_HOME", "")
	got, err := CaptureTranscriptHomes()
	if err != nil || got.ClaudeConfigDir != filepath.Join(home, ".claude") || got.CodexHome != filepath.Join(home, ".codex") {
		t.Fatalf("defaults = %+v, %v", got, err)
	}
	claude := filepath.Join(t.TempDir(), "claude account")
	codex := filepath.Join(t.TempDir(), "codex account")
	t.Setenv("CLAUDE_CONFIG_DIR", claude)
	t.Setenv("CODEX_HOME", codex)
	got, err = CaptureTranscriptHomes()
	if err != nil || got.ClaudeConfigDir != claude || got.CodexHome != codex {
		t.Fatalf("explicit homes = %+v, %v", got, err)
	}
	for _, key := range []string{"CLAUDE_CONFIG_DIR", "CODEX_HOME"} {
		t.Run(key, func(t *testing.T) {
			t.Setenv(key, "relative-account")
			if _, err := CaptureTranscriptHomes(); err == nil {
				t.Fatal("relative account must not resolve against the daemon's cwd")
			}
		})
	}
}

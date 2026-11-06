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
	got := CaptureTranscriptHomes()
	if got.ClaudeConfigDir != filepath.Join(home, ".claude") || got.CodexHome != filepath.Join(home, ".codex") {
		t.Fatalf("defaults = %+v", got)
	}
	claude := filepath.Join(t.TempDir(), "claude account")
	codex := filepath.Join(t.TempDir(), "codex account")
	t.Setenv("CLAUDE_CONFIG_DIR", claude)
	t.Setenv("CODEX_HOME", codex)
	got = CaptureTranscriptHomes()
	if got.ClaudeConfigDir != claude || got.CodexHome != codex {
		t.Fatalf("explicit homes = %+v", got)
	}
	for _, key := range []string{"CLAUDE_CONFIG_DIR", "CODEX_HOME"} {
		t.Run(key, func(t *testing.T) {
			for _, selection := range []string{"relative-account", "~/account"} {
				t.Setenv(key, selection)
				got := CaptureTranscriptHomes()
				want := TranscriptHomes{ClaudeConfigDir: claude, CodexHome: codex}
				if key == "CLAUDE_CONFIG_DIR" {
					want.ClaudeConfigDir = selection
					if got.ValidateClaude() == nil || got.ValidateCodex() != nil {
						t.Fatalf("Claude rejection affected Codex: %+v", got)
					}
				} else {
					want.CodexHome = selection
					if got.ValidateCodex() == nil || got.ValidateClaude() != nil {
						t.Fatalf("Codex rejection affected Claude: %+v", got)
					}
				}
				if *got != want {
					t.Fatalf("invalid selection = %+v, want %+v and disabled discovery", got, want)
				}
			}
		})
	}
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")
	got = CaptureTranscriptHomes()
	if got.ClaudeConfigDir != claude || got.CodexHome != codex {
		t.Fatalf("explicit homes without HOME = %+v", got)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	t.Setenv("CODEX_HOME", "")
	got = CaptureTranscriptHomes()
	if *got != (TranscriptHomes{}) || got.ValidateClaude() == nil || got.ValidateCodex() == nil {
		t.Fatalf("unresolved defaults = %+v, want empty roots and disabled discovery", got)
	}
}

package config

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseLogLevel(t *testing.T) {
	tests := []struct {
		input string
		want  slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"error", slog.LevelError},
		{"", slog.LevelInfo},
		{"unknown", slog.LevelInfo},
		{"DEBUG", slog.LevelInfo}, // case-sensitive, unrecognized defaults to info
	}
	for _, tt := range tests {
		got := ParseLogLevel(tt.input)
		if got != tt.want {
			t.Errorf("ParseLogLevel(%q) = %v, want %v", tt.input, got, tt.want)
		}
	}
}

// TestLoadGlobalRejectsLegacyKeys proves each removed model-selection key
// produces a strict, actionable error instead of being ignored or rewritten.
func TestLoadGlobalRejectsLegacyKeys(t *testing.T) {
	for _, key := range []string{
		"agent",
		"fallback_agents",
		"acpx_path",
		"acp_registry_overrides",
		"agent_path_override",
		"agent_args_override",
		"auto_fix",
		"babysit_timeout",
	} {
		dir := t.TempDir()
		path := filepath.Join(dir, "config.yaml")
		if err := os.WriteFile(path, []byte(key+": value\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := LoadGlobal(path)
		if err == nil {
			t.Fatalf("LoadGlobal accepted legacy key %q, want a strict error", key)
		}
		if !strings.Contains(err.Error(), key) || !strings.Contains(err.Error(), "no longer supported") {
			t.Fatalf("LoadGlobal(%q) error = %v, want an actionable rejection naming the key", key, err)
		}
	}
}

// TestLoadGlobalDefaultTemplateLoads proves the shipped default template loads
// cleanly and carries the built-in routing contract (no removed keys).
func TestLoadGlobalDefaultTemplateLoads(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	EnsureDefaultGlobalConfig(path)
	cfg, err := LoadGlobal(path)
	if err != nil {
		t.Fatalf("default template failed to load: %v", err)
	}
	if cfg.Routing.IsZero() {
		t.Fatal("default config should carry the built-in routing contract")
	}
}

// TestLoadRepoRejectsAgentAndAutoFix proves a repository cannot select an agent
// or set numeric auto-fix limits; model selection is global-only.
func TestLoadRepoRejectsAgentAndAutoFix(t *testing.T) {
	for _, key := range []string{"agent", "auto_fix"} {
		if _, err := LoadRepoFromBytes([]byte(key + ": value\n")); err == nil {
			t.Fatalf("LoadRepoFromBytes accepted repo key %q, want an error", key)
		}
	}
}

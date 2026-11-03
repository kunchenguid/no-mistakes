package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadGlobal_MaxConcurrentRuns(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("max_concurrent_runs: 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadGlobal(path)
	if err != nil {
		t.Fatalf("LoadGlobal: %v", err)
	}
	if cfg.MaxConcurrentRuns != 2 {
		t.Fatalf("max_concurrent_runs = %d, want 2", cfg.MaxConcurrentRuns)
	}
}

func TestLoadGlobal_MaxConcurrentRunsDefaultsToUnlimited(t *testing.T) {
	cfg, err := LoadGlobal("/nonexistent/config.yaml")
	if err != nil {
		t.Fatalf("LoadGlobal: %v", err)
	}
	if cfg.MaxConcurrentRuns != 0 {
		t.Fatalf("max_concurrent_runs default = %d, want 0 (unlimited)", cfg.MaxConcurrentRuns)
	}
}

func TestLoadGlobal_RejectsNegativeMaxConcurrentRuns(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("max_concurrent_runs: -1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadGlobal(path); err == nil {
		t.Fatal("LoadGlobal(max_concurrent_runs: -1) error = nil, want error")
	}
}

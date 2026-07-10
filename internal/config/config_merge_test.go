package config

import (
	"testing"
	"time"
)

func TestMerge_GlobalOnly(t *testing.T) {
	global := &GlobalConfig{
		CITimeout: 4 * time.Hour,
		LogLevel:  "info",
	}
	repo := &RepoConfig{}

	cfg := Merge(global, repo)
	if cfg.CITimeout != 4*time.Hour {
		t.Errorf("ci_timeout = %v", cfg.CITimeout)
	}
}

func TestMerge_RepoCommandsPreserveGlobal(t *testing.T) {
	global := &GlobalConfig{
		CITimeout: 4 * time.Hour,
		LogLevel:  "info",
	}
	repo := &RepoConfig{
		Commands: Commands{
			Test: "make test",
		},
	}

	cfg := Merge(global, repo)
	if cfg.Commands.Test != "make test" {
		t.Errorf("test = %q", cfg.Commands.Test)
	}
	if cfg.CITimeout != 4*time.Hour {
		t.Errorf("ci_timeout = %v", cfg.CITimeout)
	}
}

func TestMerge_RepoCommands(t *testing.T) {
	global := &GlobalConfig{
		CITimeout: 2 * time.Hour,
		LogLevel:  "debug",
	}
	repo := &RepoConfig{
		Commands: Commands{
			Lint: "eslint .",
		},
	}

	cfg := Merge(global, repo)
	if cfg.Commands.Lint != "eslint ." {
		t.Errorf("lint = %q", cfg.Commands.Lint)
	}
}

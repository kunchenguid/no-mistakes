package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPRDefaults(t *testing.T) {
	if !prDefaults().IncludePipelineSummary {
		t.Error("default IncludePipelineSummary should be true (opt-out, not opt-in)")
	}
}

func TestPRMerge_DefaultIncludesPipelineSummary(t *testing.T) {
	cfg := Merge(&GlobalConfig{}, &RepoConfig{})
	if !cfg.PR.IncludePipelineSummary {
		t.Error("unset config should default to including the pipeline summary")
	}
}

func TestPRMerge_GlobalDisable(t *testing.T) {
	disabled := false
	global := &GlobalConfig{PR: PRRaw{IncludePipelineSummary: &disabled}}

	cfg := Merge(global, &RepoConfig{})
	if cfg.PR.IncludePipelineSummary {
		t.Error("global include_pipeline_summary: false should propagate")
	}
}

func TestPRMerge_RepoOverridesGlobal(t *testing.T) {
	enabled := true
	disabled := false

	// Repo re-enables what global disabled.
	global := &GlobalConfig{PR: PRRaw{IncludePipelineSummary: &disabled}}
	repo := &RepoConfig{PR: PRRaw{IncludePipelineSummary: &enabled}}
	if cfg := Merge(global, repo); !cfg.PR.IncludePipelineSummary {
		t.Error("repo enable should override global disable")
	}

	// Repo disables what global enabled.
	global = &GlobalConfig{PR: PRRaw{IncludePipelineSummary: &enabled}}
	repo = &RepoConfig{PR: PRRaw{IncludePipelineSummary: &disabled}}
	if cfg := Merge(global, repo); cfg.PR.IncludePipelineSummary {
		t.Error("repo disable should override global enable")
	}
}

func TestPRMerge_UnsetRepoInheritsGlobalDisable(t *testing.T) {
	disabled := false
	global := &GlobalConfig{PR: PRRaw{IncludePipelineSummary: &disabled}}
	repo := &RepoConfig{} // no pr key

	cfg := Merge(global, repo)
	if cfg.PR.IncludePipelineSummary {
		t.Error("unset repo pr should inherit the global disable")
	}
}

func TestLoadGlobalConfig_PRIncludePipelineSummaryParsed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := `
agent: claude
pr:
  include_pipeline_summary: false
`
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg, err := LoadGlobal(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.PR.IncludePipelineSummary == nil || *cfg.PR.IncludePipelineSummary {
		t.Error("expected global IncludePipelineSummary=false")
	}

	merged := Merge(cfg, &RepoConfig{})
	if merged.PR.IncludePipelineSummary {
		t.Error("expected merged config to omit the pipeline summary")
	}
}

func TestLoadRepoConfig_PRIncludePipelineSummaryParsed(t *testing.T) {
	dir := t.TempDir()
	yaml := `
pr:
  include_pipeline_summary: false
`
	if err := os.WriteFile(filepath.Join(dir, ".no-mistakes.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg, err := LoadRepo(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.PR.IncludePipelineSummary == nil || *cfg.PR.IncludePipelineSummary {
		t.Error("expected repo IncludePipelineSummary=false")
	}
}

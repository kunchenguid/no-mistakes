package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPRDefaults(t *testing.T) {
	got := prDefaults()
	if !got.PipelineSummary {
		t.Error("default PipelineSummary should be true (current behavior preserved)")
	}
}

func TestPRMerge_DefaultKeepsPipelineSummary(t *testing.T) {
	cfg := Merge(&GlobalConfig{}, &RepoConfig{})
	if !cfg.PR.PipelineSummary {
		t.Error("unset pr.pipeline_summary should resolve to true")
	}
}

func TestPRMerge_GlobalDisable(t *testing.T) {
	disabled := false
	global := &GlobalConfig{PR: PRRaw{PipelineSummary: &disabled}}

	cfg := Merge(global, &RepoConfig{})
	if cfg.PR.PipelineSummary {
		t.Error("global pipeline_summary=false should propagate")
	}
}

func TestPRMerge_RepoOverridesGlobal(t *testing.T) {
	enabled := true
	disabled := false
	global := &GlobalConfig{PR: PRRaw{PipelineSummary: &disabled}}
	repo := &RepoConfig{PR: PRRaw{PipelineSummary: &enabled}}

	cfg := Merge(global, repo)
	if !cfg.PR.PipelineSummary {
		t.Error("repo enable should override global disable")
	}

	global = &GlobalConfig{PR: PRRaw{PipelineSummary: &enabled}}
	repo = &RepoConfig{PR: PRRaw{PipelineSummary: &disabled}}

	cfg = Merge(global, repo)
	if cfg.PR.PipelineSummary {
		t.Error("repo disable should override global enable")
	}
}

func TestLoadGlobalConfig_PRParsed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := `
agent: claude
pr:
  pipeline_summary: false
`
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg, err := LoadGlobal(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.PR.PipelineSummary == nil || *cfg.PR.PipelineSummary {
		t.Error("expected PipelineSummary=false")
	}
}

func TestLoadRepoConfig_PRParsed(t *testing.T) {
	dir := t.TempDir()
	yaml := `
pr:
  pipeline_summary: false
`
	if err := os.WriteFile(filepath.Join(dir, ".no-mistakes.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg, err := LoadRepo(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.PR.PipelineSummary == nil || *cfg.PR.PipelineSummary {
		t.Error("expected repo PipelineSummary=false")
	}
}

// pr.pipeline_summary is a non-executing cosmetic field, so it stays honored
// from the pushed branch like auto_fix/commit (see EffectiveRepoConfig).
func TestEffectiveRepoConfig_PRComesFromPushedBranch(t *testing.T) {
	disabled := false
	pushed := &RepoConfig{PR: PRRaw{PipelineSummary: &disabled}}
	trusted := &RepoConfig{}

	got := EffectiveRepoConfig(pushed, trusted, false)
	if got.PR.PipelineSummary == nil || *got.PR.PipelineSummary {
		t.Error("pushed pr.pipeline_summary should survive EffectiveRepoConfig")
	}
}

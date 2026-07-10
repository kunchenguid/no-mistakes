package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPRDefaults(t *testing.T) {
	got := prDefaults()
	if !got.Draft {
		t.Error("default Draft should be true (PRs open as drafts by default)")
	}
}

func TestPRMerge_RepoEnablesDraft(t *testing.T) {
	enabled := true
	repo := &RepoConfig{PR: PRRaw{Draft: &enabled}}

	cfg := Merge(&GlobalConfig{}, repo)
	if !cfg.PR.Draft {
		t.Error("repo pr.draft: true should propagate to merged config")
	}
}

func TestPRMerge_RepoOptsOutOfDraft(t *testing.T) {
	disabled := false
	repo := &RepoConfig{PR: PRRaw{Draft: &disabled}}

	cfg := Merge(&GlobalConfig{}, repo)
	if cfg.PR.Draft {
		t.Error("repo pr.draft: false should override the draft-by-default default")
	}
}

func TestPRMerge_UnsetDefaultsToDraftTrue(t *testing.T) {
	cfg := Merge(&GlobalConfig{}, &RepoConfig{})
	if !cfg.PR.Draft {
		t.Error("PR.Draft should default to true when unset")
	}
}

func TestLoadRepoConfig_PRDraftParsed(t *testing.T) {
	dir := t.TempDir()
	yaml := `
pr:
  draft: true
`
	if err := os.WriteFile(filepath.Join(dir, ".no-mistakes.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg, err := LoadRepo(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.PR.Draft == nil || !*cfg.PR.Draft {
		t.Error("expected repo pr.draft=true")
	}
}

// TestEffectiveRepoConfig_PRDraftComesFromPushed proves pr.draft is a
// non-executing field: it is read from the pushed branch, not gated behind
// allow_repo_commands or restricted to the trusted default-branch copy.
func TestEffectiveRepoConfig_PRDraftComesFromPushed(t *testing.T) {
	enabled := true
	pushed := &RepoConfig{PR: PRRaw{Draft: &enabled}}
	trusted := &RepoConfig{}

	got := EffectiveRepoConfig(pushed, trusted, false)

	if got.PR.Draft == nil || !*got.PR.Draft {
		t.Error("pr.draft should come from the pushed copy even without allow_repo_commands")
	}
}

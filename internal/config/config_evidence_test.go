package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTestEvidenceDefaults(t *testing.T) {
	got := testDefaults()
	if got.Evidence.StoreInRepo {
		t.Error("default StoreInRepo should be false (opt-in)")
	}
	if got.Evidence.Dir != ".no-mistakes/evidence" {
		t.Errorf("default Dir = %q, want .no-mistakes/evidence", got.Evidence.Dir)
	}
}

func TestTestEvidenceMerge_GlobalEnable(t *testing.T) {
	enabled := true
	global := &GlobalConfig{Test: TestRaw{Evidence: EvidenceRaw{StoreInRepo: &enabled}}}
	repo := &RepoConfig{}

	cfg := Merge(global, repo)
	if !cfg.Test.Evidence.StoreInRepo {
		t.Error("global enable should propagate")
	}
	// Default dir preserved when not overridden.
	if cfg.Test.Evidence.Dir != ".no-mistakes/evidence" {
		t.Errorf("dir = %q, want default", cfg.Test.Evidence.Dir)
	}
}

func TestTestEvidenceMerge_RepoOverridesGlobal(t *testing.T) {
	enabled := true
	disabled := false
	dir := "docs/evidence"
	global := &GlobalConfig{Test: TestRaw{Evidence: EvidenceRaw{StoreInRepo: &disabled}}}
	repo := &RepoConfig{Test: TestRaw{Evidence: EvidenceRaw{StoreInRepo: &enabled, Dir: &dir}}}

	cfg := Merge(global, repo)
	if !cfg.Test.Evidence.StoreInRepo {
		t.Error("repo enable should override global disable")
	}
	if cfg.Test.Evidence.Dir != "docs/evidence" {
		t.Errorf("dir = %q, want docs/evidence", cfg.Test.Evidence.Dir)
	}
}

func TestTestEvidenceMerge_BlankDirIgnored(t *testing.T) {
	blank := "   "
	repo := &RepoConfig{Test: TestRaw{Evidence: EvidenceRaw{Dir: &blank}}}

	cfg := Merge(&GlobalConfig{}, repo)
	if cfg.Test.Evidence.Dir != ".no-mistakes/evidence" {
		t.Errorf("blank dir should fall back to default, got %q", cfg.Test.Evidence.Dir)
	}
}

func TestLoadGlobalConfig_TestEvidenceParsed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := `
agent: claude
test:
  evidence:
    store_in_repo: true
    dir: artifacts/evidence
`
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg, err := LoadGlobal(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Test.Evidence.StoreInRepo == nil || !*cfg.Test.Evidence.StoreInRepo {
		t.Error("expected StoreInRepo=true")
	}
	if cfg.Test.Evidence.Dir == nil || *cfg.Test.Evidence.Dir != "artifacts/evidence" {
		t.Error("expected Dir=artifacts/evidence")
	}
}

func TestLoadRepoConfig_TestEvidenceParsed(t *testing.T) {
	dir := t.TempDir()
	yaml := `
test:
  evidence:
    store_in_repo: true
`
	if err := os.WriteFile(filepath.Join(dir, ".no-mistakes.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	cfg, err := LoadRepo(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Test.Evidence.StoreInRepo == nil || !*cfg.Test.Evidence.StoreInRepo {
		t.Error("expected repo StoreInRepo=true")
	}
}

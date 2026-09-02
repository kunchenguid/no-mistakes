package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/evidence"
)

func TestTestEvidenceDefaults(t *testing.T) {
	got := testDefaults()
	if got.Evidence.StoreInRepo {
		t.Error("default StoreInRepo should be false (opt-in)")
	}
	if !got.Evidence.AttachMedia {
		t.Error("default AttachMedia should be true so default-config PRs upload screenshots")
	}
	if got.Evidence.Dir != ".no-mistakes/evidence" {
		t.Errorf("default Dir = %q, want .no-mistakes/evidence", got.Evidence.Dir)
	}
	if got.Evidence.Branch != evidence.DefaultBranch {
		t.Errorf("default Branch = %q, want %q", got.Evidence.Branch, evidence.DefaultBranch)
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
	// Defaults preserved when not overridden.
	if cfg.Test.Evidence.Dir != ".no-mistakes/evidence" {
		t.Errorf("dir = %q, want default", cfg.Test.Evidence.Dir)
	}
	if cfg.Test.Evidence.Branch != evidence.DefaultBranch {
		t.Errorf("branch = %q, want default %q", cfg.Test.Evidence.Branch, evidence.DefaultBranch)
	}
	if !cfg.Test.Evidence.AttachMedia {
		t.Error("attach_media should default on when unset")
	}
}

func TestTestEvidenceMerge_AttachMediaOptOut(t *testing.T) {
	disabled := false
	global := &GlobalConfig{Test: TestRaw{Evidence: EvidenceRaw{AttachMedia: &disabled}}}
	cfg := Merge(global, &RepoConfig{})
	if cfg.Test.Evidence.AttachMedia {
		t.Error("explicit attach_media: false should opt out")
	}
}

func TestTestEvidenceMerge_RepoOverridesGlobal(t *testing.T) {
	enabled := true
	disabled := false
	dir := "docs/evidence"
	globalBranch := "no-mistakes/global-evidence"
	repoBranch := "team/ci/evidence"
	global := &GlobalConfig{Test: TestRaw{Evidence: EvidenceRaw{StoreInRepo: &disabled, Branch: &globalBranch}}}
	repo := &RepoConfig{Test: TestRaw{Evidence: EvidenceRaw{StoreInRepo: &enabled, Dir: &dir, Branch: &repoBranch}}}

	cfg := Merge(global, repo)
	if !cfg.Test.Evidence.StoreInRepo {
		t.Error("repo enable should override global disable")
	}
	if cfg.Test.Evidence.Dir != "docs/evidence" {
		t.Errorf("dir = %q, want docs/evidence", cfg.Test.Evidence.Dir)
	}
	if cfg.Test.Evidence.Branch != repoBranch {
		t.Errorf("branch = %q, want %q", cfg.Test.Evidence.Branch, repoBranch)
	}
}

func TestTestEvidenceMerge_GlobalBranchAppliesWithoutRepoOverride(t *testing.T) {
	branch := "no-mistakes/global-evidence"
	global := &GlobalConfig{Test: TestRaw{Evidence: EvidenceRaw{Branch: &branch}}}

	cfg := Merge(global, &RepoConfig{})
	if cfg.Test.Evidence.Branch != branch {
		t.Errorf("branch = %q, want %q", cfg.Test.Evidence.Branch, branch)
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

func TestTestEvidenceMerge_BlankBranchFallsBackToDefault(t *testing.T) {
	blank := "   "
	repo := &RepoConfig{Test: TestRaw{Evidence: EvidenceRaw{Branch: &blank}}}

	cfg := Merge(&GlobalConfig{}, repo)
	if cfg.Test.Evidence.Branch != evidence.DefaultBranch {
		t.Errorf("blank branch should fall back to default, got %q", cfg.Test.Evidence.Branch)
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
    branch: team/ci/evidence
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
	if cfg.Test.Evidence.Branch == nil || *cfg.Test.Evidence.Branch != "team/ci/evidence" {
		t.Error("expected Branch=team/ci/evidence")
	}
}

func TestLoadGlobalConfig_InvalidEvidenceBranchFailsClosed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	yaml := `
test:
  evidence:
    branch: "not a branch"
`
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, err := LoadGlobal(path)
	if err == nil {
		t.Fatal("expected an invalid evidence branch name to fail the config")
	}
	if !strings.Contains(err.Error(), "test.evidence.branch") || !strings.Contains(err.Error(), "not a branch") {
		t.Errorf("error %q does not name the offending key and value", err)
	}
}

func TestLoadRepoConfig_TestEvidenceParsed(t *testing.T) {
	dir := t.TempDir()
	yaml := `
test:
  evidence:
    store_in_repo: true
    branch: team/ci/evidence
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
	if cfg.Test.Evidence.Branch == nil || *cfg.Test.Evidence.Branch != "team/ci/evidence" {
		t.Error("expected repo Branch=team/ci/evidence")
	}
}

func TestLoadRepoConfig_InvalidEvidenceBranchFailsClosed(t *testing.T) {
	dir := t.TempDir()
	yaml := `
test:
  evidence:
    branch: "evidence..branch"
`
	if err := os.WriteFile(filepath.Join(dir, ".no-mistakes.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	if _, err := LoadRepo(dir); err == nil {
		t.Fatal("expected an invalid evidence branch name to fail the repo config")
	} else if !strings.Contains(err.Error(), "test.evidence.branch") {
		t.Errorf("error %q does not name the offending key", err)
	}
}

// The evidence branch names a ref the daemon pushes to with the maintainer's
// credentials, so a contributor's pushed branch must not be able to pick it.
func TestEffectiveRepoConfig_EvidenceBranchTrustedOnly(t *testing.T) {
	pushed := "attacker/branch"
	trusted := "team/ci/evidence"
	enabled := true

	effective := EffectiveRepoConfig(
		&RepoConfig{Test: TestRaw{Evidence: EvidenceRaw{Branch: &pushed, StoreInRepo: &enabled}}},
		&RepoConfig{Test: TestRaw{Evidence: EvidenceRaw{Branch: &trusted}}},
		true,
	)
	if effective.Test.Evidence.Branch == nil || *effective.Test.Evidence.Branch != trusted {
		t.Fatalf("effective branch = %v, want the trusted %q", effective.Test.Evidence.Branch, trusted)
	}
	// The rest of test.evidence still comes from the pushed copy.
	if effective.Test.Evidence.StoreInRepo == nil || !*effective.Test.Evidence.StoreInRepo {
		t.Error("pushed store_in_repo should still apply")
	}

	withoutTrusted := EffectiveRepoConfig(
		&RepoConfig{Test: TestRaw{Evidence: EvidenceRaw{Branch: &pushed}}},
		nil,
		true,
	)
	if withoutTrusted.Test.Evidence.Branch != nil {
		t.Errorf("without a trusted copy the branch must fall back to the default, got %q", *withoutTrusted.Test.Evidence.Branch)
	}
}

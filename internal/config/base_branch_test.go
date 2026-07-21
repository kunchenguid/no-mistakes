package config

import "testing"

func TestRepoConfigDecodesBaseBranch(t *testing.T) {
	cfg, err := LoadRepoFromBytes([]byte("base_branch: staging\n"))
	if err != nil {
		t.Fatalf("LoadRepoFromBytes: %v", err)
	}
	if cfg.BaseBranch != "staging" || !cfg.BaseBranchSet {
		t.Fatalf("BaseBranch = %q set=%v, want staging/true", cfg.BaseBranch, cfg.BaseBranchSet)
	}
}

func TestEffectiveRepoConfigTakesBaseBranchOnlyFromTrustedCopy(t *testing.T) {
	pushed := &RepoConfig{BaseBranch: "feature-controlled", BaseBranchSet: true}
	trusted := &RepoConfig{BaseBranch: "staging", BaseBranchSet: true}

	got := EffectiveRepoConfig(pushed, trusted, true)
	if got.BaseBranch != "staging" || !got.BaseBranchSet {
		t.Fatalf("BaseBranch = %q set=%v, want trusted staging/true", got.BaseBranch, got.BaseBranchSet)
	}

	got = EffectiveRepoConfig(pushed, nil, true)
	if got.BaseBranch != "" {
		t.Fatalf("BaseBranch without trusted config = %q, want empty", got.BaseBranch)
	}
}

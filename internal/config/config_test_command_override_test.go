package config

import (
	"strings"
	"testing"
)

// TestEffectiveRepoConfig_AllowApproveOverFailureTrustedOnly proves a feature
// branch cannot waive the require-no-mistakes check for an approved-over-failure
// commands.test: the opt-in reason comes only from the trusted default-branch
// copy, and allow_repo_commands does not leak a pushed waiver.
func TestEffectiveRepoConfig_AllowApproveOverFailureTrustedOnly(t *testing.T) {
	pushed := &RepoConfig{Test: TestRaw{AllowApproveOverFailure: "waive from the branch"}}
	trusted := &RepoConfig{Test: TestRaw{AllowApproveOverFailure: "legacy suite is red on purpose"}}

	effective := EffectiveRepoConfig(pushed, trusted, false)
	if effective.Test.AllowApproveOverFailure != trusted.Test.AllowApproveOverFailure {
		t.Fatalf("AllowApproveOverFailure = %q, want the trusted reason", effective.Test.AllowApproveOverFailure)
	}

	effective = EffectiveRepoConfig(pushed, trusted, true)
	if effective.Test.AllowApproveOverFailure != trusted.Test.AllowApproveOverFailure {
		t.Fatalf("AllowApproveOverFailure = %q under allow_repo_commands, want the trusted reason", effective.Test.AllowApproveOverFailure)
	}

	effective = EffectiveRepoConfig(pushed, nil, false)
	if effective.Test.AllowApproveOverFailure != "" {
		t.Fatalf("without a trusted copy the pushed waiver must be dropped, got %q", effective.Test.AllowApproveOverFailure)
	}

	if pushed.Test.AllowApproveOverFailure != "waive from the branch" {
		t.Fatal("pushed config was mutated")
	}
}

func TestLoadRepo_AllowApproveOverFailure(t *testing.T) {
	reason := strings.Repeat("legacy-reason-", 64)
	cfg, err := LoadRepoFromBytes([]byte("test:\n  allow_approve_over_failure: " + reason + "\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.Test.AllowApproveOverFailure != reason {
		t.Fatalf("AllowApproveOverFailure = %q, want %q", cfg.Test.AllowApproveOverFailure, reason)
	}
}

func TestMerge_ResolvesAllowApproveOverFailure(t *testing.T) {
	repo := &RepoConfig{Test: TestRaw{AllowApproveOverFailure: "  recorded waiver  "}}
	got := Merge(&GlobalConfig{}, repo)
	if got.Test.AllowApproveOverFailure != "recorded waiver" {
		t.Fatalf("AllowApproveOverFailure = %q, want trimmed", got.Test.AllowApproveOverFailure)
	}
}

func TestMerge_GlobalAllowApproveOverFailureIsNotUsed(t *testing.T) {
	global := &GlobalConfig{Test: TestRaw{AllowApproveOverFailure: "global waiver"}}
	got := Merge(global, &RepoConfig{})
	if got.Test.AllowApproveOverFailure != "" {
		t.Fatalf("global waiver leaked into the resolved config: %q", got.Test.AllowApproveOverFailure)
	}
}

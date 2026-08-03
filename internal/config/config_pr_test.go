package config

import (
	"strconv"
	"strings"
	"testing"
)

func TestLoadRepoConfig_PRBaseBranch(t *testing.T) {
	cfg, err := LoadRepoFromBytes([]byte("pr:\n  base_branch: quality-assurance\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PR.BaseBranch != "quality-assurance" {
		t.Fatalf("PR base branch = %q, want quality-assurance", cfg.PR.BaseBranch)
	}

	merged := Merge(DefaultGlobalConfig(), cfg)
	if merged.PR.BaseBranch != "quality-assurance" {
		t.Fatalf("merged PR base branch = %q, want quality-assurance", merged.PR.BaseBranch)
	}
	if !merged.PR.HasExplicitBaseBranch() {
		t.Fatal("configured PR base was not marked explicit")
	}
}

func TestLoadRepoConfig_PRBaseBranchUnsetPreservesDefaultBehavior(t *testing.T) {
	cfg, err := LoadRepoFromBytes(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PR.BaseBranch != "" {
		t.Fatalf("unset PR base branch = %q, want empty fallback", cfg.PR.BaseBranch)
	}
	merged := Merge(DefaultGlobalConfig(), cfg)
	if merged.PR.BaseBranch != "" {
		t.Fatalf("merged unset PR base branch = %q, want empty fallback", merged.PR.BaseBranch)
	}
	if merged.PR.HasExplicitBaseBranch() {
		t.Fatal("unset PR base was marked explicit")
	}
}

func TestLoadRepoConfig_PRBaseBranchRejectsMalformedExplicitValues(t *testing.T) {
	for name, input := range map[string]string{
		"empty":         "pr:\n  base_branch: \"\"\n",
		"null":          "pr:\n  base_branch: null\n",
		"implicit_null": "pr:\n  base_branch:\n",
		"non_string":    "pr:\n  base_branch: 42\n",
		"unknown_field": "pr:\n  base_brnch: quality-assurance\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := LoadRepoFromBytes([]byte(input))
			if err == nil {
				t.Fatal("expected malformed explicit PR target to be rejected")
			}
			if !strings.Contains(err.Error(), "pr") {
				t.Fatalf("error %q does not identify pr configuration", err)
			}
		})
	}
}

func TestLoadRepoConfig_PRBaseBranchRejectsUnsafeNames(t *testing.T) {
	for _, branch := range []string{
		"-quality-assurance",
		"refs/heads/quality-assurance",
		"quality assurance",
		"quality..assurance",
		"quality@{upstream}",
		"quality~1",
		"quality^2",
		"quality:assurance",
		"quality\\assurance",
		"quality/",
		"/quality",
		"quality//assurance",
		"quality.lock",
		".quality",
		"quality.",
		"@",
		"HEAD",
	} {
		t.Run(strings.NewReplacer("/", "_", "\\", "_").Replace(branch), func(t *testing.T) {
			_, err := LoadRepoFromBytes([]byte("pr:\n  base_branch: " + strconv.Quote(branch) + "\n"))
			if err == nil {
				t.Fatalf("expected pr.base_branch %q to be rejected", branch)
			}
			if !strings.Contains(err.Error(), "pr.base_branch") {
				t.Fatalf("error %q does not name pr.base_branch", err)
			}
		})
	}
}

func TestEffectiveRepoConfig_PRBaseBranchTrustedOnly(t *testing.T) {
	pushed := &RepoConfig{PR: PRRaw{BaseBranch: "attacker-target"}}
	trusted := &RepoConfig{PR: PRRaw{BaseBranch: "quality-assurance"}}

	for _, allowRepoCommands := range []bool{false, true} {
		got := EffectiveRepoConfig(pushed, trusted, allowRepoCommands)
		if got.PR.BaseBranch != "quality-assurance" {
			t.Fatalf("allow_repo_commands=%v: PR base branch = %q, want trusted target", allowRepoCommands, got.PR.BaseBranch)
		}
	}

	got := EffectiveRepoConfig(pushed, nil, true)
	if got.PR.BaseBranch != "" {
		t.Fatalf("missing trusted config accepted pushed PR base branch %q", got.PR.BaseBranch)
	}
}

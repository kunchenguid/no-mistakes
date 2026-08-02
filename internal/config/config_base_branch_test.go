package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestValidateBaseBranch(t *testing.T) {
	cases := []struct {
		name    string
		value   string
		wantErr bool
	}{
		{"simple", "main", false},
		{"namespaced", "captain/preserve-firstmate-project-touch-d78", false},
		{"dots and dashes", "release-1.2.x", false},
		{"deep namespace", "a/b/c", false},

		{"empty", "", true},
		{"whitespace padded", " main ", true},
		{"full ref", "refs/heads/main", true},
		{"option-like", "--upload-pack=touch /tmp/pwn", true},
		{"leading dash", "-main", true},
		{"revision range", "main..feature", true},
		{"reflog operator", "main@{1}", true},
		{"glob", "captain/*", true},
		{"space inside", "my branch", true},
		{"trailing slash", "captain/", true},
		{"empty segment", "captain//preserve", true},
		{"lock suffix", "captain/preserve.lock", true},
		{"lock component", "captain.lock/preserve", true},
		{"tilde", "main~1", true},
		{"caret", "main^", true},
		{"colon refspec", "main:main", true},
		{"backtick", "main`id`", true},
		{"newline", "main\nrm -rf /", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateBaseBranch(tc.value)
			if tc.wantErr && err == nil {
				t.Fatalf("ValidateBaseBranch(%q) = nil, want error", tc.value)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("ValidateBaseBranch(%q) = %v, want nil", tc.value, err)
			}
		})
	}
}

// The base decides what gets diffed, reviewed, and PR'd. A pushed branch that
// could name its own base could name itself and collapse the reviewed delta to
// nothing, so base_branch must come from the trusted default-branch copy only.
func TestEffectiveRepoConfig_BaseBranchIsTrustedOnly(t *testing.T) {
	pushed := &RepoConfig{BaseBranch: "attacker-controlled"}
	trusted := &RepoConfig{BaseBranch: "captain/preserve-firstmate-project-touch-d78"}

	got := EffectiveRepoConfig(pushed, trusted, false)
	if got.BaseBranch != trusted.BaseBranch {
		t.Fatalf("BaseBranch = %q, want trusted %q", got.BaseBranch, trusted.BaseBranch)
	}

	// allow_repo_commands opts in to pushed *commands*, never to a pushed base:
	// the base is a review-scope boundary, not a command-selection convenience.
	if got := EffectiveRepoConfig(pushed, trusted, true); got.BaseBranch != trusted.BaseBranch {
		t.Fatalf("BaseBranch with allow_repo_commands = %q, want trusted %q", got.BaseBranch, trusted.BaseBranch)
	}
}

func TestEffectiveRepoConfig_BaseBranchDropsPushedValueWithoutTrustedCopy(t *testing.T) {
	got := EffectiveRepoConfig(&RepoConfig{BaseBranch: "attacker-controlled"}, nil, false)
	if got.BaseBranch != "" {
		t.Fatalf("BaseBranch = %q, want empty when no trusted copy exists", got.BaseBranch)
	}
}

// Absent configuration must be indistinguishable from before the feature.
func TestMerge_BaseBranchEmptyWhenUnset(t *testing.T) {
	cfg := Merge(&GlobalConfig{}, &RepoConfig{})
	if cfg.BaseBranch != "" {
		t.Fatalf("BaseBranch = %q, want empty by default", cfg.BaseBranch)
	}
}

func TestMerge_BaseBranchCarriesTrustedValue(t *testing.T) {
	cfg := Merge(&GlobalConfig{}, &RepoConfig{BaseBranch: "captain/preserve-firstmate-project-touch-d78"})
	if cfg.BaseBranch != "captain/preserve-firstmate-project-touch-d78" {
		t.Fatalf("BaseBranch = %q, want the configured base", cfg.BaseBranch)
	}
}

func TestRepoConfigUnmarshalYAML_BaseBranch(t *testing.T) {
	var cfg RepoConfig
	if err := yaml.Unmarshal([]byte("base_branch: \"  captain/preserve-firstmate-project-touch-d78  \"\n"), &cfg); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if cfg.BaseBranch != "captain/preserve-firstmate-project-touch-d78" {
		t.Fatalf("BaseBranch = %q, want the trimmed value", cfg.BaseBranch)
	}

	var absent RepoConfig
	if err := yaml.Unmarshal([]byte("agent: claude\n"), &absent); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if absent.BaseBranch != "" {
		t.Fatalf("BaseBranch = %q, want empty when the key is absent", absent.BaseBranch)
	}
}

// The rejection message must name the offending value so an operator can see
// which config line stopped the run.
func TestValidateBaseBranch_ErrorNamesValue(t *testing.T) {
	err := ValidateBaseBranch("refs/heads/main")
	if err == nil {
		t.Fatal("want error")
	}
	if !strings.Contains(err.Error(), "refs/heads/main") {
		t.Fatalf("error %q does not name the rejected value", err)
	}
}

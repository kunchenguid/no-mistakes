package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// test_command_sufficient permits omission of an agent gate, so it is
// gate-control configuration: a contributor's pushed branch must not be able to
// declare its own test sufficient, and the declaration must never combine with
// a missing command into permission to skip Test.

// TestEffectiveRepoConfig_TestCommandSufficientTrustedOnly is the core security
// property: the pushed branch's value is ignored entirely, in both directions.
func TestEffectiveRepoConfig_TestCommandSufficientTrustedOnly(t *testing.T) {
	t.Run("pushed_cannot_self_declare", func(t *testing.T) {
		pushed := &RepoConfig{TestCommandSufficient: true, Commands: Commands{Test: "true"}}
		trusted := &RepoConfig{Commands: Commands{Test: "make test"}}
		got := EffectiveRepoConfig(pushed, trusted, false)
		if got.TestCommandSufficient {
			t.Fatal("a pushed branch must not be able to declare its own test sufficient")
		}
	})

	t.Run("pushed_cannot_clear_trusted_declaration", func(t *testing.T) {
		pushed := &RepoConfig{TestCommandSufficient: false}
		trusted := &RepoConfig{TestCommandSufficient: true, Commands: Commands{Test: "make test"}}
		got := EffectiveRepoConfig(pushed, trusted, false)
		if !got.TestCommandSufficient {
			t.Fatal("the trusted declaration must win over a pushed branch that omits it")
		}
	})

	t.Run("no_trusted_copy_fails_closed", func(t *testing.T) {
		pushed := &RepoConfig{TestCommandSufficient: true, Commands: Commands{Test: "true"}}
		got := EffectiveRepoConfig(pushed, nil, false)
		if got.TestCommandSufficient {
			t.Fatal("with no trusted copy the declaration must fail closed to false")
		}
	})
}

// TestEffectiveRepoConfig_TestCommandSufficientDroppedUnderRepoCommandsOptIn
// pins the rule that separates this field from the other trusted-only fields.
// Sufficiency describes a specific command and cannot outlive the trust of the
// command it describes: under allow_repo_commands, commands.test comes from the
// pushed branch, so a branch could otherwise pair a trivially passing command
// with an inherited trusted permission to skip its own evidence gate.
func TestEffectiveRepoConfig_TestCommandSufficientDroppedUnderRepoCommandsOptIn(t *testing.T) {
	pushed := &RepoConfig{Commands: Commands{Test: "true"}}
	trusted := &RepoConfig{TestCommandSufficient: true, Commands: Commands{Test: "make test"}}

	got := EffectiveRepoConfig(pushed, trusted, true)
	if got.Commands.Test != "true" {
		t.Fatalf("the opt-in should still honor the pushed command, got %q", got.Commands.Test)
	}
	if got.TestCommandSufficient {
		t.Fatal("sufficiency must be dropped when the command it describes is no longer the trusted one")
	}
}

// TestEffectiveRepoConfig_TestCommandSufficientDroppedUnderOptInWithNoTrustedCopy
// covers the same rule for a repository that ships no trusted copy at all.
func TestEffectiveRepoConfig_TestCommandSufficientDroppedUnderOptInWithNoTrustedCopy(t *testing.T) {
	pushed := &RepoConfig{TestCommandSufficient: true, Commands: Commands{Test: "true"}}
	if EffectiveRepoConfig(pushed, nil, true).TestCommandSufficient {
		t.Fatal("the opt-in must not turn a pushed declaration into an effective one")
	}
}

// TestLoadRepoConfig_TestCommandSufficientRequiresCommand is the fail-closed
// validation: the one combination that could be read as "skip Test" is rejected
// at parse time, before any agent or shell command starts.
func TestLoadRepoConfig_TestCommandSufficientRequiresCommand(t *testing.T) {
	for _, tc := range []struct {
		name string
		yaml string
	}{
		{name: "no_commands_block", yaml: "test_command_sufficient: true\n"},
		{name: "empty_command", yaml: "test_command_sufficient: true\ncommands:\n  test: \"\"\n"},
		{name: "whitespace_command", yaml: "test_command_sufficient: true\ncommands:\n  test: \"   \"\n"},
		{name: "only_other_commands", yaml: "test_command_sufficient: true\ncommands:\n  lint: \"make lint\"\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadRepoFromBytes([]byte(tc.yaml))
			if err == nil {
				t.Fatal("expected a declaration without a command to fail config parsing closed")
			}
			if !strings.Contains(err.Error(), "test_command_sufficient") {
				t.Fatalf("expected the error to name the offending field, got: %v", err)
			}
			if !strings.Contains(err.Error(), "commands.test") {
				t.Fatalf("expected the error to name what is missing, got: %v", err)
			}
		})
	}
}

// TestLoadRepoConfig_TestCommandSufficientValidCombinations keeps the validator
// from over-rejecting: the declaration with a command is valid, and its absence
// is valid with or without a command.
func TestLoadRepoConfig_TestCommandSufficientValidCombinations(t *testing.T) {
	for _, tc := range []struct {
		name           string
		yaml           string
		wantSufficient bool
		wantCommand    string
	}{
		{
			name:           "declared_with_command",
			yaml:           "test_command_sufficient: true\ncommands:\n  test: \"make test\"\n",
			wantSufficient: true,
			wantCommand:    "make test",
		},
		{
			name:        "absent_with_command",
			yaml:        "commands:\n  test: \"make test\"\n",
			wantCommand: "make test",
		},
		{name: "absent_without_command", yaml: "commands:\n  lint: \"make lint\"\n"},
		{name: "explicit_false_without_command", yaml: "test_command_sufficient: false\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := LoadRepoFromBytes([]byte(tc.yaml))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cfg.TestCommandSufficient != tc.wantSufficient {
				t.Fatalf("TestCommandSufficient = %v, want %v", cfg.TestCommandSufficient, tc.wantSufficient)
			}
			if cfg.Commands.Test != tc.wantCommand {
				t.Fatalf("commands.test = %q, want %q", cfg.Commands.Test, tc.wantCommand)
			}
		})
	}
}

// TestLoadRepoConfig_TestCommandSufficientDefaultsFalse pins backward
// compatibility at the config layer: an existing file that has never heard of
// the field resolves to today's behavior.
func TestLoadRepoConfig_TestCommandSufficientDefaultsFalse(t *testing.T) {
	cfg, err := LoadRepoFromBytes([]byte("commands:\n  test: \"make test\"\n  lint: \"make lint\"\n"))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.TestCommandSufficient {
		t.Fatal("the field must default to false so existing repositories keep the evidence agent")
	}
}

// TestMerge_TestCommandSufficientIsRepoOnly proves the resolved Config carries
// the trusted repository declaration and that an operator's global config
// cannot set it machine-wide for every repository on the host.
func TestMerge_TestCommandSufficientIsRepoOnly(t *testing.T) {
	repo := &RepoConfig{TestCommandSufficient: true, Commands: Commands{Test: "make test"}}
	if got := Merge(&GlobalConfig{}, repo); !got.TestCommandSufficient {
		t.Fatal("expected the trusted repository declaration to reach the resolved config")
	}

	// GlobalConfig has no such field. If one is ever added under the `test:`
	// block, this asserts the merge must not silently start honoring it.
	if got := Merge(&GlobalConfig{}, &RepoConfig{}); got.TestCommandSufficient {
		t.Fatal("no global setting may declare every repository's test command sufficient")
	}
}

// TestLoadGlobal_RejectsTestCommandSufficient keeps the field out of the
// operator's machine-wide config, the same way allow_repo_commands is kept out.
// Whether a command proves a change is a per-repository product judgement; a
// global default would silently apply to every repository on the host.
func TestLoadGlobal_RejectsTestCommandSufficient(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("agent: claude\ntest_command_sufficient: true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadGlobal(path); err == nil {
		t.Fatal("expected error: test_command_sufficient must be rejected in global config (it is per-repo, trusted-only)")
	}
}

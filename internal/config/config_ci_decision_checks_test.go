package config

import (
	"strings"
	"testing"
)

func TestCI_MatchesDecisionCheck(t *testing.T) {
	cfg := CI{DecisionChecks: []string{"workflow pin*", "Signed Manifest"}}
	cases := []struct {
		name  string
		check string
		want  bool
	}{
		{"exact, case-insensitive", "signed manifest", true},
		{"glob suffix", "workflow pin / verify (pull_request)", true},
		{"declared prefix alone", "workflow pin", true},
		{"unrelated check", "build", false},
		{"substring is not a match", "manifest", false},
		{"empty name", "  ", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cfg.MatchesDecisionCheck(tc.check); got != tc.want {
				t.Fatalf("MatchesDecisionCheck(%q) = %v, want %v", tc.check, got, tc.want)
			}
		})
	}
	if (CI{}).MatchesDecisionCheck("anything") {
		t.Fatal("an unconfigured repository must declare no decision checks")
	}
}

func TestMerge_CIDecisionChecksFromRepoConfig(t *testing.T) {
	repo := &RepoConfig{}
	patterns := []string{" workflow pin* ", "", "policy-attestation"}
	repo.CI.DecisionChecks = &patterns

	got := Merge(DefaultGlobalConfig(), repo).CI.DecisionChecks
	want := []string{"workflow pin*", "policy-attestation"}
	if len(got) != len(want) {
		t.Fatalf("ci.decision_checks = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("ci.decision_checks = %v, want %v", got, want)
		}
	}
	if len(Merge(DefaultGlobalConfig(), &RepoConfig{}).CI.DecisionChecks) != 0 {
		t.Fatal("ci.decision_checks must default to empty")
	}
}

// TestEffectiveRepoConfig_CIDecisionChecksTrustedOnly is the security property
// this setting exists for: a pushed branch must not be able to take a check out
// of the protected set and hand its own guard back to the fix agent.
func TestEffectiveRepoConfig_CIDecisionChecksTrustedOnly(t *testing.T) {
	none := []string{}
	trustedPatterns := []string{"workflow pin*"}
	pushed := &RepoConfig{}
	pushed.CI.DecisionChecks = &none
	trusted := &RepoConfig{}
	trusted.CI.DecisionChecks = &trustedPatterns

	effective := EffectiveRepoConfig(pushed, trusted, false)
	if got := Merge(DefaultGlobalConfig(), effective).CI.DecisionChecks; len(got) != 1 || got[0] != "workflow pin*" {
		t.Fatalf("ci.decision_checks = %v, want the trusted copy", got)
	}

	// pr.base_branch is the deliberate exception that honours allow_repo_commands.
	// A guard declaration is not: enabling the commands opt-in must not let a
	// pushed branch clear it either.
	optedIn := EffectiveRepoConfig(pushed, trusted, true)
	if got := Merge(DefaultGlobalConfig(), optedIn).CI.DecisionChecks; len(got) != 1 || got[0] != "workflow pin*" {
		t.Fatalf("ci.decision_checks with allow_repo_commands = %v, want the trusted copy", got)
	}

	// With no trusted copy at all the pushed value is dropped rather than used.
	withoutTrusted := EffectiveRepoConfig(pushed, nil, false)
	if withoutTrusted.CI.DecisionChecks != nil {
		t.Fatalf("ci.decision_checks without a trusted copy = %v, want unset", withoutTrusted.CI.DecisionChecks)
	}
}

func TestLoadRepoConfig_InvalidDecisionCheckPatternFailsClosed(t *testing.T) {
	_, err := parseRepoConfig([]byte("ci:\n  decision_checks:\n    - \"work[flow\"\n"))
	if err == nil {
		t.Fatal("an unmatchable ci.decision_checks pattern must fail the config")
	}
	if !strings.Contains(err.Error(), "decision_checks") {
		t.Fatalf("error = %v, want it to name ci.decision_checks", err)
	}
	if _, err := parseRepoConfig([]byte("ci:\n  decision_checks:\n    - \"workflow pin*\"\n")); err != nil {
		t.Fatalf("a valid pattern was rejected: %v", err)
	}
}

func TestLoadGlobal_InvalidDecisionCheckPatternFailsClosed(t *testing.T) {
	if _, err := LoadGlobalFromBytes([]byte("ci:\n  decision_checks:\n    - \"[\"\n")); err == nil {
		t.Fatal("an unmatchable global ci.decision_checks pattern must fail the config")
	}
}

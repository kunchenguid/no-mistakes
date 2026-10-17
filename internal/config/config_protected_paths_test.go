package config

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestProtectedPaths_TrustedPolicySurvivesPushedConfig(t *testing.T) {
	t.Parallel()
	trusted, err := LoadRepoFromBytes([]byte("protected_paths: [' *.lock ', '.github/**']\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, allow := range []bool{false, true} {
		for _, pushedYAML := range []string{"", "protected_paths: []", "protected_paths: ['other.json']"} {
			pushed, err := LoadRepoFromBytes([]byte(pushedYAML))
			if err != nil {
				t.Fatal(err)
			}
			got := Merge(DefaultGlobalConfig(), EffectiveRepoConfig(pushed, trusted, allow)).ProtectedPaths
			if !reflect.DeepEqual(got, []string{"*.lock", ".github/**"}) {
				t.Errorf("allow_repo_commands=%v, pushed=%q: protected_paths=%q", allow, pushedYAML, got)
			}
			if got := Merge(DefaultGlobalConfig(), EffectiveRepoConfig(pushed, nil, allow)).ProtectedPaths; len(got) != 0 {
				t.Errorf("pushed-only rule became active: %q", got)
			}
		}
	}
}

func TestProtectedPaths_InvalidRulesFailConfigLoading(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"['']", "['  ']", "['[']", "['/**']", "not-a-list"} {
		if _, err := LoadRepoFromBytes([]byte("protected_paths: " + value)); err == nil {
			t.Errorf("accepted invalid protected_paths: %s", value)
		} else if value != "not-a-list" && !strings.Contains(err.Error(), "protected_paths") {
			t.Errorf("error does not identify the setting: %v", err)
		}
	}
}

// TestCheckProtectedPathsBranchLocal_RefusesSilentlyDroppedRules is the
// fail-closed regression for #1036: a submitted branch that declares
// protected_paths entries the trusted default-branch config does not carry is
// refused, while a branch that omits the setting or only re-declares (or
// subsets) the maintainer's rules is not.
func TestCheckProtectedPathsBranchLocal_RefusesSilentlyDroppedRules(t *testing.T) {
	t.Parallel()
	trusted := mustRepoConfig(t, "protected_paths: ['*.lock', '.github/**']\n")
	none := mustRepoConfig(t, "")
	tests := []struct {
		name    string
		pushed  *RepoConfig
		trusted *RepoConfig
		wantErr bool
	}{
		{name: "nil pushed is fine", pushed: nil, trusted: trusted},
		{name: "empty pushed list is fine", pushed: none, trusted: trusted},
		{name: "pushed re-declares trusted rules", pushed: mustRepoConfig(t, "protected_paths: ['*.lock', '.github/**']\n"), trusted: trusted},
		{name: "pushed subset of trusted is fine", pushed: mustRepoConfig(t, "protected_paths: ['*.lock']\n"), trusted: trusted},
		{name: "pushed adds a rule trusted lacks", pushed: mustRepoConfig(t, "protected_paths: ['*.lock', 'tests/**']\n"), trusted: trusted, wantErr: true},
		{name: "trusted absent, pushed declares rules", pushed: mustRepoConfig(t, "protected_paths: ['tests/**']\n"), trusted: nil, wantErr: true},
		{name: "trusted empty, pushed declares rules", pushed: mustRepoConfig(t, "protected_paths: ['tests/**']\n"), trusted: none, wantErr: true},
		{name: "trusted nil, pushed empty", pushed: none, trusted: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := CheckProtectedPathsBranchLocal(tc.pushed, tc.trusted, "feature/x")
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("unexpected refusal: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected refusal, got nil")
			}
			if !errors.Is(err, ErrProtectedPathsBranchLocal) {
				t.Errorf("error %v does not wrap ErrProtectedPathsBranchLocal", err)
			}
			if !strings.Contains(err.Error(), "tests/**") {
				t.Errorf("error %q does not name the dropped entry", err)
			}
			if !strings.Contains(err.Error(), "default branch") {
				t.Errorf("error %q does not explain the trusted-only policy", err)
			}
			if !strings.Contains(err.Error(), "feature/x") {
				t.Errorf("error %q does not name the branch", err)
			}
		})
	}
}

// TestCheckProtectedPathsBranchLocal_ReportsOnlyDroppedEntries proves the
// comparison is entry-wise: a branch that re-declares a trusted rule alongside
// new ones is refused, but the error names only the entries that would be
// dropped.
func TestCheckProtectedPathsBranchLocal_ReportsOnlyDroppedEntries(t *testing.T) {
	t.Parallel()
	trusted := mustRepoConfig(t, "protected_paths: ['*.lock']\n")
	pushed := mustRepoConfig(t, "protected_paths: ['*.lock', 'tests/**', 'docs/**']\n")
	err := CheckProtectedPathsBranchLocal(pushed, trusted, "feature/x")
	if err == nil {
		t.Fatal("expected refusal, got nil")
	}
	var pple *ProtectedPathsBranchLocalError
	if !errors.As(err, &pple) {
		t.Fatalf("error %T is not *ProtectedPathsBranchLocalError", err)
	}
	if !reflect.DeepEqual(pple.Entries, []string{"tests/**", "docs/**"}) {
		t.Errorf("Entries = %q, want [tests/** docs/**] (trusted *.lock must not be reported)", pple.Entries)
	}
	if pple.Branch != "feature/x" {
		t.Errorf("Branch = %q, want feature/x", pple.Branch)
	}
	if strings.Contains(err.Error(), "*.lock") {
		t.Errorf("error %q names the honored trusted rule", err)
	}
}

// TestProtectedPaths_TrustedOnlyPolicyWithBranchAdditions states both halves of
// the #1036 posture together: a submitted branch that adds entries never changes
// the effective (trusted) set, and that same branch is refused rather than run
// with its declaration silently dropped.
func TestProtectedPaths_TrustedOnlyPolicyWithBranchAdditions(t *testing.T) {
	t.Parallel()
	trusted := mustRepoConfig(t, "protected_paths: ['*.lock']\n")
	pushed := mustRepoConfig(t, "protected_paths: ['*.lock', 'tests/**']\n")
	for _, allow := range []bool{false, true} {
		got := EffectiveRepoConfig(pushed, trusted, allow).ProtectedPaths
		if !reflect.DeepEqual(got, []string{"*.lock"}) {
			t.Errorf("allow_repo_commands=%v: effective protected_paths = %q, want trusted-only [*.lock]", allow, got)
		}
	}
	if err := CheckProtectedPathsBranchLocal(pushed, trusted, "feature"); !errors.Is(err, ErrProtectedPathsBranchLocal) {
		t.Errorf("expected refusal for the dropped entry, got %v", err)
	}
}

func mustRepoConfig(t *testing.T, yaml string) *RepoConfig {
	t.Helper()
	cfg, err := LoadRepoFromBytes([]byte(yaml))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

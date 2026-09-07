package config

import (
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

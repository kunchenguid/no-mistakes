package config

import "testing"

// A pushed branch cannot turn base attribution on for its own run: it spends
// a second suite run and executes commands on another checkout, so only the
// trusted default-branch copy decides, with or without allow_repo_commands.
func TestTestBaseAttributionTrustAndMerge(t *testing.T) {
	pushed, err := LoadRepoFromBytes([]byte("test:\n  base_attribution: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, allow := range []bool{false, true} {
		for _, trustedValue := range []bool{false, true} {
			trusted := &RepoConfig{Test: TestRaw{BaseAttribution: trustedValue}}
			got := Merge(&GlobalConfig{Test: TestRaw{BaseAttribution: true}}, EffectiveRepoConfig(pushed, trusted, allow))
			if got.Test.BaseAttribution != trustedValue {
				t.Fatalf("allow=%v trusted=%v: base_attribution=%v, want the trusted value", allow, trustedValue, got.Test.BaseAttribution)
			}
		}
		if Merge(&GlobalConfig{}, EffectiveRepoConfig(pushed, nil, allow)).Test.BaseAttribution {
			t.Fatalf("allow=%v: base_attribution honored without a trusted copy", allow)
		}
	}
	if !pushed.Test.BaseAttribution {
		t.Fatal("pushed config was mutated")
	}
	if Merge(&GlobalConfig{}, &RepoConfig{}).Test.BaseAttribution {
		t.Fatal("base attribution must default off")
	}
}

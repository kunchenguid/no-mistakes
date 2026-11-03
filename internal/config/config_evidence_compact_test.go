package config

import "testing"

func TestEvidenceCompactMerge(t *testing.T) {
	for _, tc := range []struct {
		name, global, repo string
		want               bool
	}{
		{"default", "", "", true},
		{"global opt out", "compact: false", "", false},
		{"repo opt out", "", "compact: false", false},
		{"repo restores compact", "compact: false", "compact: true", true},
		{"repo overrides global", "compact: true", "compact: false", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			global, err := LoadGlobalFromBytes([]byte("test:\n  evidence:\n    " + tc.global + "\n"))
			if err != nil {
				t.Fatal(err)
			}
			repo, err := LoadRepoFromBytes([]byte("test:\n  evidence:\n    " + tc.repo + "\n"))
			if err != nil {
				t.Fatal(err)
			}
			// Presentation is pushed-readable, unlike the evidence branch target.
			effective := EffectiveRepoConfig(repo, &RepoConfig{}, false)
			got := Merge(global, effective).Test.Evidence.Compact
			if got != tc.want {
				t.Fatalf("Compact = %v, want %v", got, tc.want)
			}
		})
	}
}

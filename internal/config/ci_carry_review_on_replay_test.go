package config

import "testing"

// ci.carry_review_on_replay is off unless asked for, the trusted repository
// value wins over the operator's in both directions, and a pushed branch
// cannot set it: it decides whether a rebased head may skip Review.
func TestCICarryReviewOnReplay_OffByDefaultAndTrustedOnly(t *testing.T) {
	t.Parallel()
	const on = "ci:\n  carry_review_on_replay: true\n"
	const off = "ci:\n  carry_review_on_replay: false\n"
	const unset = "{}\n"

	for _, tc := range []struct {
		name, global, trusted, pushed string
		want                          bool
	}{
		{"unset_everywhere", unset, unset, unset, false},
		{"shipped_default_config", defaultConfigYAML, unset, unset, false},
		{"global_on", on, unset, unset, true},
		{"trusted_on", unset, on, unset, true},
		{"trusted_off_beats_global_on", on, off, unset, false},
		{"pushed_on_is_ignored", unset, unset, on, false},
		{"pushed_off_cannot_disable_trusted_on", unset, on, off, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			global, err := LoadGlobalFromBytes([]byte(tc.global))
			if err != nil {
				t.Fatal(err)
			}
			trusted, err := LoadRepoFromBytes([]byte(tc.trusted))
			if err != nil {
				t.Fatal(err)
			}
			pushed, err := LoadRepoFromBytes([]byte(tc.pushed))
			if err != nil {
				t.Fatal(err)
			}
			if got := Merge(global, EffectiveRepoConfig(pushed, trusted, false)).CI.CarryReviewOnReplay; got != tc.want {
				t.Fatalf("CI.CarryReviewOnReplay = %v, want %v", got, tc.want)
			}
		})
	}
}

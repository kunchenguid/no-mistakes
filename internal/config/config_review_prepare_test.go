package config

import "testing"

func TestReviewPrepare_TrustedOnlyAndDefaultsOff(t *testing.T) {
	pushed, err := LoadRepoFromBytes([]byte("review:\n  prepare: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	trusted, err := LoadRepoFromBytes([]byte("review:\n  prepare: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, allow := range []bool{false, true} {
		for _, tc := range []struct {
			name    string
			pushed  *RepoConfig
			trusted *RepoConfig
			want    bool
		}{
			{"no trusted copy", pushed, nil, false},
			{"trusted copy without opt-in", pushed, &RepoConfig{}, false},
			{"trusted opt-in", &RepoConfig{}, trusted, true},
		} {
			got := Merge(&GlobalConfig{}, EffectiveRepoConfig(tc.pushed, tc.trusted, allow)).Review.Prepare
			if got != tc.want {
				t.Errorf("%s, allow_repo_commands=%v: review.prepare=%v, want %v", tc.name, allow, got, tc.want)
			}
		}
	}
	if Merge(&GlobalConfig{}, &RepoConfig{}).Review.Prepare {
		t.Fatal("review.prepare must default off")
	}
}

package config

import (
	"strings"
	"testing"
)

func TestReviewMaxFixRounds_TrustedOnlyAndDefaultsUnbounded(t *testing.T) {
	pushed, err := LoadRepoFromBytes([]byte("review:\n  max_fix_rounds: 1\n"))
	if err != nil {
		t.Fatal(err)
	}
	trusted, err := LoadRepoFromBytes([]byte("review:\n  max_fix_rounds: 2\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, allow := range []bool{false, true} {
		for _, tc := range []struct {
			trusted *RepoConfig
			want    int
		}{{nil, 0}, {&RepoConfig{}, 0}, {trusted, 2}} {
			cfg := Merge(&GlobalConfig{}, EffectiveRepoConfig(pushed, tc.trusted, allow))
			if cfg.Review.MaxFixRounds != tc.want {
				t.Errorf("allow_repo_commands=%v: max_fix_rounds=%d, want %d", allow, cfg.Review.MaxFixRounds, tc.want)
			}
		}
	}
}

func TestReviewMaxFixRounds_Validation(t *testing.T) {
	for _, value := range []string{"-1", "1.5", "many"} {
		_, err := LoadRepoFromBytes([]byte("review:\n  max_fix_rounds: " + value + "\n"))
		if err == nil {
			t.Errorf("accepted invalid max_fix_rounds %q", value)
		}
	}
	_, err := LoadRepoFromBytes([]byte("review:\n  max_fix_rounds: -1\n"))
	if err == nil || !strings.Contains(err.Error(), "review.max_fix_rounds") {
		t.Fatalf("negative limit error should identify the key, got %v", err)
	}
}

package config

import "testing"

func TestReviewNarrowAfterRound_DefaultsToTwo(t *testing.T) {
	if got := reviewDefaults().NarrowAfterRound; got != DefaultReviewNarrowAfterRound {
		t.Errorf("NarrowAfterRound = %d, want %d", got, DefaultReviewNarrowAfterRound)
	}
}

func TestReviewNarrowAfterRound_GlobalOverrides(t *testing.T) {
	tests := []struct {
		name string
		set  *int
		want int
	}{
		{"unset keeps default", nil, DefaultReviewNarrowAfterRound},
		{"explicit value", intPtr(5), 5},
		{"zero disables narrowing", intPtr(0), 0},
		{"negative disables narrowing", intPtr(-1), 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			global := &GlobalConfig{Review: ReviewRaw{NarrowAfterRound: tt.set}}
			if got := Merge(global, &RepoConfig{}).Review.NarrowAfterRound; got != tt.want {
				t.Errorf("NarrowAfterRound = %d, want %d", got, tt.want)
			}
		})
	}
}

// Review breadth steers how strict a rereview is. A contributor's pushed
// branch must not be able to loosen the review of its own change, so the
// setting is global-only: RepoConfig has no field to carry it.
func TestReviewNarrowAfterRound_IsGlobalOnly(t *testing.T) {
	cfg, err := LoadRepoFromBytes([]byte("review:\n  narrow_after_round: 99\n"))
	if err != nil {
		t.Fatalf("LoadRepoFromBytes: %v", err)
	}
	global := &GlobalConfig{}
	if got := Merge(global, cfg).Review.NarrowAfterRound; got != DefaultReviewNarrowAfterRound {
		t.Errorf("NarrowAfterRound = %d, want %d (repo config must not override)", got, DefaultReviewNarrowAfterRound)
	}
}

func intPtr(i int) *int { return &i }

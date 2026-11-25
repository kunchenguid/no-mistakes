package config

import (
	"strings"
	"testing"
)

func TestReviewMaxFixRounds_TrustedOnlyWithDefaultCap(t *testing.T) {
	pushed, err := LoadRepoFromBytes([]byte("review:\n  max_fix_rounds: 1\n"))
	if err != nil {
		t.Fatal(err)
	}
	trusted, err := LoadRepoFromBytes([]byte("review:\n  max_fix_rounds: 2\n"))
	if err != nil {
		t.Fatal(err)
	}
	// An explicit trusted 0 is the unlimited opt-out; only an ABSENT key takes
	// the default cap. A pushed value never selects the budget either way.
	unlimited, err := LoadRepoFromBytes([]byte("review:\n  max_fix_rounds: 0\n"))
	if err != nil {
		t.Fatal(err)
	}
	otherTrusted, err := LoadRepoFromBytes([]byte("review:\n  conversation: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, allow := range []bool{false, true} {
		for _, tc := range []struct {
			trusted *RepoConfig
			want    int
		}{
			{nil, DefaultReviewMaxFixRounds},
			{&RepoConfig{}, DefaultReviewMaxFixRounds},
			{otherTrusted, DefaultReviewMaxFixRounds},
			{unlimited, 0},
			{trusted, 2},
		} {
			cfg := Merge(&GlobalConfig{}, EffectiveRepoConfig(pushed, tc.trusted, allow))
			if cfg.Review.MaxFixRounds != tc.want {
				t.Errorf("allow_repo_commands=%v: max_fix_rounds=%d, want %d", allow, cfg.Review.MaxFixRounds, tc.want)
			}
		}
	}
}

// A serialized effective config must round-trip the budget it resolved: the
// absent-key sentinel marshals as null (absent), never as a negative value
// that would fail validation on replay.
func TestReviewMaxFixRounds_ProvenanceRoundTrip(t *testing.T) {
	for _, doc := range []string{"", "review:\n  max_fix_rounds: 0\n", "review:\n  max_fix_rounds: 2\n"} {
		repo, err := LoadRepoFromBytes([]byte(doc))
		if err != nil {
			t.Fatal(err)
		}
		effective := EffectiveRepoConfig(repo, repo, false)
		cfg := Merge(&GlobalConfig{}, effective)
		if err := cfg.EnableEvalProvenance(&GlobalConfig{}, effective); err != nil {
			t.Fatal(err)
		}
		replayed, err := LoadRepoFromBytes(cfg.ReplayRepoYAML)
		if err != nil {
			t.Fatalf("replay of %q failed to parse: %v", doc, err)
		}
		got := Merge(&GlobalConfig{}, EffectiveRepoConfig(replayed, replayed, false)).Review.MaxFixRounds
		if got != cfg.Review.MaxFixRounds {
			t.Errorf("doc %q: replay budget %d, want %d", doc, got, cfg.Review.MaxFixRounds)
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

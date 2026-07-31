package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
			global := &GlobalConfig{Review: GlobalReviewRaw{NarrowAfterRound: tt.set}}
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

// path_instructions is repo-owned: Merge reads it from the trusted repo config
// and never from the global one. A global copy is therefore unusable, and
// dropping it silently would leave a maintainer believing their reviewer
// guidance is in force. Strict decoding rejects it instead.
func TestGlobalConfig_RejectsReviewPathInstructions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := "review:\n  narrow_after_round: 3\n  path_instructions:\n    - path: internal/**\n      instructions: be strict\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := LoadGlobal(path)
	if err == nil {
		t.Fatal("expected LoadGlobal to reject a global review.path_instructions")
	}
	if !strings.Contains(err.Error(), "path_instructions") {
		t.Errorf("error = %q, want it to name the rejected field", err)
	}
}

// The global-only review field still loads from the same block.
func TestGlobalConfig_AcceptsReviewNarrowAfterRound(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("review:\n  narrow_after_round: 3\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadGlobal(path)
	if err != nil {
		t.Fatalf("LoadGlobal: %v", err)
	}
	if cfg.Review.NarrowAfterRound == nil || *cfg.Review.NarrowAfterRound != 3 {
		t.Errorf("NarrowAfterRound = %v, want 3", cfg.Review.NarrowAfterRound)
	}
}

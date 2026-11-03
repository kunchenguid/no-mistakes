package config

import (
	"strings"
	"testing"
)

func TestLoadGlobalRejectsFreeModelKeysInsteadOfEnablingThem(t *testing.T) {
	for _, tc := range []struct {
		key      string
		contents string
	}{
		{key: "minimax_extra_review", contents: "minimax_extra_review:\n  enabled: true\n  model: opencode-go/minimax-m3.1-flash-free\n  fallback_model: opencode-go/muse-spark\n  mode: advisory\n  steps: [review]\n  repos: [skills]\n"},
		{key: "beam_drafts", contents: "beam_drafts:\n  enabled: true\n  model: opencode-go/beam-free\n  fallback_model: opencode-go/muse-spark\n  steps: [commit, document]\n  repos: [skills]\n"},
	} {
		t.Run(tc.key, func(t *testing.T) {
			if _, err := LoadGlobalFromBytes([]byte(tc.contents)); err == nil || !strings.Contains(err.Error(), tc.key) {
				t.Fatalf("LoadGlobalFromBytes error = %v, want an unknown-field error naming %s", err, tc.key)
			}
		})
	}
}

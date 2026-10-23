package steps

import (
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
)

// The Review step's per-invocation budget is the effective, merged value: a
// trusted repository key that tightens the global limit reaches it, and a
// looser one does not.
func TestReviewAgentTimeoutUsesTrustedRepositoryLimit(t *testing.T) {
	t.Parallel()
	global := &config.GlobalConfig{ReviewAgentTimeout: 30 * time.Minute}
	for value, want := range map[string]time.Duration{"5m": 5 * time.Minute, "2h": 30 * time.Minute} {
		trusted, err := config.LoadRepoFromBytes([]byte("review:\n  agent_timeout: " + value + "\n"))
		if err != nil {
			t.Fatal(err)
		}
		cfg := config.Merge(global, config.EffectiveRepoConfig(&config.RepoConfig{}, trusted, false))
		if got := reviewAgentTimeout(cfg); got != want {
			t.Errorf("repository %s: review budget = %v, want %v", value, got, want)
		}
	}
}

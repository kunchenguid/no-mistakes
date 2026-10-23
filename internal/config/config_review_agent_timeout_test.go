package config

import (
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func reviewTimeoutRepo(t *testing.T, value string) *RepoConfig {
	t.Helper()
	repo, err := LoadRepoFromBytes([]byte("review:\n  agent_timeout: \"" + value + "\"\n"))
	if err != nil {
		t.Fatalf("LoadRepoFromBytes(%q): %v", value, err)
	}
	return repo
}

func globalWithReviewTimeout(d time.Duration) *GlobalConfig {
	return &GlobalConfig{ReviewAgentTimeout: d, ReviewAgentWorkingTimeout: 2 * time.Hour}
}

func TestReviewAgentTimeout_RepositoryTightensGlobal(t *testing.T) {
	trusted := reviewTimeoutRepo(t, "10m")
	cfg := Merge(globalWithReviewTimeout(30*time.Minute), EffectiveRepoConfig(&RepoConfig{}, trusted, false))
	if cfg.ReviewAgentTimeout != 10*time.Minute {
		t.Fatalf("review timeout = %v, want the repository's 10m", cfg.ReviewAgentTimeout)
	}
	if !cfg.AgentTimeoutConfiguredFor(types.StepReview) {
		t.Error("a tightened timeout must count as configured for Review so budget cuts are not retried")
	}
	if cfg.AgentTimeoutsConfigured {
		t.Error("the shared flag read for every step must stay false: only Review's budget changed")
	}
	for _, step := range []types.StepName{types.StepDocument, types.StepLint, types.StepTest} {
		if cfg.AgentTimeoutConfiguredFor(step) {
			t.Errorf("%s keeps its default-budget crash retry: the repository did not configure its timeout", step)
		}
	}
	if cfg.ReviewAgentWorkingTimeout != 2*time.Hour {
		t.Errorf("working cap = %v, the repository key must not touch it", cfg.ReviewAgentWorkingTimeout)
	}
	if cfg.AgentTimeout != 0 || cfg.TestAgentTimeout != 0 {
		t.Errorf("only the review budget may move, got agent=%v test=%v", cfg.AgentTimeout, cfg.TestAgentTimeout)
	}
}

func TestReviewAgentTimeout_RepositoryCannotLoosenGlobal(t *testing.T) {
	for _, repoValue := range []string{"45m", "30m"} {
		trusted := reviewTimeoutRepo(t, repoValue)
		cfg := Merge(globalWithReviewTimeout(30*time.Minute), EffectiveRepoConfig(&RepoConfig{}, trusted, false))
		if cfg.ReviewAgentTimeout != 30*time.Minute {
			t.Errorf("repository %s: review timeout = %v, want the global 30m", repoValue, cfg.ReviewAgentTimeout)
		}
		if cfg.AgentTimeoutsConfigured || cfg.ReviewAgentTimeoutTightened || cfg.AgentTimeoutConfiguredFor(types.StepReview) {
			t.Errorf("repository %s: an ignored value must not mark timeouts configured", repoValue)
		}
	}
}

func TestReviewAgentTimeout_AbsentKeepsGlobal(t *testing.T) {
	trusted, err := LoadRepoFromBytes([]byte("review:\n  conversation: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := Merge(globalWithReviewTimeout(DefaultReviewAgentTimeout), EffectiveRepoConfig(&RepoConfig{}, trusted, false))
	if cfg.ReviewAgentTimeout != DefaultReviewAgentTimeout || cfg.AgentTimeoutConfiguredFor(types.StepReview) {
		t.Fatalf("absent key changed behavior: timeout=%v configured=%v", cfg.ReviewAgentTimeout, cfg.AgentTimeoutsConfigured)
	}
}

// The timeout gates the review of the pushed branch, so the pushed copy must
// neither set it nor change the trusted value, with or without the commands
// opt-in.
func TestReviewAgentTimeout_PushedBranchCannotAlterIt(t *testing.T) {
	pushed := reviewTimeoutRepo(t, "1m")
	for _, allow := range []bool{false, true} {
		cfg := Merge(globalWithReviewTimeout(30*time.Minute), EffectiveRepoConfig(pushed, nil, allow))
		if cfg.ReviewAgentTimeout != 30*time.Minute {
			t.Errorf("allow_repo_commands=%v, no trusted copy: pushed value reached the review timeout (%v)", allow, cfg.ReviewAgentTimeout)
		}

		trusted := reviewTimeoutRepo(t, "20m")
		cfg = Merge(globalWithReviewTimeout(30*time.Minute), EffectiveRepoConfig(pushed, trusted, allow))
		if cfg.ReviewAgentTimeout != 20*time.Minute {
			t.Errorf("allow_repo_commands=%v: pushed 1m replaced the trusted 20m (%v)", allow, cfg.ReviewAgentTimeout)
		}

		trustedWithout, err := LoadRepoFromBytes([]byte("review:\n  conversation: true\n"))
		if err != nil {
			t.Fatal(err)
		}
		cfg = Merge(globalWithReviewTimeout(30*time.Minute), EffectiveRepoConfig(pushed, trustedWithout, allow))
		if cfg.ReviewAgentTimeout != 30*time.Minute {
			t.Errorf("allow_repo_commands=%v: pushed value filled in for an unset trusted key (%v)", allow, cfg.ReviewAgentTimeout)
		}
	}
}

func TestReviewAgentTimeout_RejectsUnusableValues(t *testing.T) {
	for value, want := range map[string]string{
		"0s":    "duration must be positive",
		"-5m":   "duration must be positive",
		"soon":  "review.agent_timeout",
		"10":    "review.agent_timeout",
		"1d":    "review.agent_timeout",
		"10min": "review.agent_timeout",
	} {
		_, err := LoadRepoFromBytes([]byte("review:\n  agent_timeout: \"" + value + "\"\n"))
		if err == nil {
			t.Errorf("%q: expected an error", value)
			continue
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%q: error %q does not mention %q", value, err, want)
		}
	}
}

func TestReviewAgentTimeout_HandBuiltInvalidValueNeverLoosens(t *testing.T) {
	repo := &RepoConfig{Review: ReviewRaw{AgentTimeout: "not-a-duration"}}
	cfg := Merge(globalWithReviewTimeout(30*time.Minute), repo)
	if cfg.ReviewAgentTimeout != 30*time.Minute {
		t.Fatalf("review timeout = %v, want the global 30m", cfg.ReviewAgentTimeout)
	}
}

package config

import (
	"strings"
	"testing"
)

func resolveReviewStrategy(t *testing.T, globalYAML, trustedRepoYAML, pushedRepoYAML string) string {
	t.Helper()
	global, err := LoadGlobalFromBytes([]byte(globalYAML))
	if err != nil {
		t.Fatalf("load global config: %v", err)
	}
	trusted, err := LoadRepoFromBytes([]byte(trustedRepoYAML))
	if err != nil {
		t.Fatalf("load trusted repo config: %v", err)
	}
	pushed, err := LoadRepoFromBytes([]byte(pushedRepoYAML))
	if err != nil {
		t.Fatalf("load pushed repo config: %v", err)
	}
	return Merge(global, EffectiveRepoConfig(pushed, trusted, false)).Review.Strategy
}

func TestReviewStrategy_DefaultAndPrecedence(t *testing.T) {
	t.Parallel()
	const unset = "{}\n"
	const bounded = "review:\n  strategy: bounded\n"
	const iterative = "review:\n  strategy: iterative\n"

	if got := resolveReviewStrategy(t, unset, unset, unset); got != ReviewStrategyIterative {
		t.Fatalf("default Review.Strategy = %q, want %q", got, ReviewStrategyIterative)
	}
	if got := resolveReviewStrategy(t, bounded, unset, unset); got != ReviewStrategyBounded {
		t.Fatalf("global Review.Strategy = %q, want %q", got, ReviewStrategyBounded)
	}
	if got := resolveReviewStrategy(t, bounded, iterative, unset); got != ReviewStrategyIterative {
		t.Fatalf("trusted repo Review.Strategy = %q, want %q", got, ReviewStrategyIterative)
	}
}

func TestReviewStrategy_IsTrustedOnly(t *testing.T) {
	t.Parallel()
	const unset = "{}\n"
	const bounded = "review:\n  strategy: bounded\n"
	const iterative = "review:\n  strategy: iterative\n"

	if got := resolveReviewStrategy(t, unset, bounded, iterative); got != ReviewStrategyBounded {
		t.Fatalf("Review.Strategy = %q, want trusted value %q", got, ReviewStrategyBounded)
	}
	if got := resolveReviewStrategy(t, unset, unset, bounded); got != ReviewStrategyIterative {
		t.Fatalf("Review.Strategy = %q, pushed branch selected bounded strategy", got)
	}
}

func TestReviewStrategy_RejectsUnknownValue(t *testing.T) {
	t.Parallel()
	for _, load := range []func([]byte) error{
		func(raw []byte) error { _, err := LoadRepoFromBytes(raw); return err },
		func(raw []byte) error { _, err := LoadGlobalFromBytes(raw); return err },
	} {
		err := load([]byte("review:\n  strategy: forever\n"))
		if err == nil || !strings.Contains(err.Error(), "review.strategy") {
			t.Fatalf("unknown review.strategy error = %v", err)
		}
	}
}

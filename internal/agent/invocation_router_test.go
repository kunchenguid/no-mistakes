package agent

import (
	"context"
	"errors"
	"testing"
)

func TestInvocationRouterSelectsReviewAndFixAgents(t *testing.T) {
	base := &recordingAgent{name: "pi"}
	reviewer := &recordingAgent{name: "pi"}
	fixer := &recordingAgent{name: "codex", resumable: true}
	routed := NewInvocationRouter(base, map[string]Agent{
		"review":     reviewer,
		"review-fix": fixer,
	})

	for _, purpose := range []string{"review", "review-fix", "lint"} {
		result, err := routed.Run(context.Background(), RunOpts{Purpose: purpose})
		if err != nil {
			t.Fatalf("Run(%s): %v", purpose, err)
		}
		wantProvider := "pi"
		if purpose == "review-fix" {
			wantProvider = "codex"
		}
		if result.Provider != wantProvider {
			t.Fatalf("Run(%s) provider = %q, want %q", purpose, result.Provider, wantProvider)
		}
	}
	if reviewer.runCalls != 1 || fixer.runCalls != 1 || base.runCalls != 1 {
		t.Fatalf("calls = reviewer %d fixer %d base %d, want 1/1/1", reviewer.runCalls, fixer.runCalls, base.runCalls)
	}
	if !SupportsSessionResume(routed) {
		t.Fatal("router hid a routed agent's session capability")
	}
	if err := routed.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !base.closed || !reviewer.closed || !fixer.closed {
		t.Fatalf("Close did not reach every owned agent: base=%v reviewer=%v fixer=%v", base.closed, reviewer.closed, fixer.closed)
	}
}

func TestInvocationRouterDoesNotFallbackExplicitRouteOnAuthenticationFailure(t *testing.T) {
	base := &fallbackTestAgent{
		name: "claude",
		run:  func() (*Result, error) { return &Result{Text: "fallback ran"}, nil },
	}
	routedAgent := &fallbackTestAgent{
		name: "codex",
		run:  func() (*Result, error) { return nil, errors.New("authentication required") },
	}
	routed := NewInvocationRouter(NewFallback([]Agent{base}), map[string]Agent{"review-fix": routedAgent})

	_, err := routed.Run(context.Background(), RunOpts{Purpose: "review-fix"})
	if err == nil || err.Error() != "authentication required" {
		t.Fatalf("Run(review-fix) error = %v, want the routed authentication failure", err)
	}
	if base.calls != 0 || routedAgent.calls != 1 {
		t.Fatalf("calls = default %d routed %d, want 0/1", base.calls, routedAgent.calls)
	}
}

func TestInvocationRouterAbsentRoutesReturnsDefaultUnchanged(t *testing.T) {
	base := &recordingAgent{name: "pi"}
	if got := NewInvocationRouter(base, nil); got != base {
		t.Fatalf("NewInvocationRouter(base, nil) = %T, want the original agent", got)
	}
}

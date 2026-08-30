package agent

import (
	"context"
	"testing"
)

func TestInvocationRouterSelectsReviewAndFixAgents(t *testing.T) {
	base := &recordingAgent{name: "pi"}
	reviewer := &recordingAgent{name: "pi"}
	fixer := &recordingAgent{name: "pi", resumable: true}
	routed := NewInvocationRouter(base, map[string]Agent{
		"review":     reviewer,
		"review-fix": fixer,
	})

	for _, purpose := range []string{"review", "review-fix", "lint"} {
		if _, err := routed.Run(context.Background(), RunOpts{Purpose: purpose}); err != nil {
			t.Fatalf("Run(%s): %v", purpose, err)
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

func TestInvocationRouterAbsentRoutesReturnsDefaultUnchanged(t *testing.T) {
	base := &recordingAgent{name: "pi"}
	if got := NewInvocationRouter(base, nil); got != base {
		t.Fatalf("NewInvocationRouter(base, nil) = %T, want the original agent", got)
	}
}

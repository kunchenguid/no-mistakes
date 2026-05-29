package agent

import (
	"context"
	"strings"
	"testing"
)

// recordingAgent captures the RunOpts it was invoked with.
type recordingAgent struct {
	name     string
	gotOpts  RunOpts
	runCalls int
	closed   bool
}

func (r *recordingAgent) Name() string { return r.name }

func (r *recordingAgent) Run(_ context.Context, opts RunOpts) (*Result, error) {
	r.runCalls++
	r.gotOpts = opts
	return &Result{Text: "ok"}, nil
}

func (r *recordingAgent) Close() error {
	r.closed = true
	return nil
}

func TestWithSteering_PrependsPreamble(t *testing.T) {
	inner := &recordingAgent{name: "claude"}
	steered := WithSteering(inner)

	const userPrompt = "Fix the failing test in foo_test.go"
	if _, err := steered.Run(context.Background(), RunOpts{Prompt: userPrompt, CWD: "/tmp/wt"}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !strings.HasPrefix(inner.gotOpts.Prompt, WorktreeSteering) {
		t.Errorf("prompt did not start with steering preamble:\n%q", inner.gotOpts.Prompt)
	}
	if !strings.HasSuffix(inner.gotOpts.Prompt, userPrompt) {
		t.Errorf("original prompt not preserved at end:\n%q", inner.gotOpts.Prompt)
	}
	// Other opts must pass through untouched.
	if inner.gotOpts.CWD != "/tmp/wt" {
		t.Errorf("CWD = %q, want /tmp/wt", inner.gotOpts.CWD)
	}
}

func TestWithSteering_PassesThroughNameAndClose(t *testing.T) {
	inner := &recordingAgent{name: "codex"}
	steered := WithSteering(inner)

	if steered.Name() != "codex" {
		t.Errorf("Name() = %q, want codex", steered.Name())
	}
	if err := steered.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !inner.closed {
		t.Error("Close() did not propagate to inner agent")
	}
}

func TestWithSteering_DoesNotDoubleWrap(t *testing.T) {
	inner := &recordingAgent{name: "pi"}
	once := WithSteering(inner)
	twice := WithSteering(once)

	const userPrompt = "do the thing"
	if _, err := twice.Run(context.Background(), RunOpts{Prompt: userPrompt}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := strings.Count(inner.gotOpts.Prompt, WorktreeSteering); got != 1 {
		t.Errorf("steering preamble appeared %d times, want 1:\n%q", got, inner.gotOpts.Prompt)
	}
}

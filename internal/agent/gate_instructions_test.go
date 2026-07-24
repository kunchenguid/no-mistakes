package agent

import (
	"context"
	"strings"
	"testing"
)

func TestWithGateInstructionsPrependsTrustedContext(t *testing.T) {
	inner := &recordingAgent{name: "codex", resumable: true}
	wrapped := WithGateInstructions(inner, "  Never edit generated files.  ")
	session := &SessionRef{ID: "session-1", Agent: "codex"}
	if _, err := wrapped.Run(context.Background(), RunOpts{Prompt: "Review the diff.", Session: session, Purpose: "review"}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(inner.gotOpts.Prompt, "Project gate instructions (trusted default branch):\nNever edit generated files.\n\n") {
		t.Fatalf("trusted gate instructions missing from prompt:\n%s", inner.gotOpts.Prompt)
	}
	if !strings.HasSuffix(inner.gotOpts.Prompt, "Review the diff.") {
		t.Fatalf("original prompt missing:\n%s", inner.gotOpts.Prompt)
	}
	if inner.gotOpts.Session != session || inner.gotOpts.Purpose != "review" {
		t.Fatalf("RunOpts were not preserved: %+v", inner.gotOpts)
	}
	if !SupportsSessionResume(wrapped) {
		t.Fatal("session-resume capability was hidden")
	}
}

func TestWithGateInstructionsEmptyIsNoOp(t *testing.T) {
	inner := &recordingAgent{name: "claude"}
	if got := WithGateInstructions(inner, " \n\t "); got != inner {
		t.Fatal("empty instructions must return the original agent")
	}
}

func TestWithGateInstructionsDoesNotDoubleWrap(t *testing.T) {
	inner := &recordingAgent{name: "claude"}
	once := WithGateInstructions(inner, "compact context")
	twice := WithGateInstructions(once, "compact context")
	if _, err := twice.Run(context.Background(), RunOpts{Prompt: "test"}); err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(inner.gotOpts.Prompt, "Project gate instructions"); got != 1 {
		t.Fatalf("gate instruction preamble count = %d, want 1", got)
	}
}

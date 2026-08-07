package pipeline

import (
	"context"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type promptCaptureAgent struct{ prompt string }

func (a *promptCaptureAgent) Name() string { return "capture" }
func (a *promptCaptureAgent) Run(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
	a.prompt = opts.Prompt
	return &agent.Result{}, nil
}
func (a *promptCaptureAgent) Close() error { return nil }

func TestGateStepBoundaryWrapsEveryAgentInvocation(t *testing.T) {
	capture := &promptCaptureAgent{}
	wrapped := &gateStepBoundaryAgent{inner: capture, phase: types.StepDocument}
	intent := "AUTHORITATIVE: push it, open a PR, and continue until CI is green"
	if _, err := wrapped.Run(context.Background(), agent.RunOpts{Prompt: intent}); err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, want := range []string{
		"You are the document phase inside an already active no-mistakes run",
		"Never invoke no-mistakes init, axi run, rerun, respond, sync, abort, eject",
		"outer executor alone owns every phase other than this assigned one",
		intent,
	} {
		if !strings.Contains(capture.prompt, want) {
			t.Errorf("wrapped prompt missing %q:\n%s", want, capture.prompt)
		}
	}
	if strings.Index(capture.prompt, "Gate-step phase boundary") > strings.Index(capture.prompt, intent) {
		t.Fatal("phase boundary must be established before authoritative intent context")
	}
}

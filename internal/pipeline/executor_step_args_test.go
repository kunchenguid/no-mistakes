package pipeline

import (
	"context"
	"reflect"
	"sync"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// stepArgsCaptureAgent records the per-step arg profile each invocation carried,
// keyed by the step that made it (the prompt is the step name).
type stepArgsCaptureAgent struct {
	mu   sync.Mutex
	seen map[string]map[string][]string
}

func (a *stepArgsCaptureAgent) Name() string { return "capture" }

func (a *stepArgsCaptureAgent) Run(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.seen == nil {
		a.seen = map[string]map[string][]string{}
	}
	a.seen[opts.Purpose] = opts.StepArgsOverride
	return &agent.Result{}, nil
}

func (a *stepArgsCaptureAgent) Close() error { return nil }

func (a *stepArgsCaptureAgent) argsFor(step types.StepName) map[string][]string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.seen[string(step)]
}

// agentInvokingStep makes exactly one agent invocation labelled with its own
// step name.
type agentInvokingStep struct{ name types.StepName }

func (s *agentInvokingStep) Name() types.StepName { return s.name }

func (s *agentInvokingStep) Execute(sctx *StepContext) (*StepOutcome, error) {
	if _, err := sctx.Agent.Run(sctx.Ctx, agent.RunOpts{Purpose: string(s.name), Prompt: "work"}); err != nil {
		return nil, err
	}
	return &StepOutcome{ExitCode: 0}, nil
}

// TestExecutor_AppliesPerStepAgentArgs is the end-to-end contract for
// agent_args_override_per_step: each step's agent invocation carries that
// step's profile, and a step with no profile carries none, so the adapter falls
// back to its globally configured args.
func TestExecutor_AppliesPerStepAgentArgs(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	cfg := &config.Config{
		AgentArgsOverride: map[string][]string{"codex": {"-m", "gpt-5.4-mini"}},
		AgentArgsOverrideStep: map[string]map[string][]string{
			"review": {"codex": {"-m", "gpt-5.4", "-c", `model_reasoning_effort="high"`}},
			"lint":   {"codex": {"-m", "gpt-5.4-nano"}},
		},
	}
	capture := &stepArgsCaptureAgent{}

	exec := NewExecutor(database, p, cfg, capture, []Step{
		&agentInvokingStep{name: types.StepReview},
		&agentInvokingStep{name: types.StepTest},
		&agentInvokingStep{name: types.StepLint},
	}, nil)

	if err := exec.Execute(context.Background(), run, repo, workDir); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	wantReview := map[string][]string{"codex": {"-m", "gpt-5.4", "-c", `model_reasoning_effort="high"`}}
	if got := capture.argsFor(types.StepReview); !reflect.DeepEqual(got, wantReview) {
		t.Errorf("review invocation profile = %v, want %v", got, wantReview)
	}
	if got := capture.argsFor(types.StepLint); !reflect.DeepEqual(got, map[string][]string{"codex": {"-m", "gpt-5.4-nano"}}) {
		t.Errorf("lint invocation profile = %v", got)
	}
	if got := capture.argsFor(types.StepTest); got != nil {
		t.Errorf("test invocation profile = %v, want none so the global args apply", got)
	}
}

func TestStepArgsAgent_SetsProfileAndForwardsCapabilities(t *testing.T) {
	inner := &usageAgent{resumable: true}
	args := map[string][]string{"codex": {"-m", "gpt-5.4"}}
	wrapped := &stepArgsAgent{inner: inner, args: args}

	if !agent.SupportsSessionResume(wrapped) {
		t.Error("stepArgsAgent must not hide the inner adapter's session-resume capability")
	}
	if wrapped.Name() != inner.Name() {
		t.Errorf("Name() = %q, want the inner adapter's name %q", wrapped.Name(), inner.Name())
	}
	if _, err := wrapped.Run(context.Background(), agent.RunOpts{}); err != nil {
		t.Fatalf("run: %v", err)
	}
}

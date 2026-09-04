package pipeline

import (
	"context"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
)

type reviewFixRoutingTestAgent struct {
	name  string
	calls []string
}

func (a *reviewFixRoutingTestAgent) Name() string { return a.name }
func (a *reviewFixRoutingTestAgent) Run(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
	a.calls = append(a.calls, opts.Purpose)
	return &agent.Result{Text: a.name}, nil
}
func (a *reviewFixRoutingTestAgent) Close() error { return nil }

func TestStepContextRoutesOnlyReviewFixSessionToOverride(t *testing.T) {
	reviewer := &reviewFixRoutingTestAgent{name: "claude"}
	fixer := &reviewFixRoutingTestAgent{name: "pi"}
	sctx := &StepContext{
		Ctx:            context.Background(),
		Agent:          reviewer,
		ReviewFixAgent: fixer,
		Log:            func(string) {},
	}

	if _, err := sctx.RunAgentContext(sctx.Ctx, agent.RunOpts{Purpose: "review"}); err != nil {
		t.Fatal(err)
	}
	if _, err := sctx.RunAgentSessionContext(sctx.Ctx, SessionRoleFixer, agent.RunOpts{Purpose: "review-fix"}); err != nil {
		t.Fatal(err)
	}
	if _, err := sctx.RunAgentSessionContext(sctx.Ctx, SessionRoleReviewer, agent.RunOpts{Purpose: "legacy-reviewer"}); err != nil {
		t.Fatal(err)
	}

	if len(reviewer.calls) != 2 || reviewer.calls[0] != "review" || reviewer.calls[1] != "legacy-reviewer" {
		t.Fatalf("reviewer calls = %v, want review and legacy-reviewer", reviewer.calls)
	}
	if len(fixer.calls) != 1 || fixer.calls[0] != "review-fix" {
		t.Fatalf("fixer calls = %v, want review-fix", fixer.calls)
	}
}

func TestExecutorCreatesDurableSessionsForReviewFixOverride(t *testing.T) {
	reviewer := &reviewFixRoutingTestAgent{name: "claude"}
	fixer := &reviewFixRoutingTestAgent{name: "pi"}
	selected := agent.NewReviewFixSelection(reviewer, fixer)
	exec := NewExecutor(nil, nil, &config.Config{SessionReuse: true}, selected, nil, nil)
	exec.initializeRunScopes("run-1")

	if exec.reviewFixAgent != fixer {
		t.Fatal("executor did not retain the selected Review-fixer agent")
	}
	if exec.sessions == nil || exec.sessions.agent != fixer {
		t.Fatal("durable Review-fixer sessions were not created against the fixer override")
	}
}

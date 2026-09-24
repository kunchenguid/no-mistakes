package pipeline

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// roundProbeAgent records the round every invocation arrived with.
type roundProbeAgent struct {
	name   string
	rounds []int
}

func (a *roundProbeAgent) Name() string { return a.name }
func (a *roundProbeAgent) Close() error { return nil }
func (a *roundProbeAgent) Run(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
	a.rounds = append(a.rounds, opts.Round)
	return &agent.Result{Output: json.RawMessage(`{}`)}, nil
}

// TestExecutorStampsRoundSoReviewRolesCanHandOver is the behavioral regression
// for per-round review-role routing. The executor is the only component that
// knows which round an invocation belongs to; before it stamped that round, a
// configured later-round role could never take over because every invocation
// reached the router with no round at all.
func TestExecutorStampsRoundSoReviewRolesCanHandOver(t *testing.T) {
	database, p, run, repo := setupTest(t)
	early := &roundProbeAgent{name: "early-fixer"}
	late := &roundProbeAgent{name: "late-fixer"}
	routed := agent.WithReviewRoles(&roundProbeAgent{name: "primary"}, agent.ReviewRoles{
		Fixer: agent.RoundedRole{Agent: early, Late: late, LateFrom: 2},
	})

	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			if _, err := sctx.Agent.Run(sctx.Ctx, agent.RunOpts{Prompt: "fix", Purpose: "review-fix"}); err != nil {
				return nil, err
			}
			return &StepOutcome{}, nil
		},
	}
	exec := NewExecutor(database, p, &config.Config{Agent: types.AgentClaude}, routed, []Step{step}, nil)
	if err := exec.Execute(context.Background(), run, repo, t.TempDir()); err != nil {
		t.Fatalf("execute: %v", err)
	}

	// The run's first round is round 1, so the configured first-round fixer
	// serves it and the later-round override stays idle.
	if len(early.rounds) != 1 || early.rounds[0] != 1 {
		t.Fatalf("first-round fixer rounds = %v, want [1]", early.rounds)
	}
	if len(late.rounds) != 0 {
		t.Fatalf("later-round fixer served the first round: %v", late.rounds)
	}

	// The same routed agent, stamped with a later round, hands the fix turn
	// over. This is the wrapper the executor applies, driven at the round a
	// fix loop reaches after its first gate.
	stamped := &roundStampingAgent{inner: routed, round: func() int { return 2 }}
	if _, err := stamped.Run(context.Background(), agent.RunOpts{Prompt: "fix", Purpose: "review-fix"}); err != nil {
		t.Fatal(err)
	}
	if len(late.rounds) != 1 || late.rounds[0] != 2 {
		t.Fatalf("later-round fixer rounds = %v, want [2]", late.rounds)
	}
	if len(early.rounds) != 1 {
		t.Fatalf("first-round fixer served round 2: %v", early.rounds)
	}

	// A caller that already numbered its own invocation keeps that number.
	if _, err := stamped.Run(context.Background(), agent.RunOpts{Prompt: "fix", Purpose: "review-fix", Round: 1}); err != nil {
		t.Fatal(err)
	}
	if len(early.rounds) != 2 || early.rounds[1] != 1 {
		t.Fatalf("caller-supplied round was overwritten: %v", early.rounds)
	}
}

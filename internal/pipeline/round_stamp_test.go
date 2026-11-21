package pipeline

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type roundProbeAgent struct {
	rounds []int
}

func (a *roundProbeAgent) Name() string { return "omp" }
func (a *roundProbeAgent) Close() error { return nil }
func (a *roundProbeAgent) Run(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
	a.rounds = append(a.rounds, opts.Round)
	return &agent.Result{Output: json.RawMessage(`{}`)}, nil
}

// TestExecutorStampsRoundForTelemetry pins the one remaining meaning of
// RunOpts.Round: the executor numbers the review round so local telemetry can
// attribute an invocation without agent-role routing.
func TestExecutorStampsRoundForTelemetry(t *testing.T) {
	database, p, run, repo := setupTest(t)
	probe := &roundProbeAgent{}
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			if _, err := sctx.Agent.Run(sctx.Ctx, agent.RunOpts{Prompt: "review", Purpose: "review"}); err != nil {
				return nil, err
			}
			return &StepOutcome{}, nil
		},
	}
	exec := NewExecutor(database, p, &config.Config{Agent: types.AgentOMP}, probe, []Step{step}, nil)
	if err := exec.Execute(context.Background(), run, repo, t.TempDir()); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(probe.rounds) != 1 || probe.rounds[0] != 1 {
		t.Fatalf("rounds = %v, want [1]", probe.rounds)
	}
}

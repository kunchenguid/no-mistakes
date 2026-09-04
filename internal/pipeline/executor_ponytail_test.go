package pipeline

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type phasePonytailAgent struct {
	handoffs int
	work     int
	prompts  []string
}

func (a *phasePonytailAgent) Name() string { return "phase-agent" }

func (a *phasePonytailAgent) Close() error { return nil }

func (a *phasePonytailAgent) Run(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
	a.prompts = append(a.prompts, opts.Prompt)
	if opts.Purpose == "ponytail-handoff" {
		a.handoffs++
		var schema struct {
			Properties map[string]struct {
				Enum []any `json:"enum"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(opts.JSONSchema, &schema); err != nil {
			return nil, err
		}
		challenge, _ := schema.Properties["challenge"].Enum[0].(string)
		output, _ := json.Marshal(map[string]any{
			"protocol": agent.PonytailHandoffProtocol,
			"mode":     "full", "challenge": challenge, "acknowledged": true,
		})
		return &agent.Result{Output: output}, nil
	}
	a.work++
	return &agent.Result{Text: "done"}, nil
}

func TestExecutor_RequiredPonytailCoversEveryOwnedAgentPhase(t *testing.T) {
	for _, phase := range types.AllSteps() {
		t.Run(string(phase), func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			run.PonytailRequired = true
			ag := &phasePonytailAgent{}
			step := &adaptiveCallStep{name: phase, fn: func(sctx *StepContext) (*StepOutcome, error) {
				_, err := sctx.Agent.Run(sctx.Ctx, agent.RunOpts{Prompt: "PRIVATE_PHASE_TASK", Purpose: "phase-" + string(phase)})
				return &StepOutcome{}, err
			}}
			if err := NewExecutor(database, p, nil, ag, []Step{step}, nil).Execute(context.Background(), run, repo, t.TempDir()); err != nil {
				t.Fatal(err)
			}
			if ag.handoffs != 1 || ag.work != 1 || len(ag.prompts) != 2 {
				t.Fatalf("handoffs/work/prompts = %d/%d/%d, want 1/1/2", ag.handoffs, ag.work, len(ag.prompts))
			}
			if strings.Contains(ag.prompts[0], "PRIVATE_PHASE_TASK") || !strings.Contains(ag.prompts[1], "Ponytail full operating context") {
				t.Fatalf("phase %s did not acknowledge before receiving project work: %#v", phase, ag.prompts)
			}
		})
	}
}

func TestExecutor_PonytailRemainsOptIn(t *testing.T) {
	database, p, run, repo := setupTest(t)
	ag := &phasePonytailAgent{}
	step := &adaptiveCallStep{name: types.StepTest, fn: func(sctx *StepContext) (*StepOutcome, error) {
		_, err := sctx.Agent.Run(sctx.Ctx, agent.RunOpts{Prompt: "project work"})
		return &StepOutcome{}, err
	}}
	if err := NewExecutor(database, p, nil, ag, []Step{step}, nil).Execute(context.Background(), run, repo, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if ag.handoffs != 0 || ag.work != 1 {
		t.Fatalf("handoffs/work = %d/%d, want 0/1", ag.handoffs, ag.work)
	}
}

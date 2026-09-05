package agent

import (
	"context"
	"errors"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// stageEffortAgent owns immutable adapter selections. Each uses the original
// raw flags and same model, executable, environment and permissions. No Run
// mutates an adapter or profile, including concurrent invocations and resumes.
// This lives inside each fallback provider, so fallback keeps the same duty.
type stageEffortAgent struct {
	Agent
	stages map[string]Agent
}

func newStageEffortAgent(name types.AgentName, bin string, raw []string, opts Options) (Agent, error) {
	stages := opts.StageEfforts
	if err := agentcfg.ValidateStageEfforts(name, stages); err != nil {
		return nil, err
	}
	opts.StageEfforts = nil
	base, err := NewWithOptions(name, bin, raw, opts)
	if err != nil {
		return nil, err
	}
	if openCode, ok := base.(*opencodeAgent); ok {
		openCode.stageEfforts = make(agentcfg.StageEfforts, len(stages))
		for stage, effort := range stages {
			openCode.stageEfforts[stage] = effort
		}
		return openCode, nil
	}
	a := &stageEffortAgent{Agent: base, stages: make(map[string]Agent, len(stages))}
	for stage, effort := range stages {
		selection := opts
		selection.Profile.Effort = effort
		next, err := NewWithOptions(name, bin, raw, selection)
		if err != nil {
			_ = a.Close()
			return nil, err
		}
		a.stages[stage] = next
	}
	return a, nil
}

func (a *stageEffortAgent) Run(ctx context.Context, opts RunOpts) (*Result, error) {
	selected := a.Agent
	if stage := a.stages[agentcfg.EffortStage(opts.Purpose)]; stage != nil {
		selected = stage
	}
	return selected.Run(ctx, opts)
}

func (a *stageEffortAgent) Close() error {
	errs := []error{a.Agent.Close()}
	for _, stage := range a.stages {
		errs = append(errs, stage.Close())
	}
	return errors.Join(errs...)
}

// Every selection is the same adapter with only effort changed; forward its
// optional capabilities rather than accidentally disabling sessions or guards.
func (a *stageEffortAgent) SupportsSessionResume() bool { return SupportsSessionResume(a.Agent) }
func (a *stageEffortAgent) SupportsSessionProvider(provider string) bool {
	return SupportsSessionProvider(a.Agent, provider)
}
func (a *stageEffortAgent) ReportsAgentAttempts() bool { return ReportsAgentAttempts(a.Agent) }
func (a *stageEffortAgent) NeutralizesGateInstructions() bool {
	return NeutralizesGateInstructions(a.Agent)
}

package pipeline

import (
	"context"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/db"
)

type callerRestrictionsAgent struct {
	inner agent.Agent
	run   *db.Run
}

func bindCallerRestrictions(ag agent.Agent, run *db.Run) agent.Agent {
	if ag == nil || run == nil {
		return ag
	}
	if bound, ok := ag.(*callerRestrictionsAgent); ok && bound.run == run {
		return ag
	}
	return &callerRestrictionsAgent{inner: ag, run: run}
}

func (a *callerRestrictionsAgent) Name() string { return a.inner.Name() }

func (a *callerRestrictionsAgent) Run(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
	r := a.run
	if r.CallerRestrictions != nil {
		if err := r.CallerRestrictions.Validate(); err != nil {
			return nil, err
		}
		submitted := ""
		if r.SubmittedHeadSHA != nil {
			submitted = *r.SubmittedHeadSHA
		}
		opts.Prompt += r.CallerRestrictions.Prompt(r.ID, r.RepoID, r.Branch, submitted, r.HeadSHA)
	}
	return a.inner.Run(ctx, opts)
}

func (a *callerRestrictionsAgent) Close() error { return a.inner.Close() }

func (a *callerRestrictionsAgent) SupportsSessionResume() bool {
	return agent.SupportsSessionResume(a.inner)
}

func (a *callerRestrictionsAgent) SupportsSessionProvider(provider string) bool {
	return agent.SupportsSessionProvider(a.inner, provider)
}

func (a *callerRestrictionsAgent) ReportsAgentAttempts() bool {
	return agent.ReportsAgentAttempts(a.inner)
}

func (a *callerRestrictionsAgent) NeutralizesGateInstructions() bool {
	return agent.NeutralizesGateInstructions(a.inner)
}

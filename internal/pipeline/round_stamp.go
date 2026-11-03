package pipeline

import (
	"context"

	"github.com/kunchenguid/no-mistakes/internal/agent"
)

// roundStampingAgent records the round each invocation belongs to on the
// invocation itself, so a decorator further in (the review-role router) can
// honor an operator-configured later-round role override without re-deriving
// the round from the database. It reads the same round closure the local
// performance recorder uses, so routing and evidence cannot disagree about
// which round an invocation served. A caller that already set a round keeps
// it.
type roundStampingAgent struct {
	inner agent.Agent
	// round returns the 1-based round the current invocation belongs to.
	round func() int
}

func (a *roundStampingAgent) Name() string { return a.inner.Name() }

func (a *roundStampingAgent) Close() error { return a.inner.Close() }

func (a *roundStampingAgent) SupportsSessionResume() bool {
	return agent.SupportsSessionResume(a.inner)
}

func (a *roundStampingAgent) SupportsSessionProvider(provider string) bool {
	return agent.SupportsSessionProvider(a.inner, provider)
}

func (a *roundStampingAgent) ReportsAgentAttempts() bool {
	return agent.ReportsAgentAttempts(a.inner)
}

func (a *roundStampingAgent) NeutralizesGateInstructions() bool {
	return agent.NeutralizesGateInstructions(a.inner)
}

func (a *roundStampingAgent) Run(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
	if opts.Round == 0 && a.round != nil {
		opts.Round = a.round()
	}
	return a.inner.Run(ctx, opts)
}

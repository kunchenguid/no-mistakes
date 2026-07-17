package agent

import "context"

func authorizeLaunch(ctx context.Context, opts RunOpts) error {
	if opts.AuthorizeLaunch == nil {
		return nil
	}
	return opts.AuthorizeLaunch(ctx)
}

type launchAuthorizedAgent struct {
	Agent
	authorize func(context.Context) error
}

func (a launchAuthorizedAgent) Run(ctx context.Context, opts RunOpts) (*Result, error) {
	opts.AuthorizeLaunch = a.authorize
	return a.Agent.Run(ctx, opts)
}

func (a launchAuthorizedAgent) SupportsSessionResume() bool {
	return SupportsSessionResume(a.Agent)
}

func (a launchAuthorizedAgent) SupportsSessionProvider(provider string) bool {
	return SupportsSessionProvider(a.Agent, provider)
}

func (a launchAuthorizedAgent) ReportsAgentAttempts() bool {
	return ReportsAgentAttempts(a.Agent)
}

func (a launchAuthorizedAgent) NeutralizesGateInstructions() bool {
	return NeutralizesGateInstructions(a.Agent)
}

// WithLaunchAuthorization injects a fresh authorization call into every
// concrete adapter attempt. Adapter retries and fallback providers receive the
// callback in RunOpts, so each subprocess launch reauthorizes independently.
func WithLaunchAuthorization(a Agent, authorize func(context.Context) error) Agent {
	if a == nil || authorize == nil {
		return a
	}
	return launchAuthorizedAgent{Agent: a, authorize: authorize}
}

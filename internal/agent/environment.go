package agent

import "context"

// environmentAgent installs an exact repository-scoped child environment on
// every concrete adapter invocation. Capability methods are forwarded so this
// decorator does not disable sessions, attempt reporting, or gate hardening.
type environmentAgent struct {
	Agent
	env []string
}

func (a environmentAgent) Run(ctx context.Context, opts RunOpts) (*Result, error) {
	opts.Env = append([]string(nil), a.env...)
	return a.Agent.Run(ctx, opts)
}

func (a environmentAgent) SupportsSessionResume() bool {
	return SupportsSessionResume(a.Agent)
}

func (a environmentAgent) SupportsSessionProvider(provider string) bool {
	return SupportsSessionProvider(a.Agent, provider)
}

func (a environmentAgent) ReportsAgentAttempts() bool {
	return ReportsAgentAttempts(a.Agent)
}

func (a environmentAgent) NeutralizesGateInstructions() bool {
	return NeutralizesGateInstructions(a.Agent)
}

// WithEnvironment wraps a pipeline agent with one repository's exact child
// environment. A nil environment preserves legacy behavior.
func WithEnvironment(a Agent, env []string) Agent {
	if a == nil || len(env) == 0 {
		return a
	}
	return environmentAgent{Agent: a, env: append([]string(nil), env...)}
}

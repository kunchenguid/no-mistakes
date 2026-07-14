package agent

import (
	"context"
	"strings"
)

// gateInstructionsAgent prepends trusted compact project context to every
// invocation while forwarding all optional adapter capabilities.
type gateInstructionsAgent struct {
	Agent
	instructions string
}

func (g gateInstructionsAgent) Run(ctx context.Context, opts RunOpts) (*Result, error) {
	opts.Prompt = "Project gate instructions (trusted default branch):\n" + g.instructions + "\n\n" + opts.Prompt
	return g.Agent.Run(ctx, opts)
}

func (g gateInstructionsAgent) SupportsSessionResume() bool {
	return SupportsSessionResume(g.Agent)
}

func (g gateInstructionsAgent) SupportsSessionProvider(provider string) bool {
	return SupportsSessionProvider(g.Agent, provider)
}

func (g gateInstructionsAgent) ReportsAgentAttempts() bool {
	return ReportsAgentAttempts(g.Agent)
}

func (g gateInstructionsAgent) NeutralizesGateInstructions() bool {
	return NeutralizesGateInstructions(g.Agent)
}

// WithGateInstructions adds compact trusted project context to every agent
// invocation. Empty instructions are a no-op.
func WithGateInstructions(a Agent, instructions string) Agent {
	if a == nil || strings.TrimSpace(instructions) == "" {
		return a
	}
	if _, ok := a.(gateInstructionsAgent); ok {
		return a
	}
	return gateInstructionsAgent{Agent: a, instructions: strings.TrimSpace(instructions)}
}

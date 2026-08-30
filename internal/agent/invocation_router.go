package agent

import (
	"context"
	"fmt"
	"strings"
)

// invocationRouter selects an explicitly configured agent for an invocation
// purpose. The default remains authoritative for an empty or unconfigured
// purpose, which keeps all pre-existing configurations and call sites
// unchanged.
type invocationRouter struct {
	defaultAgent Agent
	routes       map[string]Agent
}

// NewInvocationRouter adds purpose-specific routes to defaultAgent. Each route
// is owned by the returned agent and is closed with it. Callers must not put the
// same Agent instance under more than one key.
func NewInvocationRouter(defaultAgent Agent, routes map[string]Agent) Agent {
	if defaultAgent == nil || len(routes) == 0 {
		return defaultAgent
	}
	copied := make(map[string]Agent, len(routes))
	for purpose, routed := range routes {
		if strings.TrimSpace(purpose) != "" && routed != nil {
			copied[purpose] = routed
		}
	}
	if len(copied) == 0 {
		return defaultAgent
	}
	return &invocationRouter{defaultAgent: defaultAgent, routes: copied}
}

func (a *invocationRouter) Name() string { return a.defaultAgent.Name() }

func (a *invocationRouter) Run(ctx context.Context, opts RunOpts) (*Result, error) {
	selected := a.defaultAgent
	if routed := a.routes[opts.Purpose]; routed != nil {
		selected = routed
	}
	result, err := selected.Run(ctx, opts)
	if err == nil && result != nil && result.Provider == "" {
		result.Provider = selected.Name()
	}
	return result, err
}

func (a *invocationRouter) SupportsSessionResume() bool {
	if SupportsSessionResume(a.defaultAgent) {
		return true
	}
	for _, routed := range a.routes {
		if SupportsSessionResume(routed) {
			return true
		}
	}
	return false
}

func (a *invocationRouter) SupportsSessionProvider(provider string) bool {
	if SupportsSessionProvider(a.defaultAgent, provider) {
		return true
	}
	for _, routed := range a.routes {
		if SupportsSessionProvider(routed, provider) {
			return true
		}
	}
	return false
}

func (a *invocationRouter) NeutralizesGateInstructions() bool {
	if !NeutralizesGateInstructions(a.defaultAgent) {
		return false
	}
	for _, routed := range a.routes {
		if !NeutralizesGateInstructions(routed) {
			return false
		}
	}
	return true
}

func (a *invocationRouter) Close() error {
	var errs []string
	if err := a.defaultAgent.Close(); err != nil {
		errs = append(errs, fmt.Sprintf("default: %v", err))
	}
	for purpose, routed := range a.routes {
		if err := routed.Close(); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", purpose, err))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("close invocation-routed agents: %s", strings.Join(errs, "; "))
	}
	return nil
}

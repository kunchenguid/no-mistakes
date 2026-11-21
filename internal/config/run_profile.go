package config

import (
	"fmt"
	"maps"
	"slices"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// ResolveRunProfile resolves an opt-in omp model/effort pin. Native selection
// overrides are ambiguous with a run pin and are refused, not silently given
// precedence.
func (c *GlobalConfig) ResolveRunProfile(request *agentcfg.RunProfile) (*agentcfg.RunProfile, error) {
	if request == nil {
		return nil, nil
	}
	if err := request.ValidateRequest(); err != nil {
		return nil, err
	}
	raw := c.AgentArgsOverride[string(types.AgentOMP)]
	if slices.Contains(raw, "--") {
		return nil, fmt.Errorf("run profile conflicts with '--' in agent_args_override.omp")
	}
	if _, found := agentcfg.SelectionArgs(raw); found {
		return nil, fmt.Errorf("run profile conflicts with native selection flags in agent_args_override.omp; use agent_config.omp defaults instead")
	}
	defaults := c.AgentConfig[string(types.AgentOMP)]
	resolved := *request
	if resolved.Model == "" {
		resolved.Model = defaults.Model
	}
	if resolved.Effort == "" {
		resolved.Effort = defaults.Effort
	}
	if err := resolved.Validate(); err != nil {
		return nil, err
	}
	return &resolved, nil
}

// ValidateRunProfileAgents runs after trusted repository config is merged. The
// single supported harness must be omp.
func (c *Config) ValidateRunProfileAgents() error {
	if c.Agent != "" && c.Agent != types.AgentOMP {
		return fmt.Errorf("run profile requires agent: omp, got %q", c.Agent)
	}
	for _, name := range c.Agents {
		if name != types.AgentOMP {
			return fmt.Errorf("run profile requires agent: omp without other harnesses, got %q", name)
		}
	}
	return nil
}

// ApplyRunProfile uses fresh maps/slices: no global or concurrent run can be
// mutated. The run pin outranks later global model/effort selections. All other
// configuration, including trusted instruction suppression, stays live.
func (c *Config) ApplyRunProfile(pin *agentcfg.RunProfile) error {
	if pin == nil {
		return nil
	}
	if err := pin.Validate(); err != nil {
		return err
	}
	raw := c.AgentArgsFor(types.AgentOMP)
	if slices.Contains(raw, "--") {
		return fmt.Errorf("cannot enforce run pin after '--' in agent_args_override.omp")
	}
	c.Agent = types.AgentOMP
	c.Agents = []types.AgentName{types.AgentOMP}
	c.AgentConfig = map[string]agentcfg.Profile{
		string(types.AgentOMP): {Model: pin.Model, Effort: pin.Effort},
	}
	selection, _ := agentcfg.SelectionArgs(raw)
	c.AgentArgsOverride = maps.Clone(c.AgentArgsOverride)
	if c.AgentArgsOverride == nil {
		c.AgentArgsOverride = make(map[string][]string)
	}
	if len(selection) == 0 {
		delete(c.AgentArgsOverride, string(types.AgentOMP))
	} else {
		c.AgentArgsOverride[string(types.AgentOMP)] = selection
	}
	return nil
}

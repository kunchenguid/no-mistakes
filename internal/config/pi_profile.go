package config

import (
	"fmt"
	"maps"
	"slices"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// ResolvePiProfile resolves only opt-in requests. Native selection overrides
// are ambiguous with a run pin and are refused, not silently given precedence.
// Global agent and review_agents must already be Pi-only so a mixed harness is
// refused before any active run is superseded. Trusted default-branch agent
// selection is a separate pre-cancel check in the daemon.
func (c *GlobalConfig) ResolvePiProfile(request *agentcfg.PiProfile) (*agentcfg.PiProfile, error) {
	if request == nil {
		return nil, nil
	}
	if err := request.ValidateRequest(); err != nil {
		return nil, err
	}
	if err := validatePiProfileAgents(c.Agent, c.Agents, c.ReviewAgents); err != nil {
		return nil, err
	}
	if slices.Contains(c.AgentArgsOverride["pi"], "--") {
		return nil, fmt.Errorf("Pi run profile conflicts with '--' in agent_args_override.pi")
	}
	if _, found := agentcfg.PiSelectionArgs(c.AgentArgsOverride["pi"]); found {
		return nil, fmt.Errorf("Pi run profile conflicts with native selection flags in agent_args_override.pi; use agent_config.pi defaults instead")
	}
	defaults := c.AgentConfig["pi"]
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

// ValidatePiProfileAgents runs after trusted repository config is merged. A
// single-model pin must not silently drop a configured non-Pi reviewer/fallback.
func (c *Config) ValidatePiProfileAgents() error {
	return validatePiProfileAgents(c.Agent, c.Agents, c.ReviewAgents)
}

func validatePiProfileAgents(agent types.AgentName, agents []types.AgentName, reviewAgents map[string]ReviewAgent) error {
	names := agents
	if len(names) == 0 {
		names = []types.AgentName{agent}
	}
	for _, name := range names {
		if name != types.AgentPi {
			return fmt.Errorf("Pi run profile requires agent: pi without non-Pi fallbacks")
		}
	}
	for _, entry := range reviewAgents {
		if entry.Agent != types.AgentPi {
			return fmt.Errorf("Pi run profile conflicts with non-Pi review_agents")
		}
	}
	return nil
}

// ApplyPiProfile uses fresh maps/slices: no global or concurrent run can be
// mutated. The run pin outranks role profiles and later global selections.
// All other configuration, including trusted instruction suppression, stays live.
func (c *Config) ApplyPiProfile(pin *agentcfg.PiProfile) error {
	if pin == nil {
		return nil
	}
	if err := pin.Validate(); err != nil {
		return err
	}
	if slices.Contains(c.AgentArgsFor(types.AgentPi), "--") {
		return fmt.Errorf("cannot enforce Pi run pin after '--' in agent_args_override.pi")
	}
	c.Agent = types.AgentPi
	c.Agents = []types.AgentName{types.AgentPi}
	c.ReviewAgents = nil // both roles now use the same pinned primary
	c.AgentConfig = map[string]agentcfg.Profile{"pi": {Model: pin.Model, Effort: pin.Effort}}
	args := pin.PinnedBaseArgs(c.AgentArgsFor(types.AgentPi))
	c.AgentArgsOverride = maps.Clone(c.AgentArgsOverride)
	if c.AgentArgsOverride == nil {
		c.AgentArgsOverride = make(map[string][]string)
	}
	c.AgentArgsOverride["pi"] = args
	return nil
}

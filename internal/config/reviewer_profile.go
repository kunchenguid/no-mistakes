package config

import (
	"fmt"
	"maps"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
)

func (c *GlobalConfig) ResolveReviewerProfile(request *agentcfg.ReviewerProfile) (*agentcfg.ReviewerProfile, error) {
	if request == nil {
		return nil, nil
	}
	if err := request.ValidateRequest(); err != nil {
		return nil, err
	}
	// Raw arguments can name config files or native model selectors. A pinned
	// reviewer accepts only the typed profile rather than claiming precedence
	// over arbitrary harness configuration.
	if len(c.AgentArgsOverride[string(request.Agent)]) != 0 {
		return nil, fmt.Errorf("reviewer selection conflicts with agent_args_override for its harness; use agent_config defaults")
	}
	resolved := *request
	defaults := agentSettingFor(c.AgentConfig, request.Agent)
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

// ApplyReviewerProfile copies the role map. Concurrent runs and the fixer keep
// their own selections. The pin also removes later-round reviewer routing.
func (c *Config) ApplyReviewerProfile(pin *agentcfg.ReviewerProfile) error {
	if err := pin.Validate(); err != nil || pin == nil {
		return err
	}
	c.ReviewAgents = maps.Clone(c.ReviewAgents)
	if c.ReviewAgents == nil {
		c.ReviewAgents = make(map[string]ReviewAgent)
	}
	c.ReviewAgents[RoleReviewer] = ReviewAgent{Agent: pin.Agent, Model: pin.Model, Effort: pin.Effort, Pinned: true}
	delete(c.ReviewAgents, RoleReviewerAfterRound)
	return nil
}

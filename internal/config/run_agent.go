package config

import (
	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// ApplyRunAgent changes only the model-backed roles of this run's merged config.
func (c *Config) ApplyRunAgent(selection agentcfg.RunAgent) error {
	if err := selection.Validate(); err != nil {
		return err
	}
	if selection == agentcfg.RunAgentCodex {
		c.Agent = types.AgentCodex
		c.Agents = []types.AgentName{types.AgentCodex}
		c.ReviewAgents = nil
		c.QuotaRouting.Enabled = false
	}
	return nil
}

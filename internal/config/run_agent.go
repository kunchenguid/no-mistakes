package config

import (
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// ApplyRunAgent changes only this run's routing. Models, executable paths,
// credentials, and trusted instruction suppression still come from their owners.
func (c *Config) ApplyRunAgent(name types.AgentName, pi *agentcfg.PiProfile) error {
	if err := agentcfg.ValidateRunAgent(name); err != nil {
		return err
	}
	if name == "" {
		return nil
	}
	if pi != nil && name != types.AgentPi {
		return fmt.Errorf("--agent conflicts with the run's Pi model profile")
	}
	c.Agent = name
	c.Agents = []types.AgentName{name}
	c.ReviewAgents = nil
	c.QuotaRouting.Enabled = false
	return nil
}

package config

import (
	"encoding/json"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const reviewFixRunConfigVersion = 1

type reviewFixRunConfig struct {
	Version  int                                `json:"version"`
	Agents   []types.AgentName                  `json:"agents,omitempty"`
	Profiles map[string]reviewFixRunProfileJSON `json:"profiles,omitempty"`
}

type reviewFixRunProfileJSON struct {
	Model  string          `json:"model,omitempty"`
	Effort agentcfg.Effort `json:"effort,omitempty"`
	Fast   bool            `json:"fast,omitempty"`
}

// HasReviewFixProfileFor reports whether any selected agent has a role-specific
// profile. Profiles for unselected agents stay inert, just like ordinary
// agent_config entries.
func (c *Config) HasReviewFixProfileFor(names []types.AgentName) bool {
	if c == nil || c.ReviewFixAgentConfig == nil {
		return false
	}
	for _, name := range names {
		if _, ok := c.ReviewFixAgentConfig[string(name)]; ok {
			return true
		}
	}
	return false
}

// ReviewFixAgentsForRun returns the effective fixer candidates before agent
// availability filtering. An absent selector follows the effective agent.
func (c *Config) ReviewFixAgentsForRun() []types.AgentName {
	if c == nil {
		return nil
	}
	if c.HasReviewFixAgentOverride() {
		if len(c.ReviewFixAgents) > 0 {
			return copyAgents(c.ReviewFixAgents)
		}
		return []types.AgentName{c.ReviewFixAgent}
	}
	if len(c.Agents) > 0 {
		return copyAgents(c.Agents)
	}
	return []types.AgentName{c.Agent}
}

// MarshalReviewFixRunConfig snapshots the resolved, global-only fixer routing
// onto a new run.
func (c *Config) MarshalReviewFixRunConfig() (string, error) {
	if c == nil {
		return "", fmt.Errorf("snapshot review fixer: configuration is missing")
	}
	names := c.ReviewFixAgentsForRun()
	snapshot := reviewFixRunConfig{
		Version:  reviewFixRunConfigVersion,
		Agents:   names,
		Profiles: make(map[string]reviewFixRunProfileJSON, len(names)),
	}
	for _, name := range names {
		profile, _ := c.ReviewFixProfileFor(name)
		snapshot.Profiles[string(name)] = reviewFixRunProfileJSON{
			Model:  profile.Profile.Model,
			Effort: profile.Profile.Effort,
			Fast:   profile.Fast,
		}
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return "", fmt.Errorf("snapshot review fixer: %w", err)
	}
	return string(encoded), nil
}

// ApplyReviewFixRunConfig restores the role routing recorded when a run was
// created. A nil database column identifies a run created before snapshots and
// deliberately retains the legacy recovery behavior.
func (c *Config) ApplyReviewFixRunConfig(encoded string) error {
	if c == nil {
		return fmt.Errorf("restore review fixer: configuration is missing")
	}
	var snapshot reviewFixRunConfig
	if err := json.Unmarshal([]byte(encoded), &snapshot); err != nil {
		return fmt.Errorf("restore review fixer: invalid stored configuration: %w", err)
	}
	if snapshot.Version != reviewFixRunConfigVersion {
		return fmt.Errorf("restore review fixer: unsupported stored version %d", snapshot.Version)
	}
	c.ReviewFixAgent = ""
	c.ReviewFixAgents = nil
	c.ReviewFixAgentConfig = nil
	if len(snapshot.Agents) == 0 {
		return fmt.Errorf("restore review fixer: stored agent list is empty")
	}
	c.ReviewFixAgents = copyAgents(snapshot.Agents)
	c.ReviewFixAgent = firstAgent(c.ReviewFixAgents)
	if len(snapshot.Profiles) == 0 {
		return nil
	}
	c.ReviewFixAgentConfig = make(map[string]ReviewFixProfile, len(snapshot.Profiles))
	for name, stored := range snapshot.Profiles {
		agentName := types.AgentName(name)
		profile := ReviewFixProfile{
			Profile: agentcfg.Profile{Model: stored.Model, Effort: stored.Effort},
			Fast:    stored.Fast,
		}
		if err := agentcfg.Validate(agentName, profile.Profile); err != nil {
			return fmt.Errorf("restore review fixer %s: %w", name, err)
		}
		if err := validateReviewFixProfile(agentName, profile); err != nil {
			return fmt.Errorf("restore review fixer %s: %w", name, err)
		}
		c.ReviewFixAgentConfig[name] = profile
	}
	return nil
}

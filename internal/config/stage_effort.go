package config

import "github.com/kunchenguid/no-mistakes/internal/agentcfg"

// CombineHousekeeping reports whether a single document/lint invocation can
// honor both selections for every resolved fallback provider. This compares
// effective effort, not YAML spelling, so legacy native flags keep precedence.
func (c *Config) CombineHousekeeping() bool {
	if c.Commands.Lint != "" {
		return false
	}
	names := c.Agents
	if len(names) == 0 {
		names = append(names, c.Agent)
	}
	for _, name := range names {
		if !agentcfg.SameStageEffort(name, c.AgentProfileFor(name), c.StageEffort[string(name)], c.AgentArgsFor(name), "document", "lint") {
			return false
		}
	}
	return true
}

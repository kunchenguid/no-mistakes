package config

import (
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Review-loop role keys. The base roles serve every round. The *_after_round
// roles are opt-in later-round overlays: unset, every round keeps running on
// the base role exactly as before.
const (
	RoleReviewer           = "reviewer"
	RoleFixer              = "fixer"
	RoleReviewerAfterRound = "reviewer_after_round"
	RoleFixerAfterRound    = "fixer_after_round"
)

// ReviewAgentRoles lists every valid review_agents key in a stable order.
var ReviewAgentRoles = []string{RoleReviewer, RoleFixer, RoleReviewerAfterRound, RoleFixerAfterRound}

// baseRoleFor names the role an *_after_round entry takes over from.
var baseRoleFor = map[string]string{
	RoleReviewerAfterRound: RoleReviewer,
	RoleFixerAfterRound:    RoleFixer,
}

// ReviewAgent pins one review-loop role to an explicit harness. Empty model or
// effort inherits agent_config for that harness; native argument overrides win.
type ReviewAgent struct {
	Agent  types.AgentName `yaml:"agent"`
	Model  string          `yaml:"model"`
	Effort agentcfg.Effort `yaml:"effort"`
	// AfterRound is the number of leading review rounds that stay on the base
	// role; this entry serves every round above it. Only the *_after_round
	// roles accept it, and an unset value means 1 (take over from round 2).
	// It never changes which rounds happen, only which harness serves them.
	// The pointer keeps an explicit `after_round: 0` distinguishable from an
	// omitted key, so a value the documented contract rejects fails closed
	// instead of being read as the default.
	AfterRound *int `yaml:"after_round"`
}

// TakesOverFromRound is the first 1-based round an *_after_round entry serves.
func (e ReviewAgent) TakesOverFromRound() int {
	if e.AfterRound == nil {
		return 2
	}
	return *e.AfterRound + 1
}

func validateReviewAgents(roles map[string]ReviewAgent) error {
	for role, entry := range roles {
		if !slices.Contains(ReviewAgentRoles, role) {
			return fmt.Errorf("invalid review_agents role %q (valid: %s)", role, strings.Join(ReviewAgentRoles, ", "))
		}
		if !agentcfg.Known(entry.Agent) {
			return fmt.Errorf("review_agents.%s.agent must name an explicit harness, got %q", role, entry.Agent)
		}
		if err := agentcfg.Validate(entry.Agent, agentcfg.Profile{Model: strings.TrimSpace(entry.Model), Effort: entry.Effort}); err != nil {
			return fmt.Errorf("invalid review_agents.%s: %w", role, err)
		}
		if _, isOverlay := baseRoleFor[role]; !isOverlay {
			if entry.AfterRound != nil {
				return fmt.Errorf("review_agents.%s does not accept after_round; configure review_agents.%s_after_round instead", role, role)
			}
			continue
		}
		if entry.AfterRound != nil {
			if *entry.AfterRound < 1 {
				return fmt.Errorf("review_agents.%s.after_round must be at least 1, got %d", role, *entry.AfterRound)
			}
			if *entry.AfterRound >= math.MaxInt {
				return fmt.Errorf("review_agents.%s.after_round overflows the takeover round", role)
			}
		}
	}
	return nil
}

// ReviewAgentTakeoverRound reports the first 1-based round the configured
// <role>_after_round overlay serves, or 0 when that overlay is unset. Zero
// keeps every round on the base role, which is the unconfigured behavior.
func (c *Config) ReviewAgentTakeoverRound(role string) int {
	entry, ok := c.ReviewAgents[role]
	if !ok {
		return 0
	}
	return entry.TakesOverFromRound()
}

// ForReviewAgent returns an isolated configuration for a role without mutating
// shared per-harness profiles (both roles may use the same harness).
func (c *Config) ForReviewAgent(entry ReviewAgent) *Config {
	role := *c
	role.Agent = entry.Agent
	role.Agents = []types.AgentName{entry.Agent}
	profile := c.AgentProfileFor(entry.Agent)
	if entry.Model != "" {
		profile.Model = strings.TrimSpace(entry.Model)
	}
	if entry.Effort != "" {
		profile.Effort = entry.Effort
	}
	role.AgentConfig = map[string]agentcfg.Profile{string(entry.Agent): profile}
	role.ReviewAgents = nil
	return &role
}

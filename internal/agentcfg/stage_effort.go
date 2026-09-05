package agentcfg

import (
	"fmt"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// StageEfforts overrides reasoning depth, never the harness or model. Review-fix
// is a separate duty: absent an override it inherits the global effort, not review.
type StageEfforts map[string]Effort

// EffortStage maps invocation purposes onto the public agent-driven stages.
// Unknown purposes deliberately retain the harness-global profile.
func EffortStage(purpose string) string {
	switch purpose {
	case "intent", "review", "review-fix", "rebase", "test", "document", "lint", "pr", "ci":
		return purpose
	case "test-evidence", "test-fix":
		return "test"
	case "housekeeping", "document-fix":
		return "document"
	case "lint-fix":
		return "lint"
	default:
		return ""
	}
}

func ValidateStageEfforts(name types.AgentName, stages StageEfforts) error {
	if !Known(name) {
		return fmt.Errorf("unknown agent %q", name)
	}
	for stage, effort := range stages {
		if EffortStage(stage) != stage || stage == "" {
			return fmt.Errorf("invalid stage %q (valid: intent, rebase, review, review-fix, test, document, lint, pr, ci)", stage)
		}
		parsed, err := ParseEffort(string(effort))
		if err != nil {
			return fmt.Errorf("%s: %w", stage, err)
		}
		if parsed == "" || parsed != effort {
			return fmt.Errorf("%s: effort must be a non-empty canonical effort value", stage)
		}
		if err := Validate(name, Profile{Effort: effort}); err != nil {
			return fmt.Errorf("%s: %w", stage, err)
		}
	}
	return nil
}

// StageProfile preserves model ownership and the global default.
func StageProfile(base Profile, stages StageEfforts, purpose string) Profile {
	if effort := stages[EffortStage(purpose)]; effort != "" {
		base.Effort = effort
	}
	return base
}

// SameStageEffort compares effective selections, including legacy native pins.
// A raw pin applies to both duties, even when their structured overrides differ.
func SameStageEffort(name types.AgentName, base Profile, stages StageEfforts, raw []string, a, b string) bool {
	h, ok := lookup(name)
	if ok && h.effort.pinned != nil && h.effort.pinned(raw) {
		return true
	}
	return StageProfile(base, stages, a).Effort == StageProfile(base, stages, b).Effort
}

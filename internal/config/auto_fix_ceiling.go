package config

import (
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// AutoFixCeiling is the operator's cap on the core-step auto_fix budgets,
// set in global config or a repository_overrides entry and never in
// .no-mistakes.yaml. auto_fix itself comes from the pushed branch, so without
// it a contributor could raise their own repair budget on a repository the
// operator does not own. A nil field leaves that step uncapped. Gate budgets
// are not covered: they are already trusted-default-branch only.
type AutoFixCeiling struct {
	Lint     *int `yaml:"lint"`
	Test     *int `yaml:"test"`
	Review   *int `yaml:"review"`
	Document *int `yaml:"document"`
	CI       *int `yaml:"ci"`
	Rebase   *int `yaml:"rebase"`
}

var autoFixCeilingSteps = []types.StepName{types.StepLint, types.StepTest, types.StepReview, types.StepDocument, types.StepCI, types.StepRebase}

func (c AutoFixCeiling) byStep() map[types.StepName]*int {
	return map[types.StepName]*int{
		types.StepLint: c.Lint, types.StepTest: c.Test, types.StepReview: c.Review,
		types.StepDocument: c.Document, types.StepCI: c.CI, types.StepRebase: c.Rebase,
	}
}

func validateAutoFixCeiling(c AutoFixCeiling) error {
	values := c.byStep()
	for _, step := range autoFixCeilingSteps {
		if v := values[step]; v != nil && *v < 0 {
			return fmt.Errorf("auto_fix_ceiling.%s must be non-negative", step)
		}
	}
	return nil
}

// applyAutoFixCeiling caps af in place. A matching repository_overrides
// field replaces the global field for the same step. It returns the
// pre-ceiling limit of every step the ceiling lowered, nil when none was.
func applyAutoFixCeiling(af *AutoFix, global AutoFixCeiling, override *RepositoryOverride) map[types.StepName]int {
	ceiling := global.byStep()
	if override != nil {
		for step, v := range override.AutoFixCeiling.byStep() {
			if v != nil {
				ceiling[step] = v
			}
		}
	}
	limits := map[types.StepName]*int{
		types.StepLint: &af.Lint, types.StepTest: &af.Test, types.StepReview: &af.Review,
		types.StepDocument: &af.Document, types.StepCI: &af.CI, types.StepRebase: &af.Rebase,
	}
	var capped map[types.StepName]int
	for step, maxAttempts := range ceiling {
		if maxAttempts == nil || *limits[step] <= *maxAttempts {
			continue
		}
		if capped == nil {
			capped = map[types.StepName]int{}
		}
		capped[step] = *limits[step]
		*limits[step] = *maxAttempts
	}
	return capped
}

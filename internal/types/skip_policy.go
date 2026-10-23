package types

import "fmt"

// RequireSkippedSteps asserts caller scope without changing a run's policy.
func RequireSkippedSteps(stored []StepName, known bool, required []StepName) error {
	if len(required) == 0 {
		return nil
	}
	if !known {
		return fmt.Errorf("run has no durable skip policy; refusing scoped attachment or response")
	}
	have := map[StepName]bool{}
	for _, step := range stored {
		have[step] = true
	}
	for _, step := range required {
		if !IsCoreStepName(step) {
			return fmt.Errorf("invalid required skip %q", step)
		}
		if !have[step] {
			return fmt.Errorf("run skip policy does not cover required skip %q", step)
		}
	}
	return nil
}

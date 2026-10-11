package types

import "fmt"

const ExternalCIOwnerControllerShipPR = "controller-ship-pr"

// ValidateExternalCIOwnerRequest admits the only supported handoff and its
// exact, explicit skip set. An empty owner preserves ordinary runs.
func ValidateExternalCIOwnerRequest(owner string, skips []StepName) error {
	if owner == "" {
		return nil
	}
	if owner != ExternalCIOwnerControllerShipPR {
		return fmt.Errorf("unsupported external CI owner %q", owner)
	}
	if len(skips) != 3 {
		return fmt.Errorf("--external-ci-owner requires exactly --skip=push,pr,ci")
	}
	seen := map[StepName]bool{}
	for _, step := range skips {
		if step != StepPush && step != StepPR && step != StepCI || seen[step] {
			return fmt.Errorf("--external-ci-owner requires exactly --skip=push,pr,ci")
		}
		seen[step] = true
	}
	return nil
}

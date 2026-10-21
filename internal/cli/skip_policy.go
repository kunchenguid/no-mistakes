package cli

import (
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func firstRequiredSkips(required [][]types.StepName) []types.StepName {
	if len(required) == 0 {
		return nil
	}
	return required[0]
}

func requireRunSkips(run *ipc.RunInfo, required []types.StepName) error {
	if len(required) == 0 {
		return nil
	}
	if run == nil {
		return fmt.Errorf("run is unavailable for skip policy verification")
	}
	if run.SkipPolicyError != "" {
		return fmt.Errorf("run %s skip policy: %s", run.ID, run.SkipPolicyError)
	}
	return types.RequireSkippedSteps(run.SkipSteps, run.SkipPolicyKnown, required)
}

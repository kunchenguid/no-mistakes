package steps

import (
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// RefreshReviewSupport obtains new configured-command evidence for the final
// comparison without rerunning the completed Test agent or live scenarios.
func (s *TestStep) RefreshReviewSupport(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	claims, _, err := currentReviewSupportClaims(sctx, types.FindingClaimTest)
	if err != nil {
		return nil, err
	}
	if len(claims) == 0 {
		return &pipeline.StepOutcome{}, nil
	}
	if sctx.Config == nil || sctx.Config.Commands.Test == "" {
		return nil, fmt.Errorf("final Test support requires a trusted configured command")
	}
	command := sctx.Config.Commands.Test
	for _, claim := range claims {
		if claim.Support.Test == nil || claim.Support.Test.Command != command {
			return nil, fmt.Errorf("pending Test claim does not select the trusted configured command")
		}
	}
	if err := ensurePrepared(sctx, s.Name()); err != nil {
		return nil, err
	}
	before := currentTestCommandHead(sctx)
	if before == "" || sctx.PRContext == nil || before != sctx.PRContext.LocalHeadSHA {
		return nil, fmt.Errorf("final Test support has no matching command head")
	}
	sctx.Log("refreshing configured Test support for the final comparison")
	output, exit, err := runStepShellCommand(sctx, command)
	logConfiguredCommandOutput(sctx, output, types.StepTest)
	if err != nil {
		return nil, err
	}
	after := currentTestCommandHead(sctx)
	if after != before {
		return nil, fmt.Errorf("configured Test support command changed HEAD")
	}
	results, err := resolveTestReviewSupport(sctx, command, &exit, after, supportObservationTime())
	if err != nil {
		return nil, err
	}
	outcome := &pipeline.StepOutcome{}
	if err := appendOwnerSupportResults(outcome, results); err != nil {
		return nil, err
	}
	return outcome, nil
}

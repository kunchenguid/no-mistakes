package steps

import (
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// retryRetainedPublication never asks an agent to recreate an existing repair.
// Verification failures stay at the gate with the correction untouched.
func (s *CIStep) retryRetainedPublication(sctx *pipeline.StepContext) (bool, *pipeline.StepOutcome) {
	binding, err := sctx.DB.RetainedCIRepair(sctx.Run.ID)
	park := func(reason string) (bool, *pipeline.StepOutcome) {
		findings, _ := types.ParseFindingsJSON(sctx.PreviousFindings)
		return true, ciRepairParkOutcome(findings, sctx.DeferredFindings, reason)
	}
	if err != nil {
		return park(fmt.Sprintf("read retained CI repair: %v", err))
	}
	if binding == nil {
		if sctx.Fixing && !pipeline.HasProtectedPathRefusal(sctx.PreviousFindings) {
			head, err := stepGitHeadSHA(sctx)
			if err != nil || head != sctx.Run.HeadSHA {
				return park("Unbound retained CI correction: refusing publication or another fixer invocation; preserve this worktree for explicit recovery")
			}
		}
		return false, nil
	}
	if !sctx.Fixing {
		return park("Retained CI repair requires an explicit fix response to retry publication")
	}
	if ciRevalidatesRepairs(sctx) {
		return park("Retained CI repair requires Review under the trusted policy; publication-only retry refused")
	}
	verified, err := pipeline.VerifyRetainedCIRepair(sctx.Ctx, sctx.DB, sctx.Run, sctx.GateDir, sctx.WorkDir)
	if err != nil || verified == nil || verified.StepID != sctx.StepResultID {
		return park(fmt.Sprintf("retained CI repair verification failed: %v", err))
	}
	if err := setCIMonitorReadiness(sctx, false, false); err != nil {
		return park(fmt.Sprintf("clear stale CI readiness: %v", err))
	}
	if err := sctx.DB.SetRunPushActive(sctx.Run.ID, true); err != nil {
		return park(fmt.Sprintf("claim retained CI publication: %v", err))
	}
	defer func() { _ = sctx.DB.SetRunPushActive(sctx.Run.ID, false) }()
	if _, err := s.publishRepair(sctx, verified.RetainedHead); err != nil {
		return park(fmt.Sprintf("retained CI publication remains unsettled: %v", err))
	}
	s.pendingRepairPublish = true
	if sctx.MarkRunning != nil {
		if err := sctx.MarkRunning(); err != nil {
			return park(fmt.Sprintf("resume CI monitoring: %v", err))
		}
	}
	return true, nil
}

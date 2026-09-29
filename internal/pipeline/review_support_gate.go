package pipeline

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// validateReviewSupportOwners refuses a terminal verdict while a Review
// hypothesis still lacks current evidence from its owning step. Only current
// step results count; superseded rounds are discarded when the receipt resets.
func (e *Executor) validateReviewSupportOwners(runID string, through types.StepName) error {
	results, err := e.db.GetStepsByRun(runID)
	if err != nil {
		return fmt.Errorf("read support owner steps: %w", err)
	}
	byName := make(map[types.StepName]*db.StepResult, len(results))
	for _, result := range results {
		byName[result.StepName] = result
	}
	review := byName[types.StepReview]
	if review == nil || review.Status != types.StepStatusCompleted || review.FindingsJSON == nil {
		return nil
	}
	findings, err := types.ParseFindingsJSON(*review.FindingsJSON)
	if err != nil {
		return fmt.Errorf("read current Review support: %w", err)
	}
	var pending []types.Finding
	for _, finding := range findings.Items {
		if finding.Category == "review-support-pending" && finding.Support != nil &&
			(finding.Support.ClaimType == types.FindingClaimTest || finding.Support.ClaimType == types.FindingClaimCI) {
			pending = append(pending, finding)
		}
	}
	if len(pending) == 0 {
		return nil
	}
	current, err := e.db.GetRun(runID)
	if err != nil {
		return fmt.Errorf("read run for support verdict: %w", err)
	}
	receipt, err := e.db.GetRunPRContext(runID)
	if err != nil {
		return fmt.Errorf("read PR comparison for support verdict: %w", err)
	}
	if receipt == nil || current.ReviewApprovedHeadSHA == nil || receipt.LocalHeadSHA != current.HeadSHA {
		return fmt.Errorf("pending Review support lacks an approved current-head comparison")
	}
	approvedHead := *current.ReviewApprovedHeadSHA
	if approvedHead != current.HeadSHA {
		if _, err := git.Run(context.Background(), e.workDir, "merge-base", "--is-ancestor", approvedHead, current.HeadSHA); err != nil {
			return fmt.Errorf("pending Review support is not continuous with approved head: %w", err)
		}
	}
	for _, claim := range pending {
		if claim.ID == "" || claim.Support.OwnerResult != nil {
			return fmt.Errorf("pending Review claim has invalid owner identity")
		}
		if err := claim.Support.Validate(); err != nil {
			return fmt.Errorf("pending Review claim %s has invalid support: %w", claim.ID, err)
		}
		ownerName := types.StepTest
		if claim.Support.ClaimType == types.FindingClaimCI {
			ownerName = types.StepCI
		}
		if ownerName.Order() > through.Order() {
			continue
		}
		owner := byName[ownerName]
		if ownerName == types.StepCI && owner != nil && owner.Status == types.StepStatusSkipped &&
			current.ExternalCIOwner == types.ExternalCIOwnerControllerShipPR &&
			stepsSkipped(byName, types.StepPush, types.StepPR, types.StepCI) {
			// Controller explicitly owns this exact-head CI decision. The run
			// remains pending-external-ci at the AXI surface; this is a handoff,
			// never a local CI proof or an approval to merge.
			continue
		}
		if owner == nil || owner.Status != types.StepStatusCompleted || owner.FindingsJSON == nil {
			return fmt.Errorf("pending Review claim %s has no completed %s evidence owner", types.ReviewSupportClaimID(claim), ownerName)
		}
		ownerFindings, err := types.ParseFindingsJSON(*owner.FindingsJSON)
		if err != nil {
			return fmt.Errorf("read %s support result: %w", ownerName, err)
		}
		claimID := types.ReviewSupportClaimID(claim)
		resolved := false
		for _, candidate := range ownerFindings.Items {
			if candidate.ID != claimID || candidate.Category != types.FindingCategoryReviewSupportResolved || candidate.Support == nil || candidate.Support.OwnerResult == nil || candidate.Support.ClaimType != claim.Support.ClaimType {
				continue
			}
			result := candidate.Support.OwnerResult
			if result.ReviewFindingID != claim.ID || result.HeadSHA != approvedHead || result.TargetSHA != receipt.TargetSHA ||
				result.Generation > receipt.Generation || result.Disposition != types.FindingSupportDispositionDisproven {
				continue
			}
			if approvedHead == current.HeadSHA && result.DiffDigest != receipt.DiffDigest {
				continue
			}
			observedAt, err := time.Parse(time.RFC3339Nano, result.ObservedAt)
			if err != nil || observedAt.IsZero() {
				continue
			}
			if ownerName == types.StepTest && (result.ExitCode == nil || *result.ExitCode != 0) {
				continue
			}
			if ownerName == types.StepCI && result.CheckState != "pass" && !strings.HasPrefix(result.CheckState, "pass:") {
				continue
			}
			resolved = true
			break
		}
		if !resolved {
			return fmt.Errorf("pending Review claim %s lacks current %s proof", claimID, ownerName)
		}
	}
	return nil
}

func stepsSkipped(results map[types.StepName]*db.StepResult, names ...types.StepName) bool {
	for _, name := range names {
		step := results[name]
		if step == nil || step.Status != types.StepStatusSkipped {
			return false
		}
	}
	return true
}

package db

import (
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// PendingExternalCISupport returns only pending CI claims from this run's
// completed Review under its current approved PR comparison. It is a request
// for external verification, never a result of that verification.
func (d *DB) PendingExternalCISupport(run *Run) ([]types.PendingCISupport, error) {
	if run == nil || run.ExternalCIOwner == "" {
		return nil, nil
	}
	steps, err := d.GetStepsByRun(run.ID)
	if err != nil {
		return nil, err
	}
	var review *StepResult
	for _, step := range steps {
		if step.StepName == types.StepReview && step.Status == types.StepStatusCompleted {
			if review != nil {
				return nil, fmt.Errorf("multiple completed Review steps for external CI handoff")
			}
			review = step
		}
	}
	if review == nil || review.FindingsJSON == nil {
		return nil, nil
	}
	findings, err := types.ParseFindingsJSON(*review.FindingsJSON)
	if err != nil {
		return nil, fmt.Errorf("read Review support claims: %w", err)
	}
	var claims []types.Finding
	for _, finding := range findings.Items {
		if finding.Category != types.FindingCategoryReviewSupportPending || finding.Support == nil || finding.Support.ClaimType != types.FindingClaimCI {
			continue
		}
		if finding.ID == "" || finding.Support.OwnerResult != nil || finding.Support.CI == nil {
			return nil, fmt.Errorf("invalid pending Review CI support claim")
		}
		if err := finding.Support.Validate(); err != nil {
			return nil, fmt.Errorf("validate pending Review CI support claim: %w", err)
		}
		if len(finding.ID) > 1024 || len(finding.Support.CI.CheckID) > 1024 || strings.TrimSpace(finding.Support.CI.CheckID) != finding.Support.CI.CheckID || !validGitSHA(finding.Support.CI.HeadSHA) {
			return nil, fmt.Errorf("pending Review CI support claim exceeds handoff bounds or has invalid historical head")
		}
		claims = append(claims, finding)
	}
	if len(claims) == 0 {
		return nil, nil
	}
	if len(claims) > 128 {
		return nil, fmt.Errorf("pending Review CI support exceeds 128 handoff claims")
	}
	receipt, err := d.GetRunPRContext(run.ID)
	if err != nil {
		return nil, err
	}
	if receipt == nil || run.ReviewApprovedHeadSHA == nil || *run.ReviewApprovedHeadSHA == "" ||
		run.HeadSHA != receipt.LocalHeadSHA || *run.ReviewApprovedHeadSHA != receipt.LocalHeadSHA ||
		strings.TrimSpace(receipt.SourceRepo) == "" || strings.TrimSpace(receipt.SourceBranch) == "" {
		return nil, fmt.Errorf("pending Review CI support has no current approved PR comparison")
	}
	rounds, err := d.GetRoundsByStep(review.ID)
	if err != nil {
		return nil, fmt.Errorf("read approved Review rounds: %w", err)
	}
	approvedRoundHead := ""
	for _, round := range rounds {
		if round.ReviewedHeadSHA != nil {
			approvedRoundHead = *round.ReviewedHeadSHA
		}
	}
	if approvedRoundHead == "" || approvedRoundHead != *run.ReviewApprovedHeadSHA {
		return nil, fmt.Errorf("pending Review CI support has no matching approved Review round")
	}
	result := make([]types.PendingCISupport, 0, len(claims))
	for _, claim := range claims {
		result = append(result, types.PendingCISupport{
			ClaimID: types.ReviewSupportClaimID(claim), OriginalFindingID: claim.ID,
			HistoricalCheckID: claim.Support.CI.CheckID, HistoricalHeadSHA: claim.Support.CI.HeadSHA,
			SourceHeadSHA: receipt.LocalHeadSHA, ForgeHeadSHA: receipt.ForgeHeadSHA,
			TargetBranch: receipt.TargetBranch, TargetSHA: receipt.TargetSHA,
			DiffDigest: receipt.DiffDigest, ReceiptGeneration: receipt.Generation,
			RunID: run.ID, PRURL: receipt.PRURL, Owner: run.ExternalCIOwner,
		})
	}
	return result, nil
}

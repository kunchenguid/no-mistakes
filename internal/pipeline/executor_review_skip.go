package pipeline

import (
	"fmt"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// validateSkippedReview guards continuation from durable state, regardless of
// how Review became skipped or whether the executor was recovered. A reviewed
// candidate is not approval authority. Only completing Review grants that.
func (e *Executor) validateSkippedReview(run *db.Run, repo *db.Repo) error {
	results, err := e.db.GetStepsByRun(run.ID)
	if err != nil {
		return fmt.Errorf("validate skipped review: read step plan: %w", err)
	}
	var review *db.StepResult
	pushScheduled := false
	for _, result := range results {
		if result.StepName == types.StepReview && result.Status == types.StepStatusSkipped {
			review = result
		}
		if result.StepName == types.StepPush && result.Status != types.StepStatusSkipped && result.Status != types.StepStatusCompleted {
			pushScheduled = true
		}
	}
	if review == nil || !pushScheduled {
		return nil
	}
	durable, err := e.db.GetRun(run.ID)
	if err != nil {
		return fmt.Errorf("validate skipped review: read approval authority: %w", err)
	}
	if durable == nil {
		return fmt.Errorf("validate skipped review: run is not recorded")
	}
	if durable.ReviewApprovedHeadSHA != nil && strings.TrimSpace(*durable.ReviewApprovedHeadSHA) != "" {
		return nil
	}
	err = fmt.Errorf("review skip prevents push: run has no durably recorded review-approved head while Push is scheduled; rerun Review and use --action approve to decline findings, or --action fix with --ignore to decline individual findings")
	duration := recoveredStepDuration(review)
	if dbErr := e.db.FailStep(review.ID, err.Error(), duration); dbErr != nil {
		return fmt.Errorf("record review-skip failure: %w", dbErr)
	}
	if durable.AwaitingAgentSince != nil {
		if dbErr := e.db.CompleteRunAwaitingAgent(run.ID, time.Since(time.Unix(*durable.AwaitingAgentSince, 0)).Milliseconds()); dbErr != nil {
			return fmt.Errorf("clear review-skip awaiting-agent state: %w", dbErr)
		}
	}
	e.emitStepEventWithFindingsAndError(ipc.EventStepCompleted, run, repo, types.StepReview, string(types.StepStatusFailed), "", err.Error(), &duration)
	return err
}

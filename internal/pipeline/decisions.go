package pipeline

import (
	"log/slog"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
)

// BindBranchDecisions loads the decisions a human already made on this branch
// in earlier runs onto the step context, so every step's prompt can carry them.
//
// This mirrors BindUncertifiedPipelineRange, with two deliberate differences.
// It binds for EVERY step rather than review only, because the steps that
// silently undid a decision were the ones that never saw it - test, document,
// and lint all write to the worktree. And nothing ever clears what it loads:
// the uncertified range is deleted the moment a review completes, which is
// exactly why that channel could not carry a decision into the next run,
// whereas approving a gate is itself a decision that must keep standing.
//
// Best effort. A read failure logs a bounded reason and leaves the context
// without the section, degrading to the previous behavior rather than failing
// the run.
func BindBranchDecisions(sctx *StepContext) {
	if sctx == nil || sctx.DB == nil || sctx.Repo == nil || sctx.Run == nil {
		return
	}
	branch := strings.TrimSpace(sctx.Run.Branch)
	if sctx.Repo.ID == "" || branch == "" {
		return
	}
	decisions, truncated, err := sctx.DB.GetBranchDecisionRounds(sctx.Repo.ID, branch, sctx.Run.ID, db.MaxBranchDecisionRounds)
	if err != nil {
		slog.Warn("failed to read prior branch decisions; continuing without them", "repo_id", sctx.Repo.ID, "error", err)
		return
	}
	sctx.PriorBranchDecisions = decisions
	sctx.PriorBranchDecisionsTruncated = truncated
}

// BindPreviousRunReviewRounds loads the review rounds of the most recent other
// run on this branch, so a run that REPLACED a parked one still sees what was
// already reviewed and why.
//
// This is the supersede channel for the review conversation. With the change
// author applying review fixes in their own worktree, the fix arrives as a
// push: the parked run is superseded and a fresh run starts whose review step
// has no round history at all. The code must still be reviewed cold - that is
// the whole guarantee - but the reviewer should not be blind to the fact that
// a conversation happened, or re-derive from scratch what the previous round
// already found and the author already answered.
//
// It is skipped when the uncertified-range channel already carries the same
// run's rounds, so a previous run's rounds are never rendered twice.
//
// Best effort, like BindBranchDecisions: a read failure logs a bounded reason
// and leaves the context without the section.
func BindPreviousRunReviewRounds(sctx *StepContext) {
	if sctx == nil || sctx.DB == nil || sctx.Repo == nil || sctx.Run == nil {
		return
	}
	// The supersede channel exists for the conversation, so it is off with it:
	// a repository that did not ask for the conversation gets the review prompt
	// it got before this feature, and a superseded run's rounds stay where they
	// already were - the uncertified-range channel and the branch decisions.
	if sctx.Config == nil || !sctx.Config.Review.Conversation {
		return
	}
	branch := strings.TrimSpace(sctx.Run.Branch)
	if sctx.Repo.ID == "" || branch == "" {
		return
	}
	previous, err := sctx.DB.GetPreviousRunReviewRounds(sctx.Repo.ID, branch, sctx.Run.ID)
	if err != nil {
		slog.Warn("failed to read the previous run's review rounds; continuing without them", "repo_id", sctx.Repo.ID, "error", err)
		return
	}
	if previous == nil || previous.RunID == strings.TrimSpace(sctx.UncertifiedSourceRunID) {
		return
	}
	sctx.PreviousRunReviewRounds = previous.Rounds
}

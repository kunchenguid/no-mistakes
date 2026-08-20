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

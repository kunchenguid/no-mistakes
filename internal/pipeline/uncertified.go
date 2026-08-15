package pipeline

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// BindUncertifiedPipelineRange copies a persisted uncertified fixer range
// onto the review step context when this run's head is that range's tip or a
// descendant of it. Missing objects fail open: the run continues without the
// provenance clause and a bounded warning is logged. Never blocks the run.
func BindUncertifiedPipelineRange(sctx *StepContext) {
	if sctx == nil || sctx.DB == nil || sctx.Repo == nil || sctx.Run == nil || sctx.Fixing {
		return
	}
	rng, err := sctx.DB.GetUncertifiedPipelineRange(sctx.Repo.ID, sctx.Run.Branch)
	if err != nil {
		slog.Warn("failed to read uncertified pipeline range; not applying provenance", "repo_id", sctx.Repo.ID, "error", err)
		return
	}
	if rng == nil {
		return
	}
	head := strings.TrimSpace(sctx.Run.HeadSHA)
	if head == "" {
		head = strings.TrimSpace(sctx.ReviewStartingHeadSHA)
	}
	if !commitIsSelfOrAncestor(sctx.Ctx, sctx.WorkDir, rng.ToSHA, head) {
		warnUncertifiedRangeSkipped(sctx, rng, "uncertified range %s..%s not in gate; not applying provenance")
		return
	}
	sctx.UncertifiedFromSHA = rng.FromSHA
	sctx.UncertifiedToSHA = rng.ToSHA
	sctx.UncertifiedSourceRunID = rng.SourceRunID
	sctx.UncertifiedPriorRounds = loadUncertifiedPriorRounds(sctx.DB, rng.SourceRunID)
}

// PersistUncertifiedPipelineRange records the fixer commit span after a
// review fix round commits and before its re-review completes.
func PersistUncertifiedPipelineRange(sctx *StepContext, fromSHA, toSHA string) {
	if sctx == nil || sctx.DB == nil || sctx.Repo == nil || sctx.Run == nil {
		return
	}
	fromSHA = strings.TrimSpace(fromSHA)
	toSHA = strings.TrimSpace(toSHA)
	if fromSHA == "" || toSHA == "" || fromSHA == toSHA {
		return
	}
	existing, err := sctx.DB.GetUncertifiedPipelineRange(sctx.Repo.ID, sctx.Run.Branch)
	if err != nil {
		slog.Warn("failed to read uncertified pipeline range before persist", "run_id", sctx.Run.ID, "error", err)
		existing = nil
	}
	if existing != nil && existing.SourceRunID == sctx.Run.ID && strings.TrimSpace(existing.FromSHA) != "" {
		fromSHA = existing.FromSHA
	}
	if err := sctx.DB.UpsertUncertifiedPipelineRange(sctx.Repo.ID, sctx.Run.Branch, fromSHA, toSHA, sctx.Run.ID); err != nil {
		slog.Warn("failed to persist uncertified pipeline range", "run_id", sctx.Run.ID, "error", err)
		if sctx.Log != nil {
			sctx.Log("warning: failed to persist uncertified fixer commit range")
		}
	}
}

// ClearUncertifiedPipelineRangeIfCertified drops the branch marker once a
// full review has completed. A completed review of the current head certifies
// the previously uncertified fixer commits on this branch.
func ClearUncertifiedPipelineRangeIfCertified(ctx context.Context, database *db.DB, repoID, branch, approvedHead, workDir string) {
	if database == nil {
		return
	}
	rng, err := database.GetUncertifiedPipelineRange(repoID, branch)
	if err != nil {
		slog.Warn("failed to read uncertified pipeline range before clear", "repo_id", repoID, "error", err)
		return
	}
	if rng == nil {
		return
	}
	approvedHead = strings.TrimSpace(approvedHead)
	if approvedHead == "" {
		return
	}
	if rng.ToSHA != approvedHead && !commitIsSelfOrAncestor(ctx, workDir, rng.ToSHA, approvedHead) {
		return
	}
	if err := database.DeleteUncertifiedPipelineRange(repoID, branch); err != nil {
		slog.Warn("failed to clear uncertified pipeline range after certified review", "repo_id", repoID, "error", err)
	}
}

func warnUncertifiedRangeSkipped(sctx *StepContext, rng *db.UncertifiedPipelineRange, format string) {
	msg := fmt.Sprintf(format, rng.FromSHA, rng.ToSHA)
	slog.Warn(msg, "repo_id", sctx.Repo.ID, "branch", sctx.Run.Branch)
	if sctx.Log != nil {
		sctx.Log("warning: " + msg)
	}
}

func commitIsSelfOrAncestor(ctx context.Context, workDir, ancestor, descendent string) bool {
	ancestor = strings.TrimSpace(ancestor)
	descendent = strings.TrimSpace(descendent)
	if ancestor == "" || descendent == "" || workDir == "" {
		return false
	}
	if ancestor == descendent {
		return true
	}
	if ctx == nil {
		ctx = context.Background()
	}
	_, err := git.Run(ctx, workDir, "merge-base", "--is-ancestor", ancestor, descendent)
	return err == nil
}

func loadUncertifiedPriorRounds(database *db.DB, sourceRunID string) []*db.StepRound {
	sourceRunID = strings.TrimSpace(sourceRunID)
	if database == nil || sourceRunID == "" {
		return nil
	}
	steps, err := database.GetStepsByRun(sourceRunID)
	if err != nil {
		slog.Warn("failed to read uncertified source-run steps", "run_id", sourceRunID, "error", err)
		return nil
	}
	for _, step := range steps {
		if step.StepName != types.StepReview {
			continue
		}
		rounds, err := database.GetRoundsByStep(step.ID)
		if err != nil {
			slog.Warn("failed to read uncertified source-run review rounds", "run_id", sourceRunID, "error", err)
			return nil
		}
		return rounds
	}
	return nil
}

package steps

import (
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

// acceptReviewReword runs only at a successful Review fixer's handoff, before
// the independent re-review. Step-entry and publication guards remain ancestry
// checks. The proof is anchored on the head the run's own Rebase step
// recorded, never on a caller-supplied head. Rewrites of that work, merges,
// squashes and mixed code edits remain outside this metadata-only exception.
func acceptReviewReword(sctx *pipeline.StepContext, before string) (bool, error) {
	run := sctx.Run
	if before == "" || before != run.HeadSHA || run.SubmittedHeadSHA == nil ||
		run.ReviewApprovedHeadSHA != nil || (run.PushGeneration != nil && *run.PushGeneration != 0) ||
		(run.LastPushedSHA != nil && *run.LastPushedSHA != "") {
		return false, nil
	}
	head, err := git.HeadSHA(sctx.Ctx, sctx.WorkDir)
	if err != nil {
		return false, err
	}
	if head == before {
		return false, nil
	}
	if _, err := git.Run(sctx.Ctx, sctx.WorkDir, "merge-base", "--is-ancestor", before, head); err == nil {
		return false, nil
	}
	recordedRun, err := sctx.DB.GetRun(run.ID)
	if err != nil || recordedRun == nil || recordedRun.RebasedHeadSHA == nil {
		return false, fmt.Errorf("refusing review fix rewrite: this run recorded no post-rebase head to anchor the proof; inspect the Review log and reconcile manually")
	}
	rebased := *recordedRun.RebasedHeadSHA
	if err := custody.VerifyReword(sctx.Ctx, sctx.WorkDir, rebased, before, head); err != nil {
		return false, fmt.Errorf("refusing review fix rewrite: %w", err)
	}
	status, err := git.Run(sctx.Ctx, sctx.WorkDir, "status", "--porcelain")
	if err != nil || strings.TrimSpace(status) != "" || mergeInProgress(sctx.Ctx, sctx.WorkDir) || rebaseInProgress(sctx.Ctx, sctx.WorkDir) {
		return false, fmt.Errorf("refusing review fix rewrite with uncommitted or unreadable work")
	}
	if err := custody.PreserveReword(sctx.Ctx, sctx.WorkDir, run.ID, before, head); err != nil {
		return false, err
	}
	current, err := git.HeadSHA(sctx.Ctx, sctx.WorkDir)
	if err != nil || current != head {
		return false, fmt.Errorf("review fix head moved while preserving its reworded history")
	}
	if err := custody.RemapRewordedRange(sctx.Ctx, sctx.DB, sctx.WorkDir, sctx.Repo.ID, run.Branch, run.ID, rebased, before, head); err != nil {
		return false, fmt.Errorf("carry uncertified fixer range across review fix rewrite: %w", err)
	}
	if err := updateNonSharedBranchRef(sctx, head); err != nil {
		return false, err
	}
	if err := sctx.DB.UpdateRunHeadSHA(run.ID, head); err != nil {
		return false, err
	}
	run.HeadSHA = head
	sctx.Log(fmt.Sprintf("preserved metadata-only review rewrite: %s -> %s; re-review required", before, head))
	return true, nil
}

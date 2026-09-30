package pipeline

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// A completed Test can refresh only its configured-command support evidence.
// This does not repeat its agent turn, scenarios, or earlier pipeline stages.
type reviewTestSupportRefresher interface {
	RefreshReviewSupport(*StepContext) (*StepOutcome, error)
}

func (e *Executor) refreshReviewTestSupport(ctx context.Context, run *db.Run, repo *db.Repo, workDir string) error {
	if err := e.validateReviewSupportOwners(run.ID, types.StepTest); err == nil {
		return nil
	}
	results, err := e.db.GetStepsByRun(run.ID)
	if err != nil {
		return err
	}
	var review, owner *db.StepResult
	for _, result := range results {
		switch result.StepName {
		case types.StepReview:
			review = result
		case types.StepTest:
			owner = result
		}
	}
	if review == nil || owner == nil || review.Status != types.StepStatusCompleted || owner.Status != types.StepStatusCompleted || review.FindingsJSON == nil || owner.FindingsJSON == nil {
		return nil
	}
	claims, err := types.ParseFindingsJSON(*review.FindingsJSON)
	if err != nil {
		return err
	}
	prior, err := types.ParseFindingsJSON(*owner.FindingsJSON)
	if err != nil {
		return err
	}
	receipt, err := e.db.GetRunPRContext(run.ID)
	if err != nil {
		return err
	}
	if receipt == nil {
		return fmt.Errorf("refresh Test support requires current comparison")
	}
	stale := false
	for _, claim := range claims.Items {
		if claim.Category != types.FindingCategoryReviewSupportPending || claim.Support == nil || claim.Support.ClaimType != types.FindingClaimTest {
			continue
		}
		found := false
		for _, result := range prior.Items {
			if result.ID != types.ReviewSupportClaimID(claim) || result.Category != types.FindingCategoryReviewSupportResolved || result.Support == nil || result.Support.OwnerResult == nil {
				continue
			}
			proof := result.Support.OwnerResult
			if proof.ReviewFindingID != claim.ID || proof.Disposition != types.FindingSupportDispositionDisproven || proof.ExitCode == nil || *proof.ExitCode != 0 {
				continue
			}
			found = true
			stale = stale || proof.HeadSHA != receipt.LocalHeadSHA || proof.TargetSHA != receipt.TargetSHA || proof.DiffDigest != receipt.DiffDigest || proof.Generation != receipt.Generation
		}
		if !found {
			return nil
		} // Missing or failing proof is not a stale success.
	}
	if !stale {
		return nil
	}
	var refresher reviewTestSupportRefresher
	for _, step := range e.steps {
		if step.Name() == types.StepTest {
			refresher, _ = step.(reviewTestSupportRefresher)
			break
		}
	}
	if refresher == nil {
		return fmt.Errorf("stale Test support has no configured-command evidence owner")
	}
	head, err := git.HeadSHA(ctx, workDir)
	if err != nil {
		return err
	}
	dirty, err := git.HasUncommittedChanges(ctx, workDir)
	if err != nil {
		return err
	}
	if dirty || head != receipt.LocalHeadSHA || run.HeadSHA != head {
		return fmt.Errorf("refresh Test support requires the clean final head")
	}
	logDir := e.paths.RunLogDir(run.ID)
	if err := os.MkdirAll(logDir, 0755); err != nil {
		return err
	}
	log, err := os.OpenFile(filepath.Join(logDir, "test.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return err
	}
	defer log.Close()
	writeFile := func(message string) {
		fmt.Fprintln(log, message)
	}
	write := func(message string) {
		writeFile(message)
		e.emitLogChunk(run, repo, types.StepTest, message+"\n")
	}
	sctx := &StepContext{Ctx: ctx, Run: run, Repo: repo, WorkDir: workDir, GateDir: e.paths.RepoDir(repo.ID), Config: e.config, ForgeContext: e.forge, DB: e.db, PRTarget: e.prTarget, PRContext: receipt, StepResultID: owner.ID, Log: write, LogChunk: write, LogFile: writeFile, Shared: e.shared}
	outcome, err := refresher.RefreshReviewSupport(sctx)
	if err != nil {
		return fmt.Errorf("refresh final Test support: %w", err)
	}
	afterHead, err := git.HeadSHA(ctx, workDir)
	if err != nil {
		return err
	}
	dirty, err = git.HasUncommittedChanges(ctx, workDir)
	if err != nil {
		return err
	}
	if dirty || afterHead != head {
		return fmt.Errorf("Test support refresh changed the final worktree")
	}
	if e.prContextGuard != nil {
		index, err := e.stepIndex(types.StepCI)
		if err != nil {
			return err
		}
		restart, err := e.checkPRContext(ctx, run, repo, workDir, types.StepCI, index, true)
		if err != nil {
			return err
		}
		if restart >= 0 {
			return fmt.Errorf("PR comparison changed during Test support refresh")
		}
	}
	current, err := e.db.GetRunPRContext(run.ID)
	if err != nil {
		return err
	}
	if current == nil || *current != *receipt {
		return fmt.Errorf("Test support refresh comparison changed")
	}
	if outcome == nil || outcome.Findings == "" {
		return fmt.Errorf("Test support refresh returned no owner evidence")
	}
	fresh, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		return err
	}
	ids := map[string]bool{}
	for _, item := range fresh.Items {
		if item.Support == nil || item.Support.ClaimType != types.FindingClaimTest || item.Support.OwnerResult == nil {
			return fmt.Errorf("Test support refresh returned invalid owner evidence")
		}
		ids[item.ID] = true
	}
	kept := prior.Items[:0]
	for _, item := range prior.Items {
		if !ids[item.ID] {
			kept = append(kept, item)
		}
	}
	prior.Items = append(kept, fresh.Items...)
	encoded, err := types.MarshalFindingsJSON(prior)
	if err != nil {
		return err
	}
	if err := e.db.SetStepFindings(owner.ID, encoded); err != nil {
		return err
	}
	return e.validateReviewSupportOwners(run.ID, types.StepTest)
}

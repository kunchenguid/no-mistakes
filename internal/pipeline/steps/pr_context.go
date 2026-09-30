package steps

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// GuardPRContext pins the exact target comparison before Rebase and checks it
// at every later core-step boundary. A changed external comparison after a
// completed boundary stops the run; a new run can validate that comparison
// without replaying completed steps. A step's own forward edit advances the
// receipt while retaining earlier evidence.
func GuardPRContext(sctx *pipeline.StepContext, step types.StepName) (pipeline.PRContextDecision, error) {
	if sctx != nil && sctx.PRContextAfterStep && step == types.StepPR &&
		sctx.DB != nil && sctx.Run != nil {
		// PRStep persists a newly created URL before returning its outcome.
		// The executor checks the comparison before it copies that outcome to
		// the in-memory run, so read the committed identity first.
		fresh, err := sctx.DB.GetRun(sctx.Run.ID)
		if err != nil {
			return pipeline.PRContextDecision{}, err
		}
		if fresh != nil && fresh.PRURL != nil {
			sctx.Run.PRURL = fresh.PRURL
		}
	}
	if selected, ok := localOnlyPRTarget(sctx); ok {
		return guardPRContextWithSelection(sctx, step, selected)
	}
	host, reason := buildHost(sctx, resolvedProvider(sctx))
	if host == nil {
		return pipeline.PRContextDecision{}, fmt.Errorf("read pull request target: %s", reason)
	}
	if err := host.Available(sctx.Ctx); err != nil {
		return pipeline.PRContextDecision{}, fmt.Errorf("read pull request target: %w", err)
	}
	reader, ok := host.(scm.PRFactsReader)
	if !ok {
		return pipeline.PRContextDecision{}, fmt.Errorf("provider cannot read complete pull request facts")
	}
	retargeter, _ := host.(scm.PRBaseRetargeter)
	selection, err := resolveAndApplyPRTarget(sctx, reader, retargeter)
	if err != nil {
		return pipeline.PRContextDecision{}, err
	}
	return guardPRContextWithSelection(sctx, step, selection)
}

// A per-run flag records a fresh explicit --base-branch request separately
// from an inherited target. It is consumed exactly once: a later forge-side
// retarget remains authoritative instead of being undone at every boundary.
func resolveAndApplyPRTarget(sctx *pipeline.StepContext, reader scm.PRFactsReader, retargeter scm.PRBaseRetargeter) (pipeline.PRTargetSelection, error) {
	selection, err := resolvePRTargetWithReader(sctx, reader)
	if err != nil || !sctx.Run.PRBaseBranchRequested {
		return selection, err
	}
	if sctx.DB == nil {
		return pipeline.PRTargetSelection{}, fmt.Errorf("fresh PR retarget requires durable run state")
	}
	// An unowned PR discovered by source/head is not mutation authority.
	// A new run's --base-branch remains a prospective target only.
	if runPRURL(sctx) == "" {
		return selection, nil
	}
	if sctx.Run.PRBaseBranch == nil {
		return pipeline.PRTargetSelection{}, fmt.Errorf("fresh PR retarget has no requested branch")
	}
	requested, err := ValidateRunPRBaseBranchName(*sctx.Run.PRBaseBranch)
	if err != nil {
		return pipeline.PRTargetSelection{}, fmt.Errorf("invalid fresh PR retarget branch: %w", err)
	}
	if requested == "" {
		return pipeline.PRTargetSelection{}, fmt.Errorf("fresh PR retarget branch is empty")
	}
	localHead, err := git.HeadSHA(sctx.Ctx, sctx.WorkDir)
	if err != nil {
		return pipeline.PRTargetSelection{}, fmt.Errorf("read local head before PR retarget: %w", err)
	}
	if selection.ForgeHeadSHA != localHead {
		return pipeline.PRTargetSelection{}, fmt.Errorf("recorded pull request head differs from local head before retarget")
	}
	if selection.TargetBranch != requested {
		if retargeter == nil {
			return pipeline.PRTargetSelection{}, fmt.Errorf("provider cannot retarget recorded pull request to %s", requested)
		}
		if err := retargeter.SetPRBaseBranch(sctx.Ctx, prFromOwnedURL(selection.PRURL), requested); err != nil {
			return pipeline.PRTargetSelection{}, fmt.Errorf("retarget recorded pull request to %s: %w", requested, err)
		}
		selection, err = resolvePRTargetWithReader(sctx, reader)
		if err != nil {
			return pipeline.PRTargetSelection{}, fmt.Errorf("read back retargeted pull request: %w", err)
		}
		if selection.TargetBranch != requested || selection.ForgeHeadSHA != localHead {
			return pipeline.PRTargetSelection{}, fmt.Errorf("retargeted pull request base or head differs on read-back")
		}
	}
	if err := sctx.DB.ConsumePRBaseBranchRequest(sctx.Run.ID); err != nil {
		return pipeline.PRTargetSelection{}, fmt.Errorf("consume fresh PR retarget request: %w", err)
	}
	sctx.Run.PRBaseBranchRequested = false
	return selection, nil
}

func guardPRContextWithSelection(sctx *pipeline.StepContext, step types.StepName, selection pipeline.PRTargetSelection) (pipeline.PRContextDecision, error) {
	if sctx == nil || sctx.DB == nil || sctx.Run == nil || sctx.Repo == nil {
		return pipeline.PRContextDecision{}, fmt.Errorf("PR context requires a run, repository, and database")
	}
	target, err := ValidateRunPRBaseBranchName(selection.TargetBranch)
	if err != nil {
		return pipeline.PRContextDecision{}, fmt.Errorf("invalid selected PR target: %w", err)
	}
	selection.TargetBranch = target
	if err := fetchRunUpstreamBranch(sctx.Ctx, sctx, target); err != nil {
		return pipeline.PRContextDecision{}, fmt.Errorf("fetch selected PR target %q: %w", target, err)
	}
	targetSHA, err := git.Run(sctx.Ctx, sctx.WorkDir, "rev-parse", "--verify", "refs/remotes/origin/"+target+"^{commit}")
	if err != nil {
		return pipeline.PRContextDecision{}, fmt.Errorf("resolve fetched PR target %q: %w", target, err)
	}
	localHead, err := git.HeadSHA(sctx.Ctx, sctx.WorkDir)
	if err != nil {
		return pipeline.PRContextDecision{}, fmt.Errorf("read local head for PR comparison: %w", err)
	}
	if sctx.Run.ExternalCIOwner == types.ExternalCIOwnerControllerShipPR &&
		step.Order() >= types.StepCI.Order() && selection.PRURL != "" && selection.ForgeHeadSHA != localHead {
		return pipeline.PRContextDecision{}, fmt.Errorf("pull request head %s differs from local head %s for external CI handoff", selection.ForgeHeadSHA, localHead)
	}
	mergeBase, err := git.Run(sctx.Ctx, sctx.WorkDir, "merge-base", targetSHA, localHead)
	if err != nil {
		return pipeline.PRContextDecision{}, fmt.Errorf("compute PR merge base: %w", err)
	}
	// Hash raw patch bytes. Trimming output would collapse distinct patches
	// whose final whitespace differs and undermine the receipt comparison.
	patch, err := git.RunRaw(sctx.Ctx, sctx.WorkDir,
		"diff", "--no-ext-diff", "--no-textconv", "--binary", "--full-index",
		targetSHA+"..."+localHead)
	if err != nil {
		return pipeline.PRContextDecision{}, fmt.Errorf("compute canonical PR diff: %w", err)
	}
	digest := sha256.Sum256(patch)
	candidate := db.PRContextCandidate{
		LocalHeadSHA: localHead, TargetBranch: target,
		TargetSHA: targetSHA, MergeBaseSHA: mergeBase,
		DiffDigest: hex.EncodeToString(digest[:]),
	}
	if selection.SourceRepo != "" {
		candidate.SourceRepo = selection.SourceRepo
		candidate.SourceBranch = selection.SourceBranch
	}
	if selection.PRURL != "" {
		candidate.PRURL = selection.PRURL
		candidate.ForgeHeadSHA = selection.ForgeHeadSHA
	}
	previous, err := sctx.DB.GetRunPRContext(sctx.Run.ID)
	if err != nil {
		return pipeline.PRContextDecision{}, err
	}
	resetFrom := types.StepReview
	if previous == nil || previous.TargetBranch != candidate.TargetBranch ||
		previous.TargetSHA != candidate.TargetSHA ||
		previous.SourceRepo != candidate.SourceRepo || previous.SourceBranch != candidate.SourceBranch ||
		(previous.PRURL != "" && previous.ForgeHeadSHA != candidate.ForgeHeadSHA) {
		resetFrom = types.StepRebase
	}
	forward := false
	if previous != nil && sctx.PRContextAfterStep &&
		previous.TargetBranch == candidate.TargetBranch && previous.TargetSHA == candidate.TargetSHA &&
		previous.SourceRepo == candidate.SourceRepo && previous.SourceBranch == candidate.SourceBranch &&
		previous.PRURL == candidate.PRURL && candidate.LocalHeadSHA == sctx.Run.HeadSHA {
		if step.Order() >= types.StepReview.Order() && step.Order() <= types.StepLint.Order() &&
			previous.ForgeHeadSHA == candidate.ForgeHeadSHA && previous.LocalHeadSHA != localHead {
			_, err := git.Run(sctx.Ctx, sctx.WorkDir, "merge-base", "--is-ancestor", previous.LocalHeadSHA, localHead)
			forward = err == nil
		}
		if step == types.StepPush && previous.LocalHeadSHA == localHead &&
			candidate.ForgeHeadSHA == localHead {
			forward = true
		}
	}
	if previous != nil && previous.PRContextCandidate != candidate && !forward &&
		!sameComparisonExceptNewPRIdentity(previous, candidate) && step.Order() > resetFrom.Order() {
		return pipeline.PRContextDecision{}, fmt.Errorf("PR comparison changed after %s; start a new run for the current target and head", resetFrom)
	}
	var bound db.PRContextBindResult
	if forward {
		bound, err = sctx.DB.AdvanceRunPRContext(sctx.Run.ID, candidate)
	} else {
		bound, err = sctx.DB.BindRunPRContext(sctx.Run.ID, candidate, resetFrom)
	}
	if err != nil {
		return pipeline.PRContextDecision{}, err
	}
	if candidate.PRURL != "" && runPRURL(sctx) == "" {
		url := candidate.PRURL
		sctx.Run.PRURL = &url
	}
	decision := pipeline.PRContextDecision{Target: selection}
	if previous != nil && bound.Changed && !forward && !sameComparisonExceptNewPRIdentity(previous, candidate) {
		if step.Order() >= resetFrom.Order() {
			decision.RestartFrom = resetFrom
		}
		if sctx.Log != nil {
			sctx.Log(fmt.Sprintf("PR context changed at %s; revalidating from %s", step, resetFrom))
		}
	}
	return decision, nil
}

func sameComparisonExceptNewPRIdentity(previous *db.PRContext, next db.PRContextCandidate) bool {
	return previous != nil && previous.PRURL == "" && next.PRURL != "" &&
		strings.EqualFold(next.ForgeHeadSHA, next.LocalHeadSHA) &&
		previous.SourceRepo == next.SourceRepo && previous.SourceBranch == next.SourceBranch &&
		previous.LocalHeadSHA == next.LocalHeadSHA &&
		previous.TargetBranch == next.TargetBranch &&
		previous.TargetSHA == next.TargetSHA &&
		previous.MergeBaseSHA == next.MergeBaseSHA &&
		previous.DiffDigest == next.DiffDigest
}

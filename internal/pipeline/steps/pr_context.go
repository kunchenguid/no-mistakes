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
		return guardPRContextWithSelection(sctx, step, selected, false)
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
	if step == types.StepCI && sctx.PRContextAfterStep {
		current, err := sctx.DB.GetRun(sctx.Run.ID)
		if err != nil {
			return pipeline.PRContextDecision{}, fmt.Errorf("read terminal PR state after CI: %w", err)
		}
		state := sctx.Run.PRState
		if state == nil && current != nil {
			state = current.PRState
		}
		if state != nil && (*state == "merged" || *state == "closed") {
			receipt, err := sctx.DB.GetRunPRContext(sctx.Run.ID)
			if err != nil {
				return pipeline.PRContextDecision{}, fmt.Errorf("read terminal PR comparison after CI: %w", err)
			}
			if receipt == nil || receipt.PRURL == "" || current == nil || current.PRURL == nil || *current.PRURL != receipt.PRURL ||
				current.HeadSHA != receipt.LocalHeadSHA || sctx.Run.HeadSHA != receipt.LocalHeadSHA {
				return pipeline.PRContextDecision{}, fmt.Errorf("terminal CI state lacks a matching PR receipt")
			}
			sctx.Run.PRURL = current.PRURL
			selection, err := resolvePRTargetWithReader(sctx, reader, true)
			if err != nil {
				return pipeline.PRContextDecision{}, err
			}
			if !strings.EqualFold(string(selection.State), *state) {
				return pipeline.PRContextDecision{}, fmt.Errorf("terminal CI state differs from the live PR state")
			}
			if err := verifyCurrentPRComparison(sctx, host, selection, receipt, nil); err != nil {
				return pipeline.PRContextDecision{}, fmt.Errorf("terminal CI comparison: %w", err)
			}
			sctx.Run.PRState = state
			return pipeline.PRContextDecision{Target: selection}, nil
		}
	}
	retargeter, _ := host.(scm.PRBaseRetargeter)
	selection, freshRetarget, err := resolveAndApplyPRTarget(sctx, reader, retargeter, step == types.StepCI)
	if err != nil {
		return pipeline.PRContextDecision{}, err
	}
	if step == types.StepCI && (selection.State == scm.PRStateMerged || selection.State == scm.PRStateClosed) {
		// A merge can land before CI starts or while a parked CI gate is being
		// recovered. Its result must be proven before the owner step runs, too.
		receipt, err := sctx.DB.GetRunPRContext(sctx.Run.ID)
		if err != nil {
			return pipeline.PRContextDecision{}, err
		}
		current, err := sctx.DB.GetRun(sctx.Run.ID)
		if err != nil {
			return pipeline.PRContextDecision{}, err
		}
		if receipt == nil || current == nil || current.PRURL == nil || *current.PRURL != receipt.PRURL ||
			current.HeadSHA != receipt.LocalHeadSHA || sctx.Run.HeadSHA != receipt.LocalHeadSHA {
			return pipeline.PRContextDecision{}, fmt.Errorf("terminal CI entry lacks a matching PR receipt")
		}
		if err := verifyCurrentPRComparison(sctx, host, selection, receipt, nil); err != nil {
			return pipeline.PRContextDecision{}, fmt.Errorf("terminal CI entry comparison: %w", err)
		}
		return pipeline.PRContextDecision{Target: selection}, nil
	}
	return guardPRContextWithSelection(sctx, step, selection, freshRetarget)
}

// A per-run flag records a fresh explicit --base-branch request separately
// from an inherited target. It is consumed exactly once: a later forge-side
// retarget remains authoritative instead of being undone at every boundary.
func resolveAndApplyPRTarget(sctx *pipeline.StepContext, reader scm.PRFactsReader, retargeter scm.PRBaseRetargeter, allowTerminal bool) (pipeline.PRTargetSelection, bool, error) {
	selection, err := resolvePRTargetWithReader(sctx, reader, allowTerminal)
	if err != nil || !sctx.Run.PRBaseBranchRequested {
		return selection, false, err
	}
	if sctx.DB == nil {
		return pipeline.PRTargetSelection{}, false, fmt.Errorf("fresh PR retarget requires durable run state")
	}
	// An unowned PR discovered by source/head is not mutation authority.
	// A new run's --base-branch remains a prospective target only.
	if runPRURL(sctx) == "" {
		return selection, false, nil
	}
	if sctx.Run.PRBaseBranch == nil {
		return pipeline.PRTargetSelection{}, false, fmt.Errorf("fresh PR retarget has no requested branch")
	}
	requested, err := ValidateRunPRBaseBranchName(*sctx.Run.PRBaseBranch)
	if err != nil {
		return pipeline.PRTargetSelection{}, false, fmt.Errorf("invalid fresh PR retarget branch: %w", err)
	}
	if requested == "" {
		return pipeline.PRTargetSelection{}, false, fmt.Errorf("fresh PR retarget branch is empty")
	}
	localHead, err := git.HeadSHA(sctx.Ctx, sctx.WorkDir)
	if err != nil {
		return pipeline.PRTargetSelection{}, false, fmt.Errorf("read local head before PR retarget: %w", err)
	}
	if selection.ForgeHeadSHA != localHead {
		return pipeline.PRTargetSelection{}, false, fmt.Errorf("recorded pull request head differs from local head before retarget")
	}
	freshRetarget := selection.TargetBranch == requested
	if selection.TargetBranch != requested {
		if retargeter == nil {
			return pipeline.PRTargetSelection{}, false, fmt.Errorf("provider cannot retarget recorded pull request to %s", requested)
		}
		if err := retargeter.SetPRBaseBranch(sctx.Ctx, prFromOwnedURL(selection.PRURL), requested); err != nil {
			return pipeline.PRTargetSelection{}, false, fmt.Errorf("retarget recorded pull request to %s: %w", requested, err)
		}
		selection, err = resolvePRTargetWithReader(sctx, reader, allowTerminal)
		if err != nil {
			return pipeline.PRTargetSelection{}, false, fmt.Errorf("read back retargeted pull request: %w", err)
		}
		if selection.TargetBranch != requested || selection.ForgeHeadSHA != localHead {
			return pipeline.PRTargetSelection{}, false, fmt.Errorf("retargeted pull request base or head differs on read-back")
		}
		freshRetarget = true
	}
	if err := sctx.DB.ConsumePRBaseBranchRequest(sctx.Run.ID); err != nil {
		return pipeline.PRTargetSelection{}, false, fmt.Errorf("consume fresh PR retarget request: %w", err)
	}
	sctx.Run.PRBaseBranchRequested = false
	return selection, freshRetarget, nil
}

func guardPRContextWithSelection(sctx *pipeline.StepContext, step types.StepName, selection pipeline.PRTargetSelection, freshRetarget bool) (pipeline.PRContextDecision, error) {
	if sctx == nil || sctx.DB == nil || sctx.Run == nil || sctx.Repo == nil {
		return pipeline.PRContextDecision{}, fmt.Errorf("PR context requires a run, repository, and database")
	}
	candidate, err := readPRComparison(sctx, selection)
	if err != nil {
		return pipeline.PRContextDecision{}, err
	}
	selection.TargetBranch = candidate.TargetBranch
	localHead := candidate.LocalHeadSHA
	if step == types.StepPR && selection.ForgeHeadSHA != "" && selection.ForgeHeadSHA != localHead {
		return pipeline.PRContextDecision{}, fmt.Errorf("pull request head %s differs from local head %s before PR publication", selection.ForgeHeadSHA, localHead)
	}
	if sctx.Run.ExternalCIOwner == types.ExternalCIOwnerControllerShipPR &&
		step.Order() >= types.StepCI.Order() && selection.PRURL != "" && selection.ForgeHeadSHA != localHead {
		return pipeline.PRContextDecision{}, fmt.Errorf("pull request head %s differs from local head %s for external CI handoff", selection.ForgeHeadSHA, localHead)
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
	if previous != nil && previous.SourceBranch == candidate.SourceBranch &&
		scm.SameSourceRepository(resolvedProvider(sctx), previous.SourceRepo, candidate.SourceRepo) {
		candidate.SourceRepo = previous.SourceRepo
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
	// Push and a later CI repair can publish a descendant of the last bound
	// head. The durable publication record attributes that advance to this run;
	// the receipt follows it without replaying completed steps.
	pipelineOwnedPublicationAdvance := false
	publicationBoundary := (sctx.PRContextAfterStep && step == types.StepPush) || step == types.StepCI
	if previous != nil && publicationBoundary &&
		previous.LocalHeadSHA != candidate.LocalHeadSHA &&
		previous.TargetBranch == candidate.TargetBranch && previous.TargetSHA == candidate.TargetSHA &&
		previous.SourceRepo == candidate.SourceRepo && previous.SourceBranch == candidate.SourceBranch &&
		(previous.PRURL == "" || previous.PRURL == candidate.PRURL) {
		current, err := sctx.DB.GetRun(sctx.Run.ID)
		if err != nil {
			return pipeline.PRContextDecision{}, fmt.Errorf("read pipeline-published head: %w", err)
		}
		pipelineOwnedPublicationAdvance = current != nil && current.HeadSHA == candidate.LocalHeadSHA &&
			current.LastPushedSHA != nil && *current.LastPushedSHA == candidate.LocalHeadSHA
		if pipelineOwnedPublicationAdvance {
			_, err := git.Run(sctx.Ctx, sctx.WorkDir, "merge-base", "--is-ancestor", previous.LocalHeadSHA, candidate.LocalHeadSHA)
			pipelineOwnedPublicationAdvance = err == nil
		}
	}
	forward = forward || pipelineOwnedPublicationAdvance
	if previous != nil && previous.PRContextCandidate != candidate && !forward && !freshRetarget &&
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
	if previous != nil && bound.Changed {
		if !forward && !sameComparisonExceptNewPRIdentity(previous, candidate) && step.Order() >= resetFrom.Order() {
			decision.RestartFrom = resetFrom
		}
		if decision.RestartFrom != "" && sctx.Log != nil {
			sctx.Log(fmt.Sprintf("PR context changed at %s; revalidating from %s", step, decision.RestartFrom))
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

// readPRComparison always fetches the actual target. A terminal PR does not
// turn a cached target ref into current comparison evidence.
func readPRComparison(sctx *pipeline.StepContext, selection pipeline.PRTargetSelection) (db.PRContextCandidate, error) {
	target, err := ValidateRunPRBaseBranchName(selection.TargetBranch)
	if err != nil {
		return db.PRContextCandidate{}, fmt.Errorf("invalid selected PR target: %w", err)
	}
	if target == "" {
		return db.PRContextCandidate{}, fmt.Errorf("selected PR target is empty")
	}
	if err := fetchRunUpstreamBranch(sctx.Ctx, sctx, target); err != nil {
		return db.PRContextCandidate{}, fmt.Errorf("fetch selected PR target %q: %w", target, err)
	}
	targetSHA, err := git.Run(sctx.Ctx, sctx.WorkDir, "rev-parse", "--verify", "refs/remotes/origin/"+target+"^{commit}")
	if err != nil {
		return db.PRContextCandidate{}, fmt.Errorf("resolve fetched PR target %q: %w", target, err)
	}
	localHead, err := git.HeadSHA(sctx.Ctx, sctx.WorkDir)
	if err != nil {
		return db.PRContextCandidate{}, fmt.Errorf("read local head for PR comparison: %w", err)
	}
	return prComparisonAtTarget(sctx, target, targetSHA, localHead)
}

func prComparisonAtTarget(sctx *pipeline.StepContext, target, targetSHA, localHead string) (db.PRContextCandidate, error) {
	mergeBase, err := git.Run(sctx.Ctx, sctx.WorkDir, "merge-base", targetSHA, localHead)
	if err != nil {
		return db.PRContextCandidate{}, fmt.Errorf("compute PR merge base: %w", err)
	}
	// Hash raw bytes, including the patch's trailing whitespace.
	patch, err := git.RunRaw(sctx.Ctx, sctx.WorkDir, "diff", "--no-ext-diff", "--no-textconv", "--binary", "--full-index", targetSHA+"..."+localHead)
	if err != nil {
		return db.PRContextCandidate{}, fmt.Errorf("compute canonical PR diff: %w", err)
	}
	digest := sha256.Sum256(patch)
	return db.PRContextCandidate{LocalHeadSHA: localHead, TargetBranch: target, TargetSHA: targetSHA, MergeBaseSHA: mergeBase, DiffDigest: hex.EncodeToString(digest[:])}, nil
}

func verifyPRMutationComparison(sctx *pipeline.StepContext, host scm.Host, pr *scm.PR, proposedHead string, allowProposed bool) error {
	if sctx == nil || sctx.DB == nil || sctx.Run == nil || pr == nil {
		return fmt.Errorf("PR update lacks a run or comparison receipt")
	}
	receipt, err := sctx.DB.GetRunPRContext(sctx.Run.ID)
	if err != nil {
		return err
	}
	if receipt == nil || receipt.PRURL == "" || !samePRIdentity(receipt.PRURL, pr) ||
		proposedHead == "" || sctx.Run.HeadSHA != receipt.LocalHeadSHA ||
		(!allowProposed && proposedHead != receipt.LocalHeadSHA) {
		return fmt.Errorf("PR update lacks the current run comparison")
	}
	durable, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		return err
	}
	if durable == nil || durable.HeadSHA != receipt.LocalHeadSHA {
		return fmt.Errorf("durable run head differs from PR comparison receipt before update")
	}
	if allowProposed {
		dirty, err := git.HasUncommittedChanges(sctx.Ctx, sctx.WorkDir)
		if err != nil {
			return fmt.Errorf("check worktree before PR attestation update: %w", err)
		}
		if dirty {
			return fmt.Errorf("worktree is dirty before PR attestation update")
		}
	}
	reader, ok := host.(scm.PRFactsReader)
	if !ok {
		return fmt.Errorf("provider cannot read complete pull request facts before update")
	}
	facts, err := reader.ReadPRFacts(sctx.Ctx, pr)
	if err != nil {
		return fmt.Errorf("read pull request before update: %w", err)
	}
	if facts.State != scm.PRStateOpen || !samePRIdentity(receipt.PRURL, &facts.PR) ||
		!scm.SameSourceRepository(resolvedProvider(sctx), facts.SourceRepository, receipt.SourceRepo) ||
		facts.SourceBranch != receipt.SourceBranch || facts.HeadSHA != receipt.ForgeHeadSHA ||
		facts.BaseBranch != receipt.TargetBranch {
		return fmt.Errorf("live pull request differs from its comparison receipt before update")
	}
	selection := pipeline.PRTargetSelection{TargetBranch: receipt.TargetBranch}
	current, err := readPRComparison(sctx, selection)
	if err != nil {
		return err
	}
	if current.LocalHeadSHA != proposedHead {
		return fmt.Errorf("local head differs from proposed PR update head")
	}
	if proposedHead != receipt.LocalHeadSHA {
		if _, err := git.Run(sctx.Ctx, sctx.WorkDir, "merge-base", "--is-ancestor", receipt.LocalHeadSHA, proposedHead); err != nil {
			return fmt.Errorf("proposed PR update head does not descend from comparison receipt: %w", err)
		}
		current, err = prComparisonAtTarget(sctx, receipt.TargetBranch, current.TargetSHA, receipt.LocalHeadSHA)
		if err != nil {
			return err
		}
	}
	expected := receipt.PRContextCandidate
	expected.PRURL, expected.SourceRepo, expected.SourceBranch, expected.ForgeHeadSHA = "", "", "", ""
	if current != expected {
		return fmt.Errorf("live target commit, merge base, or diff differs from PR comparison receipt before update")
	}
	return nil
}

// verifyCurrentPRComparison is read-only: owner evidence and terminal CI must
// validate the receipt, never replace it with the post-merge empty diff.
func verifyCurrentPRComparison(sctx *pipeline.StepContext, host scm.Host, selection pipeline.PRTargetSelection, receipt *db.PRContext, facts *scm.PRFacts) error {
	if receipt == nil || receipt.PRURL == "" || selection.PRURL != receipt.PRURL ||
		!scm.SameSourceRepository(resolvedProvider(sctx), selection.SourceRepo, receipt.SourceRepo) ||
		selection.SourceBranch != receipt.SourceBranch || selection.ForgeHeadSHA != receipt.ForgeHeadSHA ||
		selection.ForgeHeadSHA != receipt.LocalHeadSHA || selection.TargetBranch != receipt.TargetBranch {
		return fmt.Errorf("live PR identity, head, or target differs from its comparison receipt")
	}
	current, err := readPRComparison(sctx, selection)
	if err != nil {
		return err
	}
	if current.LocalHeadSHA != receipt.LocalHeadSHA {
		return fmt.Errorf("local head differs from PR comparison receipt")
	}
	expected := receipt.PRContextCandidate
	expected.PRURL, expected.SourceRepo, expected.SourceBranch, expected.ForgeHeadSHA = "", "", "", ""
	if selection.State != scm.PRStateMerged {
		if current != expected {
			return fmt.Errorf("live target commit, merge base, or diff differs from PR comparison receipt")
		}
		return nil
	}
	// Merging changes the target, so recheck the original immutable comparison
	// and separately prove its exact result landed on the fetched target.
	original, err := prComparisonAtTarget(sctx, receipt.TargetBranch, receipt.TargetSHA, receipt.LocalHeadSHA)
	if err != nil {
		return err
	}
	if original != expected {
		return fmt.Errorf("original merged PR comparison differs from its receipt")
	}
	if facts == nil {
		reader, ok := host.(scm.PRFactsReader)
		if !ok {
			return fmt.Errorf("provider cannot read merged PR facts")
		}
		observed, err := reader.ReadPRFacts(sctx.Ctx, prFromOwnedURL(receipt.PRURL))
		if err != nil {
			return fmt.Errorf("read merged PR evidence: %w", err)
		}
		facts = &observed
	}
	if facts.State != scm.PRStateMerged || facts.PR.URL != receipt.PRURL ||
		facts.HeadSHA != receipt.LocalHeadSHA || facts.SourceBranch != receipt.SourceBranch ||
		!scm.SameSourceRepository(resolvedProvider(sctx), facts.SourceRepository, receipt.SourceRepo) || facts.BaseBranch != receipt.TargetBranch || facts.MergeCommitSHA == "" {
		return fmt.Errorf("merged PR evidence differs from its comparison receipt")
	}
	mergeSHA := facts.MergeCommitSHA
	if proofHost, ok := host.(scm.MergedProofHost); ok {
		pr := prFromOwnedURL(receipt.PRURL)
		proof, err := proofHost.GetMergedProof(sctx.Ctx, pr, receipt.LocalHeadSHA)
		if err != nil {
			return fmt.Errorf("read exact merged PR proof: %w", err)
		}
		if !proof.Merged || proof.Number != pr.Number || proof.URL != receipt.PRURL || proof.HeadSHA != receipt.LocalHeadSHA || proof.MergeCommitSHA != mergeSHA || proof.MergedAt.IsZero() || proof.MergedBy == "" {
			return fmt.Errorf("merged PR proof differs from its comparison receipt")
		}
	} else if host.Capabilities().MergedProof {
		return fmt.Errorf("provider advertises merged proof without implementing it")
	}
	resolvedMerge, err := git.Run(sctx.Ctx, sctx.WorkDir, "rev-parse", "--verify", mergeSHA+"^{commit}")
	if err != nil || resolvedMerge != mergeSHA {
		return fmt.Errorf("merged PR result is not an exact commit")
	}
	if _, err := git.Run(sctx.Ctx, sctx.WorkDir, "merge-base", "--is-ancestor", mergeSHA, current.TargetSHA); err != nil {
		return fmt.Errorf("merged PR result is absent from current target: %w", err)
	}
	if _, err := git.Run(sctx.Ctx, sctx.WorkDir, "merge-base", "--is-ancestor", receipt.TargetSHA, mergeSHA); err != nil {
		return fmt.Errorf("merged PR result does not contain the reviewed target: %w", err)
	}
	mergeTree, err := git.Run(sctx.Ctx, sctx.WorkDir, "merge-tree", "--write-tree", receipt.TargetSHA, receipt.LocalHeadSHA)
	if err != nil {
		return fmt.Errorf("cannot prove the reviewed merge result: %w", err)
	}
	actualTree, err := git.Run(sctx.Ctx, sctx.WorkDir, "rev-parse", mergeSHA+"^{tree}")
	if err != nil || mergeTree != actualTree {
		return fmt.Errorf("merged PR tree differs from the reviewed comparison result")
	}
	return nil
}

package gate

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/git"
)

// StaleBranchReconciliation reports a private gate branch that was archived
// and removed so the caller can submit the live head with an ordinary push.
type StaleBranchReconciliation struct {
	Reconciled   bool
	PreviousHead string
	ArchivedTag  string
}

// StaleBranchPlan is the verdict of a non-mutating stale-branch inspection.
// Planning proves containment and refuses unique private content without
// touching any ref, so a caller can decide before it publishes anything;
// applying the plan is the only step that archives and removes the branch.
type StaleBranchPlan struct {
	PreserveDescendantOf string
	Reconcile            bool
	Branch               string
	BranchRef            string
	PreviousHead         string
	ArchiveTag           string
}

// ReconcileStaleBranch plans and immediately applies stale private gate branch
// reconciliation. It removes the branch only after Git proves the live head
// contains all of its content, or after runOwnedHead proves the branch still
// carries this run's own submission.
func ReconcileStaleBranch(ctx context.Context, gateDir, workDir, branch, liveHead, runOwnedHead string) (StaleBranchReconciliation, error) {
	plan, err := PlanStaleBranchReconciliation(ctx, gateDir, workDir, branch, liveHead, runOwnedHead)
	if err != nil || !plan.Reconcile {
		return StaleBranchReconciliation{}, err
	}
	return ApplyStaleBranchReconciliation(ctx, gateDir, plan)
}

// PlanStaleBranchReconciliation inspects a private gate branch and reports
// whether it must be archived and removed before the live head can enter
// through an ordinary push. It mutates no ref: an unproven private head is
// refused here, before the caller publishes anything.
//
// Containment is proven by direct ancestry, by Git's stable per-file patch-id
// comparison for rewritten histories, or by runOwnedHead. The last is the
// exact commit this run was launched from: the run already owns and is
// republishing that content, so requiring its rebased lineage to also be
// patch-identical would refuse the pipeline's own ordinary rebase whenever
// context drift or an agent-resolved conflict changed the patch. The head is
// still archived before removal, so the commit remains recoverable.
func PlanStaleBranchReconciliation(ctx context.Context, gateDir, workDir, branch, liveHead, runOwnedHead string) (StaleBranchPlan, error) {
	return planStaleBranchReconciliation(ctx, gateDir, workDir, branch, liveHead, runOwnedHead, false)
}

func PlanMirrorPublicationReconciliation(ctx context.Context, gateDir, workDir, branch, liveHead, runOwnedHead string) (StaleBranchPlan, error) {
	return planStaleBranchReconciliation(ctx, gateDir, workDir, branch, liveHead, runOwnedHead, true)
}

func planStaleBranchReconciliation(ctx context.Context, gateDir, workDir, branch, liveHead, runOwnedHead string, preserveDescendants bool) (StaleBranchPlan, error) {
	var plan StaleBranchPlan
	branch = strings.TrimSpace(branch)
	liveHead = strings.TrimSpace(liveHead)
	runOwnedHead = strings.TrimSpace(runOwnedHead)
	if branch == "" || liveHead == "" {
		return plan, fmt.Errorf("reconcile stale gate branch: branch and live head are required")
	}
	if err := git.ValidateBareRepository(ctx, gateDir); err != nil {
		return plan, fmt.Errorf("reconcile stale gate branch: %w", err)
	}
	if _, err := git.Run(ctx, workDir, "check-ref-format", "--branch", branch); err != nil {
		return plan, fmt.Errorf("reconcile stale gate branch %q: invalid branch name: %w", branch, err)
	}
	resolvedLive, err := git.Run(ctx, workDir, "rev-parse", "--verify", liveHead+"^{commit}")
	if err != nil || resolvedLive != liveHead {
		return plan, fmt.Errorf("reconcile stale gate branch %s: live head %s is not an exact commit", branch, liveHead)
	}
	workDir, err = filepath.Abs(workDir)
	if err != nil {
		return plan, fmt.Errorf("reconcile stale gate branch %s: resolve worktree path: %w", branch, err)
	}
	branchRef := "refs/heads/" + branch
	gateHead, exists, err := git.DirectRefTarget(ctx, gateDir, branchRef)
	if err != nil {
		return plan, fmt.Errorf("inspect private mirror ref %s: %w", branchRef, err)
	}
	if !exists {
		return plan, nil
	}
	archiveTag := "refs/tags/no-mistakes-abandoned/" + branch + "/" + gateHead
	archivedHead, archived, err := git.DirectRefTarget(ctx, gateDir, archiveTag)
	if err != nil {
		return plan, fmt.Errorf("inspect private mirror archive tag %s: %w", archiveTag, err)
	}
	if archived && archivedHead != gateHead {
		return plan, fmt.Errorf("private mirror archive tag %s already points at %s, not %s", archiveTag, archivedHead, gateHead)
	}
	if gateHead == liveHead {
		return plan, nil
	}
	if objectType, err := git.Run(ctx, gateDir, "cat-file", "-t", gateHead); err != nil || objectType != "commit" {
		return plan, fmt.Errorf("private mirror ref %s does not point at a commit", branchRef)
	}
	if err := git.FetchRemoteRef(ctx, gateDir, workDir, liveHead, liveHead); err != nil {
		return plan, fmt.Errorf("stage live head for private mirror reconciliation: %w", err)
	}

	// An ancestor needs no reconciliation: the caller's ordinary push is
	// already a fast-forward and preserves the private head by ancestry.
	if _, err := git.Run(ctx, gateDir, "merge-base", "--is-ancestor", gateHead, liveHead); err == nil {
		return plan, nil
	}
	if preserveDescendants {
		plan.PreserveDescendantOf = liveHead
		if _, err := git.Run(ctx, gateDir, "merge-base", "--is-ancestor", liveHead, gateHead); err == nil {
			return plan, nil
		}
	}
	if gateHead != runOwnedHead {
		atRiskCommits, err := privateCommitsAbsentFromLive(ctx, gateDir, liveHead, gateHead)
		if err != nil {
			return plan, fmt.Errorf("compare private mirror content for %s: %w", branchRef, err)
		}
		if len(atRiskCommits) > 0 {
			atRisk := make([]string, 0, len(atRiskCommits))
			for _, commit := range atRiskCommits {
				description, describeErr := git.Run(ctx, gateDir, "show", "-s", "--format=%H %s", commit)
				if describeErr != nil {
					return plan, fmt.Errorf("describe at-risk private mirror commit %s: %w", commit, describeErr)
				}
				atRisk = append(atRisk, description)
			}
			return plan, fmt.Errorf(
				"refusing to reconcile private mirror ref %s: %d at-risk commit(s) contain content absent from live head %s: %s",
				branchRef, len(atRisk), liveHead, strings.Join(atRisk, "; "),
			)
		}
	}

	return StaleBranchPlan{
		PreserveDescendantOf: plan.PreserveDescendantOf,
		Reconcile:            true,
		Branch:               branch,
		BranchRef:            branchRef,
		PreviousHead:         gateHead,
		ArchiveTag:           archiveTag,
	}, nil
}

// ApplyStaleBranchReconciliation archives the planned head and then removes the
// branch ref. It revalidates that the branch still points at the exact head the
// plan proved, so a private head that appeared after planning is never deleted.
func ApplyStaleBranchReconciliation(ctx context.Context, gateDir string, plan StaleBranchPlan) (StaleBranchReconciliation, error) {
	var result StaleBranchReconciliation
	if !plan.Reconcile {
		return result, nil
	}
	if plan.BranchRef == "" || plan.PreviousHead == "" || plan.ArchiveTag == "" {
		return result, fmt.Errorf("apply private mirror reconciliation: incomplete plan for %q", plan.Branch)
	}
	if err := git.ValidateBareRepository(ctx, gateDir); err != nil {
		return result, fmt.Errorf("apply private mirror reconciliation: %w", err)
	}
	currentHead, exists, err := git.DirectRefTarget(ctx, gateDir, plan.BranchRef)
	if err != nil {
		return result, fmt.Errorf("inspect private mirror ref %s: %w", plan.BranchRef, err)
	}
	if !exists {
		return result, nil
	}
	if currentHead != plan.PreviousHead {
		if plan.PreserveDescendantOf != "" {
			if _, err := git.Run(ctx, gateDir, "merge-base", "--is-ancestor", plan.PreserveDescendantOf, currentHead); err == nil {
				return result, nil
			}
		}
		return result, fmt.Errorf(
			"private mirror ref %s moved to %s after it was proven stale at %s",
			plan.BranchRef, currentHead, plan.PreviousHead,
		)
	}
	archivedHead, archived, err := git.DirectRefTarget(ctx, gateDir, plan.ArchiveTag)
	if err != nil {
		return result, fmt.Errorf("inspect private mirror archive tag %s: %w", plan.ArchiveTag, err)
	}
	if archived && archivedHead != plan.PreviousHead {
		return result, fmt.Errorf("private mirror archive tag %s already points at %s, not %s", plan.ArchiveTag, archivedHead, plan.PreviousHead)
	}
	if !archived {
		if _, err := git.Run(ctx, gateDir, "update-ref", "--no-deref", plan.ArchiveTag, plan.PreviousHead, strings.Repeat("0", len(plan.PreviousHead))); err != nil {
			return result, fmt.Errorf("archive stale private mirror head %s at %s: %w", plan.PreviousHead, plan.ArchiveTag, err)
		}
	}
	if _, err := git.Run(ctx, gateDir, "update-ref", "--no-deref", "-d", plan.BranchRef, plan.PreviousHead); err != nil {
		return result, fmt.Errorf("delete archived stale private mirror ref %s at %s: %w", plan.BranchRef, plan.PreviousHead, err)
	}
	return StaleBranchReconciliation{Reconciled: true, PreviousHead: plan.PreviousHead, ArchivedTag: plan.ArchiveTag}, nil
}

func RestoreReconciledBranch(ctx context.Context, gateDir, branch string, result StaleBranchReconciliation) error {
	if !result.Reconciled {
		return nil
	}
	if err := git.ValidateBareRepository(ctx, gateDir); err != nil {
		return err
	}
	if !ArchivedHeadRecorded(ctx, gateDir, branch, result.PreviousHead) {
		return fmt.Errorf("restore private mirror %q: archived head %s is unavailable", branch, result.PreviousHead)
	}
	ref := "refs/heads/" + branch
	if _, exists, err := git.DirectRefTarget(ctx, gateDir, ref); err != nil || exists {
		return err
	}
	if _, err := git.Run(ctx, gateDir, "update-ref", "--no-deref", ref, result.PreviousHead, strings.Repeat("0", len(result.PreviousHead))); err != nil {
		if _, exists, readErr := git.DirectRefTarget(ctx, gateDir, ref); readErr == nil && exists {
			return nil
		}
		return fmt.Errorf("restore private mirror %s: %w", ref, err)
	}
	return nil
}

// ArchivedHeadRecorded reports whether head is the exact commit archived for
// branch by a prior reconciliation. It is the gate's own evidence that a
// caller-reported pre-reconciliation head is genuine.
func ArchivedHeadRecorded(ctx context.Context, gateDir, branch, head string) bool {
	branch = strings.TrimSpace(branch)
	head = strings.TrimSpace(head)
	if branch == "" || head == "" {
		return false
	}
	if _, err := git.Run(ctx, gateDir, "check-ref-format", "--branch", branch); err != nil {
		return false
	}
	tag := "refs/tags/no-mistakes-abandoned/" + branch + "/" + head
	archivedHead, archived, err := git.DirectRefTarget(ctx, gateDir, tag)
	if err != nil || !archived || archivedHead != head {
		return false
	}
	objectType, err := git.Run(ctx, gateDir, "cat-file", "-t", head)
	return err == nil && objectType == "commit"
}

// privateCommitsAbsentFromLive names every private-only commit whose per-file
// content is not also present in the live-only history.
//
// The private side is computed first so the live scan can be bounded to the
// paths the private commits actually touch, and it short-circuits on the first
// unmatched patch. A rebased live head otherwise carries every default-branch
// commit since the merge base, and hashing each of those files would cost
// thousands of git invocations to answer a question about a handful of paths.
func privateCommitsAbsentFromLive(ctx context.Context, repoDir, liveHead, privateHead string) ([]string, error) {
	privateOnly, err := commitList(ctx, repoDir, "--right-only", liveHead+"..."+privateHead)
	if err != nil {
		return nil, err
	}
	if len(privateOnly) == 0 {
		return nil, nil
	}

	type privateCommit struct {
		sha        string
		patches    []string
		comparable bool
	}
	privateCommits := make([]privateCommit, 0, len(privateOnly))
	paths := make(map[string]bool)
	for _, commit := range privateOnly {
		patches, comparable, err := perFilePatchIDs(ctx, repoDir, commit)
		if err != nil {
			return nil, err
		}
		privateCommits = append(privateCommits, privateCommit{sha: commit, patches: patches, comparable: comparable})
		if !comparable {
			continue
		}
		for _, patch := range patches {
			path, _, ok := strings.Cut(patch, "\x00")
			if ok {
				paths[path] = true
			}
		}
	}

	livePatches, err := liveSidePatchIDs(ctx, repoDir, liveHead, privateHead, paths)
	if err != nil {
		return nil, err
	}

	var atRisk []string
	for _, commit := range privateCommits {
		if !commit.comparable {
			atRisk = append(atRisk, commit.sha)
			continue
		}
		remaining := make(map[string]int, len(livePatches))
		for patch, count := range livePatches {
			remaining[patch] = count
		}
		represented := true
		for _, patch := range commit.patches {
			if remaining[patch] == 0 {
				represented = false
				break
			}
			remaining[patch]--
		}
		if !represented {
			atRisk = append(atRisk, commit.sha)
			continue
		}
		for patch, count := range remaining {
			livePatches[patch] = count
		}
	}
	mergedTree, mergeErr := git.Run(ctx, repoDir, "merge-tree", "--write-tree", liveHead, privateHead)
	if mergeErr != nil {
		return privateOnly, nil
	}
	liveTree, err := git.Run(ctx, repoDir, "rev-parse", "--verify", liveHead+"^{tree}")
	if err != nil {
		return nil, err
	}
	if mergedTree != liveTree {
		return privateOnly, nil
	}
	return atRisk, nil
}

// liveSidePatchIDs collects per-file patch identities from the live-only
// history, restricted to the paths the private side needs proven.
func liveSidePatchIDs(ctx context.Context, repoDir, liveHead, privateHead string, paths map[string]bool) (map[string]int, error) {
	livePatches := make(map[string]int)
	if len(paths) == 0 {
		return livePatches, nil
	}
	args := []string{"--full-history", "--left-only", liveHead + "..." + privateHead, "--"}
	for path := range paths {
		args = append(args, ":(literal)"+path)
	}
	liveOnly, err := commitList(ctx, repoDir, args...)
	if err != nil {
		return nil, err
	}
	for _, commit := range liveOnly {
		patches, comparable, err := perFilePatchIDs(ctx, repoDir, commit)
		if err != nil {
			return nil, err
		}
		if !comparable {
			continue
		}
		for _, patch := range patches {
			path, _, ok := strings.Cut(patch, "\x00")
			if !ok || !paths[path] {
				continue
			}
			livePatches[patch]++
		}
	}
	return livePatches, nil
}

func commitList(ctx context.Context, repoDir string, args ...string) ([]string, error) {
	out, err := git.Run(ctx, repoDir, append([]string{"rev-list"}, args...)...)
	if err != nil {
		return nil, err
	}
	return strings.Fields(out), nil
}

func perFilePatchIDs(ctx context.Context, repoDir, commit string) ([]string, bool, error) {
	parentLine, err := git.Run(ctx, repoDir, "rev-list", "--parents", "-n", "1", commit)
	if err != nil {
		return nil, false, err
	}
	parents := strings.Fields(parentLine)
	if len(parents) > 2 {
		// A merge's combined meaning is not safely represented by first-parent
		// patches. It remains at risk unless direct ancestry proved containment.
		return nil, false, nil
	}
	parent := git.EmptyTreeSHA
	if len(parents) == 2 {
		parent = parents[1]
	}
	rawPaths, err := git.RunRaw(ctx, repoDir, "diff-tree", "--root", "--no-commit-id", "--name-only", "--no-renames", "-r", "-z", commit)
	if err != nil {
		return nil, false, err
	}
	var patches []string
	for _, rawPath := range strings.Split(strings.TrimSuffix(string(rawPaths), "\x00"), "\x00") {
		if rawPath == "" {
			continue
		}
		patchID, err := git.StablePatchID(ctx, repoDir, parent, commit, rawPath)
		if err != nil {
			return nil, false, err
		}
		if patchID != "" {
			patches = append(patches, rawPath+"\x00"+patchID)
		}
	}
	return patches, true, nil
}

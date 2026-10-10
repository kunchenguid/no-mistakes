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
// Planning checks containment, recovery-anchor preservation, or the exact
// run-owned-head policy exception without touching any ref, so a caller can
// decide before it publishes anything; applying the plan is the only step that
// archives and removes the branch.
type StaleBranchPlan struct {
	PreserveDescendantOf string
	Reconcile            bool
	Branch               string
	BranchRef            string
	PreviousHead         string
	ArchiveTag           string
	// PreservedByRecovery names the recovery anchor that keeps each
	// private-only commit reachable for commits this plan excluded from the
	// at-risk set on that basis. Keyed by full commit ID. Empty when no commit
	// needed the recovery-anchor credit.
	PreservedByRecovery map[string]string
}

// ReconcileStaleBranch plans and immediately applies stale private gate branch
// reconciliation. It removes the branch only after Git proves the live head
// contains all of its content or a recovery anchor preserves each private-only
// commit that proof misses, or under the exact run-owned-head exception
// described in docs/src/content/docs/concepts/gate-model.md.
func ReconcileStaleBranch(ctx context.Context, gateDir, workDir, branch, liveHead, runOwnedHead string) (StaleBranchReconciliation, error) {
	plan, err := PlanStaleBranchReconciliation(ctx, gateDir, workDir, branch, liveHead, runOwnedHead)
	if err != nil || !plan.Reconcile {
		return StaleBranchReconciliation{}, err
	}
	return ApplyStaleBranchReconciliation(ctx, gateDir, plan)
}

// PlanStaleBranchReconciliation inspects a private gate branch and reports
// whether it must be archived and removed before the live head can enter
// through an ordinary push. It mutates no ref: outside the run-owned-head
// exception, a private head carrying a private-only commit that is neither
// proven to survive nor preserved by a recovery anchor is refused before
// publication.
//
// Rewritten histories require both stable per-file patch identities and final
// tree survival. runOwnedHead is a policy exception, not containment evidence:
// a fresh submission supplies it only as the exact submitted head of the
// terminal, never-published run that released the branch
// (internal/cli/axi_drive.go releasedRunSubmittedHeadForFreshRun) and leaves it
// empty otherwise; pipeline publication goes through
// PlanMirrorPublicationReconciliation instead. The contract and rationale are
// owned by docs/src/content/docs/concepts/gate-model.md (Private mirror
// reconciliation).
func PlanStaleBranchReconciliation(ctx context.Context, gateDir, workDir, branch, liveHead, runOwnedHead string) (StaleBranchPlan, error) {
	return planStaleBranchReconciliation(ctx, gateDir, workDir, branch, liveHead, false, runOwnedHead)
}

// PlanMirrorPublicationReconciliation is the pipeline-publication variant of
// PlanStaleBranchReconciliation: it also leaves a newer descendant of the live
// head in place. runOwnedHeads are the exact heads the publishing run itself
// placed on the private mirror - Run.SubmittedHeadSHA and, once the run has
// published, its durable Run.LastPushedSHA - and nothing else. Neither an
// agent-created head nor any other recorded head is eligible; every other
// mirror head still needs the full preservation proof or the recovery-anchor
// preservation credit.
func PlanMirrorPublicationReconciliation(ctx context.Context, gateDir, workDir, branch, liveHead string, runOwnedHeads ...string) (StaleBranchPlan, error) {
	return planStaleBranchReconciliation(ctx, gateDir, workDir, branch, liveHead, true, runOwnedHeads...)
}

func planStaleBranchReconciliation(ctx context.Context, gateDir, workDir, branch, liveHead string, preserveDescendants bool, runOwnedHeads ...string) (StaleBranchPlan, error) {
	var plan StaleBranchPlan
	branch = strings.TrimSpace(branch)
	liveHead = strings.TrimSpace(liveHead)
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
	if !isRunOwnedHead(gateHead, runOwnedHeads) {
		atRiskCommits, err := privateCommitsAbsentFromLive(ctx, gateDir, liveHead, gateHead)
		if err != nil {
			return plan, fmt.Errorf("compare private mirror content for %s: %w", branchRef, err)
		}
		// Recovery anchors are the tool's own sanctioned preservation record:
		// terminalization pins every verified unpublished head at
		// refs/no-mistakes/recover/<run>, and PreserveRecoveryAnchor never
		// replaces evidence (a conflicting anchor fails closed). A commit that
		// one of those anchors keeps reachable is not at risk of being lost, so
		// it is re-derived out of the at-risk set instead of deadlocking the
		// recover -> rerun -> push loop that wrote the anchor in the first
		// place. This is a preservation credit, not containment evidence: the
		// surviving content still has to be proven by the checks above for any
		// commit no anchor holds.
		satisfiers := reconcileSatisfiers(runOwnedHeads)
		preserved, err := preservedByRecoveryAnchors(ctx, gateDir, liveHead, atRiskCommits)
		if err != nil {
			return plan, fmt.Errorf("inspect recovery anchors for %s: %w", branchRef, err)
		}
		remaining := make([]string, 0, len(atRiskCommits))
		for _, commit := range atRiskCommits {
			if _, ok := preserved[commit]; !ok {
				remaining = append(remaining, commit)
			}
		}
		if len(remaining) > 0 {
			atRisk := make([]string, 0, len(remaining))
			for _, commit := range remaining {
				description, describeErr := git.Run(ctx, gateDir, "show", "-s", "--format=%H %s", commit)
				if describeErr != nil {
					return plan, fmt.Errorf("describe at-risk private mirror commit %s: %w", commit, describeErr)
				}
				atRisk = append(atRisk, description)
			}
			return plan, fmt.Errorf(
				"refusing to reconcile private mirror ref %s: %d at-risk commit(s) contain content absent from live head %s: %s. "+
					"Clears on any of: %s",
				branchRef, len(remaining), liveHead, strings.Join(atRisk, "; "),
				joinSatisfiers(satisfiers),
			)
		}
		plan.PreservedByRecovery = preserved
	}

	return StaleBranchPlan{
		PreserveDescendantOf: plan.PreserveDescendantOf,
		Reconcile:            true,
		Branch:               branch,
		BranchRef:            branchRef,
		PreviousHead:         gateHead,
		ArchiveTag:           archiveTag,
		PreservedByRecovery:  plan.PreservedByRecovery,
	}, nil
}

// isRunOwnedHead reports whether the private mirror head is exactly one of the
// run-owned heads. Matching is on the full object ID only: an abbreviated or
// empty entry never matches, so an unknown owner cannot widen the exception.
func isRunOwnedHead(gateHead string, runOwnedHeads []string) bool {
	for _, owned := range runOwnedHeads {
		if owned = strings.TrimSpace(owned); owned != "" && owned == gateHead {
			return true
		}
	}
	return false
}

// hasRunOwnedHead reports whether the caller supplied any run-owned head.
func hasRunOwnedHead(runOwnedHeads []string) bool {
	for _, owned := range runOwnedHeads {
		if strings.TrimSpace(owned) != "" {
			return true
		}
	}
	return false
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

// recoveryAnchorPrefix is the ref namespace terminalization pins a run's
// unpublished pipeline head to (custody.RecoveryRef). An anchor is created only
// by the guarded custody path and never replaced: PreserveRecoveryAnchor
// creates it with an expected-old-value of the zero ID and fails closed on any
// conflicting existing ref, so a commit reachable from one is held by
// deliberate, immutable preservation evidence rather than by accident.
const recoveryAnchorPrefix = "refs/no-mistakes/recover/"

type recoveryAnchor struct {
	ref  string
	head string
}

// listRecoveryAnchors returns the direct commit targets under
// refs/no-mistakes/recover/. for-each-ref reports a direct commit ref as
// exactly three fields and a symbolic ref with a fourth %(symref) field, so a
// symbolic, zero, or non-commit target is skipped rather than dereferenced:
// it contributes no preservation evidence, and skipping is the conservative
// direction (nothing gets credited).
func listRecoveryAnchors(ctx context.Context, gateDir string) ([]recoveryAnchor, error) {
	out, err := git.Run(ctx, gateDir, "for-each-ref", "--format=%(refname) %(objectname) %(objecttype) %(symref)", recoveryAnchorPrefix)
	if err != nil {
		return nil, err
	}
	var anchors []recoveryAnchor
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 || fields[2] != "commit" {
			continue
		}
		ref, head := fields[0], fields[1]
		if git.IsZeroSHA(head) {
			continue
		}
		anchors = append(anchors, recoveryAnchor{ref: ref, head: head})
	}
	return anchors, nil
}

// reconcileSatisfiers names the proofs that can clear a reconciliation refusal
// in this call shape, so the refusal is actionable instead of a dead end. The
// recovery-anchor credit is always last.
func reconcileSatisfiers(runOwnedHeads []string) []string {
	satisfiers := []string{
		"the live head descending from the private head (ancestry)",
		"per-file patch-ID plus tree-survival proof (the content already landing in the live tree)",
	}
	if hasRunOwnedHead(runOwnedHeads) {
		satisfiers = append(satisfiers, "the private head being exactly the publishing run's Run.SubmittedHeadSHA or Run.LastPushedSHA (Decision 41-A)")
	}
	return append(satisfiers, "a refs/no-mistakes/recover/<run> recovery anchor reaching the commit")
}

func joinSatisfiers(satisfiers []string) string {
	if len(satisfiers) == 1 {
		return satisfiers[0]
	}
	return strings.Join(satisfiers[:len(satisfiers)-1], "; ") + "; or " + satisfiers[len(satisfiers)-1]
}

// maxRecoveryCandidates bounds how many candidate commits one recovery-anchor
// membership scan passes to git, keeping each rev-list argument list bounded.
// Larger candidate sets are scanned in successive batches, never truncated or
// refused: dropping any candidate would silently drop its preservation credit
// and reintroduce the deadlock this credit exists to prevent.
const maxRecoveryCandidates = 512

// preservedByRecoveryAnchors reports, for each candidate commit only, a
// recovery anchor that keeps it reachable in the gate repository. Reachable
// means reachable: the anchor's history contains the commit, so the object and
// its history survive whatever happens to the private branch ref.
//
// The membership scan excludes liveHead as well as the anchors. The candidates
// are private-only commits, so that walk is bounded by the private-only range
// rather than by the history the anchors or the live branch can see; anchors
// accumulate for the life of the gate. Attribution then probes anchors only for
// the credited commits, stopping at the first anchor that covers each.
//
// This is a PRESERVATION credit only. It says nothing about whether the content
// lands in the published tree, which the ancestry, patch-ID and tree-survival
// checks are responsible for proving. It exists so that the sanctioned
// recover -> rerun -> push loop cannot deadlock against the very anchor that
// loop wrote to declare the work preserved.
func preservedByRecoveryAnchors(ctx context.Context, gateDir, liveHead string, commits []string) (map[string]string, error) {
	preserved := make(map[string]string)
	if len(commits) == 0 {
		return preserved, nil
	}
	anchors, err := listRecoveryAnchors(ctx, gateDir)
	if err != nil {
		return nil, err
	}
	if len(anchors) == 0 {
		return preserved, nil
	}
	anchorHeads := make([]string, 0, len(anchors))
	for _, anchor := range anchors {
		anchorHeads = append(anchorHeads, anchor.head)
	}

	// One scan per batch: the candidates NO anchor reaches. Its complement is
	// preserved.
	unreachedSet := make(map[string]bool)
	for start := 0; start < len(commits); start += maxRecoveryCandidates {
		batch := commits[start:min(start+maxRecoveryCandidates, len(commits))]
		args := make([]string, 0, len(batch)+len(anchorHeads)+2)
		args = append(args, batch...)
		args = append(args, "--not", liveHead)
		args = append(args, anchorHeads...)
		unreached, err := commitList(ctx, gateDir, args...)
		if err != nil {
			return nil, err
		}
		for _, commit := range unreached {
			unreachedSet[commit] = true
		}
	}
	for _, commit := range commits {
		if unreachedSet[commit] {
			continue
		}
		// Attribution only, for the credited commits, stopping at the first
		// anchor that covers each one.
		for _, anchor := range anchors {
			if _, err := git.Run(ctx, gateDir, "merge-base", "--is-ancestor", commit, anchor.head); err == nil {
				preserved[commit] = anchor.ref
				break
			}
		}
	}
	return preserved, nil
}

// privateCommitsAbsentFromLive names private-only commits lacking matching
// per-file patches, or the entire private-only range when final-tree survival
// cannot be proven.
//
// The private side is computed first so the live scan can be bounded to the
// paths the private commits actually touch. Comparison stops at the first
// unmatched patch within each commit, but visits every private-only commit.
// A rebased live head otherwise carries every default-branch
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

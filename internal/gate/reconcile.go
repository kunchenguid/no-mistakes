package gate

import (
	"context"
	"fmt"
	"log/slog"
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
// Planning checks containment or the exact submitted-head policy exception
// without touching any ref, so a caller can decide before it publishes anything;
// applying the plan is the only step that archives and removes the branch.
type StaleBranchPlan struct {
	PreserveDescendantOf string
	Reconcile            bool
	Branch               string
	BranchRef            string
	PreviousHead         string
	ArchiveTag           string
}

// SupersessionEvidence is durable, run-recorded proof that a private mirror head
// is superseded by an accepted fix from the very run that submitted it. It is a
// policy input, never containment evidence: the planner still applies every
// ancestry and descendant check, then requires SubmittedHead to equal the private
// mirror head exactly and AcceptedHead to be contained in the live head. A head
// that does not match both exactly cannot be replaced on this basis, so unproven
// private content is still refused.
//
// It exists so the operator record can name WHICH run's accepted result
// superseded the archived head: replacing a private mirror ref is otherwise a
// silent rewrite of a branch other tooling may be reading.
type SupersessionEvidence struct {
	RunID         string
	SubmittedHead string
	AcceptedHead  string
}

// StaleBranchRefusal is the guard's refusal to replace a private mirror head
// whose content it cannot prove preserved. It carries the exact evidence an
// operator needs to act on instead of only a formatted sentence, so a driver can
// report the condition and the concrete next step rather than parking with no
// legal action.
//
// Every field is diagnostic. The refusal itself is a pure read: it never mutates
// a ref, so reporting it can never discard the at-risk work it names.
type StaleBranchRefusal struct {
	BranchRef    string
	LiveHead     string
	PreviousHead string
	// AtRisk lists "<sha> <subject>" for every private commit the live head does
	// not already contain.
	AtRisk []string
}

func (r *StaleBranchRefusal) Error() string {
	return fmt.Sprintf(
		"refusing to reconcile private mirror ref %s: %d at-risk commit(s) contain content absent from live head %s: %s",
		r.BranchRef, len(r.AtRisk), r.LiveHead, strings.Join(r.AtRisk, "; "),
	)
}

// Action is the concrete operator step for this refusal. The mirror is left
// untouched, so the at-risk commits are still retrievable; the offered step must
// therefore be a command that is reachable in exactly this state. `no-mistakes
// rerun` deliberately is not offered here: it resolves the recorded head and then
// refuses a clean caller-head mismatch, so naming it would point the operator at
// a path that is itself refused.
func (r *StaleBranchRefusal) Action() string {
	return "the private mirror was left untouched, so no work was discarded: run `no-mistakes axi status` and follow the `branch_sync.next_action.command` it offers; anything still needed from the mirror commits can be retrieved from it before submitting again"
}

// ReconcileStaleBranch plans and immediately applies stale private gate branch
// reconciliation. It removes the branch only after Git proves the live head
// contains all of its content, or under the exact submitted-head exception
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
// through an ordinary push. It mutates no ref: outside the submitted-head
// exception, an unproven private head is refused before publication.
//
// Rewritten histories require both stable per-file patch identities and final
// tree survival. runOwnedHead is a policy exception, not containment evidence:
// publication callers must supply only Run.SubmittedHeadSHA, and fresh
// submissions must leave it empty. The contract and rationale are owned by
// docs/src/content/docs/concepts/gate-model.md (Private mirror reconciliation).
func PlanStaleBranchReconciliation(ctx context.Context, gateDir, workDir, branch, liveHead, runOwnedHead string) (StaleBranchPlan, error) {
	return planStaleBranchReconciliation(ctx, gateDir, workDir, branch, liveHead, runOwnedHead, SupersessionSource{}, false)
}

func PlanMirrorPublicationReconciliation(ctx context.Context, gateDir, workDir, branch, liveHead, runOwnedHead string) (StaleBranchPlan, error) {
	return planStaleBranchReconciliation(ctx, gateDir, workDir, branch, liveHead, runOwnedHead, SupersessionSource{}, true)
}

// PlanSupersededSubmissionReconciliation inspects a private gate branch that a
// completed run submitted and then superseded. Unlike the submitted-head
// exception, this path is reachable for a fresh submission, so it proves the
// supersession from the repository's own run records instead of a caller
// asserting ownership. source supplies that proof; its zero value supplies none.
func PlanSupersededSubmissionReconciliation(ctx context.Context, gateDir, workDir, branch, liveHead string, source SupersessionSource) (StaleBranchPlan, error) {
	return planStaleBranchReconciliation(ctx, gateDir, workDir, branch, liveHead, "", source, false)
}

// ReconcileSupersededSubmission plans and immediately applies the reconciliation
// of a private mirror head that recorded evidence proves superseded.
func ReconcileSupersededSubmission(ctx context.Context, gateDir, workDir, branch, liveHead string, source SupersessionSource) (StaleBranchReconciliation, error) {
	plan, err := PlanSupersededSubmissionReconciliation(ctx, gateDir, workDir, branch, liveHead, source)
	if err != nil || !plan.Reconcile {
		return StaleBranchReconciliation{}, err
	}
	return ApplyStaleBranchReconciliation(ctx, gateDir, plan)
}

// authorizes reports whether recorded evidence proves that replacing privateHead
// with liveHead discards nothing the pipeline did not already accept. It is
// deliberately narrow: the private head must be exactly the head the named run
// submitted, and that run's own accepted result must survive in the live head.
// Anything else - another recorded head, an abbreviated SHA, an external or newer
// private head, an accepted head absent from the live history - leaves the
// ordinary containment check in force.
//
// The caller reaches this only through SupersessionSource.evidenceFor, which has
// already applied the run-record conditions; the head comparison is repeated here
// so a caller holding an evidence value cannot bypass it.
func (e *SupersessionEvidence) authorizes(privateHead string) bool {
	if e == nil {
		return false
	}
	submitted := strings.TrimSpace(e.SubmittedHead)
	accepted := strings.TrimSpace(e.AcceptedHead)
	return submitted != "" && accepted != "" && submitted == privateHead
}

func planStaleBranchReconciliation(ctx context.Context, gateDir, workDir, branch, liveHead, runOwnedHead string, source SupersessionSource, preserveDescendants bool) (StaleBranchPlan, error) {
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
	// Evidence is resolved only after the live head is staged, because proving
	// the accepted result survives requires it to be readable in this gate.
	evidence := source.evidenceFor(ctx, gateDir, gateHead, liveHead)
	if gateHead != runOwnedHead && evidence.authorizes(gateHead) {
		// Replacing a private mirror ref is a rewrite of a branch other tooling
		// may read, so record which run's accepted result justified it.
		slog.Info("reconciling private mirror ref on recorded supersession",
			"ref", branchRef, "previous_head", gateHead, "live_head", liveHead,
			"run_id", evidence.RunID, "accepted_head", evidence.AcceptedHead)
	}
	if gateHead != runOwnedHead && !evidence.authorizes(gateHead) {
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
			return plan, &StaleBranchRefusal{
				BranchRef:    branchRef,
				LiveHead:     liveHead,
				PreviousHead: gateHead,
				AtRisk:       atRisk,
			}
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

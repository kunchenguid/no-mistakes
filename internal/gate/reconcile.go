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

// ReconcileStaleBranch removes a stale private gate branch only after Git
// proves that the live head contains all of its content. Direct ancestry is
// sufficient; rewritten histories use Git's stable patch-id comparison.
func ReconcileStaleBranch(ctx context.Context, gateDir, workDir, branch, liveHead string) (StaleBranchReconciliation, error) {
	var result StaleBranchReconciliation
	branch = strings.TrimSpace(branch)
	liveHead = strings.TrimSpace(liveHead)
	if branch == "" || liveHead == "" {
		return result, fmt.Errorf("reconcile stale gate branch: branch and live head are required")
	}
	if err := git.ValidateBareRepository(ctx, gateDir); err != nil {
		return result, fmt.Errorf("reconcile stale gate branch: %w", err)
	}
	if _, err := git.Run(ctx, workDir, "check-ref-format", "--branch", branch); err != nil {
		return result, fmt.Errorf("reconcile stale gate branch %q: invalid branch name: %w", branch, err)
	}
	resolvedLive, err := git.Run(ctx, workDir, "rev-parse", "--verify", liveHead+"^{commit}")
	if err != nil || resolvedLive != liveHead {
		return result, fmt.Errorf("reconcile stale gate branch %s: live head %s is not an exact commit", branch, liveHead)
	}
	workDir, err = filepath.Abs(workDir)
	if err != nil {
		return result, fmt.Errorf("reconcile stale gate branch %s: resolve worktree path: %w", branch, err)
	}
	branchRef := "refs/heads/" + branch
	gateHead, exists, err := git.ExactRefTarget(ctx, gateDir, branchRef)
	if err != nil {
		return result, fmt.Errorf("inspect private mirror ref %s: %w", branchRef, err)
	}
	if !exists || gateHead == liveHead {
		return result, nil
	}
	if objectType, err := git.Run(ctx, gateDir, "cat-file", "-t", gateHead); err != nil || objectType != "commit" {
		return result, fmt.Errorf("private mirror ref %s does not point at a commit", branchRef)
	}
	if err := git.FetchRemoteRef(ctx, gateDir, workDir, liveHead, liveHead); err != nil {
		return result, fmt.Errorf("stage live head for private mirror reconciliation: %w", err)
	}

	// An ancestor needs no reconciliation: the caller's ordinary push is
	// already a fast-forward and preserves the private head by ancestry.
	if _, err := git.Run(ctx, gateDir, "merge-base", "--is-ancestor", gateHead, liveHead); err == nil {
		return result, nil
	}
	atRiskCommits, err := privateCommitsAbsentFromLive(ctx, gateDir, liveHead, gateHead)
	if err != nil {
		return result, fmt.Errorf("compare private mirror content for %s: %w", branchRef, err)
	}
	if len(atRiskCommits) > 0 {
		atRisk := make([]string, 0, len(atRiskCommits))
		for _, commit := range atRiskCommits {
			description, describeErr := git.Run(ctx, gateDir, "show", "-s", "--format=%H %s", commit)
			if describeErr != nil {
				return result, fmt.Errorf("describe at-risk private mirror commit %s: %w", commit, describeErr)
			}
			atRisk = append(atRisk, description)
		}
		return result, fmt.Errorf(
			"refusing to reconcile private mirror ref %s: %d at-risk commit(s) contain content absent from live head %s: %s",
			branchRef, len(atRisk), liveHead, strings.Join(atRisk, "; "),
		)
	}

	archiveTag := "refs/tags/no-mistakes-abandoned/" + branch + "/" + gateHead
	archivedHead, archived, err := git.ExactRefTarget(ctx, gateDir, archiveTag)
	if err != nil {
		return result, fmt.Errorf("inspect private mirror archive tag %s: %w", archiveTag, err)
	}
	if archived && archivedHead != gateHead {
		return result, fmt.Errorf("private mirror archive tag %s already points at %s, not %s", archiveTag, archivedHead, gateHead)
	}
	if !archived {
		if _, err := git.Run(ctx, gateDir, "update-ref", archiveTag, gateHead, "0000000000000000000000000000000000000000"); err != nil {
			return result, fmt.Errorf("archive stale private mirror head %s at %s: %w", gateHead, archiveTag, err)
		}
	}
	if _, err := git.Run(ctx, gateDir, "update-ref", "-d", branchRef, gateHead); err != nil {
		return result, fmt.Errorf("delete archived stale private mirror ref %s at %s: %w", branchRef, gateHead, err)
	}
	return StaleBranchReconciliation{Reconciled: true, PreviousHead: gateHead, ArchivedTag: archiveTag}, nil
}

func privateCommitsAbsentFromLive(ctx context.Context, repoDir, liveHead, privateHead string) ([]string, error) {
	liveOnly, err := commitList(ctx, repoDir, "--left-only", liveHead+"..."+privateHead)
	if err != nil {
		return nil, err
	}
	privateOnly, err := commitList(ctx, repoDir, "--right-only", liveHead+"..."+privateHead)
	if err != nil {
		return nil, err
	}

	livePatches := make(map[string]int)
	for _, commit := range liveOnly {
		patches, comparable, err := perFilePatchIDs(ctx, repoDir, commit)
		if err != nil {
			return nil, err
		}
		if !comparable {
			continue
		}
		for _, patch := range patches {
			livePatches[patch]++
		}
	}

	var atRisk []string
	for _, commit := range privateOnly {
		patches, comparable, err := perFilePatchIDs(ctx, repoDir, commit)
		if err != nil {
			return nil, err
		}
		if !comparable {
			atRisk = append(atRisk, commit)
			continue
		}
		remaining := make(map[string]int, len(livePatches))
		for patch, count := range livePatches {
			remaining[patch] = count
		}
		represented := true
		for _, patch := range patches {
			if remaining[patch] == 0 {
				represented = false
				break
			}
			remaining[patch]--
		}
		if !represented {
			atRisk = append(atRisk, commit)
			continue
		}
		for patch, count := range remaining {
			livePatches[patch] = count
		}
	}
	return atRisk, nil
}

func commitList(ctx context.Context, repoDir, side, revision string) ([]string, error) {
	out, err := git.Run(ctx, repoDir, "rev-list", side, revision)
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

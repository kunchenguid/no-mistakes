package steps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// RebaseStep syncs the pushed branch with upstream branch state and the latest default branch.
type RebaseStep struct{}

func (s *RebaseStep) Name() types.StepName { return types.StepRebase }

func (s *RebaseStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	ctx := sctx.Ctx
	branch := strings.TrimPrefix(sctx.Run.Branch, "refs/heads/")
	defaultBranch := strings.TrimSpace(sctx.Repo.DefaultBranch)
	if defaultBranch == "" {
		defaultBranch = "main"
	}

	// Detect force push before fetching so we can skip origin/<branch> sync.
	// A force push means the user explicitly rewrote the branch - the pushed
	// commit is authoritative and must not be overwritten by prior pipeline
	// state on the remote.
	forcePush := isForcePush(ctx, sctx.WorkDir, branch, sctx.Run.BaseSHA)

	sctx.Log("fetching latest upstream state...")
	if err := git.FetchRemoteBranch(ctx, sctx.WorkDir, "origin", defaultBranch); err != nil {
		sctx.LogFile(fmt.Sprintf("warning: could not fetch origin/%s: %v", defaultBranch, err))
	}
	if !forcePush && branch != "" && branch != defaultBranch {
		if err := git.FetchRemoteBranch(ctx, sctx.WorkDir, "origin", branch); err != nil {
			sctx.LogFile(fmt.Sprintf("warning: could not fetch origin/%s: %v", branch, err))
		}
	}

	targets := rebaseTargets(branch, defaultBranch)
	if forcePush {
		sctx.Log("force push detected, skipping origin/" + branch + " sync")
		targets = forcePushRebaseTargets(branch, defaultBranch)
	}

	if sctx.Fixing {
		for _, target := range targets {
			if err := rebaseWithAgent(ctx, sctx, target); err != nil {
				return nil, err
			}
		}
		return updateHeadSHA(ctx, sctx)
	}

	// Normal mode: try all rebases, track which targets had conflicts
	var conflictTargets []string
	var conflictFindings []Finding
	for _, target := range targets {
		conflictFiles, err := tryRebase(ctx, sctx, target)
		if err != nil {
			return nil, err
		}
		if len(conflictFiles) > 0 {
			conflictTargets = append(conflictTargets, target)
			for _, file := range conflictFiles {
				conflictFindings = append(conflictFindings, Finding{
					Severity:    "warning",
					File:        file,
					Description: fmt.Sprintf("merge conflict rebasing onto %s", target),
				})
			}
		}
	}

	if len(conflictTargets) > 0 {
		summary := fmt.Sprintf("conflict rebasing onto %s", strings.Join(conflictTargets, ", "))
		findingsJSON, _ := json.Marshal(Findings{Items: dedupeRebaseFindings(conflictFindings), Summary: summary})
		return &pipeline.StepOutcome{
			NeedsApproval: true,
			AutoFixable:   true,
			Findings:      string(findingsJSON),
		}, nil
	}

	return updateHeadSHA(ctx, sctx)
}

// rebaseTargets returns the ordered list of refs to rebase onto.
func rebaseTargets(branch, defaultBranch string) []string {
	var targets []string
	if branch != "" && branch != defaultBranch {
		targets = append(targets, "origin/"+branch)
	}
	if branch != defaultBranch {
		targets = append(targets, "origin/"+defaultBranch)
	}
	return targets
}

// forcePushRebaseTargets returns rebase targets for a force push. The
// origin/<branch> target is skipped because it may contain autofix commits
// from prior pipeline runs that the force push intended to discard.
func forcePushRebaseTargets(branch, defaultBranch string) []string {
	if branch == defaultBranch {
		return nil
	}
	return []string{"origin/" + defaultBranch}
}

// isForcePush returns true when the current push is non-fast-forward relative
// to the previous push (baseSHA). This indicates the user explicitly rewrote
// history and the pipeline should treat the new HEAD as authoritative.
func isForcePush(ctx context.Context, workDir, branch, baseSHA string) bool {
	if git.IsZeroSHA(baseSHA) || baseSHA == "" {
		return false
	}
	_, err := git.Run(ctx, workDir, "merge-base", "--is-ancestor", baseSHA, "HEAD")
	if err == nil {
		return false
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		return false
	}
	if branch != "" {
		remoteSHA, err := git.LsRemote(ctx, workDir, "origin", "refs/heads/"+branch)
		if err == nil && remoteSHA != "" {
			_, err := git.Run(ctx, workDir, "merge-base", "--is-ancestor", remoteSHA, "HEAD")
			if err == nil {
				return false
			}
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
				return true
			}
			return true
		}
		remoteRef := "origin/" + branch
		if _, err := git.Run(ctx, workDir, "rev-parse", "--verify", remoteRef); err == nil {
			return isRemoteBranchRewritten(ctx, workDir, remoteRef)
		}
	}
	return true
}

func isRemoteBranchRewritten(ctx context.Context, workDir, remoteRef string) bool {
	_, err := git.Run(ctx, workDir, "merge-base", "--is-ancestor", remoteRef, "HEAD")
	if err == nil {
		return false
	}
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr) && exitErr.ExitCode() == 1
}

// tryRebase attempts a rebase onto targetRef. Returns conflicted files when the
// rebase stops on merge conflicts. The rebase is aborted before returning.
func tryRebase(ctx context.Context, sctx *pipeline.StepContext, targetRef string) ([]string, error) {
	skip, err := shouldSkipRebase(ctx, sctx, targetRef)
	if err != nil {
		return nil, err
	}
	if skip {
		return nil, nil
	}

	sctx.Log(fmt.Sprintf("rebasing onto %s...", targetRef))
	if _, err := git.Run(ctx, sctx.WorkDir, "rebase", targetRef); err != nil {
		conflictFiles := rebaseConflictFiles(ctx, sctx.WorkDir)
		_, _ = git.Run(ctx, sctx.WorkDir, "rebase", "--abort")

		if len(conflictFiles) == 0 {
			return nil, fmt.Errorf("rebase onto %s: %w", targetRef, err)
		}
		return conflictFiles, nil
	}
	return nil, nil
}

// rebaseWithAgent performs a rebase and uses the agent to resolve any conflicts.
func rebaseWithAgent(ctx context.Context, sctx *pipeline.StepContext, targetRef string) error {
	skip, err := shouldSkipRebase(ctx, sctx, targetRef)
	if err != nil {
		return err
	}
	if skip {
		return nil
	}

	sctx.Log(fmt.Sprintf("rebasing onto %s...", targetRef))
	if _, err := git.Run(ctx, sctx.WorkDir, "rebase", targetRef); err == nil {
		return nil
	}

	if len(rebaseConflictFiles(ctx, sctx.WorkDir)) == 0 {
		_, _ = git.Run(ctx, sctx.WorkDir, "rebase", "--abort")
		return fmt.Errorf("rebase onto %s failed (no conflicts detected)", targetRef)
	}
	sctx.Log("conflicts detected, asking agent to resolve...")
	conflictFiles := rebaseConflictFiles(ctx, sctx.WorkDir)

	prompt := fmt.Sprintf(
		`Resolve git rebase conflicts. The rebase of the current branch onto %s has conflicts.

Current conflicted files:
- %s

Instructions:
- Find all conflicting files and resolve the conflict markers (<<<<<<< ======= >>>>>>>).
- After resolving each file, stage it with: git add <file>
- After all conflicts are resolved, run: git rebase --continue
- If additional conflicts arise during rebase --continue, resolve those too.
- Do not modify any files that don't have conflicts.
- Preserve the intent of both the current branch changes and the upstream changes.
- Return JSON with a single "summary" field describing what you resolved.
- Keep the summary under 10 words.`,
		targetRef,
		strings.Join(conflictFiles, "\n- "),
	)
	if sctx.PreviousFindings != "" {
		prompt += "\n\nPrevious findings:\n" + sctx.PreviousFindings
	}

	_, err = sctx.Agent.Run(ctx, agent.RunOpts{
		Prompt:     prompt,
		CWD:        sctx.WorkDir,
		JSONSchema: commitSummarySchema,
		OnChunk:    sctx.Log,
	})
	if err != nil {
		_, _ = git.Run(ctx, sctx.WorkDir, "rebase", "--abort")
		return fmt.Errorf("agent resolve conflicts: %w", err)
	}

	// Verify rebase completed (no rebase still in progress)
	if rebaseInProgress(ctx, sctx.WorkDir) {
		_, _ = git.Run(ctx, sctx.WorkDir, "rebase", "--abort")
		return fmt.Errorf("agent did not complete the rebase")
	}

	return nil
}

// shouldSkipRebase checks whether a rebase onto targetRef can be skipped.
// Returns true if targetRef doesn't exist, is already merged, or can be fast-forwarded.
func shouldSkipRebase(ctx context.Context, sctx *pipeline.StepContext, targetRef string) (bool, error) {
	if _, err := git.Run(ctx, sctx.WorkDir, "rev-parse", "--verify", targetRef); err != nil {
		return true, nil
	}
	localSHA, err := git.HeadSHA(ctx, sctx.WorkDir)
	if err != nil {
		return false, fmt.Errorf("get local head: %w", err)
	}
	targetSHA, err := git.Run(ctx, sctx.WorkDir, "rev-parse", targetRef)
	if err != nil {
		return false, fmt.Errorf("get target head %s: %w", targetRef, err)
	}
	if localSHA == targetSHA {
		sctx.Log(fmt.Sprintf("already up-to-date with %s", targetRef))
		return true, nil
	}
	if _, err := git.Run(ctx, sctx.WorkDir, "merge-base", "--is-ancestor", targetRef, "HEAD"); err == nil {
		sctx.Log(fmt.Sprintf("already ahead of %s", targetRef))
		return true, nil
	}
	if _, err := git.Run(ctx, sctx.WorkDir, "merge-base", "--is-ancestor", "HEAD", targetRef); err == nil {
		sctx.Log(fmt.Sprintf("fast-forwarding to %s", targetRef))
		if _, err := git.Run(ctx, sctx.WorkDir, "reset", "--hard", targetRef); err != nil {
			return false, fmt.Errorf("fast-forward to %s: %w", targetRef, err)
		}
		return true, nil
	}
	return false, nil
}

// rebaseInProgress returns true if a git rebase is currently in progress.
// Uses git rev-parse --git-path which works for both regular repos and worktrees.
func rebaseInProgress(ctx context.Context, workDir string) bool {
	for _, dir := range []string{"rebase-merge", "rebase-apply"} {
		p, err := git.Run(ctx, workDir, "rev-parse", "--git-path", dir)
		if err != nil {
			continue
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(workDir, p)
		}
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

func rebaseConflictFiles(ctx context.Context, workDir string) []string {
	out, err := git.Run(ctx, workDir, "diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return nil
	}
	var files []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		files = append(files, line)
	}
	return files
}

func dedupeRebaseFindings(findings []Finding) []Finding {
	if len(findings) < 2 {
		return findings
	}
	seen := make(map[string]bool, len(findings))
	filtered := make([]Finding, 0, len(findings))
	for _, finding := range findings {
		key := finding.File + "\x00" + finding.Description
		if seen[key] {
			continue
		}
		seen[key] = true
		filtered = append(filtered, finding)
	}
	return filtered
}

// updateHeadSHA syncs the run's head SHA after rebase and checks for an empty diff.
// When the branch diff against the default branch is empty, SkipRemaining is set.
func updateHeadSHA(ctx context.Context, sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	headSHA, err := git.HeadSHA(ctx, sctx.WorkDir)
	if err != nil {
		return nil, fmt.Errorf("resolve head after rebase: %w", err)
	}
	if headSHA != "" && headSHA != sctx.Run.HeadSHA {
		sctx.Run.HeadSHA = headSHA
		if err := sctx.DB.UpdateRunHeadSHA(sctx.Run.ID, headSHA); err != nil {
			return nil, err
		}
		sctx.Log(fmt.Sprintf("updated head SHA to %s", shortSHA(headSHA)))
	}

	// Check if the branch has any diff against the default branch.
	// If the diff is empty (e.g. branch was already merged), skip remaining steps.
	defaultBranch := strings.TrimSpace(sctx.Repo.DefaultBranch)
	if defaultBranch == "" {
		defaultBranch = "main"
	}
	baseSHA := resolveBranchBaseSHA(ctx, sctx.WorkDir, sctx.Run.BaseSHA, defaultBranch)
	diff, err := git.Diff(ctx, sctx.WorkDir, baseSHA, "HEAD")
	if err == nil && strings.TrimSpace(diff) == "" {
		sctx.Log("empty diff after rebase, skipping remaining steps")
		return &pipeline.StepOutcome{SkipRemaining: true}, nil
	}

	return &pipeline.StepOutcome{}, nil
}

func shortSHA(sha string) string {
	if len(sha) <= 12 {
		return sha
	}
	return sha[:12]
}

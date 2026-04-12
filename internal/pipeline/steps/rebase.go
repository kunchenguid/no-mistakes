package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
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

	sctx.Log("fetching latest upstream state...")
	if err := git.FetchRemoteBranch(ctx, sctx.WorkDir, "origin", defaultBranch); err != nil {
		sctx.LogFile(fmt.Sprintf("warning: could not fetch origin/%s: %v", defaultBranch, err))
	}
	if branch != "" && branch != defaultBranch {
		if err := git.FetchRemoteBranch(ctx, sctx.WorkDir, "origin", branch); err != nil {
			sctx.LogFile(fmt.Sprintf("warning: could not fetch origin/%s: %v", branch, err))
		}
	}

	targets := rebaseTargets(branch, defaultBranch)

	if sctx.Fixing {
		for _, target := range targets {
			if err := rebaseWithAgent(ctx, sctx, target); err != nil {
				return nil, err
			}
		}
		return updateHeadSHA(ctx, sctx)
	}

	// Normal mode: try rebases, return findings on conflict
	for _, target := range targets {
		outcome, err := tryRebase(ctx, sctx, target)
		if err != nil {
			return nil, err
		}
		if outcome != nil {
			return outcome, nil
		}
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

// tryRebase attempts a rebase onto targetRef. Returns nil outcome on success,
// or a StepOutcome with conflict findings if conflicts are detected.
func tryRebase(ctx context.Context, sctx *pipeline.StepContext, targetRef string) (*pipeline.StepOutcome, error) {
	skip, err := shouldSkipRebase(ctx, sctx, targetRef)
	if err != nil {
		return nil, err
	}
	if skip {
		return nil, nil
	}

	sctx.Log(fmt.Sprintf("rebasing onto %s...", targetRef))
	if _, err := git.Run(ctx, sctx.WorkDir, "rebase", targetRef); err != nil {
		// Check for conflict files before aborting
		conflictFiles := conflictingFiles(ctx, sctx.WorkDir)
		_, _ = git.Run(ctx, sctx.WorkDir, "rebase", "--abort")

		if len(conflictFiles) == 0 {
			return nil, fmt.Errorf("rebase onto %s: %w", targetRef, err)
		}

		findings := buildConflictFindings(targetRef, conflictFiles)
		findingsJSON, _ := json.Marshal(findings)
		return &pipeline.StepOutcome{
			NeedsApproval: true,
			Findings:      string(findingsJSON),
		}, nil
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

	conflictFiles := conflictingFiles(ctx, sctx.WorkDir)
	sctx.Log(fmt.Sprintf("conflicts detected in %d file(s), asking agent to resolve...", len(conflictFiles)))

	prompt := fmt.Sprintf(
		`Resolve git rebase conflicts. The rebase of the current branch onto %s has conflicts.

Conflicting files:
%s

Instructions:
- Edit each conflicting file to resolve the conflict markers (<<<<<<< ======= >>>>>>>).
- After resolving each file, stage it with: git add <file>
- After all conflicts are resolved, run: git rebase --continue
- If additional conflicts arise during rebase --continue, resolve those too.
- Do not modify any files that don't have conflicts.
- Preserve the intent of both the current branch changes and the upstream changes.
- Return JSON with a single "summary" field describing what you resolved.
- Keep the summary under 10 words.`,
		targetRef,
		strings.Join(conflictFiles, "\n"),
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
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

// conflictingFiles returns the list of files with merge conflicts in the working tree.
func conflictingFiles(ctx context.Context, workDir string) []string {
	out, err := git.Run(ctx, workDir, "diff", "--name-only", "--diff-filter=U")
	if err != nil {
		return nil
	}
	var files []string
	for _, f := range strings.Split(out, "\n") {
		f = strings.TrimSpace(f)
		if f != "" {
			files = append(files, f)
		}
	}
	return files
}

// buildConflictFindings creates a Findings struct from conflict information.
func buildConflictFindings(targetRef string, files []string) Findings {
	items := make([]Finding, len(files))
	for i, f := range files {
		items[i] = Finding{
			Severity:    "error",
			File:        f,
			Description: fmt.Sprintf("merge conflict during rebase onto %s", targetRef),
		}
	}
	return Findings{
		Items:   items,
		Summary: fmt.Sprintf("%d file(s) with merge conflicts rebasing onto %s", len(files), targetRef),
	}
}

// updateHeadSHA syncs the run's head SHA after rebase.
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
	return &pipeline.StepOutcome{}, nil
}

func shortSHA(sha string) string {
	if len(sha) <= 12 {
		return sha
	}
	return sha[:12]
}

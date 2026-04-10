package steps

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// PRStep creates or updates a pull request via gh CLI.
type PRStep struct{}

func (s *PRStep) Name() types.StepName { return types.StepPR }

func (s *PRStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	ctx := sctx.Ctx

	branch := sctx.Run.Branch
	if strings.HasPrefix(branch, "refs/heads/") {
		branch = strings.TrimPrefix(branch, "refs/heads/")
	}

	// Resolve the branch base so PR summaries cover the full branch delta.
	baseSHA := resolveBranchBaseSHA(ctx, sctx.WorkDir, sctx.Run.BaseSHA, sctx.Repo.DefaultBranch)

	// Check if PR already exists for this branch
	sctx.Log(fmt.Sprintf("checking for existing PR on branch %s...", branch))
	cmd := exec.CommandContext(ctx, "gh", "pr", "view", branch, "--json", "url", "--jq", ".url")
	cmd.Dir = sctx.WorkDir
	out, err := cmd.Output()
	if err == nil {
		prURL := strings.TrimSpace(string(out))
		if prURL != "" {
			sctx.Log(fmt.Sprintf("PR already exists: %s, updating...", prURL))

			// Update PR body with latest commit log
			commitLog, _ := git.Log(ctx, sctx.WorkDir, baseSHA, sctx.Run.HeadSHA)
			body := fmt.Sprintf("## Changes\n\n%s\n\n---\n*Updated by no-mistakes*", commitLog)

			editCmd := exec.CommandContext(ctx, "gh", "pr", "edit", branch, "--body", body)
			editCmd.Dir = sctx.WorkDir
			if editOut, editErr := editCmd.CombinedOutput(); editErr != nil {
				sctx.Log(fmt.Sprintf("warning: failed to update PR body: %s: %v", strings.TrimSpace(string(editOut)), editErr))
			}

			sctx.DB.UpdateRunPRURL(sctx.Run.ID, prURL)
			return &pipeline.StepOutcome{}, nil
		}
	}

	// Get commit log for PR body
	commitLog, _ := git.Log(ctx, sctx.WorkDir, baseSHA, sctx.Run.HeadSHA)

	// Create PR
	sctx.Log("creating pull request...")
	title := branch
	body := fmt.Sprintf("## Changes\n\n%s\n\n---\n*Created by no-mistakes*", commitLog)

	cmd = exec.CommandContext(ctx, "gh", "pr", "create",
		"--head", branch,
		"--base", sctx.Repo.DefaultBranch,
		"--title", title,
		"--body", body,
	)
	cmd.Dir = sctx.WorkDir
	out, err = cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("gh pr create: %s: %w", strings.TrimSpace(string(out)), err)
	}

	prURL := strings.TrimSpace(string(out))
	sctx.Log(fmt.Sprintf("created PR: %s", prURL))
	sctx.DB.UpdateRunPRURL(sctx.Run.ID, prURL)

	return &pipeline.StepOutcome{}, nil
}

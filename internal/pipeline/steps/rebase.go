package steps

import (
	"context"
	"fmt"
	"strings"

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
		sctx.Log(fmt.Sprintf("warning: could not fetch origin/%s: %v", defaultBranch, err))
	}
	if branch != "" && branch != defaultBranch {
		if err := git.FetchRemoteBranch(ctx, sctx.WorkDir, "origin", branch); err != nil {
			sctx.Log(fmt.Sprintf("warning: could not fetch origin/%s: %v", branch, err))
		}
	}

	if branch != "" && branch != defaultBranch {
		if err := rebaseOntoIfPresent(ctx, sctx, "origin/"+branch); err != nil {
			return nil, err
		}
	}
	if branch != defaultBranch {
		if err := rebaseOntoIfPresent(ctx, sctx, "origin/"+defaultBranch); err != nil {
			return nil, err
		}
	}

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

func rebaseOntoIfPresent(ctx context.Context, sctx *pipeline.StepContext, targetRef string) error {
	if _, err := git.Run(ctx, sctx.WorkDir, "rev-parse", "--verify", targetRef); err != nil {
		return nil
	}
	localSHA, err := git.HeadSHA(ctx, sctx.WorkDir)
	if err != nil {
		return fmt.Errorf("get local head: %w", err)
	}
	targetSHA, err := git.Run(ctx, sctx.WorkDir, "rev-parse", targetRef)
	if err != nil {
		return fmt.Errorf("get target head %s: %w", targetRef, err)
	}
	if localSHA == targetSHA {
		sctx.Log(fmt.Sprintf("already up-to-date with %s", targetRef))
		return nil
	}
	if _, err := git.Run(ctx, sctx.WorkDir, "merge-base", "--is-ancestor", targetRef, "HEAD"); err == nil {
		sctx.Log(fmt.Sprintf("already ahead of %s", targetRef))
		return nil
	}
	if _, err := git.Run(ctx, sctx.WorkDir, "merge-base", "--is-ancestor", "HEAD", targetRef); err == nil {
		sctx.Log(fmt.Sprintf("fast-forwarding to %s", targetRef))
		if _, err := git.Run(ctx, sctx.WorkDir, "reset", "--hard", targetRef); err != nil {
			return fmt.Errorf("fast-forward to %s: %w", targetRef, err)
		}
		return nil
	}

	sctx.Log(fmt.Sprintf("rebasing onto %s...", targetRef))
	if _, err := git.Run(ctx, sctx.WorkDir, "rebase", targetRef); err != nil {
		_, _ = git.Run(ctx, sctx.WorkDir, "rebase", "--abort")
		return fmt.Errorf("rebase onto %s: %w", targetRef, err)
	}
	return nil
}

func shortSHA(sha string) string {
	if len(sha) <= 12 {
		return sha
	}
	return sha[:12]
}

package steps

import (
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// PushStep force-pushes the worktree state to the upstream remote.
type PushStep struct{}

func (s *PushStep) Name() types.StepName { return types.StepPush }

func (s *PushStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	ctx := sctx.Ctx
	newHeadSHA := ""

	// Run format command if configured (before committing, so changes are formatted)
	if fmtCmd := sctx.Config.Commands.Format; fmtCmd != "" {
		sctx.Log(fmt.Sprintf("running formatter: %s", fmtCmd))
		output, exitCode, err := runShellCommand(ctx, sctx.WorkDir, fmtCmd)
		if err != nil {
			sctx.Log(fmt.Sprintf("warning: format command failed: %v", err))
		} else if exitCode != 0 {
			sctx.Log(fmt.Sprintf("warning: format command exited with code %d: %s", exitCode, output))
		}
	}

	// Commit any uncommitted changes from agent fixes
	status, _ := git.Run(ctx, sctx.WorkDir, "status", "--porcelain")
	if strings.TrimSpace(status) != "" {
		sctx.Log("committing agent changes...")
		if _, err := git.Run(ctx, sctx.WorkDir, "add", "-A"); err != nil {
			return nil, fmt.Errorf("stage agent changes: %w", err)
		}
		_, err := git.Run(ctx, sctx.WorkDir, "commit", "-m", "no-mistakes: apply agent fixes")
		if err != nil {
			return nil, fmt.Errorf("commit agent changes: %w", err)
		}
		headSHA, err := git.HeadSHA(ctx, sctx.WorkDir)
		if err != nil {
			return nil, fmt.Errorf("resolve head after commit: %w", err)
		}
		newHeadSHA = headSHA
	}

	ref := normalizedBranchRef(sctx.Run.Branch)

	upstream := sctx.Repo.UpstreamURL
	sctx.Log(fmt.Sprintf("pushing to %s (%s)...", upstream, ref))

	// Query upstream for current ref SHA to enable safe --force-with-lease.
	// Without an explicit SHA, --force-with-lease offers no protection when
	// pushing to a URL (no remote tracking refs), silently degrading to --force.
	upstreamSHA, lsErr := git.LsRemote(ctx, sctx.WorkDir, upstream, ref)
	if lsErr != nil {
		return nil, fmt.Errorf("ls-remote upstream: %w", lsErr)
	}
	if upstreamSHA != "" {
		// Existing branch: force-with-lease with explicit expected SHA
		if err := git.Push(ctx, sctx.WorkDir, upstream, ref, upstreamSHA, true); err != nil {
			return nil, fmt.Errorf("push to upstream: %w", err)
		}
	} else {
		// New branch: regular push (no force needed)
		if err := git.Push(ctx, sctx.WorkDir, upstream, ref, "", false); err != nil {
			return nil, fmt.Errorf("push to upstream: %w", err)
		}
	}

	if newHeadSHA != "" {
		if _, err := git.Run(ctx, sctx.WorkDir, "update-ref", ref, newHeadSHA); err != nil {
			return nil, fmt.Errorf("update local branch ref: %w", err)
		}
		sctx.Run.HeadSHA = newHeadSHA
		if err := sctx.DB.UpdateRunHeadSHA(sctx.Run.ID, newHeadSHA); err != nil {
			return nil, err
		}
	}

	sctx.Log("pushed successfully")
	return &pipeline.StepOutcome{}, nil
}

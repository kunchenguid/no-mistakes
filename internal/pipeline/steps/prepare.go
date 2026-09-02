package steps

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const prepareCleanupTimeout = 30 * time.Second

// ensurePrepared runs the trusted preparation command before the first
// configured Test, Lint, or Format command. Successful preparation is shared
// for the executor lifetime. Only ignored materialization (for example
// node_modules) survives: tracked and ordinary untracked changes are removed
// so setup cannot ride into a later pipeline fix commit.
func ensurePrepared(sctx *pipeline.StepContext, logStep types.StepName) error {
	prepareCmd := strings.TrimSpace(sctx.Config.Commands.Prepare)
	if prepareCmd == "" {
		return nil
	}
	return sctx.Shared.EnsurePrepared(func() error {
		marker, err := preparationMarkerPath(sctx.Ctx, sctx.WorkDir)
		if err != nil {
			return err
		}
		if _, err := os.Stat(marker); err == nil {
			sctx.Log("dependencies already prepared for this worktree")
			return nil
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("read preparation marker: %w", err)
		}

		status, err := git.Run(sctx.Ctx, sctx.WorkDir, "status", "--porcelain", "--untracked-files=all")
		if err != nil {
			return fmt.Errorf("check worktree before preparation: %w", err)
		}
		if strings.TrimSpace(status) != "" {
			return fmt.Errorf("refusing to prepare dependencies in a dirty worktree; commit or clean pipeline changes first")
		}
		head, err := git.HeadSHA(sctx.Ctx, sctx.WorkDir)
		if err != nil {
			return fmt.Errorf("resolve head before preparation: %w", err)
		}

		sctx.Log(fmt.Sprintf("preparing dependencies once for this worktree: %s", prepareCmd))
		started := time.Now()
		output, exitCode, commandErr := runStepShellCommand(sctx, prepareCmd)
		if output != "" {
			logCommandOutput(sctx, output, "Prepare", logStep)
		}

		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(sctx.Ctx), prepareCleanupTimeout)
		defer cancel()
		cleanupErr := cleanupPreparationChanges(cleanupCtx, sctx.WorkDir, head)
		if commandErr != nil {
			commandErr = fmt.Errorf("run prepare command: %w", commandErr)
		} else if exitCode != 0 {
			commandErr = fmt.Errorf("prepare command exited with code %d", exitCode)
		}
		if cleanupErr != nil {
			cleanupErr = fmt.Errorf("clean preparation changes: %w", cleanupErr)
		}
		if err := errors.Join(commandErr, cleanupErr); err != nil {
			return err
		}
		if err := os.WriteFile(marker, []byte(prepareCmd+"\n"), 0o600); err != nil {
			return fmt.Errorf("write preparation marker: %w", err)
		}
		sctx.Log(fmt.Sprintf("dependency preparation completed in %s", time.Since(started).Round(time.Millisecond)))
		return nil
	})
}

func preparationMarkerPath(ctx context.Context, workDir string) (string, error) {
	path, err := git.Run(ctx, workDir, "rev-parse", "--git-path", "no-mistakes-prepared")
	if err != nil {
		return "", fmt.Errorf("resolve preparation marker: %w", err)
	}
	path = strings.TrimSpace(path)
	if !filepath.IsAbs(path) {
		path = filepath.Join(workDir, path)
	}
	return filepath.Clean(path), nil
}

func cleanupPreparationChanges(ctx context.Context, workDir, originalHead string) error {
	if _, err := git.Run(ctx, workDir, "reset", "--hard", originalHead); err != nil {
		return err
	}
	if _, err := git.Run(ctx, workDir, "clean", "-fd"); err != nil {
		return err
	}
	return nil
}

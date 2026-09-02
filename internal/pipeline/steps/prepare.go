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
// node_modules) survives preparation; its tracked and ordinary untracked
// mutations are removed so setup cannot ride into a later pipeline fix commit.
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

		snapshot, err := snapshotPreparationState(sctx.Ctx, sctx.WorkDir)
		if err != nil {
			return err
		}
		defer snapshot.remove()
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
		restoreErr := snapshot.restore(cleanupCtx)
		if commandErr != nil {
			commandErr = fmt.Errorf("run prepare command: %w", commandErr)
		} else if exitCode != 0 {
			commandErr = fmt.Errorf("prepare command exited with code %d", exitCode)
		}
		if cleanupErr != nil {
			cleanupErr = fmt.Errorf("clean preparation changes: %w", cleanupErr)
		}
		if restoreErr != nil {
			restoreErr = fmt.Errorf("restore pre-preparation changes: %w", restoreErr)
		}
		if err := errors.Join(commandErr, cleanupErr, restoreErr); err != nil {
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
	if _, err := git.Run(ctx, workDir, "submodule", "update", "--init", "--recursive", "--force"); err != nil {
		return err
	}
	if _, err := git.Run(ctx, workDir, "submodule", "foreach", "--recursive", "--quiet", "git reset --hard"); err != nil {
		return err
	}
	if _, err := git.Run(ctx, workDir, "submodule", "foreach", "--recursive", "--quiet", "git clean -ffd"); err != nil {
		return err
	}
	if _, err := git.Run(ctx, workDir, "clean", "-ffd"); err != nil {
		return err
	}
	return nil
}

type preparationSnapshot struct {
	dir          string
	repositories []preparationRepositorySnapshot
}

type preparationRepositorySnapshot struct {
	workDir       string
	head          string
	stagedPatch   string
	unstagedPatch string
	untrackedDir  string
}

func snapshotPreparationState(ctx context.Context, workDir string) (preparationSnapshot, error) {
	dir, err := os.MkdirTemp("", "no-mistakes-prepare-")
	if err != nil {
		return preparationSnapshot{}, fmt.Errorf("create preparation snapshot: %w", err)
	}
	snapshot := preparationSnapshot{dir: dir}
	if err := snapshot.captureRepository(ctx, workDir, "root"); err != nil {
		snapshot.remove()
		return preparationSnapshot{}, err
	}
	paths, err := preparationSubmodulePaths(ctx, workDir)
	if err != nil {
		snapshot.remove()
		return preparationSnapshot{}, err
	}
	for i, path := range paths {
		if err := snapshot.captureRepository(ctx, filepath.Join(workDir, path), fmt.Sprintf("submodule-%d", i)); err != nil {
			snapshot.remove()
			return preparationSnapshot{}, err
		}
	}
	return snapshot, nil
}

func preparationSubmodulePaths(ctx context.Context, workDir string) ([]string, error) {
	out, err := git.RunRaw(ctx, workDir, "submodule", "foreach", "--recursive", "--quiet", `printf '%s\0' "$displaypath"`)
	if err != nil {
		return nil, fmt.Errorf("list registered submodules: %w", err)
	}
	var paths []string
	for _, path := range strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00") {
		if path == "" {
			continue
		}
		path, err = preparationRelativePath(path)
		if err != nil {
			return nil, fmt.Errorf("invalid submodule path: %w", err)
		}
		paths = append(paths, path)
	}
	return paths, nil
}

func preparationRelativePath(path string) (string, error) {
	path = filepath.Clean(path)
	if filepath.IsAbs(path) || path == "." || path == ".." || strings.HasPrefix(path, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%q", path)
	}
	return path, nil
}

func (s *preparationSnapshot) captureRepository(ctx context.Context, workDir, name string) error {
	head, err := git.HeadSHA(ctx, workDir)
	if err != nil {
		return fmt.Errorf("resolve %s head before preparation: %w", name, err)
	}
	dir := filepath.Join(s.dir, name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create %s snapshot directory: %w", name, err)
	}
	stagedPatch := filepath.Join(dir, "staged.patch")
	unstagedPatch := filepath.Join(dir, "unstaged.patch")
	for _, patch := range []struct {
		path string
		args []string
	}{
		{stagedPatch, []string{"diff", "--no-ext-diff", "--binary", "--cached"}},
		{unstagedPatch, []string{"diff", "--no-ext-diff", "--binary"}},
	} {
		contents, err := git.RunRaw(ctx, workDir, patch.args...)
		if err != nil {
			return fmt.Errorf("snapshot %s changes: %w", name, err)
		}
		if err := os.WriteFile(patch.path, contents, 0o600); err != nil {
			return fmt.Errorf("write %s snapshot: %w", name, err)
		}
	}
	untrackedDir := filepath.Join(dir, "untracked")
	untracked, err := git.UntrackedFiles(ctx, workDir)
	if err != nil {
		return fmt.Errorf("list %s untracked files: %w", name, err)
	}
	for _, path := range untracked {
		path, err = preparationRelativePath(path)
		if err != nil {
			return fmt.Errorf("invalid untracked path: %w", err)
		}
		if err := os.MkdirAll(filepath.Dir(filepath.Join(untrackedDir, path)), 0o700); err != nil {
			return fmt.Errorf("create snapshot parent for %s: %w", path, err)
		}
		if err := copyPath(filepath.Join(workDir, path), filepath.Join(untrackedDir, path)); err != nil {
			return fmt.Errorf("snapshot untracked %s: %w", path, err)
		}
	}
	s.repositories = append(s.repositories, preparationRepositorySnapshot{
		workDir: workDir, head: head, stagedPatch: stagedPatch, unstagedPatch: unstagedPatch, untrackedDir: untrackedDir,
	})
	return nil
}

func (s preparationSnapshot) restore(ctx context.Context) error {
	for _, repository := range s.repositories {
		if _, err := git.Run(ctx, repository.workDir, "reset", "--hard", repository.head); err != nil {
			return err
		}
		for _, patch := range []struct {
			path string
			args []string
		}{
			{repository.stagedPatch, []string{"apply", "--index"}},
			{repository.unstagedPatch, []string{"apply"}},
		} {
			contents, err := os.ReadFile(patch.path)
			if err != nil {
				return fmt.Errorf("read preparation snapshot: %w", err)
			}
			if len(contents) == 0 {
				continue
			}
			if _, err := git.Run(ctx, repository.workDir, append(patch.args, patch.path)...); err != nil {
				return err
			}
		}
		entries, err := os.ReadDir(repository.untrackedDir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("read untracked preparation snapshot: %w", err)
		}
		for _, entry := range entries {
			if err := copyPath(filepath.Join(repository.untrackedDir, entry.Name()), filepath.Join(repository.workDir, entry.Name())); err != nil {
				return fmt.Errorf("restore untracked %s: %w", entry.Name(), err)
			}
		}
	}
	return nil
}

func (s preparationSnapshot) remove() {
	if s.dir != "" {
		_ = os.RemoveAll(s.dir)
	}
}

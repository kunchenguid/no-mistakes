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

var (
	prepareCleanupTimeout = 30 * time.Second
	prepareRestoreTimeout = 30 * time.Second
	runPreparationCleanup = cleanupPreparationChanges
)

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

		snapshot, err := snapshotPreparationState(sctx.Ctx, sctx.WorkDir, sctx.GateDir)
		if err != nil {
			return err
		}
		removeSnapshot := true
		defer func() {
			if removeSnapshot {
				snapshot.remove()
			}
		}()
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
		cleanupErr := runPreparationCleanup(cleanupCtx, sctx.WorkDir, head, snapshot.submodules)
		cancel()
		restoreCtx, cancelRestore := context.WithTimeout(context.WithoutCancel(sctx.Ctx), prepareRestoreTimeout)
		restoreErr := snapshot.restore(restoreCtx)
		cancelRestore()
		if commandErr != nil {
			commandErr = fmt.Errorf("run prepare command: %w", commandErr)
		} else if exitCode != 0 {
			commandErr = fmt.Errorf("prepare command exited with code %d", exitCode)
		}
		if cleanupErr != nil {
			cleanupErr = fmt.Errorf("clean preparation changes: %w", cleanupErr)
		}
		if restoreErr != nil {
			removeSnapshot = false
			restoreErr = preparationRestoreError(snapshot, restoreErr)
		}
		sctx.Log(fmt.Sprintf("dependency preparation attempt completed in %s", time.Since(started).Round(time.Millisecond)))
		if err := errors.Join(commandErr, cleanupErr, restoreErr); err != nil {
			return err
		}
		if err := os.WriteFile(marker, []byte(prepareCmd+"\n"), 0o600); err != nil {
			return fmt.Errorf("write preparation marker: %w", err)
		}
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

func cleanupPreparationChanges(ctx context.Context, workDir, originalHead string, submodules []preparationSubmodule) error {
	if _, err := git.Run(ctx, workDir, "reset", "--hard", originalHead); err != nil {
		return err
	}
	for _, submodule := range submodules {
		if !submodule.initialized {
			continue
		}
		if _, err := git.Run(ctx, workDir, "submodule", "update", "--init", "--no-fetch", "--force", "--", submodule.path); err != nil {
			return err
		}
	}
	if _, err := git.Run(ctx, workDir, "submodule", "foreach", "--recursive", "--quiet", "git reset --hard"); err != nil {
		return err
	}
	if _, err := git.Run(ctx, workDir, "submodule", "foreach", "--recursive", "--quiet", "git clean -ffd"); err != nil {
		return err
	}
	for _, submodule := range submodules {
		if submodule.initialized {
			continue
		}
		if !submoduleWorktreeInitialized(workDir, submodule.path) {
			continue
		}
		if _, err := git.Run(ctx, workDir, "submodule", "deinit", "--force", "--", submodule.path); err != nil {
			return err
		}
	}
	if _, err := git.Run(ctx, workDir, "clean", "-ffd"); err != nil {
		return err
	}
	return nil
}

func submoduleWorktreeInitialized(workDir, path string) bool {
	_, err := os.Lstat(filepath.Join(workDir, path, ".git"))
	return err == nil
}

func preparationRestoreError(snapshot preparationSnapshot, err error) error {
	return fmt.Errorf("restore pre-preparation changes; recovery snapshot retained at %s: %w", snapshot.dir, err)
}

type preparationSnapshot struct {
	dir          string
	repositories []preparationRepositorySnapshot
	submodules   []preparationSubmodule
}

type preparationSubmodule struct {
	path        string
	initialized bool
}

type preparationRepositorySnapshot struct {
	workDir       string
	head          string
	indexPath     string
	indexSnapshot string
	stagedPatch   string
	unstagedPatch string
	untrackedDir  string
	directories   []preparationDirectorySnapshot
}

type preparationDirectorySnapshot struct {
	path string
	mode os.FileMode
}

func snapshotPreparationState(ctx context.Context, workDir, gateDir string) (preparationSnapshot, error) {
	parent, err := preparationSnapshotParent(ctx, workDir, gateDir)
	if err != nil {
		return preparationSnapshot{}, err
	}
	dir, err := os.MkdirTemp(parent, "prepare-")
	if err != nil {
		return preparationSnapshot{}, fmt.Errorf("create preparation snapshot: %w", err)
	}
	snapshot := preparationSnapshot{dir: dir}
	if err := snapshot.captureRepository(ctx, workDir, "root"); err != nil {
		snapshot.remove()
		return preparationSnapshot{}, err
	}
	submodules, err := preparationSubmodules(ctx, workDir)
	if err != nil {
		snapshot.remove()
		return preparationSnapshot{}, err
	}
	snapshot.submodules = submodules
	for i, submodule := range submodules {
		if !submodule.initialized {
			continue
		}
		if err := snapshot.captureRepository(ctx, filepath.Join(workDir, submodule.path), fmt.Sprintf("submodule-%d", i)); err != nil {
			snapshot.remove()
			return preparationSnapshot{}, err
		}
	}
	return snapshot, nil
}

func preparationSnapshotParent(ctx context.Context, workDir, gateDir string) (string, error) {
	if strings.TrimSpace(gateDir) == "" {
		return "", fmt.Errorf("preparation snapshot requires a gate directory")
	}
	workDir, err := filepath.EvalSymlinks(workDir)
	if err != nil {
		return "", fmt.Errorf("resolve preparation worktree: %w", err)
	}
	gateDir, err = filepath.EvalSymlinks(gateDir)
	if err != nil {
		return "", fmt.Errorf("resolve preparation gate directory: %w", err)
	}
	if err := git.ValidateBareRepository(ctx, gateDir); err != nil {
		return "", fmt.Errorf("validate preparation gate directory: %w", err)
	}
	if rel, err := filepath.Rel(workDir, gateDir); err != nil || rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
		return "", fmt.Errorf("preparation gate directory %q must be outside worktree %q", gateDir, workDir)
	}
	parent := filepath.Join(gateDir, "no-mistakes-preparation")
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return "", fmt.Errorf("create preparation snapshot directory: %w", err)
	}
	return parent, nil
}

func preparationSubmodules(ctx context.Context, workDir string) ([]preparationSubmodule, error) {
	registered, err := registeredSubmodulePaths(ctx, workDir)
	if err != nil {
		return nil, err
	}
	out, err := git.RunRaw(ctx, workDir, "submodule", "foreach", "--recursive", "--quiet", `printf '%s\0' "$displaypath"`)
	if err != nil {
		return nil, fmt.Errorf("list registered submodules: %w", err)
	}
	initialized := make(map[string]bool)
	initializedPaths := make([]string, 0)
	for _, path := range strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00") {
		if path == "" {
			continue
		}
		path, err = preparationRelativePath(path)
		if err != nil {
			return nil, fmt.Errorf("invalid submodule path: %w", err)
		}
		if !initialized[path] {
			initialized[path] = true
			initializedPaths = append(initializedPaths, path)
		}
	}
	submodules := make([]preparationSubmodule, 0, len(registered)+len(initializedPaths))
	registeredPaths := make(map[string]bool, len(registered))
	for _, path := range registered {
		registeredPaths[path] = true
		submodules = append(submodules, preparationSubmodule{path: path, initialized: initialized[path]})
	}
	for _, path := range initializedPaths {
		if !registeredPaths[path] {
			submodules = append(submodules, preparationSubmodule{path: path, initialized: true})
		}
	}
	return submodules, nil
}

func registeredSubmodulePaths(ctx context.Context, workDir string) ([]string, error) {
	out, err := git.RunRaw(ctx, workDir, "config", "--null", "--file", ".gitmodules", "--get-regexp", `^submodule\..*\.path$`)
	if err != nil {
		if strings.Contains(err.Error(), "exit status 1") {
			return nil, nil
		}
		return nil, fmt.Errorf("list registered submodules: %w", err)
	}
	var paths []string
	for _, entry := range strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00") {
		_, path, ok := strings.Cut(entry, "\n")
		if !ok {
			return nil, fmt.Errorf("invalid registered submodule entry %q", entry)
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
	indexPath, err := preparationGitPath(ctx, workDir, "index")
	if err != nil {
		return fmt.Errorf("resolve %s index: %w", name, err)
	}
	indexInfo, err := os.Stat(indexPath)
	if err != nil {
		return fmt.Errorf("read %s index: %w", name, err)
	}
	indexSnapshot := filepath.Join(dir, "index")
	if err := copyFile(indexPath, indexSnapshot, indexInfo.Mode().Perm()); err != nil {
		return fmt.Errorf("snapshot %s index: %w", name, err)
	}
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
	directories := make(map[string]os.FileMode)
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
		for directory := filepath.Dir(path); directory != "."; directory = filepath.Dir(directory) {
			if _, ok := directories[directory]; ok {
				continue
			}
			info, err := os.Stat(filepath.Join(workDir, directory))
			if err != nil {
				return fmt.Errorf("read untracked directory %s: %w", directory, err)
			}
			directories[directory] = info.Mode().Perm()
		}
	}
	if err := captureEmptyUntrackedDirectories(ctx, workDir, directories); err != nil {
		return fmt.Errorf("snapshot empty untracked directories: %w", err)
	}
	directorySnapshots := make([]preparationDirectorySnapshot, 0, len(directories))
	for path, mode := range directories {
		directorySnapshots = append(directorySnapshots, preparationDirectorySnapshot{path: path, mode: mode})
	}
	s.repositories = append(s.repositories, preparationRepositorySnapshot{
		workDir: workDir, head: head, indexPath: indexPath, indexSnapshot: indexSnapshot, stagedPatch: stagedPatch, unstagedPatch: unstagedPatch, untrackedDir: untrackedDir, directories: directorySnapshots,
	})
	return nil
}

func captureEmptyUntrackedDirectories(ctx context.Context, workDir string, directories map[string]os.FileMode) error {
	out, err := git.RunRaw(ctx, workDir, "ls-files", "--others", "--directory", "--exclude-standard", "-z")
	if err != nil {
		return err
	}
	for _, root := range strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00") {
		if root == "" {
			continue
		}
		root, err = preparationRelativePath(strings.TrimSuffix(root, "/"))
		if err != nil {
			return err
		}
		rootPath := filepath.Join(workDir, root)
		info, err := os.Lstat(rootPath)
		if err != nil {
			return err
		}
		if !info.IsDir() {
			continue
		}
		if err := filepath.WalkDir(rootPath, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if !entry.IsDir() {
				return nil
			}
			rel, err := filepath.Rel(workDir, path)
			if err != nil {
				return err
			}
			ignored, err := preparationPathIgnored(ctx, workDir, rel)
			if err != nil {
				return err
			}
			if ignored {
				return filepath.SkipDir
			}
			entries, err := os.ReadDir(path)
			if err != nil {
				return err
			}
			if len(entries) != 0 {
				return nil
			}
			for directory := rel; directory != "."; directory = filepath.Dir(directory) {
				if _, ok := directories[directory]; ok {
					continue
				}
				info, err := os.Stat(filepath.Join(workDir, directory))
				if err != nil {
					return err
				}
				directories[directory] = info.Mode().Perm()
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

func preparationPathIgnored(ctx context.Context, workDir, path string) (bool, error) {
	_, err := git.RunRaw(ctx, workDir, "check-ignore", "--quiet", "--", path)
	if err == nil {
		return true, nil
	}
	if strings.Contains(err.Error(), "exit status 1") {
		return false, nil
	}
	return false, err
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
		indexInfo, err := os.Stat(repository.indexSnapshot)
		if err != nil {
			return fmt.Errorf("read preparation index snapshot: %w", err)
		}
		if err := copyFile(repository.indexSnapshot, repository.indexPath, indexInfo.Mode().Perm()); err != nil {
			return fmt.Errorf("restore preparation index: %w", err)
		}
		contents, err := os.ReadFile(repository.unstagedPatch)
		if err != nil {
			return fmt.Errorf("read preparation snapshot: %w", err)
		}
		if len(contents) > 0 {
			if _, err := git.Run(ctx, repository.workDir, "apply", repository.unstagedPatch); err != nil {
				return err
			}
		}
		entries, err := os.ReadDir(repository.untrackedDir)
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("read untracked preparation snapshot: %w", err)
		}
		if err == nil {
			for _, entry := range entries {
				if err := copyPath(filepath.Join(repository.untrackedDir, entry.Name()), filepath.Join(repository.workDir, entry.Name())); err != nil {
					return fmt.Errorf("restore untracked %s: %w", entry.Name(), err)
				}
			}
		}
		for _, directory := range repository.directories {
			path := filepath.Join(repository.workDir, directory.path)
			if err := os.MkdirAll(path, directory.mode); err != nil {
				return fmt.Errorf("restore untracked directory %s: %w", directory.path, err)
			}
			if err := os.Chmod(path, directory.mode); err != nil {
				return fmt.Errorf("restore untracked directory %s: %w", directory.path, err)
			}
		}
	}
	return nil
}

func preparationGitPath(ctx context.Context, workDir, name string) (string, error) {
	path, err := git.Run(ctx, workDir, "rev-parse", "--git-path", name)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(workDir, path)
	}
	return filepath.Clean(path), nil
}

func (s preparationSnapshot) remove() {
	if s.dir != "" {
		_ = os.RemoveAll(s.dir)
	}
}

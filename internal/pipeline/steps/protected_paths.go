package steps

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

// stagePipelineChanges guards every pipeline-owned catch-all staging path,
// including Push's leftover commit. Refusal preserves the index and worktree.
func stagePipelineChanges(sctx *pipeline.StepContext) error {
	if len(sctx.Config.ProtectedPaths) > 0 {
		// Disable renames so both source and destination are checked, and list
		// individual untracked files so a protected path inside a new directory
		// cannot hide behind the directory entry. NULs preserve unusual names.
		status, err := stepGitRunRaw(sctx, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--no-renames", "--ignore-submodules=none")
		if err != nil {
			return fmt.Errorf("check protected_paths: %w", err)
		}
		for _, entry := range strings.Split(strings.TrimSuffix(status, "\x00"), "\x00") {
			if entry == "" {
				continue
			}
			if len(entry) < 4 || entry[2] != ' ' {
				return fmt.Errorf("check protected_paths: invalid git status entry %q", entry)
			}
			file := entry[3:]
			for _, pattern := range sctx.Config.ProtectedPaths {
				if !matchIgnorePattern(file, pattern) {
					continue
				}
				populated, err := isPopulatedRecordedSubmodule(sctx, file)
				if err != nil {
					return fmt.Errorf("check protected_paths: %w", err)
				}
				if populated {
					break
				}
				return &pipeline.ProtectedPathError{Path: file, Rule: pattern}
			}
		}
	}
	if _, err := stepGitRun(sctx, "add", "-A"); err != nil {
		return err
	}
	return unstageSubmodulePointerMoves(sctx)
}

// isPopulatedRecordedSubmodule reports whether path is a gitlink in HEAD that is
// still a checked-out submodule in the worktree. Its status entry is a pointer
// or content difference that catch-all staging never records, so it is not a
// protected-path edit; an added or removed submodule is still checked.
func isPopulatedRecordedSubmodule(sctx *pipeline.StepContext, path string) (bool, error) {
	if !submoduleWorktreeInitialized(sctx.WorkDir, path) {
		return false, nil
	}
	entry, err := stepGitRunRaw(sctx, "--literal-pathspecs", "ls-tree", "-z", "HEAD", "--", path)
	if err != nil {
		return false, err
	}
	return strings.HasPrefix(entry, "160000 "), nil
}

// unstageSubmodulePointerMoves keeps catch-all staging from recording a
// submodule checkout as a new pointer. A rebase moves the recorded pointer but
// not the populated checkout, so `git add -A` would commit the stale checkout
// as a silent revert of the base's submodule bump, and a populated checkout
// whose gitlink the base removed would be re-added as an embedded repository;
// pipeline corrections never move a submodule pointer. A gitlink addition the
// staged .gitmodules registers is a deliberately added submodule and stays.
func unstageSubmodulePointerMoves(sctx *pipeline.StepContext) error {
	raw, err := stepGitRunRaw(sctx, "diff", "--cached", "--raw", "-z", "--no-renames", "--ignore-submodules=none")
	if err != nil {
		return fmt.Errorf("check staged submodule pointers: %w", err)
	}
	fields := strings.Split(strings.TrimSuffix(raw, "\x00"), "\x00")
	var moved []string
	var registered map[string]bool
	for i := 0; i+1 < len(fields); i += 2 {
		modes := strings.Fields(strings.TrimPrefix(fields[i], ":"))
		if len(modes) < 2 || modes[1] != "160000" {
			continue
		}
		if modes[0] == "160000" {
			moved = append(moved, fields[i+1])
			continue
		}
		if modes[0] != "000000" {
			continue
		}
		if registered == nil {
			if registered, err = stagedRegisteredSubmodules(sctx); err != nil {
				return fmt.Errorf("check staged submodule pointers: %w", err)
			}
		}
		if !registered[fields[i+1]] {
			moved = append(moved, fields[i+1])
		}
	}
	if len(moved) == 0 {
		return nil
	}
	if _, err := stepGitRun(sctx, append([]string{"--literal-pathspecs", "reset", "-q", "--"}, moved...)...); err != nil {
		return fmt.Errorf("unstage submodule pointers: %w", err)
	}
	sctx.Log(fmt.Sprintf("left submodule pointers as recorded: %s", strings.Join(moved, ", ")))
	return nil
}

// stagedRegisteredSubmodules returns the submodule paths the staged .gitmodules
// registers.
func stagedRegisteredSubmodules(sctx *pipeline.StepContext) (map[string]bool, error) {
	registered := map[string]bool{}
	if _, err := stepGitRun(sctx, "cat-file", "-e", ":.gitmodules"); err != nil {
		return registered, nil
	}
	out, err := stepGitRunRaw(sctx, "config", "--null", "--blob", ":.gitmodules", "--get-regexp", `^submodule\..*\.path$`)
	if err != nil {
		if strings.Contains(err.Error(), "exit status 1") {
			return registered, nil
		}
		return nil, fmt.Errorf("list staged submodules: %w", err)
	}
	paths, err := parseRegisteredSubmodulePaths([]byte(out))
	if err != nil {
		return nil, err
	}
	for _, path := range paths {
		registered[filepath.ToSlash(path)] = true
	}
	return registered, nil
}

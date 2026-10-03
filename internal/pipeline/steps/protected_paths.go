package steps

import (
	"fmt"
	"io"
	"path"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

// stagePipelineChanges guards every pipeline-owned catch-all staging path,
// including Push's leftover commit. Refusal preserves the index and worktree.
// New tool caches and scratch files stay in the run worktree but out of commits.
func stagePipelineChanges(sctx *pipeline.StepContext) error {
	// List individual untracked files so protected paths cannot hide behind
	// a directory entry. Renames carry the destination, then the source, as
	// separate NUL-delimited paths; both must be checked and staged together.
	status, err := stepGitRunRaw(sctx, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--renames", "--ignore-submodules=none")
	if err != nil {
		return fmt.Errorf("check protected_paths and scratch: %w", err)
	}
	var changed, unstage []string
	scratch := newScratchExclusions()
	entries := strings.Split(strings.TrimSuffix(status, "\x00"), "\x00")
	for i := 0; i < len(entries); i++ {
		entry := entries[i]
		if entry == "" {
			continue
		}
		if len(entry) < 4 || entry[2] != ' ' {
			return fmt.Errorf("check protected_paths and scratch: invalid git status entry %q", entry)
		}
		file := entry[3:]
		files := []string{file}
		if strings.ContainsAny(entry[:2], "RC") {
			i++
			if i >= len(entries) || entries[i] == "" {
				return fmt.Errorf("check protected_paths and scratch: missing source for %q", entry)
			}
			files = append(files, entries[i])
		}
		for _, changedPath := range files {
			for _, pattern := range sctx.Config.ProtectedPaths {
				if matchIgnorePattern(changedPath, pattern) {
					return &pipeline.ProtectedPathError{Path: changedPath, Rule: pattern}
				}
			}
		}
		if len(files) == 1 && (entry[:2] == "??" || entry[0] == 'A' || entry[1] == 'A') && scratch.add(file) {
			if entry[:2] != "??" {
				unstage = append(unstage, file)
			}
			continue
		}
		changed = append(changed, files...)
	}
	if len(unstage) > 0 {
		if _, err := stepGitRunInput(sctx, nulPathspecs(literalPathspecs(unstage)), "rm", "--cached", "-f", "-q", "--pathspec-from-file=-", "--pathspec-file-nul"); err != nil {
			return err
		}
	}
	specs := append([]string{"."}, scratch.pathspecs(changed)...)
	if _, err := stepGitRunInput(sctx, nulPathspecs(specs), "add", "-A", "--pathspec-from-file=-", "--pathspec-file-nul"); err != nil {
		return err
	}
	if summary := scratch.summary(); summary != "" {
		sctx.Log("left new tool caches and scratch out of the commit (still in the run worktree): " + summary)
	}
	return unstageSubmodulePointerMoves(sctx)
}

func literalPathspecs(files []string) []string {
	specs := make([]string, len(files))
	for i, file := range files {
		specs[i] = ":(literal)" + file
	}
	return specs
}

func nulPathspecs(specs []string) io.Reader {
	return strings.NewReader(strings.Join(specs, "\x00") + "\x00")
}

// scratchCacheDirs are directory names that only ever hold tool caches or
// installed dependencies. A new file under one is never an intended change.
var scratchCacheDirs = map[string]bool{
	"node_modules":  true,
	".cache":        true,
	".corepack":     true,
	".npm":          true,
	".pnpm-store":   true,
	"__pycache__":   true,
	".pytest_cache": true,
	".mypy_cache":   true,
	".ruff_cache":   true,
}

// scratchRoot reports whether a new path is a tool cache or an ad-hoc scratch
// file, and the path to exclude for it. Tracked files are never classified.
func scratchRoot(file string) (root, reason string, ok bool) {
	isDir := strings.HasSuffix(file, "/")
	parts := strings.Split(strings.TrimSuffix(file, "/"), "/")
	dirs := parts
	if !isDir {
		dirs = parts[:len(parts)-1]
	}
	inCache := false
	for i, part := range dirs {
		if i == 0 && part == "scratch" {
			return part, "scratch directory", true
		}
		if scratchCacheDirs[part] || (part == "corepack" && inCache) {
			return strings.Join(parts[:i+1], "/"), "tool cache", true
		}
		inCache = inCache || part == "cache" || strings.HasPrefix(part, ".")
	}
	base := parts[len(parts)-1]
	if !isDir && len(parts) == 2 && (parts[0] == "tests" || parts[0] == "test") && strings.HasPrefix(base, "_") {
		switch path.Ext(base) {
		case ".sh", ".bash", ".zsh":
			return file, "scratch script", true
		}
	}
	return "", "", false
}

type scratchExclusion struct {
	root   string
	reason string
	files  []string
}

type scratchExclusions struct {
	byRoot map[string]*scratchExclusion
	order  []string
}

func newScratchExclusions() *scratchExclusions {
	return &scratchExclusions{byRoot: map[string]*scratchExclusion{}}
}

func (s *scratchExclusions) add(file string) bool {
	root, reason, ok := scratchRoot(file)
	if !ok {
		return false
	}
	ex := s.byRoot[root]
	if ex == nil {
		ex = &scratchExclusion{root: root, reason: reason}
		s.byRoot[root] = ex
		s.order = append(s.order, root)
	}
	ex.files = append(ex.files, file)
	return true
}

// pathspecs excludes each cache directory whole, keeping the pathspec list
// short for large caches, unless a tracked change lies under it; then only its
// untracked files are excluded so the tracked change is still staged.
func (s *scratchExclusions) pathspecs(changed []string) []string {
	var specs []string
	for _, root := range s.order {
		ex := s.byRoot[root]
		excluded := []string{root}
		for _, file := range changed {
			if strings.HasPrefix(file, root+"/") {
				excluded = ex.files
				break
			}
		}
		for _, file := range excluded {
			specs = append(specs, ":(exclude,literal)"+file)
		}
	}
	return specs
}

func (s *scratchExclusions) summary() string {
	const maxNamed = 10
	var named []string
	for _, root := range s.order[:min(len(s.order), maxNamed)] {
		ex := s.byRoot[root]
		if len(ex.files) == 1 && ex.files[0] == root {
			named = append(named, fmt.Sprintf("%s (%s)", root, ex.reason))
		} else {
			named = append(named, fmt.Sprintf("%s/ (%s, %d files)", root, ex.reason, len(ex.files)))
		}
	}
	if len(s.order) > maxNamed {
		named = append(named, fmt.Sprintf("and %d more", len(s.order)-maxNamed))
	}
	return strings.Join(named, ", ")
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

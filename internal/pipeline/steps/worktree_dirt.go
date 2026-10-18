package steps

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// worktreeDirt fingerprints the paths that were already dirty before an agent
// ran, so the step's commit can leave out every one the agent never touched:
// an earlier step's lockfile churn is not this step's work, and committing it
// under this step's message misdescribes it (issue #762).
type worktreeDirt map[string]string

func snapshotWorktreeDirt(sctx *pipeline.StepContext) (worktreeDirt, error) {
	status, err := stepGitRunRaw(sctx, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--no-renames", "--ignore-submodules=none")
	if err != nil {
		return nil, fmt.Errorf("snapshot pre-existing changes: %w", err)
	}
	dirt := worktreeDirt{}
	for _, entry := range strings.Split(strings.TrimSuffix(status, "\x00"), "\x00") {
		if entry == "" {
			continue
		}
		if len(entry) < 4 || entry[2] != ' ' {
			return nil, fmt.Errorf("snapshot pre-existing changes: invalid git status entry %q", entry)
		}
		file := entry[3:]
		dirt[file] = fingerprintWorktreePath(sctx.WorkDir, file)
	}
	return dirt, nil
}

// fingerprintWorktreePath identifies a path's current worktree state. A
// directory (a submodule checkout) is never content-compared.
func fingerprintWorktreePath(workDir, file string) string {
	path := filepath.Join(workDir, filepath.FromSlash(file))
	info, err := os.Lstat(path)
	if err != nil {
		return "absent"
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, _ := os.Readlink(path)
		return "symlink:" + target
	}
	if info.IsDir() {
		return "dir"
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "unreadable"
	}
	return fmt.Sprintf("file:%o:%x", info.Mode().Perm(), sha256.Sum256(data))
}

// untouched returns the snapshotted paths whose state has not changed since.
func (d worktreeDirt) untouched(workDir string) []string {
	var paths []string
	for file, fingerprint := range d {
		if fingerprintWorktreePath(workDir, file) == fingerprint {
			paths = append(paths, file)
		}
	}
	sort.Strings(paths)
	return paths
}

// unstageUntouchedDirt runs after catch-all staging and returns every path the
// agent left exactly as the snapshot found it to its HEAD index entry. The
// worktree keeps those changes; only this step's commit leaves them out.
func unstageUntouchedDirt(sctx *pipeline.StepContext, stepName types.StepName, dirt worktreeDirt) error {
	untouched := dirt.untouched(sctx.WorkDir)
	if len(untouched) == 0 {
		return nil
	}
	if _, err := stepGitRun(sctx, append([]string{"--literal-pathspecs", "reset", "-q", "--"}, untouched...)...); err != nil {
		return fmt.Errorf("leave pre-existing changes uncommitted: %w", err)
	}
	sctx.Log(fmt.Sprintf("warning: left changes that predate the %s step uncommitted: %s", stepName, strings.Join(untouched, ", ")))
	return nil
}

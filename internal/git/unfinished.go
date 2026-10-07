package git

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// UnfinishedOperation inspects operation markers and unmerged index entries
// through the caller's Git runner, preserving its environment and cancellation.
func UnfinishedOperation(workDir string, run func(...string) (string, error)) (bool, error) {
	for _, name := range []string{"MERGE_HEAD", "rebase-merge", "rebase-apply"} {
		path, err := run("rev-parse", "--git-path", name)
		if err != nil {
			return false, fmt.Errorf("inspect unfinished Git operation: %w", err)
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(workDir, path)
		}
		if _, err := os.Lstat(path); err == nil {
			return true, nil
		} else if !os.IsNotExist(err) {
			return false, fmt.Errorf("inspect %s: %w", name, err)
		}
	}
	unmerged, err := run("ls-files", "--unmerged", "-z")
	if err != nil {
		return false, fmt.Errorf("inspect unmerged index: %w", err)
	}
	return strings.TrimSpace(unmerged) != "", nil
}

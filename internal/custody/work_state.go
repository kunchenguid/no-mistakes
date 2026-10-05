package custody

import (
	"context"
	"os"
	"path/filepath"

	"github.com/kunchenguid/no-mistakes/internal/git"
)

func GitOperationInProgress(ctx context.Context, dir string) (bool, error) {
	gitDir, err := git.Run(ctx, dir, "rev-parse", "--absolute-git-dir")
	if err != nil {
		return true, err
	}
	for _, marker := range []string{"MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD", "rebase-merge", "rebase-apply"} {
		if _, err := os.Stat(filepath.Join(gitDir, marker)); err == nil {
			return true, nil
		} else if !os.IsNotExist(err) {
			return true, err
		}
	}
	return false, nil
}

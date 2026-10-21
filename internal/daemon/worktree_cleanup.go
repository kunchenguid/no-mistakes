package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// runWorktreeEditsCleanupReason keeps the existing explicit operator discard
// contract. A crash, superseding push, or failed recovery is never that decision.
func runWorktreeEditsCleanupReason(ctx context.Context, d *db.DB, run *db.Run, dir string) string {
	if run != nil {
		if run.Status == types.RunCancelled && run.Error != nil && *run.Error == types.RunCancelReasonAbortedByUser {
			return ""
		}
		if run.Status == types.RunFailed {
			if d == nil {
				return "cannot read worktree discard decision: database unavailable"
			}
			steps, err := d.GetStepsByRun(run.ID)
			if err != nil {
				return fmt.Sprintf("cannot read worktree discard decision: %v", err)
			}
			for _, step := range steps {
				if step.Status == types.StepStatusFailed && step.Error != nil && types.IsUserAbortStepError(*step.Error) {
					return ""
				}
			}
		}
	}
	return worktreeEditsCleanupReason(ctx, dir)
}

// worktreeEditsCleanupReason protects edits that a recovery ref cannot save.
// Check at removal as well as discovery: a retained directory can become dirty
// between cleanup passes. Empty setup leftovers need no Git metadata, but a
// populated directory with unreadable metadata must never reach RemoveAll.
func worktreeEditsCleanupReason(ctx context.Context, dir string) string {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		return fmt.Sprintf("cannot inspect worktree: %v", err)
	}
	if len(entries) == 0 {
		return ""
	}
	if _, err := os.Lstat(filepath.Join(dir, ".git")); err != nil {
		return fmt.Sprintf("cannot inspect worktree Git metadata: %v", err)
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	status, err := git.Run(ctx, dir, "-c", "core.fsmonitor=false", "status", "--porcelain=v1", "--untracked-files=all", "--ignore-submodules=none")
	if err != nil {
		return fmt.Sprintf("cannot determine worktree dirtiness: %v", err)
	}
	if status != "" {
		return "worktree has uncommitted tracked or untracked edits"
	}
	return ""
}

package daemon

import (
	"context"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/worktrees"
)

// maxStepDiffBytes bounds one fix-review diff response. The IPC transport
// frames a whole JSON document per line and caps a line at 1 MiB, so an
// unbounded diff would not merely be large - it would break the connection
// carrying it. Half the frame budget leaves ample room for the rest of the
// response and keeps a pathological change a partial view rather than a
// transport failure.
const maxStepDiffBytes = 512 * 1024

// StepDiff returns the working-tree diff for a run that is parked at a
// fix-review gate, derived on demand from the run's worktree.
//
// This diff is the only piece of gate context the pipeline never persists, so
// it is the one thing a subscriber cannot rebuild from get_run. Serving it
// here rather than attaching it to a step_completed event keeps the largest
// possible payload off the event stream, where a frame over the transport
// limit would kill the subscription and hide every event after it.
//
// It is read-only, fails closed on an unknown run or repo, and never persists
// source text.
func (m *RunManager) StepDiff(ctx context.Context, runID string) (string, bool, error) {
	run, err := m.db.GetRun(runID)
	if err != nil {
		return "", false, fmt.Errorf("get run: %w", err)
	}
	if run == nil {
		return "", false, fmt.Errorf("run not found: %s", runID)
	}
	repo, err := m.db.GetRepo(run.RepoID)
	if err != nil {
		return "", false, fmt.Errorf("get repo: %w", err)
	}
	if repo == nil {
		return "", false, fmt.Errorf("repo not found for run %s", runID)
	}

	diff, err := git.DiffHead(ctx, worktrees.RecordedDir(m.paths, run.WorktreePath(), repo.ID, run.ID))
	if err != nil {
		return "", false, fmt.Errorf("diff worktree: %w", err)
	}
	if len(diff) > maxStepDiffBytes {
		return diff[:maxStepDiffBytes], true, nil
	}
	return diff, false, nil
}

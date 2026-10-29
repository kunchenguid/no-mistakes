package daemon

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/fsutil"
)

// reapRunLogs bounds <NM_HOME>/logs/<run_id>, the per-run step-log directory
// Executor.Execute/Resume create (paths.RunLogDir) and never anything else
// removes. It was never given its own reaper, so a long-lived daemon
// accumulated one small text-log directory per run forever - on one real
// install this had already reached 650 directories with zero cleanup.
//
// There is no dedicated config surface for this: these are the same kind of
// per-run diagnostic artifact test.evidence.retention already bounds, so the
// daemon's own evidence policy governs both rather than adding a second knob
// an operator has to learn. `no-mistakes axi logs --run <id>` already
// tolerates a missing step log as an ordinary "not recorded for this run"
// case (see runAxiLogs), so reaping an old run's directory degrades exactly
// like evidence retention already does for screenshots - not a new trade-off.
//
// The shape mirrors reapEvidence: an entry whose run is still pending/running
// is never touched (a step may still be appending to its log), a directory
// older than the retention window is removed, and whatever survives is
// trimmed to the run ceiling, oldest first. Unlike evidence there is no
// "empty directory" rule - MkdirAll here always precedes at least one step
// log being opened for append, so an empty log directory is not the dominant
// case it is for evidence.
func reapRunLogs(ctx context.Context, d *db.DB, root string, policy evidenceReapPolicy, now time.Time) {
	if ctx.Err() != nil {
		return
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return // directory may not exist yet, which is the normal case
	}

	type candidate struct {
		path    string
		modTime time.Time
		runID   string
	}
	candidates := make([]candidate, 0, len(entries))
	for _, entry := range entries {
		if ctx.Err() != nil {
			return
		}
		if !entry.IsDir() {
			continue
		}
		runID := entry.Name()
		run, err := d.GetRun(runID)
		if err != nil {
			slog.Debug("skipping run log cleanup", "run_id", runID, "reason", err)
			continue
		}
		if run == nil {
			continue
		}
		if skip, reason := skipWorktreeCleanup(ctx, d, runID, run.WorktreePath()); skip {
			slog.Debug("skipping run log cleanup", "run_id", runID, "reason", reason)
			continue
		}
		path := filepath.Join(root, runID)
		info, err := entry.Info()
		if err != nil {
			continue
		}
		// The directory's own mtime only advances when a step log file is
		// created inside it, not when an already-open one is appended to, so
		// a long-running step's most recent writes can leave the directory
		// looking far older than its logs actually are. The newest
		// contained file's mtime catches that recent activity; the
		// directory's own mtime is still the floor for a directory with no
		// files yet.
		modTime := info.ModTime()
		if newest, ok := newestFileModTime(ctx, path); ok && newest.After(modTime) {
			modTime = newest
		}
		candidates = append(candidates, candidate{path: path, modTime: modTime, runID: runID})
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].modTime.Before(candidates[j].modTime)
	})

	removed := 0
	survivors := make([]candidate, 0, len(candidates))
	for _, c := range candidates {
		if ctx.Err() != nil {
			return
		}
		expired := policy.Retention > 0 && now.Sub(c.modTime) > policy.Retention
		if expired {
			if removeRunLogDir(ctx, c.path, c.runID) {
				removed++
			}
			continue
		}
		survivors = append(survivors, c)
	}

	if policy.MaxRuns > 0 && len(survivors) > policy.MaxRuns {
		for _, c := range survivors[:len(survivors)-policy.MaxRuns] {
			if ctx.Err() != nil {
				return
			}
			if removeRunLogDir(ctx, c.path, c.runID) {
				removed++
			}
		}
	}

	if removed > 0 {
		slog.Info("reaped run logs", "root", root, "removed", removed)
	}
}

func removeRunLogDir(ctx context.Context, path, runID string) bool {
	if err := fsutil.RemoveAll(ctx, path); err != nil {
		slog.Warn("failed to remove run log directory", "path", path, "run_id", runID, "error", err)
		return false
	}
	return true
}

// newestFileModTime returns the most recent modification time among the
// regular files directly inside dir. It reports false when dir has no
// files to compare against.
func newestFileModTime(ctx context.Context, dir string) (time.Time, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return time.Time{}, false
	}
	var newest time.Time
	found := false
	for _, entry := range entries {
		if ctx.Err() != nil {
			return time.Time{}, false
		}
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if !found || info.ModTime().After(newest) {
			newest = info.ModTime()
			found = true
		}
	}
	return newest, found
}

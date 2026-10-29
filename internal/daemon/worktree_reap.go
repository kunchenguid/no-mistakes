package daemon

import (
	"context"
	"log/slog"
	"os"
	"sort"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// worktreeReapPolicy selects leftover directories for attempted cleanup under
// the default <NM_HOME>/worktrees tree. Preservation refusals override these
// budgets, so retained real checkouts can exceed them. A zero Retention
// disables age-based selection and a zero MaxRuns disables count-based
// selection.
type worktreeReapPolicy struct {
	Retention time.Duration
	MaxRuns   int
}

// worktreeReapPolicyFor resolves the policy from a loaded global config,
// falling back to the built-in defaults when configuration is unavailable.
// Retention and the run ceiling are global-only settings (see
// config.WorktreeRaw), so no repository is consulted here.
func worktreeReapPolicyFor(global *config.GlobalConfig) worktreeReapPolicy {
	policy := worktreeReapPolicy{
		Retention: config.DefaultWorktreeRetention,
		MaxRuns:   config.DefaultWorktreeMaxRuns,
	}
	if global == nil {
		return policy
	}
	resolved := config.Merge(global, &config.RepoConfig{})
	policy.Retention = resolved.Worktree.Retention
	policy.MaxRuns = resolved.Worktree.MaxRuns
	return policy
}

// reapWorktrees attempts cleanup of selected leftover directories under the
// default <NM_HOME>/worktrees tree (issue #1093).
//
// It runs after each finished run (RunManager.cleanupRunWorktrees) and at
// startup. Both paths retain real checkouts, even when clean or successfully
// snapshotted, because stability through removal is unproven. Age and count
// budgets therefore cannot ensure reclamation until a separate stability
// proof exists. Leftover directories that are not checkouts remain removable.
//
// It reuses defaultTreeOrphanWorktrees for directory discovery and
// eligibility (never a directory whose run is still pending/running, still
// protected, or unpushed after a CI-monitor interruption - see
// skipWorktreeCleanup and protectedPathCleanupReason). Eligible directories
// older than the retention window are selected first, followed by enough
// remaining directories to meet the count target, oldest first.
// removeOrphanWorktree can refuse every selected checkout; selection is not
// proof of removal. Unlike evidence, an eligible worktree is never removed
// merely for being "empty" -
// a real checkout is never empty, so there is nothing analogous to
// reapEvidence's empty-run case. Only the default tree is scanned: an
// operator-configured worktree_roots placement is the operator's own
// directory, and startup cleanup attempts its recorded leftovers once per
// startup under the same preservation rule.
func reapWorktrees(ctx context.Context, d *db.DB, p *paths.Paths, policy worktreeReapPolicy, now time.Time) {
	if ctx.Err() != nil {
		return
	}
	removable, _ := defaultTreeOrphanWorktrees(ctx, d, p)
	if len(removable) == 0 {
		return
	}
	toRemove := worktreeReapCandidates(ctx, removable, policy, now)
	if len(toRemove) == 0 || ctx.Err() != nil {
		return
	}

	removed := removeOrphanWorktrees(ctx, p.WorktreesDir(), toRemove, "worktree_reap")

	if removed > 0 {
		slog.Info("reaped leftover run worktrees", "removed", removed)
	}
}

// worktreeReapCandidates applies policy to removable, oldest first, and
// returns exactly the directories it selects for attempted cleanup: eligible
// directories older than the retention window, plus whatever survives that
// cut trimmed to the run ceiling. It is the single source of candidate
// selection, shared by reapWorktrees and by the startup process sweep (see
// retainedDefaultTreeRunIDs in daemon.go), so a worktree the policy is
// keeping is never treated as orphaned by one caller while the other retains
// it.
func worktreeReapCandidates(ctx context.Context, removable []orphanWorktree, policy worktreeReapPolicy, now time.Time) []orphanWorktree {
	type candidate struct {
		wt      orphanWorktree
		modTime time.Time
	}
	candidates := make([]candidate, 0, len(removable))
	for _, wt := range removable {
		if ctx.Err() != nil {
			return nil
		}
		info, err := os.Stat(wt.dir)
		if err != nil {
			continue
		}
		candidates = append(candidates, candidate{wt: wt, modTime: info.ModTime()})
	}
	if len(candidates) == 0 {
		return nil
	}

	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].modTime.Before(candidates[j].modTime)
	})

	var toRemove []candidate
	survivors := make([]candidate, 0, len(candidates))
	for _, c := range candidates {
		expired := policy.Retention > 0 && now.Sub(c.modTime) > policy.Retention
		if expired {
			toRemove = append(toRemove, c)
			continue
		}
		survivors = append(survivors, c)
	}

	if policy.MaxRuns > 0 && len(survivors) > policy.MaxRuns {
		toRemove = append(toRemove, survivors[:len(survivors)-policy.MaxRuns]...)
	}

	out := make([]orphanWorktree, 0, len(toRemove))
	for _, c := range toRemove {
		out = append(out, c.wt)
	}
	return out
}

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
	"github.com/kunchenguid/no-mistakes/internal/procreap"
)

// worktreeReapPolicy bounds how many leftover run-worktree directories
// survive under the default <NM_HOME>/worktrees tree. A zero Retention
// disables age-based reaping and a zero MaxRuns disables the count ceiling,
// matching evidenceReapPolicy.
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

// reapWorktrees bounds leftover run-worktree directories under the default
// <NM_HOME>/worktrees tree (issue #1093).
//
// A run's own worktree is already removed the instant its pipeline finishes
// (RunManager.removeRunWorktree), so on the common path there is nothing here
// to reap. This exists for the uncommon path: a `git worktree remove` failure
// (e.g. a vendored/nested .git under a large node_modules tree) or a
// protected-path refusal that later became removable both leave a directory
// that immediate removal never retries. cleanupOrphanWorktrees already
// reclaims every such leftover unconditionally, but only once, at the next
// daemon startup - on a long-lived install that can be weeks away, which is
// exactly the "long-running host" accumulation the issue reports. Calling
// this after every run's own cleanup (see RunManager.cleanupRunEvidence)
// converges a live daemon on the same budget without waiting for a restart.
//
// It reuses defaultTreeOrphanWorktrees for directory discovery and
// eligibility (never a directory whose run is still pending/running, still
// protected, or unpushed after a CI-monitor interruption - see
// skipWorktreeCleanup and protectedPathCleanupReason), then applies the same
// two-rule bound reapEvidence applies to evidence: an eligible directory
// older than the retention window is removed, and whatever survives is
// trimmed to the run ceiling, oldest first. Unlike evidence, an eligible
// worktree is never removed merely for being "empty" - a real checkout is
// never empty, so there is nothing analogous to reapEvidence's empty-run
// case. Only the default tree is scanned: an operator-configured
// worktree_roots placement is the operator's own directory, and startup
// cleanup already reclaims it exactly once per crash, which is this
// function's counterpart for the tree no-mistakes owns outright.
func reapWorktrees(d *db.DB, p *paths.Paths, policy worktreeReapPolicy, now time.Time) {
	removable, _ := defaultTreeOrphanWorktrees(d, p)
	if len(removable) == 0 {
		return
	}

	type candidate struct {
		wt      orphanWorktree
		modTime time.Time
	}
	candidates := make([]candidate, 0, len(removable))
	for _, wt := range removable {
		info, err := os.Stat(wt.dir)
		if err != nil {
			continue
		}
		candidates = append(candidates, candidate{wt: wt, modTime: info.ModTime()})
	}
	if len(candidates) == 0 {
		return
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

	if len(toRemove) == 0 {
		return
	}

	// Only the directories actually selected for removal are swept: sweeping
	// a retained worktree would kill processes still using a checkout the
	// policy was meant to keep.
	sweepable := make([]procreap.Worktree, 0, len(toRemove))
	for _, c := range toRemove {
		sweepable = append(sweepable, procreap.Worktree{Dir: c.wt.dir, RepoID: c.wt.repoID, RunID: c.wt.runID})
	}
	sweepRunWorktrees(p.WorktreesDir(), sweepable, "worktree_reap")

	removed := 0
	for _, c := range toRemove {
		if removeOrphanWorktree(context.Background(), c.wt) {
			removed++
		}
	}

	if removed > 0 {
		slog.Info("reaped leftover run worktrees", "removed", removed)
	}
}

package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/worktrees"
)

// legacyEvidenceDirName is the fixed directory no-mistakes used to create
// directly inside the system temp directory. It was never reaped by anything in
// this program: on Linux the daemon's TMPDIR is unset, so it resolved to the
// shared /tmp - a RAM-backed tmpfs on current Ubuntu - and it accumulated one
// subdirectory per run until an OS timer or a reboot happened to clear it.
//
// Evidence now lives under the app root (paths.EvidenceDir). This name survives
// only so an upgraded daemon drains what earlier versions left behind, under
// the same retention policy and the same active-run guard. Delete this constant
// and reapLegacyEvidence once installs from before the relocation are gone.
const legacyEvidenceDirName = "no-mistakes-evidence"

// evidenceReapPolicy bounds how much on-disk evidence survives. A zero
// Retention disables age-based reaping and a zero MaxRuns disables the count
// ceiling; both zero keeps everything except empty directories.
type evidenceReapPolicy struct {
	Retention time.Duration
	MaxRuns   int
}

// evidenceReapPolicyFor resolves the policy from a loaded global config,
// falling back to the built-in defaults when configuration is unavailable.
// Retention and the run ceiling are global-only settings (see
// config.EvidenceRaw), so no repository is consulted here.
func evidenceReapPolicyFor(global *config.GlobalConfig) evidenceReapPolicy {
	policy := evidenceReapPolicy{
		Retention: config.DefaultEvidenceRetention,
		MaxRuns:   config.DefaultEvidenceMaxRuns,
	}
	if global == nil {
		return policy
	}
	resolved := config.Merge(global, &config.RepoConfig{})
	policy.Retention = resolved.Test.Evidence.Retention
	policy.MaxRuns = resolved.Test.Evidence.MaxRuns
	return policy
}

// evidenceRootFor resolves this machine's evidence root from a loaded global
// config, falling back to the app-root default.
func evidenceRootFor(p *paths.Paths, global *config.GlobalConfig) string {
	roots := evidenceRootsFor(p, global, nil)
	if len(roots) == 0 {
		return ""
	}
	return roots[0]
}

func evidenceWorktreePaths(p *paths.Paths, global *config.GlobalConfig, d *db.DB) ([]string, error) {
	dirs := []string{p.WorktreesDir()}
	if global != nil {
		for checkout, root := range global.WorktreeRoots {
			dirs = append(dirs, checkout, root)
		}
	}
	if d != nil {
		recorded, err := d.GetRunWorktreeDirs()
		if err != nil {
			return nil, err
		}
		dirs = append(dirs, recorded...)
		repos, err := d.GetRepos()
		if err != nil {
			return nil, err
		}
		for _, repo := range repos {
			dirs = append(dirs, repo.WorkingPath)
		}
	}
	return dirs, nil
}

func evidencePlacementConflict(root string, worktreePaths []string) string {
	for _, dir := range worktreePaths {
		if dir != "" && (worktrees.Contains(dir, root) || worktrees.Contains(root, dir)) {
			return dir
		}
	}
	return ""
}

func evidenceRootsFor(p *paths.Paths, global *config.GlobalConfig, d *db.DB) []string {
	if p == nil {
		return nil
	}
	evidenceDir := ""
	localRoot := ""
	if global != nil {
		evidenceDir = global.EvidenceDir
		localRoot = config.Merge(global, &config.RepoConfig{}).Test.Evidence.LocalRoot
	}
	root, warning := p.ResolveEvidenceRoot(evidenceDir, localRoot)
	if warning != "" {
		slog.Warn("evidence directory fallback", "detail", warning)
	}
	worktreePaths, err := evidenceWorktreePaths(p, global, d)
	if err != nil {
		slog.Warn("failed to discover worktree placements for evidence cleanup", "error", err)
		return nil
	}
	seen := map[string]bool{}
	var roots []string
	add := func(candidate string) {
		candidate = filepath.Clean(candidate)
		if candidate == "" || candidate == "." || seen[candidate] || evidencePlacementConflict(candidate, worktreePaths) != "" {
			return
		}
		seen[candidate] = true
		roots = append(roots, candidate)
	}
	add(root)
	add(p.EvidenceDir())
	add(p.EvidenceRoot(localRoot))
	if d != nil {
		dirs, err := d.GetRunEvidenceDirs()
		if err != nil {
			slog.Warn("failed to discover pinned evidence roots", "error", err)
		}
		for _, dir := range dirs {
			add(filepath.Dir(dir))
		}
	}
	return roots
}

// pinRunEvidenceDir records the evidence directory for a new run. A later
// change to evidence_dir does not move it. Runs that already have a pin,
// including a second call, stay where they are.
func (m *RunManager) pinRunEvidenceDir(run *db.Run, global *config.GlobalConfig) error {
	if m == nil || run == nil || global == nil || run.EvidencePath() != "" {
		return nil
	}
	root, warning := m.paths.ResolveEvidenceRoot(global.EvidenceDir, config.Merge(global, &config.RepoConfig{}).Test.Evidence.LocalRoot)
	worktreePaths, err := evidenceWorktreePaths(m.paths, global, m.db)
	if err != nil {
		return fmt.Errorf("read worktree placements: %w", err)
	}
	if conflict := evidencePlacementConflict(root, worktreePaths); conflict != "" {
		warning = fmt.Sprintf("evidence directory %q overlaps worktree placement %q; using %s", root, conflict, m.paths.EvidenceDir())
		root = m.paths.EvidenceDir()
	}
	if warning != "" {
		slog.Warn("evidence directory fallback", "run_id", run.ID, "detail", warning)
	}
	if conflict := evidencePlacementConflict(root, worktreePaths); conflict != "" {
		return fmt.Errorf("default evidence directory %q overlaps worktree placement %q", root, conflict)
	}
	dir := filepath.Join(root, run.ID)
	if err := m.db.SetRunEvidenceDir(run.ID, dir); err != nil {
		return err
	}
	run.EvidenceDir = &dir
	return nil
}

// recordedRunEvidenceDir returns the directory pinned on the run, or the
// local_root path for a run that predates the pin.
func (m *RunManager) recordedRunEvidenceDir(cfg *config.Config, runID string) string {
	if m != nil && m.db != nil && runID != "" {
		if run, err := m.db.GetRun(runID); err == nil && run != nil && run.EvidencePath() != "" {
			return run.EvidencePath()
		}
	}
	configured := ""
	if cfg != nil {
		configured = cfg.Test.Evidence.LocalRoot
	}
	if m == nil || m.paths == nil {
		return ""
	}
	return m.paths.RunEvidenceDir(configured, runID)
}

// pipelineEvidenceRoot is the one out-of-worktree location steering permits.
// A pinned run names its own directory so a later evidence_dir change cannot
// point the agent at a different tree. An unpinned run keeps the configured root.
func pipelineEvidenceRoot(p *paths.Paths, cfg *config.Config, run *db.Run) string {
	if run != nil && run.EvidencePath() != "" {
		return run.EvidencePath()
	}
	if p == nil {
		return ""
	}
	configured := ""
	if cfg != nil {
		configured = cfg.Test.Evidence.LocalRoot
	}
	return p.EvidenceRoot(configured)
}

// reapEvidence bounds the on-disk evidence directory. It is what makes
// no-mistakes responsible for its own scratch: before the relocation, evidence
// sat in the shared system temp directory and the only thing that ever removed
// it was an OS timer this program does not control, does not configure, and
// cannot rely on across macOS, Linux, and Windows.
//
// Three rules, applied oldest-first and all best effort:
//
//  1. An empty run directory is removed regardless of age. The test step
//     creates the directory before the agent decides whether it has anything to
//     write, so a run that produced no artifact would otherwise leave a
//     permanent entry behind. Nearly all of the observed accumulation was this.
//  2. A run directory older than the retention window is removed.
//  3. Whatever survives is trimmed to the run ceiling, oldest first.
//
// A directory whose run row is still pending or running is never touched, at
// any step - the same active-run guard cleanupOrphanWorktrees uses, for the
// same reason: only a settled run's leftovers are safe to remove.
func reapEvidence(d *db.DB, root string, policy evidenceReapPolicy, now time.Time, additionalRoots ...string) {
	type candidate struct {
		path    string
		modTime time.Time
		isEmpty bool
		runID   string
	}
	var candidates []candidate
	seen := map[string]bool{}
	for _, root := range append([]string{root}, additionalRoots...) {
		root = worktrees.Canonical(root)
		if seen[root] {
			continue
		}
		seen[root] = true
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			runID := entry.Name()
			run, err := d.GetRun(runID)
			if err != nil {
				slog.Debug("skipping evidence cleanup", "run_id", runID, "reason", err)
				continue
			}
			if run == nil {
				continue
			}
			if skip, reason := skipWorktreeCleanup(context.Background(), d, runID, run.WorktreePath()); skip {
				slog.Debug("skipping evidence cleanup", "run_id", runID, "reason", reason)
				continue
			}
			path := filepath.Join(root, runID)
			if pinned := run.EvidencePath(); pinned != "" && worktrees.Canonical(pinned) != worktrees.Canonical(path) {
				continue
			}
			if wt := run.WorktreePath(); wt != "" && evidencePlacementConflict(path, []string{wt}) != "" {
				continue
			}
			info, err := entry.Info()
			if err != nil {
				continue
			}
			inner, err := os.ReadDir(path)
			if err != nil {
				continue
			}
			candidates = append(candidates, candidate{
				path:    path,
				modTime: info.ModTime(),
				isEmpty: len(inner) == 0,
				runID:   runID,
			})
		}

	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].modTime.Before(candidates[j].modTime)
	})

	removed := 0
	survivors := make([]candidate, 0, len(candidates))
	for _, c := range candidates {
		expired := policy.Retention > 0 && now.Sub(c.modTime) > policy.Retention
		if c.isEmpty || expired {
			if removeEvidenceDir(c.path, c.runID) {
				removed++
			}
			continue
		}
		survivors = append(survivors, c)
	}

	if policy.MaxRuns > 0 && len(survivors) > policy.MaxRuns {
		for _, c := range survivors[:len(survivors)-policy.MaxRuns] {
			if removeEvidenceDir(c.path, c.runID) {
				removed++
			}
		}
	}

	if removed > 0 {
		slog.Info("reaped run evidence", "roots", len(seen), "removed", removed)
	}
}

func removeEvidenceDir(path, runID string) bool {
	if err := os.RemoveAll(path); err != nil {
		slog.Warn("failed to remove run evidence", "path", path, "run_id", runID, "error", err)
		return false
	}
	return true
}

// reapLegacyEvidence drains the pre-relocation directory in the system temp
// directory under the same policy. Contents are deliberately NOT migrated: the
// absolute paths already written into older PR bodies name the old location, so
// moving files would not repair them, and a wholesale removal could delete
// artifacts a run started before the upgrade is still writing. Reaping by the
// same rules, with the same active-run guard, is bounded and surprises nobody.
func reapLegacyEvidence(d *db.DB, current string, policy evidenceReapPolicy, now time.Time, additionalRoots ...string) {
	legacy := filepath.Join(os.TempDir(), legacyEvidenceDirName)
	roots := append([]string{current}, additionalRoots...)
	roots = append(roots, legacy)
	reapEvidence(d, roots[0], policy, now, roots[1:]...)
	// Remove the root itself once it is empty; os.Remove fails harmlessly while
	// anything remains.
	if !worktrees.Contains(current, legacy) {
		_ = os.Remove(legacy)
	}
}

package daemon

import (
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
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
	configured := ""
	if global != nil {
		configured = config.Merge(global, &config.RepoConfig{}).Test.Evidence.LocalRoot
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
func reapEvidence(d *db.DB, root string, policy evidenceReapPolicy, now time.Time) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return // directory may not exist yet, which is the normal case
	}

	type candidate struct {
		path     string
		modTime  time.Time
		isEmpty  bool
		runID    string
		reapable bool
	}
	candidates := make([]candidate, 0, len(entries))
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
		if skip, reason := skipWorktreeCleanup(d, runID); skip {
			slog.Debug("skipping evidence cleanup", "run_id", runID, "reason", reason)
			continue
		}
		path := filepath.Join(root, runID)
		info, err := entry.Info()
		if err != nil {
			continue
		}
		inner, err := os.ReadDir(path)
		if err != nil {
			continue
		}
		candidates = append(candidates, candidate{
			path:     path,
			modTime:  info.ModTime(),
			isEmpty:  len(inner) == 0,
			runID:    runID,
			reapable: true,
		})
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
		slog.Info("reaped run evidence", "root", root, "removed", removed)
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
func reapLegacyEvidence(d *db.DB, current string, policy evidenceReapPolicy, now time.Time) {
	legacy := filepath.Join(os.TempDir(), legacyEvidenceDirName)
	if legacy == current {
		return // an operator who pointed local_root back at it owns it now
	}
	if _, err := os.Stat(legacy); err != nil {
		return
	}
	reapEvidence(d, legacy, policy, now)
	// Remove the root itself once it is empty; os.Remove fails harmlessly while
	// anything remains.
	_ = os.Remove(legacy)
}

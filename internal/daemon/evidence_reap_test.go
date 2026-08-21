package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// evidenceFixture builds a DB plus an evidence root and returns helpers for
// seeding run directories with a chosen age and status.
type evidenceFixture struct {
	t    *testing.T
	db   *db.DB
	p    *paths.Paths
	repo *db.Repo
	root string
}

func newEvidenceFixture(t *testing.T) *evidenceFixture {
	t.Helper()
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	repo, err := d.InsertRepoWithID("repo1", "/nonexistent/work", "https://example.com/owner/repo1", "main")
	if err != nil {
		t.Fatal(err)
	}
	root := p.EvidenceRoot("")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	return &evidenceFixture{t: t, db: d, p: p, repo: repo, root: root}
}

// seed creates a run in the given status plus its evidence directory, aged by
// backdating the directory's modification time.
func (f *evidenceFixture) seed(branch string, status types.RunStatus, age time.Duration, files map[string]string) string {
	f.t.Helper()
	run, err := f.db.InsertRun(f.repo.ID, branch, "head-"+branch, "base-"+branch)
	if err != nil {
		f.t.Fatal(err)
	}
	if status != types.RunPending {
		if err := f.db.UpdateRunStatus(run.ID, status); err != nil {
			f.t.Fatal(err)
		}
	}
	dir := filepath.Join(f.root, run.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.t.Fatal(err)
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			f.t.Fatal(err)
		}
	}
	when := time.Now().Add(-age)
	if err := os.Chtimes(dir, when, when); err != nil {
		f.t.Fatal(err)
	}
	return run.ID
}

func (f *evidenceFixture) exists(runID string) bool {
	f.t.Helper()
	_, err := os.Stat(filepath.Join(f.root, runID))
	return err == nil
}

// TestReapEvidenceHonorsRetentionAndSparesActiveRuns is the core reaper
// contract: no-mistakes bounds its own evidence directory on both axes, and
// never removes a directory belonging to a run that is still in flight.
func TestReapEvidenceHonorsRetentionAndSparesActiveRuns(t *testing.T) {
	f := newEvidenceFixture(t)
	artifact := map[string]string{"screenshot.png": "not really a png"}

	fresh := f.seed("fresh", types.RunCompleted, time.Hour, artifact)
	stale := f.seed("stale", types.RunCompleted, 30*24*time.Hour, artifact)
	activePending := f.seed("active-pending", types.RunPending, 30*24*time.Hour, artifact)
	activeRunning := f.seed("active-running", types.RunRunning, 30*24*time.Hour, artifact)

	reapEvidence(f.db, f.root, evidenceReapPolicy{Retention: 14 * 24 * time.Hour}, time.Now())

	if !f.exists(fresh) {
		t.Error("evidence inside the retention window was removed")
	}
	if f.exists(stale) {
		t.Error("evidence older than the retention window survived")
	}
	if !f.exists(activePending) {
		t.Error("a pending run's evidence was removed while the run is still in flight")
	}
	if !f.exists(activeRunning) {
		t.Error("a running run's evidence was removed while the run is still in flight")
	}
}

// TestReapEvidenceRemovesEmptyRunDirectoriesRegardlessOfAge targets the
// dominant source of accumulation. The test step creates the run's directory
// before the agent decides whether it has anything to write, so a run that
// produced no artifact would otherwise leave a permanent entry behind - on a
// real machine this was 94% of everything in the directory.
func TestReapEvidenceRemovesEmptyRunDirectoriesRegardlessOfAge(t *testing.T) {
	f := newEvidenceFixture(t)

	empty := f.seed("empty", types.RunCompleted, time.Minute, nil)
	emptyActive := f.seed("empty-active", types.RunRunning, time.Minute, nil)
	withArtifact := f.seed("kept", types.RunCompleted, time.Minute, map[string]string{"log.txt": "output"})

	reapEvidence(f.db, f.root, evidenceReapPolicy{Retention: 14 * 24 * time.Hour, MaxRuns: 100}, time.Now())

	if f.exists(empty) {
		t.Error("an empty evidence directory survived")
	}
	if !f.exists(emptyActive) {
		t.Error("an in-flight run's evidence directory was removed before it could write to it")
	}
	if !f.exists(withArtifact) {
		t.Error("a directory holding an artifact was removed")
	}
}

// TestReapEvidenceTrimsToTheRunCeilingOldestFirst covers the second bound: a
// burst of runs that all land inside the retention window still cannot grow the
// directory without limit.
func TestReapEvidenceTrimsToTheRunCeilingOldestFirst(t *testing.T) {
	f := newEvidenceFixture(t)
	artifact := map[string]string{"log.txt": "output"}

	oldest := f.seed("oldest", types.RunCompleted, 4*time.Hour, artifact)
	middle := f.seed("middle", types.RunCompleted, 3*time.Hour, artifact)
	newest := f.seed("newest", types.RunCompleted, time.Hour, artifact)

	reapEvidence(f.db, f.root, evidenceReapPolicy{Retention: 14 * 24 * time.Hour, MaxRuns: 2}, time.Now())

	if f.exists(oldest) {
		t.Error("the oldest run's evidence survived the run ceiling")
	}
	if !f.exists(middle) || !f.exists(newest) {
		t.Error("the two newest runs' evidence should have been kept")
	}
}

// TestReapEvidenceKeepsEverythingWhenBothBoundsAreDisabled proves the operator
// escape hatch really disables reaping, with the deliberate exception of empty
// directories, which carry nothing to lose.
func TestReapEvidenceKeepsEverythingWhenBothBoundsAreDisabled(t *testing.T) {
	f := newEvidenceFixture(t)
	artifact := map[string]string{"log.txt": "output"}

	ancient := f.seed("ancient", types.RunCompleted, 365*24*time.Hour, artifact)
	recent := f.seed("recent", types.RunCompleted, time.Hour, artifact)

	reapEvidence(f.db, f.root, evidenceReapPolicy{Retention: 0, MaxRuns: 0}, time.Now())

	if !f.exists(ancient) || !f.exists(recent) {
		t.Error("evidence was reaped even though both bounds are disabled")
	}
}

func TestReapEvidenceLeavesUnownedDirectoriesAlone(t *testing.T) {
	f := newEvidenceFixture(t)
	unowned := filepath.Join(f.root, "unrelated-directory")
	if err := os.MkdirAll(unowned, 0o755); err != nil {
		t.Fatal(err)
	}
	when := time.Now().Add(-365 * 24 * time.Hour)
	if err := os.Chtimes(unowned, when, when); err != nil {
		t.Fatal(err)
	}

	reapEvidence(f.db, f.root, evidenceReapPolicy{Retention: time.Hour, MaxRuns: 1}, time.Now())

	if _, err := os.Stat(unowned); err != nil {
		t.Errorf("reaper removed a directory not owned by a recorded run: %v", err)
	}
}

// TestReapLegacyEvidenceDrainsTheSharedTempRootUnderTheSamePolicy covers the
// upgrade path. Older versions wrote to a fixed directory inside the system
// temp directory and never reaped it; an upgraded daemon drains that under the
// same rules - including the active-run guard - instead of migrating files
// whose absolute paths older PR bodies already name, or wiping a directory a
// run started before the upgrade may still be writing to.
func TestReapLegacyEvidenceDrainsTheSharedTempRootUnderTheSamePolicy(t *testing.T) {
	f := newEvidenceFixture(t)

	// os.TempDir() reads TMPDIR on Unix, so pointing it at a temp directory
	// exercises the real resolution rather than a test-only seam.
	fakeTemp := t.TempDir()
	t.Setenv("TMPDIR", fakeTemp)
	if os.TempDir() != fakeTemp {
		t.Skipf("os.TempDir() does not follow TMPDIR on this platform (got %q)", os.TempDir())
	}
	legacy := filepath.Join(fakeTemp, legacyEvidenceDirName)

	seedLegacy := func(branch string, status types.RunStatus, age time.Duration) string {
		t.Helper()
		run, err := f.db.InsertRun(f.repo.ID, branch, "head-"+branch, "base-"+branch)
		if err != nil {
			t.Fatal(err)
		}
		if status != types.RunPending {
			if err := f.db.UpdateRunStatus(run.ID, status); err != nil {
				t.Fatal(err)
			}
		}
		dir := filepath.Join(legacy, run.ID)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "old.png"), []byte("artifact"), 0o644); err != nil {
			t.Fatal(err)
		}
		when := time.Now().Add(-age)
		if err := os.Chtimes(dir, when, when); err != nil {
			t.Fatal(err)
		}
		return run.ID
	}

	stale := seedLegacy("legacy-stale", types.RunCompleted, 30*24*time.Hour)
	fresh := seedLegacy("legacy-fresh", types.RunCompleted, time.Hour)
	inFlight := seedLegacy("legacy-active", types.RunRunning, 30*24*time.Hour)

	reapLegacyEvidence(f.db, f.root, evidenceReapPolicy{Retention: 14 * 24 * time.Hour}, time.Now())

	if _, err := os.Stat(filepath.Join(legacy, stale)); err == nil {
		t.Error("stale legacy evidence survived the drain")
	}
	if _, err := os.Stat(filepath.Join(legacy, fresh)); err != nil {
		t.Errorf("legacy evidence inside the retention window was removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(legacy, inFlight)); err != nil {
		t.Errorf("an in-flight run's legacy evidence was removed: %v", err)
	}
}

// TestReapLegacyEvidenceLeavesTheCurrentRootAlone: an operator who pointed
// local_root back at the legacy location owns it, and the drain must not apply
// its policy twice to the same directory.
func TestReapLegacyEvidenceLeavesTheCurrentRootAlone(t *testing.T) {
	f := newEvidenceFixture(t)
	fakeTemp := t.TempDir()
	t.Setenv("TMPDIR", fakeTemp)
	if os.TempDir() != fakeTemp {
		t.Skipf("os.TempDir() does not follow TMPDIR on this platform (got %q)", os.TempDir())
	}
	legacy := filepath.Join(fakeTemp, legacyEvidenceDirName)
	if err := os.MkdirAll(filepath.Join(legacy, "run-x"), 0o755); err != nil {
		t.Fatal(err)
	}

	reapLegacyEvidence(f.db, legacy, evidenceReapPolicy{Retention: time.Nanosecond}, time.Now())

	if _, err := os.Stat(filepath.Join(legacy, "run-x")); err != nil {
		t.Errorf("the drain reaped the configured current root: %v", err)
	}
}

// TestEvidenceReapPolicyForUsesGlobalConfigAndDefaults keeps the daemon's
// resolution aligned with the global-only config contract.
func TestEvidenceReapPolicyForUsesGlobalConfigAndDefaults(t *testing.T) {
	if got := evidenceReapPolicyFor(nil); got.Retention != config.DefaultEvidenceRetention || got.MaxRuns != config.DefaultEvidenceMaxRuns {
		t.Errorf("nil global config = %+v, want the built-in defaults", got)
	}

	global, err := config.LoadGlobalFromBytes([]byte("test:\n  evidence:\n    retention: 100h\n    max_runs: 7\n"))
	if err != nil {
		t.Fatal(err)
	}
	got := evidenceReapPolicyFor(global)
	if got.Retention != 100*time.Hour || got.MaxRuns != 7 {
		t.Errorf("policy = %+v, want retention 100h and max_runs 7", got)
	}
}

// TestEvidenceRootForHonorsConfiguredLocalRoot pins the daemon-side resolution
// of where evidence lives to the same answer the pipeline uses.
func TestEvidenceRootForHonorsConfiguredLocalRoot(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if got, want := evidenceRootFor(p, nil), p.EvidenceDir(); got != want {
		t.Errorf("evidenceRootFor(nil) = %q, want %q", got, want)
	}

	override := t.TempDir()
	global, err := config.LoadGlobalFromBytes([]byte("test:\n  evidence:\n    local_root: " + override + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := evidenceRootFor(p, global); got != filepath.Clean(override) {
		t.Errorf("evidenceRootFor = %q, want the configured override %q", got, override)
	}
}

// TestRunCleanupRemovesEmptyEvidenceDirAndKeepsNonEmpty is the per-run half of
// evidence ownership, and the cheapest of the three layers: a finished run that
// wrote nothing leaves nothing behind, while a run that produced artifacts
// keeps every file. Without it, the test step's up-front MkdirAll left one
// permanent directory per run no matter what the agent decided to write.
func TestRunCleanupRemovesEmptyEvidenceDirAndKeepsNonEmpty(t *testing.T) {
	f := newEvidenceFixture(t)
	m := NewRunManager(f.db, f.p, nil)

	emptyRun := f.seed("empty", types.RunCompleted, time.Minute, nil)
	artifactRun := f.seed("with-artifact", types.RunCompleted, time.Minute, map[string]string{"screenshot.png": "bytes"})

	cfg := config.Merge(config.DefaultGlobalConfig(), &config.RepoConfig{})
	m.cleanupRunEvidence(cfg, emptyRun)
	m.cleanupRunEvidence(cfg, artifactRun)

	if f.exists(emptyRun) {
		t.Error("a finished run that produced no evidence left its directory behind")
	}
	if !f.exists(artifactRun) {
		t.Fatal("a finished run's evidence artifacts were deleted at cleanup")
	}
	if _, err := os.Stat(filepath.Join(f.root, artifactRun, "screenshot.png")); err != nil {
		t.Errorf("evidence artifact did not survive run cleanup: %v", err)
	}
}

// TestRunCleanupIsSafeWithoutConfigAndForUnknownRuns keeps the cleanup path
// from ever being the thing that fails a finished run.
func TestRunCleanupIsSafeWithoutConfigAndForUnknownRuns(t *testing.T) {
	f := newEvidenceFixture(t)
	m := NewRunManager(f.db, f.p, nil)

	kept := f.seed("kept", types.RunCompleted, time.Minute, map[string]string{"log.txt": "output"})

	m.cleanupRunEvidence(nil, "run-that-never-existed")
	m.cleanupRunEvidence(nil, kept)

	if !f.exists(kept) {
		t.Error("cleanup with no config removed a run's evidence artifacts")
	}
}

// recoveredRunTestAgent is the minimum an agent must be for a recovered run to
// reach its completion boundary; nothing in this test invokes it.
type recoveredRunTestAgent struct{}

func (recoveredRunTestAgent) Name() string { return "test" }
func (recoveredRunTestAgent) Run(context.Context, agent.RunOpts) (*agent.Result, error) {
	return &agent.Result{}, nil
}
func (recoveredRunTestAgent) Close() error { return nil }

// TestRecoveredRunCleanupRemovesItsEmptyEvidenceDir covers the OTHER completion
// boundary. A run parked at an approval gate when the daemon stopped is resumed
// by resumeRecoveredRun, which finishes through its own defer rather than the
// fresh-run one - so evidence ownership is only true "after each run" if both
// paths clean up. Every outcome of a resumed run shares that defer, which is why
// asserting on the boundary is enough here: the run below is rejected early by
// Resume, and its evidence directory must still be gone afterwards.
func TestRecoveredRunCleanupRemovesItsEmptyEvidenceDir(t *testing.T) {
	f := newEvidenceFixture(t)
	m := NewRunManager(f.db, f.p, func() []pipeline.Step { return nil })

	runID := f.seed("recovered", types.RunRunning, time.Minute, nil)
	run, err := f.db.GetRun(runID)
	if err != nil {
		t.Fatal(err)
	}

	if !f.exists(runID) {
		t.Fatal("fixture did not create the run's evidence directory")
	}

	m.resumeRecoveredRun(recoveredRunPlan{
		run:     run,
		repo:    f.repo,
		workDir: t.TempDir(),
		gateDir: t.TempDir(),
		cfg:     config.Merge(config.DefaultGlobalConfig(), &config.RepoConfig{}),
		agent:   recoveredRunTestAgent{},
	})
	m.wg.Wait()

	if f.exists(runID) {
		t.Error("a resumed run left its empty evidence directory behind")
	}
}

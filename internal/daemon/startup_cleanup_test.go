package daemon

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/procreap"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func runStartupCleanupFilterProcess(mode, root string) int {
	if mode == "clean" {
		if _, err := io.Copy(os.Stdout, os.Stdin); err != nil {
			return 1
		}
		return 0
	}
	wd, err := os.Getwd()
	if err != nil {
		return 1
	}
	name := filepath.Base(wd)
	if err := os.WriteFile(filepath.Join(root, name+".entered"), nil, 0o644); err != nil {
		return 1
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(root, name+".release")); err == nil {
			if mode == "smudge-pass" {
				if _, err := io.Copy(os.Stdout, os.Stdin); err == nil {
					return 0
				}
			}
			return 1
		}
		time.Sleep(10 * time.Millisecond)
	}
	return 1
}

type startupCleanupFixture struct {
	p        *paths.Paths
	d        *db.DB
	gate     string
	head     string
	controls string
}

func newStartupCleanupFixture(t *testing.T) *startupCleanupFixture {
	t.Helper()
	p := paths.WithRoot(t.TempDir())
	scratch := t.TempDir()
	t.Setenv("TMPDIR", scratch)
	t.Setenv("TMP", scratch)
	t.Setenv("TEMP", scratch)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	_, head := setupTestGitRepo(t, p, d, "startup-cleanup")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	f := &startupCleanupFixture{p: p, d: d, gate: p.RepoDir("startup-cleanup"), head: head, controls: t.TempDir()}
	command := shellQuoteForTest(filepath.ToSlash(executable)) + " --nm-startup-cleanup-filter "
	gitCmd(t, f.gate, "config", "filter.slow.clean", command+"clean "+shellQuoteForTest(filepath.ToSlash(f.controls)))
	gitCmd(t, f.gate, "config", "filter.slow.smudge", command+"smudge "+shellQuoteForTest(filepath.ToSlash(f.controls)))
	gitCmd(t, f.gate, "config", "filter.slow.required", "true")
	previous := sweepRunWorktrees
	sweepRunWorktrees = func(context.Context, string, []procreap.Worktree, string) {}
	t.Cleanup(func() { sweepRunWorktrees = previous })
	return f
}

func (f *startupCleanupFixture) checkout(t *testing.T, recorded bool) orphanWorktree {
	t.Helper()
	run, err := f.d.InsertRun("startup-cleanup", "feature", f.head, f.head)
	if err != nil {
		t.Fatal(err)
	}
	wt := orphanWorktree{gateDir: f.gate, dir: f.p.WorktreeDir(run.RepoID, run.ID), repoID: run.RepoID, runID: run.ID, baseSHA: f.head}
	if recorded {
		wt.dir = filepath.Join(t.TempDir(), "recorded", run.ID)
	}
	gitCmd(t, f.gate, "worktree", "add", "--detach", wt.dir, f.head)
	if err := f.d.SetRunWorktreeDir(run.ID, wt.dir); err != nil {
		t.Fatal(err)
	}
	if err := f.d.UpdateRunStatus(run.ID, types.RunFailed); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt.dir, ".gitattributes"), []byte("test.txt filter=slow\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return wt
}

func (f *startupCleanupFixture) entered(wt orphanWorktree) string {
	return filepath.Join(f.controls, wt.runID+".entered")
}

func (f *startupCleanupFixture) release(t *testing.T, wt orphanWorktree) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.controls, wt.runID+".release"), nil, 0o644); err != nil {
		t.Error(err)
	}
}

func waitForStartupCleanupFilter(t *testing.T, marker string, done <-chan struct{}) {
	t.Helper()
	deadline := time.NewTimer(startupCleanupTimeout + 5*time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		if _, err := os.Stat(marker); err == nil {
			return
		}
		select {
		case <-done:
			t.Fatal("cleanup finished before the slow filter started")
		case <-deadline.C:
			t.Fatal("slow filter did not start")
		case <-tick.C:
		}
	}
}

func assertStartupCheckoutRetained(t *testing.T, wt orphanWorktree) {
	t.Helper()
	if got, err := os.ReadFile(filepath.Join(wt.dir, "test.txt")); err != nil || string(got) != "hello" {
		t.Fatalf("unfinished checkout was changed or removed: %q, %v", got, err)
	}
	if _, err := os.Lstat(filepath.Join(wt.dir, ".git")); err != nil {
		t.Fatalf("unfinished checkout metadata was removed: %v", err)
	}
	if _, err := git.Run(context.Background(), wt.gateDir, "rev-parse", "--verify", git.DirtyWorkRef(wt.runID)); err == nil {
		t.Fatal("unfinished capture published a snapshot")
	}
}

func seedCleanupDiagnostics(t *testing.T, f *startupCleanupFixture, runID string, legacy bool) []string {
	t.Helper()
	files := []string{
		filepath.Join(f.p.EvidenceDir(), runID, "proof.log"),
		filepath.Join(f.p.RunLogDir(runID), "proof.log"),
	}
	if legacy {
		files = append(files, filepath.Join(os.TempDir(), legacyEvidenceDirName, runID, "proof.log"))
	}
	stale := time.Now().Add(-60 * 24 * time.Hour)
	for _, file := range files {
		if err := os.MkdirAll(filepath.Dir(file), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, []byte("diagnostic fixture bytes"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(file, stale, stale); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(filepath.Dir(file), stale, stale); err != nil {
			t.Fatal(err)
		}
	}
	return files
}

func assertCleanupDiagnostics(t *testing.T, files []string, retained bool) {
	t.Helper()
	for _, file := range files {
		got, err := os.ReadFile(file)
		if retained {
			if err != nil || string(got) != "diagnostic fixture bytes" {
				t.Fatalf("active diagnostics changed: %q, %v", got, err)
			}
		} else if !os.IsNotExist(err) {
			t.Fatalf("expired diagnostics survived before blocking capture: %v", err)
		}
	}
}

func TestStartupCleanupCancellationReachesCaptureAndConfirmation(t *testing.T) {
	for _, phase := range []string{"default", "recorded", "confirmation"} {
		t.Run(phase, func(t *testing.T) {
			f := newStartupCleanupFixture(t)
			wt := f.checkout(t, phase == "recorded")
			leftover := []db.RunWorktree{{RepoID: wt.repoID, RunID: wt.runID, Dir: wt.dir}}
			if phase != "recorded" {
				leftover = nil
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			var confirmErr error
			go func() {
				defer close(done)
				if phase == "confirmation" {
					confirmErr = confirmDirtyWorkBeforeRemoval(ctx, wt.dir, nil)
					return
				}
				cleanupStartupStorage(ctx, f.d, f.p, nil, worktreeReapPolicy{Retention: time.Nanosecond}, time.Now().Add(time.Hour), leftover)
			}()
			t.Cleanup(func() {
				cancel()
				f.release(t, wt)
				select {
				case <-done:
				case <-time.After(10 * time.Second):
					t.Error("cancelled fixture cleanup did not stop")
				}
			})
			waitForStartupCleanupFilter(t, f.entered(wt), done)
			cancel()
			select {
			case <-done:
			case <-time.After(8 * time.Second):
				t.Fatal("cancellation did not interrupt the running Git filter")
			}
			if phase == "confirmation" && confirmErr == nil {
				t.Fatal("cancelled confirmation authorized removal")
			}
			assertStartupCheckoutRetained(t, wt)
		})
	}
}

func TestStartupCleanupSharesOneBudgetAcrossCandidatesAndSweeps(t *testing.T) {
	if startupCleanupTimeout >= 45*time.Second {
		t.Fatal("worktree cleanup budget reaches the earliest readiness timeout")
	}
	f := newStartupCleanupFixture(t)
	first := f.checkout(t, false)
	second := f.checkout(t, false)
	recorded := f.checkout(t, true)
	storageFiles := seedCleanupDiagnostics(t, f, first.runID, true)
	active, err := f.d.InsertRun(first.repoID, "feature/active", f.head, f.head)
	if err != nil {
		t.Fatal(err)
	}
	activeFiles := seedCleanupDiagnostics(t, f, active.ID, true)
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(first.dir, old, old); err != nil {
		t.Fatal(err)
	}
	leftover := []db.RunWorktree{{RepoID: recorded.repoID, RunID: recorded.runID, Dir: recorded.dir}}
	done := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	started := time.Now()
	go func() {
		defer close(done)
		cleanupStartupStorage(ctx, f.d, f.p, nil, worktreeReapPolicy{Retention: time.Nanosecond}, time.Now().Add(time.Hour), leftover)
	}()
	t.Cleanup(func() {
		cancel()
		for _, wt := range []orphanWorktree{first, second, recorded} {
			f.release(t, wt)
		}
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("fixture cleanup did not stop")
		}
	})
	waitForStartupCleanupFilter(t, f.entered(first), done)
	assertCleanupDiagnostics(t, storageFiles, false)
	assertCleanupDiagnostics(t, activeFiles, true)
	if delay := time.Until(started.Add(startupCleanupTimeout / 2)); delay > 0 {
		time.Sleep(delay)
	}
	f.release(t, first)
	waitForStartupCleanupFilter(t, f.entered(second), done)
	remaining := time.Until(started.Add(startupCleanupTimeout + 5*time.Second))
	select {
	case <-done:
	case <-time.After(max(remaining, time.Millisecond)):
		t.Fatal("serial candidates exceeded the shared startup cleanup budget")
	}
	if _, err := os.Stat(f.entered(recorded)); !os.IsNotExist(err) {
		t.Fatalf("recorded sweep restarted the exhausted budget: %v", err)
	}
	for _, wt := range []orphanWorktree{first, second, recorded} {
		assertStartupCheckoutRetained(t, wt)
	}
	assertCleanupDiagnostics(t, storageFiles, false)
	assertCleanupDiagnostics(t, activeFiles, true)
}

package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestFinishedCleanupReleasesSlotBeforeSlowMaintenance(t *testing.T) {
	for _, recovered := range []bool{false, true} {
		for _, history := range []bool{false, true} {
			name := "fresh"
			if recovered {
				name = "recovered"
			}
			if history {
				name += "/history"
			} else {
				name += "/own"
			}
			t.Run(name, func(t *testing.T) {
				f := newStartupCleanupFixture(t)
				mockClaude := writeMockClaude(t, t.TempDir())
				cfg := "agent: claude\nagent_path_override:\n  claude: " + mockClaude + "\nmax_concurrent_runs: 1\nworktree:\n  retention: 1ns\n"
				if err := os.WriteFile(f.p.ConfigFile(), []byte(cfg), 0o644); err != nil {
					t.Fatal(err)
				}
				repo, err := f.d.GetRepo("startup-cleanup")
				if err != nil {
					t.Fatal(err)
				}
				started := make(chan string, 4)
				m := NewRunManager(f.d, f.p, func() []pipeline.Step {
					return []pipeline.Step{&mockApprovalStep{name: types.StepDocument}, &notifyBlockStep{name: types.StepLint, started: started}}
				})
				t.Cleanup(m.Shutdown)
				submit := func(branch string) string {
					t.Helper()
					id, err := m.startRun(context.Background(), repo, branch, f.head, f.head, "push", nil, "", "", false, "", nil)
					if err != nil {
						t.Fatal(err)
					}
					return id
				}
				approve := func(id string) {
					t.Helper()
					deadline := time.Now().Add(5 * time.Second)
					for {
						err := m.HandleRespond(id, types.StepDocument, types.ActionApprove, nil)
						if err == nil {
							return
						}
						if time.Now().After(deadline) {
							t.Fatalf("run did not acquire its slot and accept approval: %v", err)
						}
						time.Sleep(10 * time.Millisecond)
					}
				}
				var firstID string
				if recovered {
					first := insertParkedQueueTestRun(t, f.p, f.d, repo, "feature/first", f.head)
					firstID = first.ID
					plans := m.recoverableParkedRuns(context.Background())
					if len(plans) != 1 {
						t.Fatalf("recovered plans = %d, want 1", len(plans))
					}
					m.resumeRecoveredRuns(plans)
				} else {
					firstID = submit("feature/first")
				}
				approve(firstID)
				waitForStartedBranch(t, started, "feature/first")
				slow := orphanWorktree{gateDir: f.gate, dir: f.p.WorktreeDir(repo.ID, firstID), repoID: repo.ID, runID: firstID}
				if history {
					slow = f.checkout(t, false)
					// Refuse immediate capture at the index-intent check, so the
					// historical filter is reached within the shared budget even
					// on Windows. Verifying the clean first checkout can otherwise
					// consume that budget before this test's slow phase starts.
					own := f.p.WorktreeDir(repo.ID, firstID)
					if err := os.WriteFile(filepath.Join(own, "pending.txt"), []byte("retain index intent\n"), 0o644); err != nil {
						t.Fatal(err)
					}
					gitCmd(t, own, "add", "--intent-to-add", "pending.txt")
				} else if err := os.WriteFile(filepath.Join(slow.dir, ".gitattributes"), []byte("test.txt filter=slow\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { f.release(t, slow) })
				secondID := submit("feature/second")
				if got := runStatus(t, f.d, secondID); got != types.RunPending {
					t.Fatalf("second run status = %s, want pending", got)
				}
				m.mu.Lock()
				firstDone := m.dones[firstID]
				m.mu.Unlock()
				if err := m.HandleCancel(firstID); err != nil {
					t.Fatal(err)
				}
				waitForStartupCleanupFilter(t, f.entered(slow), firstDone)
				approve(secondID)
				waitForStartedBranch(t, started, "feature/second")
				select {
				case <-firstDone:
					t.Fatal("slow cleanup ended before queued execution started")
				default:
				}
				assertStartupCheckoutRetained(t, slow)
				f.release(t, slow)
			})
		}
	}
}

func TestFinishedCleanupSharesOneBudgetAcrossOwnAndRetainedCheckouts(t *testing.T) {
	f := newStartupCleanupFixture(t)
	first := f.checkout(t, false)
	second := f.checkout(t, false)
	third := f.checkout(t, false)
	storageFiles := seedCleanupDiagnostics(t, f, first.runID, false)
	active, err := f.d.InsertRun(first.repoID, "feature/active", f.head, f.head)
	if err != nil {
		t.Fatal(err)
	}
	activeFiles := seedCleanupDiagnostics(t, f, active.ID, false)
	old := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(second.dir, old, old); err != nil {
		t.Fatal(err)
	}
	m := NewRunManager(f.d, f.p, nil)
	cfg := config.Merge(config.DefaultGlobalConfig(), &config.RepoConfig{})
	cfg.Worktree.Retention = time.Nanosecond
	done := make(chan struct{})
	started := time.Now()
	go func() {
		defer close(done)
		m.cleanupFinishedRun(cfg, first.repoID, first.runID, first.gateDir, first.dir, "test")
	}()
	t.Cleanup(func() {
		for _, wt := range []orphanWorktree{first, second, third} {
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
	if delay := time.Until(started.Add(finishedRunCleanupTimeout / 2)); delay > 0 {
		time.Sleep(delay)
	}
	f.release(t, first)
	waitForStartupCleanupFilter(t, f.entered(second), done)
	remaining := time.Until(started.Add(finishedRunCleanupTimeout + 5*time.Second))
	select {
	case <-done:
	case <-time.After(max(remaining, time.Millisecond)):
		t.Fatal("retained candidates exceeded the shared finished-run cleanup budget")
	}
	if _, err := os.Stat(f.entered(third)); !os.IsNotExist(err) {
		t.Fatalf("later candidate restarted the exhausted sweep budget: %v", err)
	}
	for _, wt := range []orphanWorktree{first, second, third} {
		assertStartupCheckoutRetained(t, wt)
	}
	assertCleanupDiagnostics(t, storageFiles, false)
	assertCleanupDiagnostics(t, activeFiles, true)
}

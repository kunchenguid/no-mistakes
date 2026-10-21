package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// A daemon lost during a review fix can leave both a committed fix and edits
// made after it. Failed recovery must preserve both, through every later reap.
func TestReviewEditsSurviveFailedRecoveryAndCleanup(t *testing.T) {
	for _, placement := range []string{"default", "recorded"} {
		t.Run(placement, func(t *testing.T) {
			p, d := newParkedRunEnv(t)
			repo, head := setupTestGitRepo(t, p, d, "dirty-review")
			run, err := d.InsertRun(repo.ID, "main", head, head)
			if err != nil {
				t.Fatal(err)
			}
			work := p.WorktreeDir(repo.ID, run.ID)
			if placement == "recorded" {
				work = filepath.Join(t.TempDir(), run.ID)
			}
			if err := d.SetRunWorktreeDir(run.ID, work); err != nil {
				t.Fatal(err)
			}
			gate := p.RepoDir(repo.ID)
			if err := git.WorktreeAdd(context.Background(), gate, work, head); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, work, "config", "user.name", "Test")
			gitCmd(t, work, "config", "user.email", "test@test.com")
			write := func(name, body string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(work, name), []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			write("fix.txt", "committed review fix\n")
			gitCmd(t, work, "add", "fix.txt")
			gitCmd(t, work, "commit", "-m", "review fix")
			fixed := gitOutput(t, work, "rev-parse", "HEAD")
			write("test.txt", "staged review edit\n")
			gitCmd(t, work, "add", "test.txt")
			write("fix.txt", "unfinished tracked review edit\n")
			write("new.txt", "untracked review file\n")
			if err := d.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
				t.Fatal(err)
			}
			sr, err := d.InsertStepResult(run.ID, types.StepReview)
			if err != nil {
				t.Fatal(err)
			}
			if err := d.StartStep(sr.ID); err != nil {
				t.Fatal(err)
			}
			if err := d.UpdateStepStatusWithDuration(sr.ID, types.StepStatusFixing, 1); err != nil {
				t.Fatal(err)
			}
			// An interrupted fixer is not an approval park that can be resumed.
			// Startup must terminalize it without replaying the fixer's side effects.
			mgr := NewRunManager(d, p, nil)
			run, err = d.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := mgr.prepareRecoveredRun(context.Background(), run); err == nil || !strings.Contains(err.Error(), "not a parked running run") {
				t.Fatalf("expected interrupted-fix recovery refusal, got %v", err)
			}
			layout, err := validatedWorktreeLayout(d, p, config.DefaultGlobalConfig())
			if err != nil {
				t.Fatal(err)
			}
			before := gitOutput(t, work, "status", "--porcelain=v1", "--untracked-files=all")
			assertPreserved := func() {
				t.Helper()
				for name, want := range map[string]string{"fix.txt": "unfinished tracked review edit\n", "test.txt": "staged review edit\n", "new.txt": "untracked review file\n"} {
					got, err := os.ReadFile(filepath.Join(work, name))
					if err != nil || string(got) != want {
						t.Fatalf("lost %s: %q, %v", name, got, err)
					}
				}
				if got := gitOutput(t, work, "status", "--porcelain=v1", "--untracked-files=all"); got != before {
					t.Fatalf("index/worktree changed: %q, want %q", got, before)
				}
				if got := gitOutput(t, gate, "rev-parse", custody.RecoveryRef(run.ID)); got != fixed {
					t.Fatalf("recovery anchor = %s, want %s", got, fixed)
				}
				if got := gitOutput(t, gate, "show", custody.RecoveryRef(run.ID)+":fix.txt"); got != "committed review fix" {
					t.Fatalf("lost committed fix: %q", got)
				}
			}
			recoverOnStartup(d, p, mgr, layout)
			terminal, err := d.GetRun(run.ID)
			if err != nil || terminal.Status != types.RunFailed || len(mgr.executors) != 0 {
				t.Fatalf("recovery did not fail closed: %+v, %v", terminal, err)
			}
			assertPreserved()
			mgr.removeRunWorktree(repo.ID, run.ID, gate, work, "resumed_run_finished")
			assertPreserved()
			reapWorktrees(d, p, worktreeReapPolicy{Retention: time.Nanosecond, MaxRuns: 1}, time.Now().Add(365*24*time.Hour))
			cleanupOrphanWorktrees(d, p, leftoverRecordedRunWorktrees(d, p))
			assertPreserved()
			// Even a stale removal candidate must re-check, before the force or fallback.
			if removeOrphanWorktree(context.Background(), d, orphanWorktree{gateDir: gate, dir: work, repoID: repo.ID, runID: run.ID}) {
				t.Fatal("dirty removal candidate reported success")
			}
			assertPreserved()
		})
	}
}

func TestWorktreeCleanupRefusesUnknownDirtiness(t *testing.T) {
	for _, metadata := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing-metadata", true: "broken-metadata"}[metadata], func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "edit.txt"), []byte("recover me"), 0o644); err != nil {
				t.Fatal(err)
			}
			if metadata {
				if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: missing"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if removeOrphanWorktree(context.Background(), nil, orphanWorktree{dir: dir, gateDir: t.TempDir()}) {
				t.Fatal("removed directory whose dirtiness could not be determined")
			}
			if got, err := os.ReadFile(filepath.Join(dir, "edit.txt")); err != nil || string(got) != "recover me" {
				t.Fatalf("lost edits: %q, %v", got, err)
			}
		})
	}
}

func TestWorktreeCleanupProtectsEachEditKind(t *testing.T) {
	for _, kind := range []string{"clean", "unstaged", "staged", "untracked"} {
		t.Run(kind, func(t *testing.T) {
			p, d := newParkedRunEnv(t)
			repo, head := setupTestGitRepo(t, p, d, "edit-kind")
			run, err := d.InsertRun(repo.ID, "main", head, head)
			if err != nil {
				t.Fatal(err)
			}
			if err := d.UpdateRunStatus(run.ID, types.RunFailed); err != nil {
				t.Fatal(err)
			}
			work := p.WorktreeDir(repo.ID, run.ID)
			gate := p.RepoDir(repo.ID)
			if err := git.WorktreeAdd(context.Background(), gate, work, head); err != nil {
				t.Fatal(err)
			}
			if kind != "clean" {
				name := "test.txt"
				if kind == "untracked" {
					name = "new.txt"
				}
				if err := os.WriteFile(filepath.Join(work, name), []byte("review edit"), 0o644); err != nil {
					t.Fatal(err)
				}
				if kind == "staged" {
					gitCmd(t, work, "add", name)
				}
			}
			mgr := NewRunManager(d, p, nil)
			mgr.removeRunWorktree(repo.ID, run.ID, gate, work, "test")
			_, err = os.Stat(work)
			if kind == "clean" {
				if !os.IsNotExist(err) {
					t.Fatalf("clean worktree was retained: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("%s edit lost by immediate cleanup: %v", kind, err)
			}
			if removeOrphanWorktree(context.Background(), d, orphanWorktree{gateDir: gate, dir: work, runID: run.ID, repoID: repo.ID}) {
				t.Fatalf("%s edit lost by orphan cleanup", kind)
			}
		})
	}
}

func TestWorktreeCleanupDiscardsEditsOnlyAfterOperatorAbort(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    types.RunStatus
		runError  string
		stepError string
		discard   bool
	}{
		{"cancel-abort", types.RunCancelled, types.RunCancelReasonAbortedByUser, "", true},
		{"gate-abort", types.RunFailed, "step failed", types.UserAbortStepError(""), true},
		{"gate-abort-with-reason", types.RunFailed, "step failed", types.UserAbortStepError("discard these edits"), true},
		{"superseded", types.RunCancelled, types.RunCancelReasonSuperseded, "", false},
		{"ordinary-cancel", types.RunCancelled, "cancelled", "", false},
		{"crash", types.RunFailed, "daemon crashed", "interrupted fix", false},
		{"failed-recovery", types.RunFailed, "recovery failed", "cannot recover", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, d := newParkedRunEnv(t)
			repo, head := setupTestGitRepo(t, p, d, "abort-edits")
			run, err := d.InsertRun(repo.ID, "main", head, head)
			if err != nil {
				t.Fatal(err)
			}
			if err := d.UpdateRunErrorStatus(run.ID, tc.runError, tc.status); err != nil {
				t.Fatal(err)
			}
			if tc.stepError != "" {
				sr, err := d.InsertStepResult(run.ID, types.StepReview)
				if err != nil {
					t.Fatal(err)
				}
				if err := d.FailStep(sr.ID, tc.stepError, 1); err != nil {
					t.Fatal(err)
				}
			}
			work := p.WorktreeDir(repo.ID, run.ID)
			gate := p.RepoDir(repo.ID)
			if err := git.WorktreeAdd(context.Background(), gate, work, head); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(work, "new.txt"), []byte("unfinished edit"), 0o644); err != nil {
				t.Fatal(err)
			}
			wt := orphanWorktree{gateDir: gate, dir: work, repoID: repo.ID, runID: run.ID}
			if got := removableOrphanWorktree(d, wt); got != tc.discard {
				t.Fatalf("eligible for cleanup = %v, want %v", got, tc.discard)
			}
			if got := removeOrphanWorktree(context.Background(), d, wt); got != tc.discard {
				t.Fatalf("removed = %v, want %v", got, tc.discard)
			}
			if tc.discard {
				if _, err := os.Stat(work); !os.IsNotExist(err) {
					t.Fatalf("operator-aborted worktree remains: %v", err)
				}
			} else if got, err := os.ReadFile(filepath.Join(work, "new.txt")); err != nil || string(got) != "unfinished edit" {
				t.Fatalf("lost unfinished edit: %q, %v", got, err)
			}
		})
	}
}

func TestWorktreeCleanupUnreadableDiscardDecisionPreservesEdits(t *testing.T) {
	p, d := newParkedRunEnv(t)
	repo, head := setupTestGitRepo(t, p, d, "unreadable-decision")
	run, err := d.InsertRun(repo.ID, "main", head, head)
	if err != nil {
		t.Fatal(err)
	}
	run.Status = types.RunFailed
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "edit.txt"), []byte("recover me"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	for _, database := range []*db.DB{nil, d} {
		if reason := runWorktreeEditsCleanupReason(context.Background(), database, run, dir); reason == "" {
			t.Fatal("unreadable discard decision allowed cleanup")
		}
	}
	if removeOrphanWorktree(context.Background(), d, orphanWorktree{dir: dir, runID: run.ID}) {
		t.Fatal("removed worktree after run read failed")
	}
	if reason := runWorktreeEditsCleanupReason(context.Background(), nil, nil, dir); reason == "" {
		t.Fatal("missing run allowed cleanup of uninspectable edits")
	}
	if got, err := os.ReadFile(filepath.Join(dir, "edit.txt")); err != nil || string(got) != "recover me" {
		t.Fatalf("lost edit after read failure: %q, %v", got, err)
	}
}

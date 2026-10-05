package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestFixProgressRescueCleanupPreservesInterruptedBytes(t *testing.T) {
	setRescueFixturePopulation(t)
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	repo, head := setupTestGitRepo(t, p, d, "repo1")
	run, err := d.InsertRun(repo.ID, "feature", head, head)
	if err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(p.WorktreesDir(), repo.ID, run.ID)
	gitCmd(t, p.RepoDir(repo.ID), "worktree", "add", "--detach", wt, head)
	if err := d.UpdateRunStatus(run.ID, types.RunFailed); err != nil {
		t.Fatal(err)
	}
	partial := []byte{0, 255, 'C'}
	if err := os.WriteFile(filepath.Join(wt, "unfinished-test.bin"), partial, 0o644); err != nil {
		t.Fatal(err)
	}
	NewRunManager(d, p, nil).removeRunWorktree(repo.ID, run.ID, p.RepoDir(repo.ID), wt, "test")
	preserved, err := d.LatestWorkRescue(run.ID)
	if err != nil || preserved == nil || preserved.State != "saved" {
		t.Fatalf("cleanup without durable rescue: %+v %v", preserved, err)
	}
	got, err := git.RunRaw(context.Background(), p.RepoDir(repo.ID), "cat-file", "blob", preserved.Ref+":unfinished-test.bin")
	if err != nil || string(got) != string(partial) {
		t.Fatalf("interrupted repair was discarded: bytes=%v error=%v", got, err)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("saved worktree not removed: %v", err)
	}
}

func TestRescueCleanupRetainsIgnoredGitlinkAcrossDeletionRoutes(t *testing.T) {
	setRescueFixturePopulation(t)
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	repo, head := setupTestGitRepo(t, p, d, "repo1")
	run, err := d.InsertRun(repo.ID, "feature", head, head)
	if err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(p.WorktreesDir(), repo.ID, run.ID)
	gitCmd(t, p.RepoDir(repo.ID), "worktree", "add", "--detach", wt, head)
	sub := t.TempDir()
	gitCmd(t, sub, "init")
	gitCmd(t, sub, "config", "user.name", "test")
	gitCmd(t, sub, "config", "user.email", "test@test.com")
	if err := os.WriteFile(filepath.Join(sub, ".gitignore"), []byte("private\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, sub, "add", ".gitignore")
	gitCmd(t, sub, "commit", "-m", "ignore nested bytes")
	gitCmd(t, wt, "-c", "protocol.file.allow=always", "submodule", "add", sub, "module")
	gitCmd(t, wt, "-c", "user.name=test", "-c", "user.email=test@test.com", "commit", "-m", "initialized submodule")
	file := filepath.Join(wt, "module", "private")
	if err := os.WriteFile(file, []byte("nested unfinished bytes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if status, err := git.Run(context.Background(), wt, "status", "--porcelain", "--ignore-submodules=none"); err != nil || status != "" {
		t.Fatalf("precondition status=%q error=%v", status, err)
	}
	if err := d.UpdateRunStatus(run.ID, types.RunFailed); err != nil {
		t.Fatal(err)
	}
	NewRunManager(d, p, nil).removeRunWorktree(repo.ID, run.ID, p.RepoDir(repo.ID), wt, "test")
	cleanupOrphanWorktrees(d, p, []db.RunWorktree{{RepoID: repo.ID, RunID: run.ID, Dir: wt}})
	reapWorktrees(d, p, worktreeReapPolicy{Retention: time.Nanosecond}, time.Now().Add(time.Hour))
	if got, err := os.ReadFile(file); err != nil || string(got) != "nested unfinished bytes\n" {
		t.Fatalf("cleanup lost nested bytes: %q %v", got, err)
	}
	rescue, err := d.LatestWorkRescue(run.ID)
	if err != nil || rescue == nil || rescue.State != "retained" {
		t.Fatalf("cleanup retention unavailable: %+v %v", rescue, err)
	}
}

func TestFixProgressFailureCleanupRetainsUnsupportedAndBrokenEvidence(t *testing.T) {
	for _, state := range []string{"ignored", "merge", "moved-ref", "symbolic-ref", "db-failure"} {
		t.Run(state, func(t *testing.T) {
			p := paths.WithRoot(t.TempDir())
			if err := p.EnsureDirs(); err != nil {
				t.Fatal(err)
			}
			d, err := db.Open(p.DB())
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			repo, head := setupTestGitRepo(t, p, d, "repo1")
			run, err := d.InsertRun(repo.ID, "feature", head, head)
			if err != nil {
				t.Fatal(err)
			}
			wt := filepath.Join(p.WorktreesDir(), repo.ID, run.ID)
			gitCmd(t, p.RepoDir(repo.ID), "worktree", "add", "--detach", wt, head)
			if err = d.UpdateRunStatus(run.ID, types.RunFailed); err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(wt, "unfinished.txt")
			if err = os.WriteFile(file, []byte("partial C"), 0o644); err != nil {
				t.Fatal(err)
			}
			switch state {
			case "ignored":
				if err = os.WriteFile(filepath.Join(wt, ".gitignore"), []byte("unfinished.txt\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			case "merge":
				gitDir := gitOutput(t, wt, "rev-parse", "--absolute-git-dir")
				if err = os.WriteFile(filepath.Join(gitDir, "MERGE_HEAD"), []byte(head+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			case "moved-ref", "symbolic-ref":
				pending, err := d.BeginWorkRescue(run, "review", "", head, wt)
				if err != nil {
					t.Fatal(err)
				}
				snapshot, err := custody.PreservePartialWork(context.Background(), wt, run.ID, "review", pending.StopID)
				if err != nil {
					t.Fatal(err)
				}
				pending.State = snapshot.State
				pending.Ref = snapshot.Ref
				pending.SHA = snapshot.SHA
				pending.IndexSHA = snapshot.IndexSHA
				if err = d.SaveWorkRescue(pending); err != nil {
					t.Fatal(err)
				}
				if state == "moved-ref" {
					gitCmd(t, wt, "update-ref", pending.Ref, head)
				} else {
					gitCmd(t, wt, "symbolic-ref", pending.Ref, "refs/heads/main")
				}
			case "db-failure":
				if err = d.Close(); err != nil {
					t.Fatal(err)
				}
			}
			NewRunManager(d, p, nil).removeRunWorktree(repo.ID, run.ID, p.RepoDir(repo.ID), wt, "test")
			if got, err := os.ReadFile(file); err != nil || string(got) != "partial C" {
				t.Fatalf("unsafe cleanup: %q %v", got, err)
			}
			cleanupOrphanWorktrees(d, p, []db.RunWorktree{{RepoID: repo.ID, RunID: run.ID, Dir: wt}})
			reapWorktrees(d, p, worktreeReapPolicy{Retention: time.Nanosecond}, time.Now().Add(time.Hour))
			if got, err := os.ReadFile(file); err != nil || string(got) != "partial C" {
				t.Fatalf("startup/retention lost work: %q %v", got, err)
			}
		})
	}
}

func setRescueFixturePopulation(t *testing.T) {
	t.Helper()
	bin := t.TempDir()
	population := fmt.Sprintf("#!/bin/sh\ncase \"$1\" in\n-eo) printf '%d 1 %d S\\n';;\n*) printf '%d 1 %d 00:01 fixture\\n';;\nesac\n", os.Getpid(), os.Getpid(), os.Getpid(), os.Getpid())
	if err := os.WriteFile(filepath.Join(bin, "ps"), []byte(population), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

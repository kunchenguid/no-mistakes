//go:build unix

package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestRescueCleanupLookupFailureRetainsCleanCheckout(t *testing.T) {
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
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "ps"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	NewRunManager(d, p, nil).removeRunWorktree(repo.ID, run.ID, p.RepoDir(repo.ID), wt, "test")
	cleanupOrphanWorktrees(d, p, []db.RunWorktree{{RepoID: repo.ID, RunID: run.ID, Dir: wt}})
	reapWorktrees(d, p, worktreeReapPolicy{Retention: time.Nanosecond}, time.Now().Add(time.Hour))
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("unverified clean checkout removed: %v", err)
	}
	rescue, err := d.LatestWorkRescue(run.ID)
	if err != nil || rescue == nil || rescue.State != "retained" {
		t.Fatalf("unverified cleanup lacks retention: %+v %v", rescue, err)
	}
}

func TestRescueVerifiedCleanupRemovesCleanCheckout(t *testing.T) {
	for _, route := range []string{"run", "startup", "retention"} {
		t.Run(route, func(t *testing.T) {
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
			switch route {
			case "run":
				NewRunManager(d, p, nil).removeRunWorktree(repo.ID, run.ID, p.RepoDir(repo.ID), wt, "test")
			case "startup":
				cleanupOrphanWorktrees(d, p, []db.RunWorktree{{RepoID: repo.ID, RunID: run.ID, Dir: wt}})
			case "retention":
				reapWorktrees(d, p, worktreeReapPolicy{Retention: time.Nanosecond}, time.Now().Add(time.Hour))
			}
			if _, err := os.Stat(wt); !os.IsNotExist(err) {
				t.Fatalf("verified clean checkout retained: %v", err)
			}
			if rescue, err := d.LatestWorkRescue(run.ID); err != nil || rescue != nil {
				t.Fatalf("clean deletion created unfinished rescue: %+v %v", rescue, err)
			}
		})
	}
}

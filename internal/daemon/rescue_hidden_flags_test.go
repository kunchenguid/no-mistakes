//go:build unix

package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestRescueHiddenFlagsCleanupPreservesBytes(t *testing.T) {
	for _, flag := range []string{"--assume-unchanged", "--skip-worktree"} {
		for _, route := range []string{"run", "startup", "retention"} {
			t.Run(flag+"/"+route, func(t *testing.T) {
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
				wt := p.WorktreeDir(repo.ID, run.ID)
				gitCmd(t, p.RepoDir(repo.ID), "worktree", "add", "--detach", wt, head)
				if err := os.WriteFile(filepath.Join(wt, "generated"), []byte("base\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				gitCmd(t, wt, "add", "generated")
				gitCmd(t, wt, "commit", "-m", "generated")
				current, err := git.HeadSHA(context.Background(), wt)
				if err != nil {
					t.Fatal(err)
				}
				if err := d.UpdateRunHeadSHA(run.ID, current); err != nil {
					t.Fatal(err)
				}
				gitCmd(t, wt, "update-index", flag, "generated")
				if err := os.WriteFile(filepath.Join(wt, "generated"), []byte("hidden cleanup bytes\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := d.UpdateRunStatus(run.ID, types.RunFailed); err != nil {
					t.Fatal(err)
				}
				switch route {
				case "run":
					NewRunManager(d, p, nil).removeRunWorktree(repo.ID, run.ID, p.RepoDir(repo.ID), wt, "fixture")
				case "startup":
					cleanupOrphanWorktrees(d, p, []db.RunWorktree{{RepoID: repo.ID, RunID: run.ID, Dir: wt}})
				case "retention":
					reapWorktrees(d, p, worktreeReapPolicy{Retention: time.Nanosecond}, time.Now().Add(time.Hour))
				}
				saved, err := d.LatestWorkRescue(run.ID)
				if err != nil || saved == nil || saved.State != "saved" {
					t.Fatalf("cleanup lost hidden bytes: %+v %v", saved, err)
				}
				if got, err := git.RunRaw(context.Background(), p.RepoDir(repo.ID), "cat-file", "blob", saved.Ref+":generated"); err != nil || string(got) != "hidden cleanup bytes\n" {
					t.Fatalf("rescued bytes=%q %v", got, err)
				}
				if _, err := os.Stat(wt); !os.IsNotExist(err) {
					t.Fatalf("verified saved worktree not removed: %v", err)
				}
			})
		}
	}
}

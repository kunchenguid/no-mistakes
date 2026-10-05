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

func TestRescueRawSettlementCleanupPreservesNormalizedAndRewrittenWork(t *testing.T) {
	for _, state := range []string{"normalized", "rewritten"} {
		for _, route := range []string{"run", "startup", "retention"} {
			t.Run(state+"/"+route, func(t *testing.T) {
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
				if err := os.WriteFile(filepath.Join(wt, "text"), []byte("private text\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				gitCmd(t, wt, "add", "text")
				gitCmd(t, wt, "commit", "-m", "text fixture")
				head, err = git.HeadSHA(context.Background(), wt)
				if err != nil {
					t.Fatal(err)
				}
				run.HeadSHA = head
				if err := d.UpdateRunHeadSHA(run.ID, head); err != nil {
					t.Fatal(err)
				}
				if state == "normalized" {
					attributes := filepath.Join(t.TempDir(), "attributes")
					if err := os.WriteFile(attributes, []byte("* text eol=crlf\n"), 0o644); err != nil {
						t.Fatal(err)
					}
					gitCmd(t, wt, "config", "core.attributesfile", attributes)
					if err := os.WriteFile(filepath.Join(wt, "text"), []byte("private text\r\n"), 0o644); err != nil {
						t.Fatal(err)
					}
				} else {
					gitCmd(t, wt, "commit", "--amend", "--allow-empty", "-m", "rewritten fixture")
				}
				if state == "normalized" {
					gitCmd(t, wt, "add", "text")
				}
				if status, err := git.RunWithEnv(context.Background(), wt, []string{"GIT_OPTIONAL_LOCKS=1"}, "status", "--porcelain"); err != nil || status != "" {
					diff, _ := git.Run(context.Background(), wt, "diff", "--", "text")
					t.Fatalf("normalization fixture status=%q error=%v diff=%s", status, err, diff)
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
				if state == "normalized" {
					if err != nil || saved == nil || saved.State != "saved" {
						t.Fatalf("raw cleanup bytes lost: %+v %v", saved, err)
					}
					if got, err := git.RunRaw(context.Background(), p.RepoDir(repo.ID), "cat-file", "blob", saved.Ref+":text"); err != nil || string(got) != "private text\r\n" {
						t.Fatalf("saved raw bytes=%q %v", got, err)
					}
					if _, err := os.Stat(wt); !os.IsNotExist(err) {
						t.Fatalf("verified saved checkout not removed: %v", err)
					}
				} else {
					if _, err := os.Stat(wt); err != nil {
						t.Fatalf("unanchored rewritten worktree deleted: %v", err)
					}
					if err != nil || saved == nil || saved.State != "retained" {
						t.Fatalf("rewrite not retained: %+v %v", saved, err)
					}
				}
			})
		}
	}
}

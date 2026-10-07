package daemon

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func unfinishedCleanupFixture(t *testing.T, operation, placement string) (*paths.Paths, *db.DB, *db.Repo, *db.Run, string) {
	t.Helper()
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	repo, head := setupTestGitRepo(t, p, database, "unfinished-cleanup")
	run, err := database.InsertRun(repo.ID, "feature", head, head)
	if err != nil {
		t.Fatal(err)
	}
	dir := p.WorktreeDir(repo.ID, run.ID)
	if placement == "recorded" {
		dir = filepath.Join(t.TempDir(), run.ID)
	}
	if err := database.SetRunWorktreeDir(run.ID, dir); err != nil {
		t.Fatal(err)
	}
	if err := git.WorktreeAdd(context.Background(), p.RepoDir(repo.ID), dir, head); err != nil {
		t.Fatal(err)
	}
	if operation != "clean" {
		gitCmd(t, dir, "config", "user.name", "test")
		gitCmd(t, dir, "config", "user.email", "test@test.com")
		gitCmd(t, dir, "checkout", "-b", "fixture-base")
		if err := os.WriteFile(filepath.Join(dir, "test.txt"), []byte("base side\n"), 0600); err != nil {
			t.Fatal(err)
		}
		gitCmd(t, dir, "add", "test.txt")
		gitCmd(t, dir, "commit", "-m", "base side")
		gitCmd(t, dir, "checkout", "--detach", head)
		if err := os.WriteFile(filepath.Join(dir, "test.txt"), []byte("feature side\n"), 0600); err != nil {
			t.Fatal(err)
		}
		gitCmd(t, dir, "add", "test.txt")
		gitCmd(t, dir, "commit", "-m", "feature side")
		feature := gitOutput(t, dir, "rev-parse", "HEAD")
		if err := database.UpdateRunHeadSHA(run.ID, feature); err != nil {
			t.Fatal(err)
		}
		args := []string{"merge", "--no-edit", "fixture-base"}
		if operation == "rebase" {
			args = []string{"rebase", "fixture-base"}
		}
		if operation == "rebase-apply" {
			args = []string{"-c", "rebase.backend=apply", "rebase", "fixture-base"}
		}
		if _, err := git.Run(context.Background(), dir, args...); err == nil {
			t.Fatal("fixture did not conflict")
		}
		if operation == "unmerged" {
			path := gitOutput(t, dir, "rev-parse", "--git-path", "MERGE_HEAD")
			if !filepath.IsAbs(path) {
				path = filepath.Join(dir, path)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		}
		if operation == "rebase" || operation == "rebase-apply" {
			if gitOutput(t, dir, "rev-parse", "HEAD") == feature {
				t.Fatal("rebase partial HEAD is not divergent")
			}
		}
		if err := os.WriteFile(filepath.Join(dir, "untracked.txt"), []byte("untracked\x00bytes\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "staged.txt"), []byte("staged bytes\n"), 0600); err != nil {
			t.Fatal(err)
		}
		gitCmd(t, dir, "add", "staged.txt")
		if err := os.WriteFile(filepath.Join(dir, "staged.txt"), []byte("dirty bytes\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := database.UpdateRunStatus(run.ID, types.RunFailed); err != nil {
		t.Fatal(err)
	}
	run, err = database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	return p, database, repo, run, dir
}

func unfinishedWorktreeSnapshot(t *testing.T, dir string) map[string]string {
	t.Helper()
	result := map[string]string{"head": gitOutput(t, dir, "rev-parse", "HEAD"), "entries": gitOutput(t, dir, "ls-files", "--stage")}
	for _, name := range []string{"test.txt", "staged.txt", "untracked.txt"} {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		result[name] = string(raw)
	}
	for _, name := range []string{"index", "MERGE_HEAD", "rebase-merge", "rebase-apply"} {
		path := gitOutput(t, dir, "rev-parse", "--git-path", name)
		if !filepath.IsAbs(path) {
			path = filepath.Join(dir, path)
		}
		if _, err := os.Stat(path); os.IsNotExist(err) {
			continue
		}
		if err := filepath.WalkDir(path, func(p string, e os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if e.IsDir() {
				return nil
			}
			raw, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(path, p)
			if err != nil {
				return err
			}
			result[name+"/"+rel] = string(raw)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	return result
}

func TestUnfinishedWorktreeSurvivesCleanupBoundaries(t *testing.T) {
	for _, route := range []string{"immediate", "startup", "retention"} {
		for _, operation := range []string{"merge", "rebase", "rebase-apply", "unmerged", "clean"} {
			t.Run(route+"/"+operation, func(t *testing.T) {
				placement := "default"
				if route == "startup" {
					placement = "recorded"
				}
				p, database, repo, run, dir := unfinishedCleanupFixture(t, operation, placement)
				var before map[string]string
				if operation != "clean" {
					before = unfinishedWorktreeSnapshot(t, dir)
				}
				switch route {
				case "immediate":
					NewRunManager(database, p, nil).removeRunWorktree(repo.ID, run.ID, p.RepoDir(repo.ID), dir, "fixture")
				case "startup":
					cleanupOrphanWorktrees(database, p, []db.RunWorktree{{RepoID: repo.ID, RunID: run.ID, Dir: dir}})
				case "retention":
					reapWorktrees(database, p, worktreeReapPolicy{Retention: time.Nanosecond, MaxRuns: 1}, time.Now().Add(time.Hour))
				}
				_, err := os.Stat(dir)
				if operation == "clean" {
					if !os.IsNotExist(err) {
						t.Fatalf("ordinary cleanup retained clean worktree: %v", err)
					}
					return
				}
				if err != nil {
					t.Fatalf("unfinished %s worktree deleted: %v", operation, err)
				}
				if after := unfinishedWorktreeSnapshot(t, dir); !reflect.DeepEqual(before, after) {
					t.Fatal("cleanup changed partial HEAD/index/operation/dirty/untracked bytes")
				}
			})
		}
	}
}

func TestUnfinishedWorktreeUnreadableGitMetadataIsRetained(t *testing.T) {
	p, database, repo, run, dir := unfinishedCleanupFixture(t, "clean", "default")
	if err := os.WriteFile(filepath.Join(dir, ".git"), []byte("gitdir: missing-metadata\n"), 0600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(dir, "test.txt"))
	if err != nil {
		t.Fatal(err)
	}
	NewRunManager(database, p, nil).removeRunWorktree(repo.ID, run.ID, p.RepoDir(repo.ID), dir, "fixture")
	cleanupOrphanWorktrees(database, p, nil)
	reapWorktrees(database, p, worktreeReapPolicy{Retention: time.Nanosecond}, time.Now().Add(time.Hour))
	after, err := os.ReadFile(filepath.Join(dir, "test.txt"))
	if err != nil || string(after) != string(before) {
		t.Fatalf("unreadable worktree removed or changed: %v", err)
	}
}

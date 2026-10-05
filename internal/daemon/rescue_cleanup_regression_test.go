//go:build unix

package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestRescueCleanupOriginalSavedIdentityAndChangedBackstop(t *testing.T) {
	for _, state := range []string{"unchanged", "working", "staged", "ignored", "lookup-failure"} {
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
				write := func(name, value string) {
					t.Helper()
					if err := os.WriteFile(filepath.Join(wt, name), []byte(value), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				write("partial", "staged\n")
				gitCmd(t, wt, "add", "partial")
				write("partial", "original working\n")
				original, err := d.BeginWorkRescue(run, "test", "selection", head, wt)
				if err != nil {
					t.Fatal(err)
				}
				if err := pipeline.PreserveRunWork(context.Background(), d, run, wt, original, "interrupted agent", true); err != nil {
					t.Fatal(err)
				}
				if original.State != "saved" {
					t.Fatalf("fixture rescue=%+v", original)
				}
				switch state {
				case "working":
					write("partial", "later working\n")
				case "staged":
					write("partial", "later staged\n")
					gitCmd(t, wt, "add", "partial")
					write("partial", "original working\n")
				case "ignored":
					write(".gitignore", "private\n")
					write("private", "uncaptured bytes\n")
				case "lookup-failure":
					bin := t.TempDir()
					if err := os.WriteFile(filepath.Join(bin, "ps"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
						t.Fatal(err)
					}
					t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
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
				latest, err := d.LatestWorkRescue(run.ID)
				if err != nil || latest == nil {
					t.Fatalf("cleanup rescue=%+v %v", latest, err)
				}
				if state == "unchanged" && (latest.StopID != original.StopID || latest.Step != "test" || latest.SHA != original.SHA) {
					t.Errorf("unchanged cleanup replaced original invocation: original=%+v latest=%+v", original, latest)
				}
				if state == "working" || state == "staged" {
					if latest.StopID == original.StopID || latest.State != "saved" {
						t.Errorf("changed content lacks separate saved backstop: %+v", latest)
					}
					rev := latest.Ref + ":partial"
					want := "later working\n"
					if state == "staged" {
						rev = latest.IndexSHA + ":partial"
						want = "later staged\n"
					}
					got, e := git.RunRaw(context.Background(), p.RepoDir(repo.ID), "cat-file", "blob", rev)
					if e != nil || string(got) != want {
						t.Errorf("changed bytes=%q error=%v", got, e)
					}
				}
				if state == "ignored" || state == "lookup-failure" {
					if latest.State != "retained" {
						t.Errorf("uncertain cleanup=%+v", latest)
					}
					if _, e := os.Stat(wt); e != nil {
						t.Errorf("uncertain checkout removed: %v", e)
					}
				} else if _, e := os.Stat(wt); !os.IsNotExist(e) {
					t.Errorf("verified saved checkout not removed: %v", e)
				}
				got, e := git.RunRaw(context.Background(), p.RepoDir(repo.ID), "cat-file", "blob", original.Ref+":partial")
				if e != nil || string(got) != "original working\n" {
					t.Errorf("original immutable rescue changed: %q %v", got, e)
				}
			})
		}
	}
}

func TestRescueCleanupUnboundStorageRequiresVerifiedEmptyRemoval(t *testing.T) {
	for _, state := range []string{"empty", "nonempty", "unreadable", "lookup-failure", "racing"} {
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
				wt := p.WorktreeDir("unbound-repo", "unbound-run")
				if err := os.MkdirAll(wt, 0o755); err != nil {
					t.Fatal(err)
				}
				file := filepath.Join(wt, "private")
				if state == "nonempty" {
					if err := os.WriteFile(file, []byte("unbound bytes\n"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				if state == "unreadable" {
					if err := os.Chmod(wt, 0); err != nil {
						t.Fatal(err)
					}
					defer os.Chmod(wt, 0o755)
				}
				if state == "lookup-failure" || state == "racing" {
					bin := t.TempDir()
					body := "#!/bin/sh\nexit 1\n"
					if state == "racing" {
						body = fmt.Sprintf("#!/bin/sh\nprintf 'unbound bytes\\n' > '%s'\nprintf '%d 1 %d 00:01 fixture\\n'\n", file, os.Getpid(), os.Getpid())
					}
					if err := os.WriteFile(filepath.Join(bin, "ps"), []byte(body), 0o755); err != nil {
						t.Fatal(err)
					}
					t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
				}
				if err := os.Chtimes(wt, time.Now().Add(-time.Hour), time.Now().Add(-time.Hour)); err != nil {
					t.Fatal(err)
				}
				switch route {
				case "run":
					NewRunManager(d, p, nil).removeRunWorktree("unbound-repo", "unbound-run", p.RepoDir("unbound-repo"), wt, "fixture")
				case "startup":
					cleanupOrphanWorktrees(d, p, []db.RunWorktree{{RepoID: "unbound-repo", RunID: "unbound-run", Dir: wt}})
				case "retention":
					reapWorktrees(d, p, worktreeReapPolicy{Retention: time.Nanosecond}, time.Now().Add(time.Hour))
				}
				_, err = os.Stat(wt)
				if state == "empty" {
					if !os.IsNotExist(err) {
						t.Errorf("verified empty orphan retained: %v", err)
					}
				} else if err != nil {
					t.Errorf("unknown/nonempty storage removed: %v", err)
				}
				if state == "nonempty" || state == "racing" {
					if got, e := os.ReadFile(file); e != nil || string(got) != "unbound bytes\n" {
						t.Errorf("unbound bytes lost: %q %v", got, e)
					}
				}
			})
		}
	}
}

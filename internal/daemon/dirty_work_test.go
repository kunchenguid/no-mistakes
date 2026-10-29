package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

func TestRemoveRunWorktreeCapturesAndRetainsUnprovenCheckout(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	_, headSHA := setupTestGitRepo(t, p, d, "repo1")
	const runID = "01JZ8XQ7V6K9M3B0T5N2R4C8YD"
	wtDir := filepath.Join(t.TempDir(), "run worktree")
	gitCmd(t, p.RepoDir("repo1"), "worktree", "add", "--detach", wtDir, headSHA)
	if err := os.WriteFile(filepath.Join(wtDir, "test.txt"), []byte("unique dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	NewRunManager(d, p, nil).removeRunWorktree(context.Background(), "repo1", runID, p.RepoDir("repo1"), wtDir, "test")

	if got, err := os.ReadFile(filepath.Join(wtDir, "test.txt")); err != nil || string(got) != "unique dirty\n" {
		t.Fatalf("unproven checkout was not retained: %q, %v", got, err)
	}
	gate := p.RepoDir("repo1")
	commit := gitOutput(t, gate, "rev-parse", "--verify", git.DirtyWorkRef(runID)+"^{commit}")
	restored := filepath.Join(t.TempDir(), "restored")
	gitCmd(t, gate, "worktree", "add", "--detach", restored, headSHA)
	if err := git.RestoreDirtyWork(context.Background(), restored, commit); err != nil {
		t.Fatalf("restore captured snapshot: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(restored, "test.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "unique dirty\n" {
		t.Fatalf("restored test.txt = %q", got)
	}
}

func TestRemoveRunWorktreeKeepsDirtyWorkWhenCaptureFails(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	_, headSHA := setupTestGitRepo(t, p, d, "repo1")
	const runID = "01JZ8XQ7V6K9M3B0T5N2R4C8YD"
	wtDir := filepath.Join(t.TempDir(), "run-wt")
	gate := p.RepoDir("repo1")
	gitCmd(t, gate, "worktree", "add", "--detach", wtDir, headSHA)
	if err := os.WriteFile(filepath.Join(wtDir, "test.txt"), []byte("keep me\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gate, "refs", "no-mistakes"), []byte("blocked\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	NewRunManager(d, p, nil).removeRunWorktree(context.Background(), "repo1", runID, gate, wtDir, "test")

	got, err := os.ReadFile(filepath.Join(wtDir, "test.txt"))
	if err != nil {
		t.Fatalf("failed capture removed the checkout: %v", err)
	}
	if string(got) != "keep me\n" {
		t.Fatalf("checkout contents = %q", got)
	}
}

func TestRemoveRunWorktreeCapturesAndRetainsSymlinkCheckout(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	_, headSHA := setupTestGitRepo(t, p, d, "repo1")
	const runID = "01JZ8XQ7V6K9M3B0T5N2R4C8YE"
	wtDir := filepath.Join(t.TempDir(), "link worktree")
	gate := p.RepoDir("repo1")
	gitCmd(t, gate, "worktree", "add", "--detach", wtDir, headSHA)
	if err := os.MkdirAll(filepath.Join(wtDir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("sub", filepath.Join(wtDir, "dirlink")); err != nil {
		t.Skipf("symlink: %v", err)
	}
	if err := os.Symlink("missing-target", filepath.Join(wtDir, "dangling")); err != nil {
		t.Fatal(err)
	}

	NewRunManager(d, p, nil).removeRunWorktree(context.Background(), "repo1", runID, gate, wtDir, "test")

	if _, err := os.Stat(wtDir); err != nil {
		t.Fatalf("unproven symlink checkout was removed: %v", err)
	}
	commit := gitOutput(t, gate, "rev-parse", "--verify", git.DirtyWorkRef(runID)+"^{commit}")
	restored := filepath.Join(t.TempDir(), "restored-links")
	gitCmd(t, gate, "worktree", "add", "--detach", restored, headSHA)
	if err := git.RestoreDirtyWork(context.Background(), restored, commit); err != nil {
		t.Fatalf("restore: %v", err)
	}
	for _, tc := range []struct{ name, want string }{
		{"dirlink", "sub"},
		{"dangling", "missing-target"},
	} {
		got, err := os.Readlink(filepath.Join(restored, tc.name))
		if err != nil || filepath.ToSlash(got) != filepath.ToSlash(tc.want) {
			t.Fatalf("restored %s = %q, %v; want %q", tc.name, got, err, tc.want)
		}
	}
}

func TestConfirmDirtyWorkBeforeRemovalRetainsUnprovenCheckouts(t *testing.T) {
	for _, state := range []string{"clean", "staged", "unstaged"} {
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
			_, head := setupTestGitRepo(t, p, d, "repo1")
			gate := p.RepoDir("repo1")
			wt := filepath.Join(t.TempDir(), "unproven checkout")
			gitCmd(t, gate, "worktree", "add", "--detach", wt, head)
			if state != "clean" {
				if err := os.WriteFile(filepath.Join(wt, "test.txt"), []byte("unique retained bytes\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				if state == "staged" {
					gitCmd(t, wt, "add", "test.txt")
				}
			}
			const runID = "01JZ8XQ7V6K9M3B0T5N2R4C8YD"
			snap, err := captureDirtyWorkBeforeRemoval(context.Background(), gate, wt, runID, head)
			if err != nil || (snap == nil) != (state == "clean") {
				t.Fatalf("capture = %#v, %v", snap, err)
			}
			if err := git.DirtyWorkStillMatches(context.Background(), wt, snap); err != nil {
				t.Fatalf("fixture does not match its capture: %v", err)
			}
			if err := confirmDirtyWorkBeforeRemoval(context.Background(), wt, snap); err == nil || !strings.Contains(err.Error(), "stability through removal cannot be proven") {
				t.Fatalf("unproven cleanup did not record its retention reason: %v", err)
			}
			removed := removeOrphanWorktrees(context.Background(), p.WorktreesDir(), []orphanWorktree{{gateDir: gate, dir: wt, repoID: "repo1", runID: runID, baseSHA: head}}, "test")
			if removed != 0 {
				t.Fatal("orphan cleanup deleted an unproven checkout")
			}
			NewRunManager(d, p, nil).removeRunWorktree(context.Background(), "repo1", runID, gate, wt, "unproven stability")
			if _, err := os.Stat(wt); err != nil {
				t.Fatalf("run cleanup deleted an unproven checkout: %v", err)
			}
			if got := gitOutput(t, wt, "rev-parse", "HEAD"); got != head {
				t.Fatalf("retention changed HEAD: %s", got)
			}
			if state != "clean" {
				if got, err := os.ReadFile(filepath.Join(wt, "test.txt")); err != nil || string(got) != "unique retained bytes\n" {
					t.Fatalf("retention changed working bytes: %q, %v", got, err)
				}
				if state == "staged" && gitOutput(t, wt, "show", ":test.txt") != "unique retained bytes" {
					t.Fatal("retention changed index intent")
				}
			} else if _, err := git.Run(context.Background(), gate, "rev-parse", "--verify", git.DirtyWorkRef(runID)); err == nil {
				t.Fatal("clean retention created a snapshot")
			}
		})
	}
}

func TestRemoveOrphanWorktreeKeepsDirtyWorkWhenCaptureFails(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	_, headSHA := setupTestGitRepo(t, p, d, "repo1")
	const runID = "01JZ8XQ7V6K9M3B0T5N2R4C8YD"
	wtDir := filepath.Join(t.TempDir(), "orphan-wt")
	gate := p.RepoDir("repo1")
	gitCmd(t, gate, "worktree", "add", "--detach", wtDir, headSHA)
	if err := os.WriteFile(filepath.Join(wtDir, "test.txt"), []byte("orphan unique\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gate, "refs", "no-mistakes"), []byte("blocked\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	removed := removeOrphanWorktrees(context.Background(), p.WorktreesDir(), []orphanWorktree{{
		gateDir: gate,
		dir:     wtDir,
		repoID:  "repo1",
		runID:   runID,
	}}, "test")
	if removed != 0 {
		t.Fatal("orphan removal deleted a checkout whose capture failed")
	}
	got, err := os.ReadFile(filepath.Join(wtDir, "test.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "orphan unique\n" {
		t.Fatalf("orphan checkout = %q", got)
	}
}

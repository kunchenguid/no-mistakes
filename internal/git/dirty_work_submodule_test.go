package git

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestCaptureDirtyWork_CleanGitlinksNeedNoSnapshot(t *testing.T) {
	for _, initialized := range []bool{true, false} {
		name := "initialized"
		if !initialized {
			name = "uninitialized"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			gate, wt, sub := newDirtyWorkSubmodule(t)
			if !initialized {
				run(t, wt, "git", "submodule", "deinit", "--force", "--", "sub module")
			}
			subHead := run(t, wt, "git", "rev-parse", "HEAD:sub module")
			if _, err := Run(ctx, gate, "cat-file", "-e", subHead); err == nil {
				t.Fatal("fixture's gitlink commit unexpectedly exists in the parent object store")
			}
			snap, err := CaptureDirtyWork(ctx, DirtyWorkRequest{GateDir: gate, Worktree: wt, RunID: dirtyWorkRunID})
			if err != nil || snap != nil {
				t.Fatalf("clean gitlink capture = %#v, %v", snap, err)
			}
			if err := DirtyWorkStillMatches(ctx, wt, nil); err != nil {
				t.Fatalf("clean gitlink confirmation: %v", err)
			}
			if err := removeLockedDirtyCheckout(ctx, gate, wt, dirtyWorkRunID); err != nil {
				t.Fatalf("clean gitlink cleanup: %v", err)
			}
			if _, err := os.Stat(sub); !os.IsNotExist(err) {
				t.Fatalf("clean checkout was not removed: %v", err)
			}
			assertNoDirtySnapshot(t, gate)
		})
	}
}

func TestCaptureDirtyWork_CleanGitlinkAllowsOtherDirtyWork(t *testing.T) {
	ctx := context.Background()
	gate, wt, _ := newDirtyWorkSubmodule(t)
	head := run(t, wt, "git", "rev-parse", "HEAD")
	writeFile(t, filepath.Join(wt, "file.txt"), "preserve alongside clean module\n")
	if err := removeLockedDirtyCheckout(ctx, gate, wt, dirtyWorkRunID); err != nil {
		t.Fatalf("capture with clean gitlink: %v", err)
	}
	commit := run(t, gate, "git", "rev-parse", DirtyWorkRef(dirtyWorkRunID))
	restored := filepath.Join(t.TempDir(), "restored")
	if err := WorktreeAdd(ctx, gate, restored, head); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = WorktreeRemove(ctx, gate, restored) })
	if err := RestoreDirtyWork(ctx, restored, commit); err != nil {
		t.Fatal(err)
	}
	if got := string(mustRead(t, filepath.Join(restored, "file.txt"))); got != "preserve alongside clean module\n" {
		t.Fatalf("restored parent work: %q", got)
	}
	if got := run(t, restored, "git", "rev-parse", ":sub module"); got != run(t, restored, "git", "rev-parse", "HEAD:sub module") {
		t.Fatalf("gitlink changed: %s", got)
	}
}

func TestCaptureDirtyWork_RejectsDirtyGitlinks(t *testing.T) {
	for _, change := range []string{"tracked", "untracked", "staged", "head", "hidden"} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			gate, wt, sub := newDirtyWorkSubmodule(t)
			switch change {
			case "untracked":
				writeFile(t, filepath.Join(sub, "new file.txt"), "unique module work\n")
			case "head":
				run(t, sub, "git", "commit", "--allow-empty", "-m", "unique module commit")
			case "tracked", "staged", "hidden":
				if change == "hidden" {
					run(t, sub, "git", "update-index", "--assume-unchanged", "README.md")
				}
				writeFile(t, filepath.Join(sub, "README.md"), "unique module work\n")
				if change == "staged" {
					run(t, sub, "git", "add", "README.md")
				}
			}
			if err := DirtyWorkStillMatches(ctx, wt, nil); err == nil {
				t.Fatal("dirty submodule looked clean")
			}
			if err := removeLockedDirtyCheckout(ctx, gate, wt, dirtyWorkRunID); err == nil {
				t.Fatal("cleanup accepted unrepresentable submodule work")
			}
			if _, err := os.Stat(sub); err != nil {
				t.Fatalf("dirty submodule was removed: %v", err)
			}
			assertNoDirtySnapshot(t, gate)
		})
	}
}

func TestDirtyWorkStillMatchesRejectsLateSubmoduleChanges(t *testing.T) {
	for _, change := range []string{"hidden", "intent"} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			gate, wt, sub := newDirtyWorkSubmodule(t)
			if change == "hidden" {
				run(t, sub, "git", "update-index", "--assume-unchanged", "README.md")
			}
			writeFile(t, filepath.Join(wt, "file.txt"), "unique parent work\n")
			snap, err := CaptureDirtyWork(ctx, DirtyWorkRequest{GateDir: gate, Worktree: wt, RunID: dirtyWorkRunID})
			if err != nil || snap == nil {
				t.Fatalf("initial capture = %#v, %v", snap, err)
			}
			previous := dirtyWorkAfterContentCheck
			t.Cleanup(func() { dirtyWorkAfterContentCheck = previous })
			fired := false
			dirtyWorkAfterContentCheck = func(path string) {
				if path != wt || fired {
					return
				}
				fired = true
				if change == "hidden" {
					writeFile(t, filepath.Join(sub, "README.md"), "late hidden module work\n")
				} else {
					writeFile(t, filepath.Join(sub, "intent.late"), "")
					run(t, sub, "git", "add", "-N", "-f", "--", "intent.late")
				}
			}
			if err := DirtyWorkStillMatches(ctx, wt, snap); err == nil || !fired {
				t.Fatalf("late submodule %s accepted: %v, callback = %v", change, err, fired)
			}
		})
	}
}

func newDirtyWorkSubmodule(t *testing.T) (gate, wt, sub string) {
	t.Helper()
	gate, wt, _, _ = newDirtyWorkGate(t, "with-submodule")
	source := initTestRepo(t)
	writeFile(t, filepath.Join(source, "submodule.txt"), "distinct submodule commit\n")
	writeFile(t, filepath.Join(source, ".gitignore"), "*.late\n")
	run(t, source, "git", "add", "submodule.txt", ".gitignore")
	run(t, source, "git", "commit", "-m", "distinct submodule")
	run(t, wt, "git", "-c", "protocol.file.allow=always", "submodule", "add", source, "sub module")
	run(t, wt, "git", "commit", "-m", "submodule")
	sub = filepath.Join(wt, "sub module")
	run(t, sub, "git", "config", "user.name", "Test")
	run(t, sub, "git", "config", "user.email", "test@test.com")
	run(t, sub, "git", "config", "core.autocrlf", "false")
	return gate, wt, sub
}

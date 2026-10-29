package git

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCaptureDirtyWork_RejectsLossFromIgnoredAttributes(t *testing.T) {
	for _, directory := range []string{"root", "nested"} {
		for _, state := range []string{"clean", "dirty"} {
			t.Run(directory+"/"+state, func(t *testing.T) {
				ctx := context.Background()
				gate, wt, _, _ := newDirtyWorkGate(t, "ignored attributes")
				run(t, wt, "git", "config", "core.eol", "lf")
				writeFile(t, filepath.Join(wt, ".gitignore"), ".gitattributes\n")
				run(t, wt, "git", "add", ".gitignore")
				run(t, wt, "git", "commit", "-m", "ignore attribute sources")
				attributePath := filepath.Join(wt, ".gitattributes")
				attributes := "file.txt text eol=crlf\n"
				dataPath := filepath.Join(wt, "file.txt")
				body := "base\r\n"
				if directory == "nested" {
					attributePath = filepath.Join(wt, "sub dir", ".gitattributes")
					attributes = "\"a b.txt\" text eol=crlf\n"
					dataPath = filepath.Join(wt, "sub dir", "a b.txt")
					body = "old\r\n"
				}
				writeFile(t, attributePath, attributes)
				writeFile(t, dataPath, body)
				if state == "dirty" {
					writeFile(t, filepath.Join(wt, "unrelated.txt"), "unrelated unique work\n")
				} else if err := DirtyWorkStillMatches(ctx, wt, nil); !errors.Is(err, ErrDirtyWorkRejected) {
					t.Fatalf("clean confirmation accepted unrestorable tracked bytes: %v", err)
				}
				if err := removeLockedDirtyCheckout(ctx, gate, wt, dirtyWorkRunID); !errors.Is(err, ErrDirtyWorkRejected) {
					t.Fatalf("cleanup accepted unrestorable tracked bytes: %v", err)
				}
				assertNoDirtySnapshot(t, gate)
				if got := string(mustRead(t, dataPath)); got != body {
					t.Fatalf("rejected cleanup changed tracked bytes: %q", got)
				}
				if got := string(mustRead(t, attributePath)); got != attributes {
					t.Fatalf("rejected cleanup changed ignored attributes: %q", got)
				}
			})
		}
	}
}

func TestCaptureDirtyWork_IgnoredIdentityAttributesStayExcluded(t *testing.T) {
	ctx := context.Background()
	gate, wt, _, _ := newDirtyWorkGate(t, "ignored identity attributes")
	run(t, wt, "git", "config", "core.eol", "lf")
	writeFile(t, filepath.Join(wt, ".gitignore"), ".gitattributes\n")
	run(t, wt, "git", "add", ".gitignore")
	run(t, wt, "git", "commit", "-m", "ignore attribute sources")
	head := run(t, wt, "git", "rev-parse", "HEAD")
	marker := "ignored-attribute-source-must-not-be-collected"
	writeFile(t, filepath.Join(wt, ".gitattributes"), "# "+marker+"\nfile.txt -text\n")
	if snap, err := CaptureDirtyWork(ctx, DirtyWorkRequest{GateDir: gate, Worktree: wt, RunID: dirtyWorkRunID}); err != nil || snap != nil {
		t.Fatalf("clean identity attributes = %#v, %v", snap, err)
	}
	body := "unique tracked bytes\n"
	writeFile(t, filepath.Join(wt, "file.txt"), body)
	if err := removeLockedDirtyCheckout(ctx, gate, wt, dirtyWorkRunID); err != nil {
		t.Fatal(err)
	}
	commit := run(t, gate, "git", "rev-parse", DirtyWorkRef(dirtyWorkRunID))
	restored := filepath.Join(t.TempDir(), "restored without ignored attributes")
	if err := WorktreeAdd(ctx, gate, restored, head); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = WorktreeRemove(ctx, gate, restored) })
	if err := RestoreDirtyWork(ctx, restored, commit); err != nil {
		t.Fatal(err)
	}
	if got := string(mustRead(t, filepath.Join(restored, "file.txt"))); got != body {
		t.Fatalf("restored tracked bytes = %q", got)
	}
	if _, err := os.Lstat(filepath.Join(restored, ".gitattributes")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ignored attribute source was restored: %v", err)
	}
	assertObjectStoreLacks(t, gate, marker)
}

func TestCaptureDirtyWork_RestoresStagedWhitespaceExactly(t *testing.T) {
	for _, whitespace := range []string{"fix", "error-all"} {
		t.Run(whitespace, func(t *testing.T) {
			ctx := context.Background()
			gate, wt, head, _ := newDirtyWorkGate(t, "staged whitespace")
			run(t, wt, "git", "config", "core.eol", "lf")
			run(t, wt, "git", "config", "apply.whitespace", whitespace)
			staged := []byte("staged trailing  \n\tindented\t \n")
			working := []byte("working trailing \t  \n")
			if err := os.WriteFile(filepath.Join(wt, "file.txt"), staged, 0o644); err != nil {
				t.Fatal(err)
			}
			run(t, wt, "git", "add", "file.txt")
			if err := os.WriteFile(filepath.Join(wt, "file.txt"), working, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := removeLockedDirtyCheckout(ctx, gate, wt, dirtyWorkRunID); err != nil {
				t.Fatal(err)
			}
			commit := run(t, gate, "git", "rev-parse", DirtyWorkRef(dirtyWorkRunID))
			for _, procedure := range []string{"helper", "documented"} {
				t.Run(procedure, func(t *testing.T) {
					restored := filepath.Join(t.TempDir(), "restored staged whitespace")
					if err := WorktreeAdd(ctx, gate, restored, head); err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { _ = WorktreeRemove(ctx, gate, restored) })
					if procedure == "helper" {
						if err := RestoreDirtyWork(ctx, restored, commit); err != nil {
							t.Fatal(err)
						}
					} else if _, err := Run(ctx, restored, "-c", "core.symlinks=true", "-c", "core.autocrlf=false", "-c", "core.eol=lf", "-c", "apply.whitespace=nowarn", "stash", "apply", "--index", commit); err != nil {
						t.Fatal(err)
					}
					index, err := RunRaw(ctx, restored, "show", ":file.txt")
					if err != nil {
						t.Fatal(err)
					}
					if !bytes.Equal(index, staged) {
						t.Fatalf("restored staged bytes = %q, want %q", index, staged)
					}
					if got := mustRead(t, filepath.Join(restored, "file.txt")); !bytes.Equal(got, working) {
						t.Fatalf("restored working bytes = %q, want %q", got, working)
					}
					if got := run(t, restored, "git", "config", "--get", "apply.whitespace"); got != whitespace {
						t.Fatalf("restoration changed shared whitespace configuration: %s", got)
					}
				})
			}
		})
	}
}

func TestDirtyWorkRecheckRejectsChangesAfterBookkeeping(t *testing.T) {
	for _, phase := range []string{"publish", "remove", "clean"} {
		for _, change := range []string{"untracked", "tracked"} {
			t.Run(phase+"/"+change, func(t *testing.T) {
				ctx := context.Background()
				gate, wt, _, _ := newDirtyWorkGate(t, "late bookkeeping change")
				if phase != "clean" {
					writeFile(t, filepath.Join(wt, "file.txt"), "unique captured work\n")
				}
				var snap *DirtyWorkSnapshot
				var err error
				if phase == "remove" {
					snap, err = CaptureDirtyWork(ctx, DirtyWorkRequest{GateDir: gate, Worktree: wt, RunID: dirtyWorkRunID})
					if err != nil || snap == nil {
						t.Fatalf("initial capture = %#v, %v", snap, err)
					}
				}
				previous := dirtyWorkAfterBookkeeping
				t.Cleanup(func() { dirtyWorkAfterBookkeeping = previous })
				fired := false
				latePath := filepath.Join(wt, "late notes.txt")
				if change == "tracked" {
					latePath = filepath.Join(wt, "gone.txt")
				}
				body := "unique bytes saved after repository bookkeeping\n"
				dirtyWorkAfterBookkeeping = func(path string) {
					if path == wt && !fired {
						fired = true
						writeFile(t, latePath, body)
					}
				}
				if phase == "publish" {
					_, err = CaptureDirtyWork(ctx, DirtyWorkRequest{GateDir: gate, Worktree: wt, RunID: dirtyWorkRunID})
				} else {
					err = DirtyWorkStillMatches(ctx, wt, snap)
				}
				if !fired || !errors.Is(err, ErrDirtyWorkRejected) {
					t.Fatalf("late %s check = %v, callback fired = %v", change, err, fired)
				}
				if got := string(mustRead(t, latePath)); got != body {
					t.Fatalf("rejected recheck changed late bytes: %q", got)
				}
				if phase != "remove" {
					assertNoDirtySnapshot(t, gate)
				} else if got := run(t, gate, "git", "rev-parse", snap.Ref); got != snap.Commit {
					t.Fatalf("recheck replaced snapshot: %s", got)
				}
			})
		}
	}
}

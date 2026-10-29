package git

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestCaptureDirtyWork_PreservesPathIdentity(t *testing.T) {
	paths := []string{"space name.txt", "forward dir/name.txt", filepath.Join("native dir", "name.txt")}
	if runtime.GOOS != "windows" {
		// Windows forbids quotes and control characters, and treats a
		// backslash as a separator rather than as a filename byte.
		paths = append(paths, `"notes.txt`, `back\slash.txt`)
	}
	for _, state := range []string{"staged", "unstaged", "both", "untracked", "staged-deletion", "unstaged-deletion"} {
		t.Run(state, func(t *testing.T) {
			ctx := context.Background()
			gate, wt, _, base := newDirtyWorkGate(t, "path identity")
			original := []byte("original\x00bytes\r\n")
			staged := []byte("staged\x00bytes\r\n")
			working := []byte("working\x00bytes\r\n")
			for _, path := range paths {
				full := filepath.Join(wt, path)
				if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
					t.Fatal(err)
				}
				if state != "untracked" {
					if err := os.WriteFile(full, original, 0o644); err != nil {
						t.Fatal(err)
					}
				}
			}
			if state != "untracked" {
				run(t, wt, "git", "add", "--all")
				run(t, wt, "git", "commit", "-m", "path fixtures")
			}
			head := run(t, wt, "git", "rev-parse", "HEAD")
			deleted := state == "staged-deletion" || state == "unstaged-deletion"
			for _, path := range paths {
				full := filepath.Join(wt, path)
				if deleted {
					if err := os.Remove(full); err != nil {
						t.Fatal(err)
					}
				} else if err := os.WriteFile(full, staged, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if state == "staged" || state == "both" || state == "staged-deletion" {
				run(t, wt, "git", "add", "--all")
			}
			wantBytes := staged
			if state == "both" || state == "unstaged" || state == "untracked" {
				wantBytes = working
				for _, path := range paths {
					if err := os.WriteFile(filepath.Join(wt, path), working, 0o644); err != nil {
						t.Fatal(err)
					}
				}
			}
			status, err := RunRaw(ctx, wt, gitStatusArgs()...)
			if err != nil {
				t.Fatal(err)
			}
			index := run(t, wt, "git", "write-tree")
			snap, err := CaptureDirtyWork(ctx, DirtyWorkRequest{GateDir: gate, Worktree: wt, RunID: dirtyWorkRunID, BaseSHA: base})
			if err != nil || snap == nil {
				t.Fatalf("capture = %#v, %v", snap, err)
			}
			if snap.Head != head || snap.Base != base {
				t.Fatalf("snapshot ancestry = %#v, want head %s, base %s", snap, head, base)
			}
			if err := DirtyWorkStillMatches(ctx, wt, snap); err != nil {
				t.Fatalf("confirm snapshot: %v", err)
			}
			assertState := func(dir string) {
				t.Helper()
				got, err := RunRaw(ctx, dir, gitStatusArgs()...)
				if err != nil || !bytes.Equal(got, status) {
					t.Fatalf("status = %q, %v; want %q", got, err, status)
				}
				if got := run(t, dir, "git", "write-tree"); got != index {
					t.Fatalf("index tree = %s, want %s", got, index)
				}
				for _, path := range paths {
					full := filepath.Join(dir, path)
					if deleted {
						if _, err := os.Lstat(full); !os.IsNotExist(err) {
							t.Fatalf("deleted path %q: %v", path, err)
						}
					} else if got := mustRead(t, full); !bytes.Equal(got, wantBytes) {
						t.Fatalf("path %q bytes = %q, want %q", path, got, wantBytes)
					}
				}
			}
			assertState(wt)
			if err := WorktreeRemove(ctx, gate, wt); err != nil {
				t.Fatal(err)
			}
			restored := filepath.Join(t.TempDir(), "restored path identity")
			if err := WorktreeAdd(ctx, gate, restored, head); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = WorktreeRemove(ctx, gate, restored) })
			if err := RestoreDirtyWork(ctx, restored, snap.Commit); err != nil {
				t.Fatal(err)
			}
			assertState(restored)
		})
	}
}

func TestCaptureDirtyWork_ControlPathsRetainSource(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows forbids control characters in filenames")
	}
	for _, path := range []string{"tab\tname.txt", "line\nname.txt", "return\rname.txt"} {
		for _, state := range []string{"staged", "unstaged", "untracked"} {
			t.Run(path+"/"+state, func(t *testing.T) {
				ctx := context.Background()
				gate, wt, _, _ := newDirtyWorkGate(t, "control path")
				full := filepath.Join(wt, path)
				writeFile(t, full, "original\n")
				if state != "untracked" {
					run(t, wt, "git", "add", "--", path)
					if state == "unstaged" {
						run(t, wt, "git", "commit", "-m", "control path fixture")
					}
				}
				body := []byte("synthetic control-path work\x00\r\n")
				if err := os.WriteFile(full, body, 0o644); err != nil {
					t.Fatal(err)
				}
				status, err := RunRaw(ctx, wt, gitStatusArgs()...)
				if err != nil {
					t.Fatal(err)
				}
				index := run(t, wt, "git", "write-tree")
				snap, err := CaptureDirtyWork(ctx, DirtyWorkRequest{GateDir: gate, Worktree: wt, RunID: dirtyWorkRunID})
				if snap != nil || !errors.Is(err, ErrDirtyWorkRejected) {
					t.Fatalf("control-path capture = %#v, %v", snap, err)
				}
				assertNoDirtySnapshot(t, gate)
				assertObjectStoreLacks(t, gate, string(body))
				if got := mustRead(t, full); !bytes.Equal(got, body) {
					t.Fatalf("refused capture changed source bytes: %q", got)
				}
				got, err := RunRaw(ctx, wt, gitStatusArgs()...)
				if err != nil || !bytes.Equal(got, status) || run(t, wt, "git", "write-tree") != index {
					t.Fatalf("refused capture changed status/index: %q, %v", got, err)
				}
			})
		}
	}
}

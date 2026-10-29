package git

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCaptureDirtyWork_AttributeOnlyChangesPreserveBytes(t *testing.T) {
	for _, state := range []string{"staged", "unstaged", "untracked"} {
		for _, ending := range []string{"lf", "crlf"} {
			t.Run(state+"/"+ending, func(t *testing.T) {
				ctx := context.Background()
				gate, wt, _, _ := newDirtyWorkGate(t, "attribute only")
				run(t, wt, "git", "config", "core.eol", "lf")
				if state != "untracked" {
					writeFile(t, filepath.Join(wt, ".gitattributes"), "/.gitattributes -text\nfile.txt -text\n")
					run(t, wt, "git", "add", ".gitattributes")
					run(t, wt, "git", "commit", "-m", "original attribute rules")
				}
				head := run(t, wt, "git", "rev-parse", "HEAD")
				attributes := []byte("/.gitattributes -text\nfile.txt text eol=" + ending + "\n")
				if err := os.WriteFile(filepath.Join(wt, ".gitattributes"), attributes, 0o644); err != nil {
					t.Fatal(err)
				}
				if state == "staged" {
					run(t, wt, "git", "add", ".gitattributes")
				}
				body := []byte("base\n")
				if ending == "crlf" {
					body = []byte("base\r\n")
				}
				if err := os.WriteFile(filepath.Join(wt, "file.txt"), body, 0o644); err != nil {
					t.Fatal(err)
				}
				if got := run(t, wt, "git", "rev-parse", ":file.txt"); got != run(t, wt, "git", "rev-parse", "HEAD:file.txt") {
					t.Fatal("fixture changed the data blob instead of only attributes")
				}
				status := run(t, wt, "git", "status", "--porcelain")
				cached := run(t, wt, "git", "diff", "--cached")
				snap, err := CaptureDirtyWork(ctx, DirtyWorkRequest{GateDir: gate, Worktree: wt, RunID: dirtyWorkRunID})
				if err != nil {
					if ending == "lf" || !errors.Is(err, ErrDirtyWorkRejected) {
						t.Fatalf("attribute-only capture: %v", err)
					}
					assertNoDirtySnapshot(t, gate)
					if got := mustRead(t, filepath.Join(wt, "file.txt")); !bytes.Equal(got, body) {
						t.Fatalf("refused capture changed original bytes: %q", got)
					}
					if got := mustRead(t, filepath.Join(wt, ".gitattributes")); !bytes.Equal(got, attributes) {
						t.Fatalf("refused capture changed attributes: %q", got)
					}
					return
				}
				if snap == nil {
					t.Fatal("attribute-only changes were treated as clean")
				}
				if err := DirtyWorkStillMatches(ctx, wt, snap); err != nil {
					t.Fatal(err)
				}
				if err := WorktreeRemove(ctx, gate, wt); err != nil {
					t.Fatal(err)
				}
				restored := filepath.Join(t.TempDir(), "restored attribute only")
				if err := WorktreeAdd(ctx, gate, restored, head); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = WorktreeRemove(ctx, gate, restored) })
				if err := RestoreDirtyWork(ctx, restored, snap.Commit); err != nil {
					t.Fatal(err)
				}
				if got := mustRead(t, filepath.Join(restored, "file.txt")); !bytes.Equal(got, body) {
					t.Fatalf("restoration changed unchanged data bytes: %q, want %q", got, body)
				}
				if got := mustRead(t, filepath.Join(restored, ".gitattributes")); !bytes.Equal(got, attributes) {
					t.Fatalf("restored attributes = %q, want %q", got, attributes)
				}
				if got := run(t, restored, "git", "status", "--porcelain"); got != status {
					t.Fatalf("restored status = %q, want %q", got, status)
				}
				if got := run(t, restored, "git", "diff", "--cached"); got != cached {
					t.Fatalf("restored index diff = %q, want %q", got, cached)
				}
			})
		}
	}
}

func TestCaptureDirtyWork_CommittedCRLFAttributesRoundTrip(t *testing.T) {
	for _, state := range []string{"staged", "unstaged", "both"} {
		t.Run(state, func(t *testing.T) {
			ctx := context.Background()
			gate, original, _, _ := newDirtyWorkGate(t, "configure text attributes")
			writeFile(t, filepath.Join(original, ".gitattributes"), "* text\n")
			run(t, original, "git", "add", ".gitattributes")
			run(t, original, "git", "commit", "-m", "committed text attributes")
			head := run(t, original, "git", "rev-parse", "HEAD")
			run(t, gate, "git", "config", "core.autocrlf", "true")
			wt := filepath.Join(t.TempDir(), "crlf attributes")
			if err := WorktreeAdd(ctx, gate, wt, head); err != nil {
				t.Fatal(err)
			}
			attributes := mustRead(t, filepath.Join(wt, ".gitattributes"))
			if !bytes.Equal(attributes, []byte("* text\r\n")) {
				t.Fatalf("attribute checkout = %q", attributes)
			}
			body := []byte("staged\r\n")
			if state != "unstaged" {
				if err := os.WriteFile(filepath.Join(wt, "file.txt"), body, 0o644); err != nil {
					t.Fatal(err)
				}
				run(t, wt, "git", "add", "file.txt")
			}
			if state != "staged" {
				body = []byte("unique unstaged\r\n")
				if err := os.WriteFile(filepath.Join(wt, "file.txt"), body, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			cached := run(t, wt, "git", "diff", "--cached")
			snap, err := CaptureDirtyWork(ctx, DirtyWorkRequest{GateDir: gate, Worktree: wt, RunID: dirtyWorkRunID})
			if err != nil || snap == nil {
				t.Fatalf("valid CRLF attribute capture = %#v, %v", snap, err)
			}
			if err := DirtyWorkStillMatches(ctx, wt, snap); err != nil {
				t.Fatal(err)
			}
			if err := WorktreeRemove(ctx, gate, wt); err != nil {
				t.Fatal(err)
			}
			restored := filepath.Join(t.TempDir(), "restored crlf attributes")
			if err := WorktreeAdd(ctx, gate, restored, head); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = WorktreeRemove(ctx, gate, restored) })
			if err := RestoreDirtyWork(ctx, restored, snap.Commit); err != nil {
				t.Fatal(err)
			}
			if got := mustRead(t, filepath.Join(restored, "file.txt")); !bytes.Equal(got, body) {
				t.Fatalf("restored data bytes = %q, want %q", got, body)
			}
			if got := mustRead(t, filepath.Join(restored, ".gitattributes")); !bytes.Equal(got, attributes) {
				t.Fatalf("restored attribute bytes = %q, want %q", got, attributes)
			}
			if got := run(t, restored, "git", "diff", "--cached"); got != cached {
				t.Fatalf("restored staged changes = %q, want %q", got, cached)
			}
		})
	}
}

func TestCaptureDirtyWork_RejectsCheckoutChangesToAttributeRules(t *testing.T) {
	ctx := context.Background()
	gate, wt, _, _ := newDirtyWorkGate(t, "converted rules")
	writeFile(t, filepath.Join(wt, ".gitattributes"), "/.gitattributes ident -text\nfile$Id$.txt merge=union\n")
	writeFile(t, filepath.Join(wt, "file$Id$.txt"), "original\n")
	run(t, wt, "git", "add", ".gitattributes", "file$Id$.txt")
	run(t, wt, "git", "commit", "-m", "attribute rules containing ident")
	converted, err := RunRaw(ctx, wt, "cat-file", "--filters", "--path=.gitattributes", "HEAD:.gitattributes")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".gitattributes"), converted, 0o644); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(wt, "file.txt"), "ordinary unique bytes\n")
	if err := removeLockedDirtyCheckout(ctx, gate, wt, dirtyWorkRunID); !errors.Is(err, ErrDirtyWorkRejected) {
		t.Fatalf("converted attribute rules were accepted: %v", err)
	}
	assertNoDirtySnapshot(t, gate)
	if got := string(mustRead(t, filepath.Join(wt, "file.txt"))); got != "ordinary unique bytes\n" {
		t.Fatalf("rejected capture changed live bytes: %q", got)
	}
}

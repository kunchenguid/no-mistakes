package git

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCaptureDirtyWork_RejectsByteChangingConversions(t *testing.T) {
	for _, state := range []string{"untracked", "tracked", "staged", "assume-unchanged", "skip-worktree"} {
		for _, token := range []string{"$Id: unique $", "$Id$"} {
			t.Run(state+"/"+token, func(t *testing.T) {
				ctx := context.Background()
				gate, wt, _, _ := newDirtyWorkGate(t, "conversion")
				writeFile(t, filepath.Join(wt, ".gitattributes"), "*.dat ident -text\n")
				run(t, wt, "git", "add", ".gitattributes")
				run(t, wt, "git", "commit", "-m", "attributes")
				rel := "bin.dat"
				if state == "untracked" {
					rel = "new data.dat"
				}
				if state == "assume-unchanged" || state == "skip-worktree" {
					run(t, wt, "git", "update-index", "--"+state, "--", rel)
				}
				body := []byte("binary\x00" + token + "\n")
				if err := os.WriteFile(filepath.Join(wt, rel), body, 0o644); err != nil {
					t.Fatal(err)
				}
				if state == "staged" {
					run(t, wt, "git", "add", "--", rel)
				}
				err := removeLockedDirtyCheckout(ctx, gate, wt, dirtyWorkRunID)
				if err == nil {
					t.Fatalf("conversion capture error = %v", err)
				}
				if got := mustRead(t, filepath.Join(wt, rel)); !bytes.Equal(got, body) {
					t.Fatalf("refused cleanup changed bytes: %q", got)
				}
				assertNoDirtySnapshot(t, gate)
			})
		}
	}
}

func TestCaptureDirtyWork_RejectsByteChangesHiddenByIdent(t *testing.T) {
	ctx := context.Background()
	gate, wt, _, _ := newDirtyWorkGate(t, "hidden-conversion")
	writeFile(t, filepath.Join(wt, ".gitattributes"), "*.dat ident -text\n")
	writeFile(t, filepath.Join(wt, "bin.dat"), "binary\x00$Id: before $\n")
	run(t, wt, "git", "add", ".gitattributes", "bin.dat")
	run(t, wt, "git", "commit", "-m", "ident file")
	body := []byte("binary\x00$Id: unique $\n")
	if err := os.WriteFile(filepath.Join(wt, "bin.dat"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := run(t, wt, "git", "status", "--porcelain"); got != "" {
		t.Fatalf("expected Git to hide the byte change, got %q", got)
	}
	if err := DirtyWorkStillMatches(ctx, wt, nil); err == nil {
		t.Fatal("nil snapshot matched bytes hidden by ident")
	}
	if err := removeLockedDirtyCheckout(ctx, gate, wt, dirtyWorkRunID); err == nil {
		t.Fatal("cleanup accepted a byte change hidden by ident")
	}
	if got := mustRead(t, filepath.Join(wt, "bin.dat")); !bytes.Equal(got, body) {
		t.Fatalf("rejected cleanup changed bytes: %q", got)
	}
	assertNoDirtySnapshot(t, gate)
}

func TestCaptureDirtyWork_IdentityConversionRoundTrips(t *testing.T) {
	ctx := context.Background()
	gate, wt, _, _ := newDirtyWorkGate(t, "identity-conversion")
	writeFile(t, filepath.Join(wt, ".gitattributes"), "*.dat ident -text\n")
	run(t, wt, "git", "add", ".gitattributes")
	run(t, wt, "git", "commit", "-m", "attributes")
	head := run(t, wt, "git", "rev-parse", "HEAD")
	body := []byte("unchanged-by-ident\x00binary\r\n")
	if err := os.WriteFile(filepath.Join(wt, "new data.dat"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := removeLockedDirtyCheckout(ctx, gate, wt, dirtyWorkRunID); err != nil {
		t.Fatalf("identity conversion: %v", err)
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
	if got := mustRead(t, filepath.Join(restored, "new data.dat")); !bytes.Equal(got, body) {
		t.Fatalf("restored bytes = %q, want %q", got, body)
	}
}

func TestDirtyWorkStillMatchesRejectsChangedCheckoutConversion(t *testing.T) {
	ctx := context.Background()
	gate, wt, _, _ := newDirtyWorkGate(t, "changed-conversion")
	writeFile(t, filepath.Join(wt, "file.txt"), "unique line\n")
	snap, err := CaptureDirtyWork(ctx, DirtyWorkRequest{GateDir: gate, Worktree: wt, RunID: dirtyWorkRunID})
	if err != nil {
		t.Fatal(err)
	}
	run(t, wt, "git", "config", "core.autocrlf", "true")
	if err := DirtyWorkStillMatches(ctx, wt, snap); !errors.Is(err, ErrDirtyWorkRejected) {
		t.Fatalf("changed checkout conversion error = %v", err)
	}
	if got := string(mustRead(t, filepath.Join(wt, "file.txt"))); got != "unique line\n" {
		t.Fatalf("recheck changed worktree: %q", got)
	}
}

func TestCaptureDirtyWork_RejectsIntentToAdd(t *testing.T) {
	for _, body := range []string{"", "new content\n"} {
		t.Run(body, func(t *testing.T) {
			ctx := context.Background()
			gate, wt, _, _ := newDirtyWorkGate(t, "intent-to-add")
			writeFile(t, filepath.Join(wt, "new file.txt"), body)
			run(t, wt, "git", "add", "-N", "--", "new file.txt")
			index, err := absoluteGitPath(ctx, wt, "index")
			if err != nil {
				t.Fatal(err)
			}
			before := mustRead(t, index)
			if err := DirtyWorkStillMatches(ctx, wt, nil); !errors.Is(err, ErrDirtyWorkRejected) {
				t.Fatalf("intent-to-add clean check = %v", err)
			}
			if err := removeLockedDirtyCheckout(ctx, gate, wt, dirtyWorkRunID); !errors.Is(err, ErrDirtyWorkRejected) {
				t.Fatalf("intent-to-add cleanup = %v", err)
			}
			if got := mustRead(t, index); !bytes.Equal(got, before) {
				t.Fatal("capture changed the original index")
			}
			if got := string(mustRead(t, filepath.Join(wt, "new file.txt"))); got != body {
				t.Fatalf("intent-to-add file changed: %q", got)
			}
			assertNoDirtySnapshot(t, gate)
		})
	}
}

func TestDirtyWorkStillMatchesRejectsLateIntentToAdd(t *testing.T) {
	ctx := context.Background()
	gate, wt, _, _ := newDirtyWorkGate(t, "late-intent")
	writeFile(t, filepath.Join(wt, "file.txt"), "unique\n")
	snap, err := CaptureDirtyWork(ctx, DirtyWorkRequest{GateDir: gate, Worktree: wt, RunID: dirtyWorkRunID})
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(wt, "new file.txt"), "")
	run(t, wt, "git", "add", "-N", "--", "new file.txt")
	if err := DirtyWorkStillMatches(ctx, wt, snap); !errors.Is(err, ErrDirtyWorkRejected) {
		t.Fatalf("late intent-to-add recheck = %v", err)
	}
}

func TestWriteDirtySnapshotRejectsSecretStagedAfterStatus(t *testing.T) {
	ctx := context.Background()
	gate, wt, _, _ := newDirtyWorkGate(t, "late-index-secret")
	writeFile(t, filepath.Join(wt, "file.txt"), "ordinary dirty work\n")
	entries, err := checkoutDirtyEntries(ctx, wt)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(wt, "credentials.json"), "{\"token\":\"late-credential\"}\n")
	run(t, wt, "git", "add", "--", "credentials.json")
	_, err = writeDirtySnapshot(ctx, DirtyWorkRequest{GateDir: gate, Worktree: wt, RunID: dirtyWorkRunID}, DirtyWorkRef(dirtyWorkRunID), entries)
	if !errors.Is(err, ErrDirtyWorkRejected) {
		t.Fatalf("late index secret error = %v", err)
	}
	assertNoDirtySnapshot(t, gate)
	if _, err := os.Stat(filepath.Join(wt, "credentials.json")); err != nil {
		t.Fatal(err)
	}
}

func TestWriteDirtySnapshotRejectsIntentAddedAfterStatus(t *testing.T) {
	ctx := context.Background()
	gate, wt, _, _ := newDirtyWorkGate(t, "late-copied-intent")
	writeFile(t, filepath.Join(wt, "file.txt"), "ordinary dirty work\n")
	entries, err := checkoutDirtyEntries(ctx, wt)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(wt, "new file.txt"), "")
	run(t, wt, "git", "add", "-N", "--", "new file.txt")
	_, err = writeDirtySnapshot(ctx, DirtyWorkRequest{GateDir: gate, Worktree: wt, RunID: dirtyWorkRunID}, DirtyWorkRef(dirtyWorkRunID), entries)
	if !errors.Is(err, ErrDirtyWorkRejected) {
		t.Fatalf("late copied index intent error = %v", err)
	}
	assertNoDirtySnapshot(t, gate)
}

func TestCaptureDirtyWork_RejectsEncryptedPKCS8(t *testing.T) {
	for _, staged := range []bool{false, true} {
		name := "worktree"
		if staged {
			name = "index"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			gate, wt, _, _ := newDirtyWorkGate(t, "encrypted-key")
			const marker = "encrypted-pkcs8-unique-marker"
			body := append(bytes.Repeat([]byte("a"), secretReadChunk-5), []byte("-----BEGIN ENCRYPTED PRIVATE KEY-----\n"+marker+"\n")...)
			if err := os.WriteFile(filepath.Join(wt, "server.key"), body, 0o644); err != nil {
				t.Fatal(err)
			}
			if staged {
				run(t, wt, "git", "add", "--", "server.key")
				writeFile(t, filepath.Join(wt, "server.key"), "plain worktree\n")
			}
			if err := removeLockedDirtyCheckout(ctx, gate, wt, dirtyWorkRunID); !errors.Is(err, ErrDirtyWorkRejected) {
				t.Fatalf("encrypted PKCS8 capture = %v", err)
			}
			assertNoDirtySnapshot(t, gate)
			if !staged {
				assertObjectStoreLacks(t, gate, marker)
			}
			if _, err := os.Stat(filepath.Join(wt, "server.key")); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func assertNoDirtySnapshot(t *testing.T, gate string) {
	t.Helper()
	if _, err := Run(context.Background(), gate, "rev-parse", "--verify", "--quiet", DirtyWorkRef(dirtyWorkRunID)); err == nil {
		t.Fatal("rejected capture published a snapshot")
	}
}

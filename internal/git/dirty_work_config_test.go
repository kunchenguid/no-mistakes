package git

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCaptureDirtyWork_CleanCRLFCheckoutNeedsNoSnapshot(t *testing.T) {
	ctx := context.Background()
	gate, original, head, _ := newDirtyWorkGate(t, "configure-crlf")
	writeFile(t, filepath.Join(original, "fixture.txt"), "-----BEGIN ENCRYPTED PRIVATE KEY-----\ncommitted literal\n")
	run(t, original, "git", "add", "fixture.txt")
	run(t, original, "git", "commit", "-m", "committed marker")
	head = run(t, original, "git", "rev-parse", "HEAD")
	run(t, gate, "git", "config", "core.autocrlf", "true")
	wt := filepath.Join(t.TempDir(), "clean crlf")
	if err := WorktreeAdd(ctx, gate, wt, head); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, filepath.Join(wt, "fixture.txt")); !bytes.Contains(got, []byte("\r\n")) {
		t.Fatalf("fixture was not checked out as CRLF: %q", got)
	}
	if got := run(t, wt, "git", "status", "--porcelain"); got != "" {
		t.Fatalf("fixture is not clean: %s", got)
	}
	snap, err := CaptureDirtyWork(ctx, DirtyWorkRequest{GateDir: gate, Worktree: wt, RunID: dirtyWorkRunID})
	if err != nil || snap != nil {
		t.Fatalf("clean CRLF capture = %#v, %v", snap, err)
	}
	if err := DirtyWorkStillMatches(ctx, wt, nil); err != nil {
		t.Fatalf("clean CRLF confirmation = %v", err)
	}
	if err := removeLockedDirtyCheckout(ctx, gate, wt, dirtyWorkRunID); err != nil {
		t.Fatal(err)
	}
	assertNoDirtySnapshot(t, gate)
}

func TestCaptureDirtyWork_CleanConvertedIdentNeedsNoSnapshot(t *testing.T) {
	ctx := context.Background()
	gate, wt, _, _ := newDirtyWorkGate(t, "clean ident")
	writeFile(t, filepath.Join(wt, ".gitattributes"), "file.txt ident -text\n")
	writeFile(t, filepath.Join(wt, "file.txt"), "binary\x00$Id$\n")
	run(t, wt, "git", "add", ".gitattributes", "file.txt")
	run(t, wt, "git", "commit", "-m", "ident checkout")
	clean, err := RunRaw(ctx, wt, "cat-file", "--filters", "--path=file.txt", "HEAD:file.txt")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "file.txt"), clean, 0o644); err != nil {
		t.Fatal(err)
	}
	snap, err := CaptureDirtyWork(ctx, DirtyWorkRequest{GateDir: gate, Worktree: wt, RunID: dirtyWorkRunID})
	if err != nil || snap != nil {
		t.Fatalf("clean ident capture = %#v, %v", snap, err)
	}
	if err := DirtyWorkStillMatches(ctx, wt, nil); err != nil {
		t.Fatalf("clean ident confirmation = %v", err)
	}
	assertNoDirtySnapshot(t, gate)
}

func TestCaptureDirtyWork_PreservesUniqueCRLFEdits(t *testing.T) {
	for _, flag := range []string{"", "staged-only", "--assume-unchanged", "--skip-worktree"} {
		t.Run(flag, func(t *testing.T) {
			ctx := context.Background()
			gate, _, head, _ := newDirtyWorkGate(t, "configure-crlf")
			run(t, gate, "git", "config", "core.autocrlf", "true")
			wt := filepath.Join(t.TempDir(), "dirty crlf")
			if err := WorktreeAdd(ctx, gate, wt, head); err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(flag, "--") {
				run(t, wt, "git", "update-index", flag, "--", "file.txt")
			} else if flag == "" {
				writeFile(t, filepath.Join(wt, "file.txt"), "staged\r\n")
				run(t, wt, "git", "add", "file.txt")
			}
			body := []byte("unique\r\nsecond line\r\n")
			if err := os.WriteFile(filepath.Join(wt, "file.txt"), body, 0o644); err != nil {
				t.Fatal(err)
			}
			if flag == "staged-only" {
				run(t, wt, "git", "add", "file.txt")
			}
			if err := removeLockedDirtyCheckout(ctx, gate, wt, dirtyWorkRunID); err != nil {
				t.Fatalf("capture CRLF edit = %v", err)
			}
			commit := run(t, gate, "git", "rev-parse", DirtyWorkRef(dirtyWorkRunID))
			restored := filepath.Join(t.TempDir(), "restored crlf")
			if err := WorktreeAdd(ctx, gate, restored, head); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = WorktreeRemove(ctx, gate, restored) })
			if err := RestoreDirtyWork(ctx, restored, commit); err != nil {
				t.Fatal(err)
			}
			if got := mustRead(t, filepath.Join(restored, "file.txt")); !bytes.Equal(got, body) {
				t.Fatalf("restored CRLF edit = %q, want %q", got, body)
			}
			if flag == "" && run(t, restored, "git", "show", ":file.txt") != "staged" {
				t.Fatal("staged content was not preserved")
			}
			if flag == "staged-only" && run(t, restored, "git", "show", ":file.txt") != "unique\nsecond line" {
				t.Fatal("staged-only content was not preserved")
			}
		})
	}
}

func TestCaptureDirtyWork_RespectsIgnoredExecutableBits(t *testing.T) {
	for _, edited := range []bool{false, true} {
		name := "clean"
		if edited {
			name = "edited"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			gate, wt, _, _ := newDirtyWorkGate(t, "filemode")
			writeFile(t, filepath.Join(wt, "script.sh"), "original\n")
			run(t, wt, "git", "add", "script.sh")
			run(t, wt, "git", "update-index", "--chmod=+x", "--", "script.sh")
			run(t, wt, "git", "commit", "-m", "executable entry")
			head := run(t, wt, "git", "rev-parse", "HEAD")
			run(t, wt, "git", "config", "core.filemode", "false")
			if err := os.Chmod(filepath.Join(wt, "script.sh"), 0o644); err != nil {
				t.Fatal(err)
			}
			if !edited {
				snap, err := CaptureDirtyWork(ctx, DirtyWorkRequest{GateDir: gate, Worktree: wt, RunID: dirtyWorkRunID})
				if err != nil || snap != nil {
					t.Fatalf("ignored mode capture = %#v, %v", snap, err)
				}
				if err := DirtyWorkStillMatches(ctx, wt, nil); err != nil {
					t.Fatal(err)
				}
				assertNoDirtySnapshot(t, gate)
				return
			}
			writeFile(t, filepath.Join(wt, "script.sh"), "edited bytes\n")
			if err := removeLockedDirtyCheckout(ctx, gate, wt, dirtyWorkRunID); err != nil {
				t.Fatal(err)
			}
			commit := run(t, gate, "git", "rev-parse", DirtyWorkRef(dirtyWorkRunID))
			if got := run(t, gate, "git", "ls-tree", commit, "--", "script.sh"); !strings.HasPrefix(got, "100755 ") {
				t.Fatalf("snapshot changed executable index intent: %s", got)
			}
			restored := filepath.Join(t.TempDir(), "restored mode")
			if err := WorktreeAdd(ctx, gate, restored, head); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = WorktreeRemove(ctx, gate, restored) })
			if err := RestoreDirtyWork(ctx, restored, commit); err != nil {
				t.Fatal(err)
			}
			if got := string(mustRead(t, filepath.Join(restored, "script.sh"))); got != "edited bytes\n" {
				t.Fatalf("restored executable content: %q", got)
			}
		})
	}
}

func TestCaptureDirtyWork_RestoresAfterWorktreeConfigIsRemoved(t *testing.T) {
	for _, setting := range []string{"autocrlf", "eol"} {
		t.Run(setting, func(t *testing.T) {
			ctx := context.Background()
			gate, wt, _, _ := newDirtyWorkGate(t, "worktree config")
			if setting == "eol" {
				writeFile(t, filepath.Join(wt, ".gitattributes"), "* text\n")
				run(t, wt, "git", "add", ".gitattributes")
				run(t, wt, "git", "commit", "-m", "text attributes")
			}
			head := run(t, wt, "git", "rev-parse", "HEAD")
			run(t, gate, "git", "config", "extensions.worktreeConfig", "true")
			run(t, gate, "git", "config", "--local", "--unset", "core.bare")
			run(t, gate, "git", "config", "--worktree", "core.bare", "true")
			run(t, wt, "git", "config", "--worktree", "core.bare", "false")
			sharedAuto := "false"
			if setting == "autocrlf" {
				sharedAuto = "true"
			}
			run(t, gate, "git", "config", "core.autocrlf", sharedAuto)
			run(t, gate, "git", "config", "core.eol", "crlf")
			run(t, wt, "git", "config", "--worktree", "core.autocrlf", "false")
			run(t, wt, "git", "config", "--worktree", "core.eol", "lf")
			configPath, err := absoluteGitPath(ctx, wt, "config.worktree")
			if err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(wt, "file.txt"), "unique\n")
			snap, err := CaptureDirtyWork(ctx, DirtyWorkRequest{GateDir: gate, Worktree: wt, RunID: dirtyWorkRunID})
			if err != nil || snap == nil {
				t.Fatalf("capture override = %#v, %v", snap, err)
			}
			config, err := readDirtyWorkRestoreConfig(ctx, wt, snap.Commit)
			if err != nil || config.autoCRLF != "false" || config.eol != "lf" {
				t.Fatalf("persisted restore configuration = %#v, %v", config, err)
			}
			if err := DirtyWorkStillMatches(ctx, wt, snap); err != nil {
				t.Fatal(err)
			}
			if err := WorktreeRemove(ctx, gate, wt); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(configPath); !os.IsNotExist(err) {
				t.Fatalf("original configuration survived removal: %v", err)
			}
			restored := filepath.Join(t.TempDir(), "restored override")
			run(t, gate, "git", "-c", "core.symlinks=true", "-c", "core.autocrlf=false", "-c", "core.eol=lf", "worktree", "add", "--detach", restored, head)
			t.Cleanup(func() { _ = WorktreeRemove(ctx, gate, restored) })
			if got := run(t, restored, "git", "config", "--get", "core.autocrlf"); got != sharedAuto {
				t.Fatalf("replacement did not inherit shared autocrlf: %q", got)
			}
			if got := run(t, restored, "git", "config", "--get", "core.eol"); got != "crlf" {
				t.Fatalf("replacement did not inherit shared eol: %q", got)
			}
			if err := RestoreDirtyWork(ctx, restored, snap.Commit); err != nil {
				t.Fatalf("restore with lost worktree config: %v", err)
			}
			if got := string(mustRead(t, filepath.Join(restored, "file.txt"))); got != "unique\n" {
				t.Fatalf("helper restored changed bytes: %q", got)
			}
			manual := filepath.Join(t.TempDir(), "documented restore")
			run(t, gate, "git", "-c", "core.symlinks=true", "-c", "core.autocrlf=false", "-c", "core.eol=lf", "worktree", "add", "--detach", manual, head)
			t.Cleanup(func() { _ = WorktreeRemove(ctx, gate, manual) })
			run(t, manual, "git", "-c", "core.symlinks=true", "-c", "core.autocrlf=false", "-c", "core.eol=lf", "-c", "apply.whitespace=nowarn", "stash", "apply", "--index", snap.Commit)
			if got := string(mustRead(t, filepath.Join(manual, "file.txt"))); got != "unique\n" {
				t.Fatalf("documented procedure restored changed bytes: %q", got)
			}
		})
	}
}

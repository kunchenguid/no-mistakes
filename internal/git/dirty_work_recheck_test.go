package git

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDirtyWorkStillMatchesRejectsRecreatedIgnoredTrackedDeletion(t *testing.T) {
	for _, staged := range []bool{false, true} {
		name := "unstaged"
		if staged {
			name = "staged"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			gate, wt, _, _ := newDirtyWorkGate(t, "ignored-deletion")
			writeFile(t, filepath.Join(wt, ".gitignore"), "gone.txt\n")
			run(t, wt, "git", "add", ".gitignore")
			run(t, wt, "git", "commit", "-m", "ignore tracked name")
			if staged {
				run(t, wt, "git", "rm", "--", "gone.txt")
			} else if err := os.Remove(filepath.Join(wt, "gone.txt")); err != nil {
				t.Fatal(err)
			}
			snap, err := CaptureDirtyWork(ctx, DirtyWorkRequest{GateDir: gate, Worktree: wt, RunID: dirtyWorkRunID})
			if err != nil || snap == nil {
				t.Fatalf("capture deletion = %#v, %v", snap, err)
			}
			if err := DirtyWorkStillMatches(ctx, wt, snap); err != nil {
				t.Fatalf("unchanged deletion = %v", err)
			}
			writeFile(t, filepath.Join(wt, "gone.txt"), "recreated unique work\n")
			if err := DirtyWorkStillMatches(ctx, wt, snap); err == nil {
				t.Fatal("recreated ignored tracked file matched the deletion")
			}
			if got := string(mustRead(t, filepath.Join(wt, "gone.txt"))); got != "recreated unique work\n" {
				t.Fatalf("recheck changed recreated file: %q", got)
			}
		})
	}
}

func TestDirtyWorkRecheckRejectsLateChanges(t *testing.T) {
	for _, phase := range []string{"publish", "remove", "clean"} {
		for _, change := range []string{"untracked", "index", "head", "intent", "tracked", "config"} {
			t.Run(phase+"/"+change, func(t *testing.T) {
				ctx := context.Background()
				gate, wt, _, _ := newDirtyWorkGate(t, "late-change")
				writeFile(t, filepath.Join(wt, ".gitignore"), "*.late\n")
				run(t, wt, "git", "add", ".gitignore")
				run(t, wt, "git", "commit", "-m", "ignore late intent path")
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
				previous := dirtyWorkAfterContentCheck
				t.Cleanup(func() { dirtyWorkAfterContentCheck = previous })
				fired := false
				dirtyWorkAfterContentCheck = func(path string) {
					if path != wt || fired {
						return
					}
					fired = true
					switch change {
					case "untracked":
						writeFile(t, filepath.Join(wt, "late.txt"), "late unique work\n")
					case "index":
						blob := run(t, wt, "git", "rev-parse", "HEAD:README.md")
						run(t, wt, "git", "update-index", "--cacheinfo", "100644", blob, "file.txt")
					case "head":
						run(t, wt, "git", "commit", "--allow-empty", "-m", "late head")
					case "intent":
						writeFile(t, filepath.Join(wt, "intent.late"), "")
						run(t, wt, "git", "add", "-N", "-f", "--", "intent.late")
					case "tracked":
						writeFile(t, filepath.Join(wt, "gone.txt"), "late tracked bytes\n")
					case "config":
						run(t, wt, "git", "config", "core.autocrlf", "input")
					}
				}
				if phase == "publish" {
					_, err = CaptureDirtyWork(ctx, DirtyWorkRequest{GateDir: gate, Worktree: wt, RunID: dirtyWorkRunID})
				} else {
					err = DirtyWorkStillMatches(ctx, wt, snap)
				}
				if !fired || err == nil {
					t.Fatalf("late %s check = %v, callback fired = %v", change, err, fired)
				}
				if _, err := os.Stat(wt); err != nil {
					t.Fatalf("rejected recheck removed checkout: %v", err)
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

func TestDirtyWorkStillMatchesRejectsLateBytesHiddenByIdent(t *testing.T) {
	ctx := context.Background()
	gate, wt, _, _ := newDirtyWorkGate(t, "late-filtered-bytes")
	writeFile(t, filepath.Join(wt, ".gitattributes"), "file.txt ident -text\n")
	writeFile(t, filepath.Join(wt, "file.txt"), "binary\x00$Id$\n")
	run(t, wt, "git", "add", ".gitattributes", "file.txt")
	run(t, wt, "git", "commit", "-m", "unchanged ident file")
	clean, err := RunRaw(ctx, wt, "cat-file", "--filters", "--path=file.txt", "HEAD:file.txt")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "file.txt"), clean, 0o644); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(wt, "gone.txt"), "unique work\n")
	snap, err := CaptureDirtyWork(ctx, DirtyWorkRequest{GateDir: gate, Worktree: wt, RunID: dirtyWorkRunID})
	if err != nil || snap == nil {
		t.Fatalf("capture = %#v, %v", snap, err)
	}
	previous := dirtyWorkAfterContentCheck
	t.Cleanup(func() { dirtyWorkAfterContentCheck = previous })
	dirtyWorkAfterContentCheck = func(path string) {
		if path != wt {
			return
		}
		full := filepath.Join(wt, "file.txt")
		writeFile(t, full, "binary\x00$Id: unique $\n")
		if err := os.Chtimes(full, time.Unix(100, 0), time.Unix(100, 0)); err != nil {
			t.Fatal(err)
		}
	}
	if err := DirtyWorkStillMatches(ctx, wt, snap); err == nil {
		t.Fatal("changed raw bytes matched after the final hash")
	}
	if got := string(mustRead(t, filepath.Join(wt, "file.txt"))); got != "binary\x00$Id: unique $\n" {
		t.Fatalf("recheck changed bytes: %q", got)
	}
}

func TestCaptureDirtyWork_RejectsDirectoryDependentConverters(t *testing.T) {
	ctx := context.Background()
	gate, wt, _, _ := newDirtyWorkGate(t, "directory-dependent-filter")
	writeFile(t, filepath.Join(wt, ".gitattributes"), "file.txt filter=local\n")
	writeFile(t, filepath.Join(wt, "sentinel"), "present during restoration\n")
	run(t, wt, "git", "add", ".gitattributes", "sentinel")
	run(t, wt, "git", "commit", "-m", "filter context")
	run(t, wt, "git", "config", "filter.local.smudge", "if test -f sentinel; then printf transformed; else cat; fi")
	writeFile(t, filepath.Join(wt, "file.txt"), "unique bytes\n")
	if err := removeLockedDirtyCheckout(ctx, gate, wt, dirtyWorkRunID); err == nil {
		t.Fatal("capture accepted a converter whose output depends on omitted working files")
	}
	if got := string(mustRead(t, filepath.Join(wt, "file.txt"))); got != "unique bytes\n" {
		t.Fatalf("rejected converter changed original bytes: %q", got)
	}
	assertNoDirtySnapshot(t, gate)
}

func TestCaptureDirtyWork_RejectsRelativeCustomAttributeSource(t *testing.T) {
	ctx := context.Background()
	gate, wt, _, _ := newDirtyWorkGate(t, "custom-attributes")
	writeFile(t, filepath.Join(wt, "custom-attributes"), "file.txt ident -text\n")
	run(t, wt, "git", "add", "custom-attributes")
	run(t, wt, "git", "commit", "-m", "custom attributes source")
	run(t, wt, "git", "config", "core.attributesfile", "custom-attributes")
	writeFile(t, filepath.Join(wt, "file.txt"), "unique $Id$\n")
	if err := removeLockedDirtyCheckout(ctx, gate, wt, dirtyWorkRunID); err == nil {
		t.Fatal("capture accepted attributes outside the verified restore-stage files")
	}
	assertNoDirtySnapshot(t, gate)
}

func TestCaptureDirtyWork_ValidatesRestoreStageAttributes(t *testing.T) {
	for _, autoCRLF := range []string{"false", "true"} {
		t.Run(autoCRLF, func(t *testing.T) {
			ctx := context.Background()
			gate, wt, head, _ := newDirtyWorkGate(t, "restore-stage-attributes")
			run(t, wt, "git", "config", "core.autocrlf", autoCRLF)
			writeFile(t, filepath.Join(wt, ".gitattributes"), "* -text\n")
			body := []byte("unique\n")
			if err := os.WriteFile(filepath.Join(wt, "file.txt"), body, 0o644); err != nil {
				t.Fatal(err)
			}
			snap, err := CaptureDirtyWork(ctx, DirtyWorkRequest{GateDir: gate, Worktree: wt, RunID: dirtyWorkRunID})
			if autoCRLF == "true" {
				if err == nil {
					t.Fatal("capture accepted LF bytes transformed before untracked attributes are restored")
				}
				assertNoDirtySnapshot(t, gate)
				if got := mustRead(t, filepath.Join(wt, "file.txt")); !bytes.Equal(got, body) {
					t.Fatalf("rejected capture changed bytes: %q", got)
				}
				return
			}
			if err != nil || snap == nil {
				t.Fatalf("identity attribute capture = %#v, %v", snap, err)
			}
			if err := WorktreeRemove(ctx, gate, wt); err != nil {
				t.Fatal(err)
			}
			restored := filepath.Join(t.TempDir(), "restored")
			if err := WorktreeAdd(ctx, gate, restored, head); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = WorktreeRemove(ctx, gate, restored) })
			if err := RestoreDirtyWork(ctx, restored, snap.Commit); err != nil {
				t.Fatal(err)
			}
			if got := mustRead(t, filepath.Join(restored, "file.txt")); !bytes.Equal(got, body) {
				t.Fatalf("restored tracked bytes: %q", got)
			}
			if got := string(mustRead(t, filepath.Join(restored, ".gitattributes"))); got != "* -text\n" {
				t.Fatalf("restored attributes: %q", got)
			}
		})
	}
}

func TestCaptureDirtyWork_AllowsUnchangedCommittedMarkers(t *testing.T) {
	ctx := context.Background()
	gate, wt, _, _ := newDirtyWorkGate(t, "committed-markers")
	writeFile(t, filepath.Join(wt, "fixture.txt"), "-----BEGIN ENCRYPTED PRIVATE KEY-----\nliteral fixture\n")
	run(t, wt, "git", "add", "fixture.txt")
	run(t, wt, "git", "commit", "-m", "committed fixture")
	head := run(t, wt, "git", "rev-parse", "HEAD")
	writeFile(t, filepath.Join(wt, "file.txt"), "staged change\n")
	run(t, wt, "git", "add", "file.txt")
	writeFile(t, filepath.Join(wt, "file.txt"), "unstaged change\n")
	if err := removeLockedDirtyCheckout(ctx, gate, wt, dirtyWorkRunID); err != nil {
		t.Fatalf("unchanged committed marker blocked capture: %v", err)
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
	if got := string(mustRead(t, filepath.Join(restored, "file.txt"))); got != "unstaged change\n" {
		t.Fatalf("restored dirty bytes: %q", got)
	}
	if got := run(t, restored, "git", "show", ":file.txt"); got != "staged change" {
		t.Fatalf("restored index bytes: %q", got)
	}
}

func TestCaptureDirtyWork_RejectsChangedCommittedMarker(t *testing.T) {
	ctx := context.Background()
	gate, wt, _, _ := newDirtyWorkGate(t, "changed-marker")
	writeFile(t, filepath.Join(wt, "fixture.txt"), "ordinary committed fixture\n")
	run(t, wt, "git", "add", "fixture.txt")
	run(t, wt, "git", "commit", "-m", "fixture")
	writeFile(t, filepath.Join(wt, "fixture.txt"), "-----BEGIN ENCRYPTED PRIVATE KEY-----\nnew material\n")
	run(t, wt, "git", "add", "fixture.txt")
	writeFile(t, filepath.Join(wt, "fixture.txt"), "plain working bytes\n")
	if err := removeLockedDirtyCheckout(ctx, gate, wt, dirtyWorkRunID); err == nil {
		t.Fatal("new staged marker was accepted")
	}
	assertNoDirtySnapshot(t, gate)
}

func TestCaptureDirtyWork_StillValidatesEveryCopiedIndexPath(t *testing.T) {
	ctx := context.Background()
	gate, wt, _, _ := newDirtyWorkGate(t, "committed-secret-name")
	writeFile(t, filepath.Join(wt, "credentials.json"), "committed path\n")
	run(t, wt, "git", "add", "credentials.json")
	run(t, wt, "git", "commit", "-m", "committed credential path")
	writeFile(t, filepath.Join(wt, "file.txt"), "ordinary dirty work\n")
	if err := removeLockedDirtyCheckout(ctx, gate, wt, dirtyWorkRunID); err == nil {
		t.Fatal("copied index path screening was skipped for an unchanged blob")
	}
	assertNoDirtySnapshot(t, gate)
}

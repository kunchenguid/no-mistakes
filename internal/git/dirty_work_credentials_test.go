package git

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const syntheticCredentialMarker = "synthetic-capture-credential-12345"

var conventionalCredentialFiles = []struct {
	rel  string
	body string
}{
	{".npmrc", "//registry.npmjs.org/:_authToken=" + syntheticCredentialMarker + "\n"},
	{".git-credentials", "https://fixture:" + syntheticCredentialMarker + "@example.invalid/blob\n"},
	{".netrc", "machine example.invalid login fixture password " + syntheticCredentialMarker + "\n"},
	{"_netrc", "machine example.invalid login fixture password " + syntheticCredentialMarker + "\n"},
	{".pypirc", "[fixture]\nrepository = https://example.invalid/legacy/\nusername = fixture\npassword = " + syntheticCredentialMarker + "\n"},
}

func TestCaptureDirtyWorkRejectsConventionalCredentialFiles(t *testing.T) {
	for _, fixture := range conventionalCredentialFiles {
		for _, rel := range []string{fixture.rel, "sub dir/" + strings.ToUpper(fixture.rel)} {
			for _, state := range []string{"untracked", "unstaged", "staged"} {
				t.Run(rel+"/"+state, func(t *testing.T) {
					ctx := context.Background()
					gate, wt, _, _ := newDirtyWorkGate(t, "credential capture")
					path := filepath.Join(wt, filepath.FromSlash(rel))
					if state == "unstaged" {
						writeFile(t, path, "committed placeholder\n")
						run(t, wt, "git", "add", "--", rel)
						run(t, wt, "git", "commit", "-m", "placeholder")
					}
					body := []byte(fixture.body)
					if err := os.WriteFile(path, body, 0o644); err != nil {
						t.Fatal(err)
					}
					if state == "staged" {
						run(t, wt, "git", "add", "--", rel)
					}
					objectsBefore := run(t, gate, "git", "cat-file", "--batch-all-objects", "--batch-check=%(objectname)")
					snap, err := CaptureDirtyWork(ctx, DirtyWorkRequest{GateDir: gate, Worktree: wt, RunID: dirtyWorkRunID})
					if !errors.Is(err, ErrDirtyWorkRejected) || snap != nil {
						t.Fatal("credential capture was not refused")
					}
					assertNoDirtySnapshot(t, gate)
					if objectsAfter := run(t, gate, "git", "cat-file", "--batch-all-objects", "--batch-check=%(objectname)"); objectsAfter != objectsBefore {
						t.Fatal("refusal wrote snapshot objects")
					}
					if got := mustRead(t, path); !bytes.Equal(got, body) {
						t.Fatal("refusal changed source credential bytes")
					}
					if state != "staged" {
						assertObjectStoreLacks(t, gate, syntheticCredentialMarker)
					} else {
						staged, err := RunRaw(ctx, wt, "show", ":"+rel)
						if err != nil || !bytes.Equal(staged, body) {
							t.Fatal("refusal changed staged credential bytes")
						}
					}
				})
			}
		}
	}
}

func TestWriteDirtySnapshotRejectsCredentialFilesAddedAfterStatus(t *testing.T) {
	for _, fixture := range conventionalCredentialFiles {
		rel := fixture.rel
		t.Run(rel, func(t *testing.T) {
			ctx := context.Background()
			gate, wt, _, _ := newDirtyWorkGate(t, "late credentials")
			writeFile(t, filepath.Join(wt, "file.txt"), "ordinary unique bytes\n")
			entries, err := checkoutDirtyEntries(ctx, wt)
			if err != nil {
				t.Fatal(err)
			}
			body := fixture.body
			path := filepath.Join(wt, rel)
			writeFile(t, path, body)
			run(t, wt, "git", "add", "--", rel)
			writeFile(t, path, "worktree replacement\n")
			objectsBefore := run(t, gate, "git", "cat-file", "--batch-all-objects", "--batch-check=%(objectname)")
			snap, err := writeDirtySnapshot(ctx, DirtyWorkRequest{GateDir: gate, Worktree: wt, RunID: dirtyWorkRunID}, DirtyWorkRef(dirtyWorkRunID), entries)
			if !errors.Is(err, ErrDirtyWorkRejected) || snap != nil {
				t.Fatal("copied-index credential path was not refused")
			}
			assertNoDirtySnapshot(t, gate)
			if objectsAfter := run(t, gate, "git", "cat-file", "--batch-all-objects", "--batch-check=%(objectname)"); objectsAfter != objectsBefore {
				t.Fatal("copied-index refusal wrote snapshot objects")
			}
			if got := string(mustRead(t, path)); got != "worktree replacement\n" {
				t.Fatal("refusal changed working bytes")
			}
			if staged, err := RunRaw(ctx, wt, "show", ":"+rel); err != nil || string(staged) != body {
				t.Fatal("refusal changed copied-index bytes")
			}
		})
	}
}

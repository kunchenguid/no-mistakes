package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	toon "github.com/toon-format/toon-go"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// These fixtures exercise the public commands against real Git repositories.
// Ignored bytes have no recovery ref, so a refusal must preserve them in place.
func TestSyncRefusesIgnoredPathCollisions(t *testing.T) {
	for _, axi := range []bool{false, true} {
		for _, diverged := range []bool{false, true} {
			for _, tc := range []struct {
				name, localKind, incomingKind, localPath, incomingPath string
				ignoreCase                                             bool
			}{
				{"directory-to-symlink", "dir", "symlink", "node_modules", "node_modules", false},
				{"file-to-file", "file", "file", "cache", "cache", false},
				{"symlink-to-file", "symlink", "file", "cache", "cache", false},
				{"directory-to-file", "dir", "file", "cache", "cache", false},
				{"file-prefix", "file", "file", "cache", "cache/new", false},
				{"symlink-prefix", "symlink", "file", "cache", "cache/new", false},
				{"directory-prefix", "dir", "file", "cache/nested", "cache", false},
				{"spaces", "file", "file", "local cache", "local cache", false},
				{"newline", "file", "file", "local\ncache", "local\ncache", false},
				{"case-file-to-file", "file", "file", "notes.txt", "Notes.txt", true},
				{"case-symlink-to-file", "symlink", "file", "Cache", "cache", true},
				{"case-directory-to-file", "dir", "file", "Cache", "cache", true},
				{"case-directory-to-symlink", "dir", "symlink", "Cache", "cache", true},
				{"case-file-prefix", "file", "file", "Cache", "cache/new", true},
				{"case-symlink-prefix", "symlink", "file", "Cache", "cache/new", true},
				{"case-directory-prefix", "dir", "file", "Cache/nested", "cache", true},
				{"case-nested-file", "file", "file", "Cache/Nested/Sentinel", "cache/nested/sentinel", true},
				{"case-nested-symlink", "symlink", "file", "Cache/Nested", "cache/nested/new", true},
				{"case-nested-directory", "dir", "file", "Cache/Nested", "cache/nested", true},
			} {
				t.Run(tc.name+"/axi="+strconv.FormatBool(axi)+"/diverged="+strconv.FormatBool(diverged), func(t *testing.T) {
					if tc.name == "newline" && runtime.GOOS == "windows" {
						t.Skip("Windows does not support newline characters in filenames")
					}
					f := newCLISyncFixture(t)
					cliGit(t, f.local, "config", "core.ignorecase", strconv.FormatBool(tc.ignoreCase))
					publishIgnoredSyncTarget(t, &f, diverged, tc.incomingPath, tc.incomingKind)
					cliWriteIgnored(t, f.local, tc.localPath, tc.localKind)
					subdir := filepath.Join(f.local, "caller-subdir")
					if err := os.MkdirAll(subdir, 0o755); err != nil {
						t.Fatal(err)
					}
					chdir(t, subdir)
					indexPath := filepath.Join(f.local, ".git", "index")
					index, err := os.ReadFile(indexPath)
					if err != nil {
						t.Fatal(err)
					}
					args := []string{"sync", "--yes"}
					if axi {
						args = []string{"axi", "sync"}
					}
					out, err := executeCmd(args...)
					message := out
					if axi {
						var document struct {
							BranchSync struct {
								Error string `toon:"note"`
							} `toon:"branch_sync"`
						}
						if decodeErr := toon.UnmarshalString(out, &document); decodeErr != nil {
							t.Fatalf("decode refusal: %v\n%s", decodeErr, out)
						}
						message = document.BranchSync.Error
					}
					wantPath := tc.localPath
					if strings.HasSuffix(tc.name, "directory-prefix") {
						wantPath = strings.Split(tc.localPath, "/")[0]
					}
					if err == nil || !strings.Contains(message, "ignored local path") || !strings.Contains(message, strconv.Quote(wantPath)) || (axi && !strings.Contains(out, "blocked_ignored_path_collision")) {
						t.Errorf("expected named ignored-path refusal, got %v\n%s", err, out)
					}
					if got := cliGit(t, f.local, "rev-parse", "HEAD"); got != f.old {
						t.Errorf("HEAD changed to %s", got)
					}
					afterIndex, err := os.ReadFile(indexPath)
					if err != nil || !bytes.Equal(index, afterIndex) {
						t.Errorf("index changed: %v", err)
					}
					assertCLIIgnoredSentinel(t, f.local, tc.localPath, tc.localKind)
				})
			}
		}
	}
}

func TestSyncPreservesNoncollidingIgnoredDirectory(t *testing.T) {
	for _, axi := range []bool{false, true} {
		for _, diverged := range []bool{false, true} {
			for _, ignoreCase := range []bool{false, true} {
				t.Run("axi="+strconv.FormatBool(axi)+"/diverged="+strconv.FormatBool(diverged)+"/ignoreCase="+strconv.FormatBool(ignoreCase), func(t *testing.T) {
					f := newCLISyncFixture(t)
					cliGit(t, f.local, "config", "core.ignorecase", strconv.FormatBool(ignoreCase))
					publishIgnoredSyncTarget(t, &f, diverged, "cache/new", "file")
					local := "cache/local"
					if ignoreCase {
						local = "Cache/Local"
					}
					cliWriteIgnored(t, f.local, local, "dir")
					args := []string{"sync", "--yes"}
					if axi {
						args = []string{"axi", "sync"}
					}
					out, err := executeCmd(args...)
					if err != nil {
						t.Fatalf("noncolliding sync: %v\n%s", err, out)
					}
					if got := cliGit(t, f.local, "rev-parse", "HEAD"); got != f.pushed {
						t.Fatalf("HEAD = %s", got)
					}
					assertCLIIgnoredSentinel(t, f.local, local, "dir")
				})
			}
		}
	}
}

func publishIgnoredSyncTarget(t *testing.T, f *cliSyncFixture, diverged bool, name, kind string) {
	t.Helper()
	pipeline := filepath.Join(filepath.Dir(f.local), "pipeline")
	if diverged {
		cliGit(t, pipeline, "checkout", "-B", "feature/sync", f.base)
		if err := os.WriteFile(filepath.Join(pipeline, "file.txt"), []byte("feature\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		cliGit(t, pipeline, "commit", "-am", "feature rebased")
	}
	if err := os.MkdirAll(filepath.Dir(filepath.Join(pipeline, name)), 0o755); err != nil {
		t.Fatal(err)
	}
	if kind == "symlink" {
		if err := os.Symlink(name, filepath.Join(pipeline, name)); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
	} else if err := os.WriteFile(filepath.Join(pipeline, name), []byte("incoming\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cliGit(t, pipeline, "add", "--", name)
	cliGit(t, pipeline, "commit", "-m", "incoming tracked path")
	f.pushed = cliGit(t, pipeline, "rev-parse", "HEAD")
	cliGit(t, pipeline, "push", "--force", f.remote, "HEAD:refs/heads/feature/sync")
	p, err := paths.New()
	if err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if err := d.UpdateRunHeadSHA(f.runID, f.pushed); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunPushBinding(f.runID, db.PushBinding{HeadSHA: f.pushed, TargetKind: "upstream", TargetFingerprint: branchsync.TargetFingerprint(f.remote), Ref: "refs/heads/feature/sync"}); err != nil {
		t.Fatal(err)
	}
}

func cliWriteIgnored(t *testing.T, root, name, kind string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, ".git", "info", "exclude"), []byte("*\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	switch kind {
	case "dir":
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
		path = filepath.Join(path, "sentinel")
	case "symlink":
		if err := os.Symlink("missing-sentinel-target", path); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		return
	}
	if err := os.WriteFile(path, []byte("precious ignored bytes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertCLIIgnoredSentinel(t *testing.T, root, name, kind string) {
	t.Helper()
	path := filepath.Join(root, name)
	if kind == "symlink" {
		got, err := os.Readlink(path)
		if err != nil || got != "missing-sentinel-target" {
			t.Errorf("ignored symlink changed: %q, %v", got, err)
		}
		return
	}
	if kind == "dir" {
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() {
			t.Errorf("ignored directory replaced: %v", err)
			return
		}
		path = filepath.Join(path, "sentinel")
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "precious ignored bytes\n" {
		t.Errorf("ignored bytes changed: %q, %v", got, err)
	}
}

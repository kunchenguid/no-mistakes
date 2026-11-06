package git

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// These entry points may change only the intended bare gate. In particular,
// neither a working repository's .git nor a stale directory inside it is a gate.
func TestGateIdentityRefusesNonGateWithoutMutation(t *testing.T) {
	operations := []struct {
		name string
		call func(context.Context, string) error
	}{
		{"init", InitBare},
		{"isolate", func(ctx context.Context, path string) error {
			_, err := EnsureHooksPathIsolation(ctx, path)
			return err
		}},
	}
	for _, operation := range operations {
		for _, target := range []string{"working-repo", "git-dir", "stale-in-git-dir", "linked-worktree", "linked-git-dir"} {
			t.Run(operation.name+"/"+target, func(t *testing.T) {
				repo := initTestRepo(t)
				writeFile(t, filepath.Join(repo, "README.md"), "tracked changes must survive\n")
				writeFile(t, filepath.Join(repo, "untracked"), "untracked content must survive\n")
				path := repo
				switch target {
				case "git-dir":
					path = filepath.Join(repo, ".git")
				case "stale-in-git-dir":
					path = filepath.Join(repo, ".git", "stale-home", "cache")
					if err := os.MkdirAll(path, 0o755); err != nil {
						t.Fatal(err)
					}
					writeFile(t, filepath.Join(path, "keep"), "not a repository\n")
				case "linked-worktree", "linked-git-dir":
					path = filepath.Join(repo, "linked")
					run(t, repo, "git", "worktree", "add", "-b", "linked", path)
					writeFile(t, filepath.Join(path, "untracked"), "linked content\n")
					if target == "linked-git-dir" {
						path = run(t, path, "git", "rev-parse", "--absolute-git-dir")
					}
				}
				resolved := run(t, path, "git", "rev-parse", "--absolute-git-dir")
				before := snapshotGateIdentityFixture(t, repo)
				err := operation.call(context.Background(), path)
				if err == nil {
					t.Error("accepted a non-gate target")
				} else {
					for _, want := range []string{fmt.Sprintf("intended %q", path), fmt.Sprintf("resolved %q", resolved)} {
						if !strings.Contains(err.Error(), want) {
							t.Errorf("refusal %q does not name path %q", err, want)
						}
					}
				}
				// Compare the whole fixture, including config bytes, HEAD, refs,
				// index, and tracked/untracked files, without asking Git to read
				// a repository the buggy implementation may have corrupted.
				if after := snapshotGateIdentityFixture(t, repo); !reflect.DeepEqual(before, after) {
					t.Error("refused target changed repository or worktree state")
				}
			})
		}
	}
}

func TestGateIdentityBarePathsRemainIdempotent(t *testing.T) {
	for _, form := range []string{"absolute", "empty-directory", "relative", "symlink", "symlink-parent"} {
		t.Run(form, func(t *testing.T) {
			base := t.TempDir()
			bare := filepath.Join(base, "gate.git")
			path := bare
			switch form {
			case "empty-directory":
				if err := os.MkdirAll(bare, 0o755); err != nil {
					t.Fatal(err)
				}
			case "relative":
				// The checkout and TempDir may be on different Windows drives.
				// Anchor relative discovery inside the fixture's own directory.
				t.Chdir(base)
				path = "gate.git"
			case "symlink-parent":
				alias := filepath.Join(t.TempDir(), "alias")
				if err := os.Symlink(base, alias); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
				path = filepath.Join(alias, "gate.git")
			case "symlink":
				run(t, base, "git", "init", "--bare", bare)
				path = filepath.Join(base, "alias.git")
				if err := os.Symlink(bare, path); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
			}
			ctx := context.Background()
			// Hardened Git must also accept a correctly identified bare gate.
			t.Setenv("GIT_CONFIG_COUNT", "1")
			t.Setenv("GIT_CONFIG_KEY_0", "safe.bareRepository")
			t.Setenv("GIT_CONFIG_VALUE_0", "explicit")
			for i := 0; i < 2; i++ {
				if err := InitBare(ctx, path); err != nil {
					t.Fatalf("init %d: %v", i, err)
				}
			}
			for i := 0; i < 2; i++ {
				isolated, err := EnsureHooksPathIsolation(ctx, path)
				if err != nil || !isolated {
					t.Fatalf("isolate %d: isolated=%v, err=%v", i, isolated, err)
				}
				if got := run(t, base, "git", "--git-dir="+bare, "rev-parse", "--is-bare-repository"); got != "true" {
					t.Fatalf("bare classification = %q", got)
				}
				if got := run(t, base, "git", "--git-dir="+bare, "config", "--worktree", "--get", "core.bare"); got != "true" {
					t.Fatalf("per-worktree core.bare = %q", got)
				}
			}
			before := snapshotGateIdentityFixture(t, bare)
			if _, err := EnsureHooksPathIsolation(ctx, path); err != nil {
				t.Fatal(err)
			}
			if after := snapshotGateIdentityFixture(t, bare); !reflect.DeepEqual(before, after) {
				t.Error("repeated isolation changed the bare gate")
			}
		})
	}
}

type gateIdentityFile struct {
	Mode fs.FileMode
	Data string
}

func snapshotGateIdentityFixture(t *testing.T, root string) map[string]gateIdentityFile {
	t.Helper()
	files := make(map[string]gateIdentityFile)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		var data []byte
		if info.Mode().IsRegular() {
			data, err = os.ReadFile(path)
		} else if info.Mode()&os.ModeSymlink != 0 {
			var target string
			target, err = os.Readlink(path)
			data = []byte(target)
		}
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		files[rel] = gateIdentityFile{Mode: info.Mode(), Data: string(data)}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/gate"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

func swapCaseForTest(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r - 'a' + 'A'
		case r >= 'A' && r <= 'Z':
			return r - 'A' + 'a'
		}
		return r
	}, s)
}

func sameDirForTest(a, b string) bool {
	infoA, err := os.Stat(a)
	if err != nil {
		return false
	}
	infoB, err := os.Stat(b)
	return err == nil && os.SameFile(infoA, infoB)
}

// Issue #1465 acceptance: a checkout registered under another spelling of its
// path is still one repository when looked up from the physical main checkout
// and from a linked worktree, and a second init from the real path refreshes it
// instead of registering another.
func TestFindRepoResolvesOneRepositoryFromMainCheckoutAndLinkedWorktree(t *testing.T) {
	for _, tc := range []struct {
		name  string
		alias func(t *testing.T, root string) string
	}{
		{"symlink alias", func(t *testing.T, root string) string {
			alias := filepath.Join(t.TempDir(), "alias")
			if err := os.Symlink(root, alias); err != nil {
				t.Skipf("cannot create a symlink here: %v", err)
			}
			return alias
		}},
		{"case variant", func(t *testing.T, root string) string {
			variant := swapCaseForTest(root)
			if variant == root || !sameDirForTest(variant, root) {
				t.Skip("filesystem is case-sensitive: a case variant is a different directory here")
			}
			return variant
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repoDir := setupTestRepo(t)
			root, err := filepath.EvalSymlinks(repoDir)
			if err != nil {
				t.Fatal(err)
			}
			p := paths.WithRoot(os.Getenv("NM_HOME"))
			d, err := db.Open(p.DB())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = d.Close() })

			registered, created, err := gate.Init(context.Background(), d, p, root)
			if err != nil || !created {
				t.Fatalf("init = %+v, created %v, err %v", registered, created, err)
			}
			if _, err := d.UpdateRepoWorkingPath(registered.ID, tc.alias(t, root)); err != nil {
				t.Fatal(err)
			}

			worktree := filepath.Join(t.TempDir(), "linked")
			run(t, root, "git", "worktree", "add", "-b", "linked-branch", worktree)

			for name, dir := range map[string]string{"main checkout": root, "linked worktree": worktree} {
				if err := os.Chdir(dir); err != nil {
					t.Fatal(err)
				}
				got, err := findRepo(d)
				if err != nil {
					t.Fatalf("findRepo from the %s: %v", name, err)
				}
				if got.ID != registered.ID {
					t.Fatalf("findRepo from the %s = repo %s, want the registered %s", name, got.ID, registered.ID)
				}
			}

			again, created, err := gate.Init(context.Background(), d, p, worktree)
			if err != nil {
				t.Fatalf("init from the linked worktree: %v", err)
			}
			if created || again.ID != registered.ID {
				t.Fatalf("init from the linked worktree = repo %s, created %v; want %s refreshed", again.ID, created, registered.ID)
			}
			if repos, err := d.GetRepos(); err != nil || len(repos) != 1 {
				t.Fatalf("registered repositories = %d (%v), want one", len(repos), err)
			}
		})
	}
}

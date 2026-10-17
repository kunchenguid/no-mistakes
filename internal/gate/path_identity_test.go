package gate

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// swapCase flips the letter case of every ASCII letter in s.
func swapCase(s string) string {
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

// sameDirectory reports whether a and b are the one directory on disk.
func sameDirectory(a, b string) bool {
	infoA, err := os.Stat(a)
	if err != nil {
		return false
	}
	infoB, err := os.Stat(b)
	return err == nil && os.SameFile(infoA, infoB)
}

// Issue #1465: one checkout is one repository identity however its path is
// spelled. A second init from a case variant (case-insensitive volume) or from
// a path whose registered spelling is a symlink alias refreshes the existing
// gate; before, it registered a second repository, or failed on the existing
// row and tore down the gate it had just reused.
func TestInitFromAnAlternateSpellingRefreshesTheRegisteredGate(t *testing.T) {
	for _, tc := range []struct {
		name string
		// register records the checkout under a spelling other than the one
		// init resolves, and returns the spelling init is then invoked with.
		register func(t *testing.T, workDir string) (initFrom string)
	}{
		{"case variant", func(t *testing.T, workDir string) string {
			variant := swapCase(workDir)
			if variant == workDir || !sameDirectory(variant, workDir) {
				t.Skip("filesystem is case-sensitive: a case variant is a different directory here")
			}
			return variant
		}},
		{"symlink alias", func(t *testing.T, workDir string) string {
			alias := filepath.Join(t.TempDir(), "alias")
			if err := os.Symlink(workDir, alias); err != nil {
				t.Skipf("cannot create a symlink here: %v", err)
			}
			return alias
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workDir := setupTestRepo(t)
			p := paths.WithRoot(t.TempDir())
			if err := p.EnsureDirs(); err != nil {
				t.Fatal(err)
			}
			d := openTestDB(t, p)
			ctx := context.Background()

			first, created, err := Init(ctx, d, p, workDir)
			if err != nil || !created {
				t.Fatalf("first init = %+v, created %v, err %v", first, created, err)
			}
			// Record the registration under the alternate spelling, as a row
			// written by an older version or from another invocation would be.
			alternate := tc.register(t, workDir)
			if _, err := d.UpdateRepoWorkingPath(first.ID, alternate); err != nil {
				t.Fatal(err)
			}

			second, created, err := Init(ctx, d, p, workDir)
			if err != nil {
				t.Fatalf("init of the registered checkout failed: %v", err)
			}
			if created || second.ID != first.ID {
				t.Fatalf("init = id %s, created %v; want the existing repository %s refreshed", second.ID, created, first.ID)
			}
			repos, err := d.GetRepos()
			if err != nil {
				t.Fatal(err)
			}
			if len(repos) != 1 {
				t.Fatalf("registered repositories = %d, want exactly one", len(repos))
			}
			if _, err := os.Stat(p.RepoDir(first.ID)); err != nil {
				t.Fatalf("the registered gate was lost: %v", err)
			}
		})
	}
}

// On a case-sensitive volume two directories whose names differ only in case
// are different checkouts: initializing both must register two repositories,
// and neither lookup may resolve to the other.
func TestInitKeepsCaseDistinctCheckoutsApartOnACaseSensitiveFilesystem(t *testing.T) {
	workDir := setupTestRepo(t)
	sibling := filepath.Join(filepath.Dir(workDir), swapCase(filepath.Base(workDir)))
	if sibling == workDir || sameDirectory(sibling, workDir) {
		t.Skip("filesystem folds case: the variant is the same directory here")
	}
	if out, err := exec.Command("git", "init", sibling).CombinedOutput(); err != nil {
		t.Fatalf("init sibling: %v: %s", err, out)
	}
	origin, err := exec.Command("git", "-C", workDir, "remote", "get-url", "origin").Output()
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "-C", sibling, "remote", "add", "origin", strings.TrimSpace(string(origin))).CombinedOutput(); err != nil {
		t.Fatalf("add origin: %v: %s", err, out)
	}

	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d := openTestDB(t, p)
	ctx := context.Background()

	first, created, err := Init(ctx, d, p, workDir)
	if err != nil || !created {
		t.Fatalf("init %s = %+v, created %v, err %v", workDir, first, created, err)
	}
	second, created, err := Init(ctx, d, p, sibling)
	if err != nil || !created {
		t.Fatalf("init %s = %+v, created %v, err %v; a case-distinct checkout is a new repository", sibling, second, created, err)
	}
	if second.ID == first.ID {
		t.Fatalf("both checkouts resolved to repository %s", first.ID)
	}
	if repos, err := d.GetRepos(); err != nil || len(repos) != 2 {
		t.Fatalf("registered repositories = %d (%v), want two", len(repos), err)
	}
	for path, want := range map[string]string{workDir: first.ID, sibling: second.ID} {
		if got, err := d.GetRepoByPath(path); err != nil || got == nil || got.ID != want {
			t.Fatalf("GetRepoByPath(%q) = %+v, %v; want %s", path, got, err, want)
		}
	}
}

// The direct form of the same report: init from the checkout's case variant
// after it was registered under its own spelling reuses that registration and
// keeps the recorded path stable.
func TestInitFromACaseVariantReusesTheRegisteredRepository(t *testing.T) {
	workDir := setupTestRepo(t)
	variant := swapCase(workDir)
	if variant == workDir || !sameDirectory(variant, workDir) {
		t.Skip("filesystem is case-sensitive: a case variant is a different directory here")
	}
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d := openTestDB(t, p)
	ctx := context.Background()

	first, _, err := Init(ctx, d, p, workDir)
	if err != nil {
		t.Fatal(err)
	}
	second, created, err := Init(ctx, d, p, variant)
	if err != nil {
		t.Fatalf("init from the case variant failed: %v", err)
	}
	if created || second.ID != first.ID {
		t.Fatalf("init from the case variant = id %s, created %v; want the existing repository %s", second.ID, created, first.ID)
	}
	if second.WorkingPath != first.WorkingPath {
		t.Fatalf("recorded path moved from %q to %q", first.WorkingPath, second.WorkingPath)
	}
	if repos, err := d.GetRepos(); err != nil || len(repos) != 1 {
		t.Fatalf("registered repositories = %d (%v), want exactly one", len(repos), err)
	}
}

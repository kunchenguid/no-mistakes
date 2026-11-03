package db

import (
	"os"
	"path/filepath"
	"testing"
)

func symlinkTo(t *testing.T, target string) string {
	t.Helper()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(target, alias); err != nil {
		t.Skipf("cannot create a symlink here: %v", err)
	}
	return alias
}

// Issue #1465: the registered spelling of a checkout and the spelling a command
// resolves from differ, yet name one directory, and lookup must find the one
// repository either way.
func TestRepoGetByPathResolvesASymlinkSpellingOfTheRegisteredDirectory(t *testing.T) {
	d := openTestDB(t)
	dir := t.TempDir()
	alias := symlinkTo(t, dir)

	repo, err := d.InsertRepo(dir, "git@github.com:user/project.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	got, err := d.GetRepoByPath(alias)
	if err != nil || got == nil || got.ID != repo.ID {
		t.Fatalf("GetRepoByPath(symlink to the registered directory) = %+v, %v; want repo %s", got, err, repo.ID)
	}

	// And the other way round: registered through the alias, found by the real path.
	other := t.TempDir()
	aliased, err := d.InsertRepo(symlinkTo(t, other), "git@github.com:user/other.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	got, err = d.GetRepoByPath(other)
	if err != nil || got == nil || got.ID != aliased.ID {
		t.Fatalf("GetRepoByPath(real path of a repo registered by alias) = %+v, %v; want repo %s", got, err, aliased.ID)
	}
}

func TestRepoGetByPathResolvesACaseVariantOnACaseInsensitiveFilesystem(t *testing.T) {
	d := openTestDB(t)
	base := t.TempDir()
	dir := filepath.Join(base, "Project")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	variant := filepath.Join(base, "pROJECT")

	repo, err := d.InsertRepo(dir, "git@github.com:user/project.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	got, err := d.GetRepoByPath(variant)
	if err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(variant); statErr != nil {
		// Case-sensitive volume: the variant is not a directory, and a path
		// that does not exist must not resolve to anything.
		if got != nil {
			t.Fatalf("GetRepoByPath(%q) = %+v, want nil for a path that does not exist", variant, got)
		}
		if err := os.Mkdir(variant, 0o755); err != nil {
			t.Fatal(err)
		}
		got, err = d.GetRepoByPath(variant)
		if err != nil || got != nil {
			t.Fatalf("GetRepoByPath(%q) = %+v, %v; a case-distinct directory is a different checkout and must not resolve to %s", variant, got, err, repo.ID)
		}
		return
	}
	if got == nil || got.ID != repo.ID {
		t.Fatalf("GetRepoByPath(%q) = %+v; want repo %s: the case variant is the registered directory", variant, got, repo.ID)
	}
}

// Identity, not shared ancestry: a sibling directory is a different checkout,
// and a registered path that no longer exists matches nothing.
func TestRepoGetByPathDoesNotConflateDifferentDirectories(t *testing.T) {
	d := openTestDB(t)
	registered, sibling := t.TempDir(), t.TempDir()
	if _, err := d.InsertRepo(registered, "git@github.com:user/project.git", "main"); err != nil {
		t.Fatal(err)
	}
	gone := filepath.Join(t.TempDir(), "gone")
	if _, err := d.InsertRepo(gone, "git@github.com:user/gone.git", "main"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{sibling, filepath.Join(registered, "nested"), "", filepath.Join(t.TempDir(), "missing")} {
		got, err := d.GetRepoByPath(path)
		if err != nil || got != nil {
			t.Fatalf("GetRepoByPath(%q) = %+v, %v; want no repository", path, got, err)
		}
	}
}

// An exact match is always preferred, and rows an older version registered
// under several spellings of one directory resolve to the oldest, not to
// whichever sorts first by id. Insertion order breaks a tie in created_at,
// which is only second resolution.
func TestRepoGetByPathPrefersTheExactRowThenTheOldest(t *testing.T) {
	d := openTestDB(t)
	dir := t.TempDir()
	aliasA, aliasB, aliasC := symlinkTo(t, dir), symlinkTo(t, dir), symlinkTo(t, dir)

	// The ids sort against the insertion order on purpose.
	first, err := d.InsertRepoWithID("zzz-first", aliasA, "git@github.com:user/a.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	second, err := d.InsertRepoWithID("aaa-second", aliasB, "git@github.com:user/b.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.sql.Exec(`UPDATE repos SET created_at = ?`, first.CreatedAt); err != nil {
		t.Fatal(err)
	}

	if got, err := d.GetRepoByPath(aliasB); err != nil || got == nil || got.ID != second.ID {
		t.Fatalf("an exact path match must win: got %+v, %v; want %s", got, err, second.ID)
	}
	if got, err := d.GetRepoByPath(aliasC); err != nil || got == nil || got.ID != first.ID {
		t.Fatalf("with equal created_at the first inserted row must win: got %+v, %v; want %s", got, err, first.ID)
	}

	// A strictly older row wins regardless of insertion order.
	if _, err := d.sql.Exec(`UPDATE repos SET created_at = created_at - 100 WHERE id = ?`, second.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := d.GetRepoByPath(aliasC); err != nil || got == nil || got.ID != second.ID {
		t.Fatalf("the older row must win: got %+v, %v; want %s", got, err, second.ID)
	}
}

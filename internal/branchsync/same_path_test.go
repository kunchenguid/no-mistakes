package branchsync

import (
	"os"
	"path/filepath"
	"testing"
)

// samePath decides whether the invoking worktree belongs to the registered
// repository. Spellings of one directory must agree, whatever the volume's case
// rule, and different directories must not.
func TestSamePathFollowsFilesystemIdentity(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "Project")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	sibling := filepath.Join(base, "Other")
	if err := os.Mkdir(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	if !samePath(dir, dir) {
		t.Fatal("a path must equal itself")
	}
	if samePath(dir, sibling) {
		t.Fatal("different directories must not match")
	}
	if samePath(dir, filepath.Join(base, "missing")) {
		t.Fatal("a path that does not exist must not match")
	}

	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(dir, alias); err == nil && !samePath(dir, alias) {
		t.Fatal("a symlink to the directory must match it")
	}

	variant := filepath.Join(base, "pROJECT")
	_, statErr := os.Stat(variant)
	if got := samePath(dir, variant); got != (statErr == nil) {
		t.Fatalf("samePath(%q, %q) = %v, but the filesystem says the variant exists = %v", dir, variant, got, statErr == nil)
	}
}

package worktrees_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/worktrees"
)

func TestLayout_DefaultPlacement(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	checkout := filepath.Join(t.TempDir(), "src", "repo1")
	layout := worktrees.New(p, nil)

	if got, want := layout.Dir("repo1", checkout, "run1"), p.WorktreeDir("repo1", "run1"); got != want {
		t.Errorf("Dir() = %q, want %q", got, want)
	}
	if got, want := layout.RepoDir("repo1", checkout), filepath.Join(p.WorktreesDir(), "repo1"); got != want {
		t.Errorf("RepoDir() = %q, want %q", got, want)
	}
	if root, ok := layout.CustomRoot(checkout); ok {
		t.Errorf("CustomRoot() = %q, true; want no configured root", root)
	}
}

// A repository with no entry keeps the default placement even when another
// repository has one.
func TestLayout_ConfiguredPlacement(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	dir := t.TempDir()
	checkout := filepath.Join(dir, "src", "repo1")
	configured := filepath.Join(dir, "work", "repo1-runs")
	layout := worktrees.New(p, map[string]string{checkout: configured})

	if got, want := layout.Dir("repo1", checkout, "run1"), filepath.Join(configured, "run1"); got != want {
		t.Errorf("Dir() = %q, want %q", got, want)
	}
	if got, want := layout.RepoDir("repo1", checkout), configured; got != want {
		t.Errorf("RepoDir() = %q, want %q", got, want)
	}
	other := filepath.Join(dir, "src", "repo2")
	if got, want := layout.Dir("repo2", other, "run2"), p.WorktreeDir("repo2", "run2"); got != want {
		t.Errorf("unconfigured repository Dir() = %q, want %q", got, want)
	}
	root, ok := layout.CustomRoot(checkout)
	if !ok || root != configured {
		t.Errorf("CustomRoot() = %q, %v; want %q, true", root, ok, configured)
	}
	if got := layout.Checkouts(); len(got) != 1 || got[0] != worktrees.Canonical(checkout) {
		t.Errorf("Checkouts() = %v, want [%q]", got, worktrees.Canonical(checkout))
	}
}

// The config key and the recorded checkout path are written by different
// people at different times, so they must match after normalization rather
// than byte for byte.
func TestLayout_MatchesUnnormalizedAndSymlinkedCheckout(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	dir := t.TempDir()
	checkout := filepath.Join(dir, "src", "repo1")
	if err := os.MkdirAll(checkout, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(checkout, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	configured := filepath.Join(dir, "work", "repo1-runs")
	layout := worktrees.New(p, map[string]string{filepath.Join(checkout, "."): configured})

	for _, spelling := range []string{checkout, link, checkout + string(filepath.Separator)} {
		if got, want := layout.Dir("repo1", spelling, "run1"), filepath.Join(configured, "run1"); got != want {
			t.Errorf("Dir(%q) = %q, want %q", spelling, got, want)
		}
	}
}

// Contains is what recognizes the pathological placements: a root inside the
// checkout it serves, or inside the directory no-mistakes already owns.
func TestContains(t *testing.T) {
	dir := t.TempDir()
	inside := filepath.Join(dir, "runs")
	if !worktrees.Contains(dir, inside) {
		t.Errorf("Contains(%q, %q) = false, want true", dir, inside)
	}
	if !worktrees.Contains(dir, dir) {
		t.Errorf("Contains(%q, %q) = false, want true for the directory itself", dir, dir)
	}
	sibling := filepath.Join(filepath.Dir(dir), filepath.Base(dir)+"-sibling")
	if worktrees.Contains(dir, sibling) {
		t.Errorf("Contains(%q, %q) = true, want false", dir, sibling)
	}
}

// Everything walked in an operator-owned root is filtered through IsRunID, so
// this is the boundary that protects their own files there.
func TestIsRunID(t *testing.T) {
	cases := map[string]bool{
		"01JZ8XQ7V6K9M3B0T5N2R4C8YD": true,
		"mise.local.toml":            false,
		"scratch":                    false,
		"":                           false,
		".envrc":                     false,
		"01JZ8XQ7V6K9M3B0T5N2R4C8Y":  false, // one character short of a ULID
		"01jz8xq7v6k9m3b0t5n2r4c8yd": false, // parses as a ULID, but no ID we mint is lowercase
	}
	for name, want := range cases {
		if got := worktrees.IsRunID(name); got != want {
			t.Errorf("IsRunID(%q) = %v, want %v", name, got, want)
		}
	}
}

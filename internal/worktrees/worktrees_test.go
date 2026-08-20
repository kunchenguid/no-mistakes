package worktrees_test

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/worktrees"
)

// namesPath reports whether a refusal identifies path to the operator who has
// to repair it. Two spellings of one directory both count, and so does the
// quoted rendering:
//
// A refusal writes paths with %q, which escapes the separator, so a Windows
// path appears as "C:\\src\\repo" and never contains its own raw spelling.
// And a refusal names either the spelling the configuration carries or its
// canonical form, which differ wherever the filesystem has a second name for a
// directory - the macOS /var -> /private/var symlink, and the 8.3 short names
// Windows keeps for the temporary directories these tests run in.
func namesPath(err error, path string) bool {
	msg := err.Error()
	for _, spelling := range []string{path, worktrees.Canonical(path)} {
		if strings.Contains(msg, spelling) || strings.Contains(msg, strconv.Quote(spelling)) {
			return true
		}
	}
	return false
}

func TestLayout_DefaultPlacement(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	checkout := filepath.Join(t.TempDir(), "src", "repo1")
	layout := worktrees.New(p, nil)

	if got, want := layout.Dir("repo1", checkout, "run1"), p.WorktreeDir("repo1", "run1"); got != want {
		t.Errorf("Dir() = %q, want %q", got, want)
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

// RecordedDir is what makes a worktree_roots edit inert for runs that already
// exist: the directory a run was created in is recorded with the run, so every
// later consumer keeps addressing it, and no configuration is consulted at all.
func TestRecordedDirReadsBackWhatTheRunRecorded(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	created := filepath.Join(t.TempDir(), "work", "repo1-runs", "run1")

	if got := worktrees.RecordedDir(p, created, "repo1", "run1"); got != created {
		t.Errorf("RecordedDir() = %q, want the recorded %q", got, created)
	}
}

// A run that recorded nothing predates the column, and the column shipped with
// worktree_roots, so such a run can only have run in the default tree. Resolving
// it through a configured root would be resolving it to a directory it never
// used - and adding an entry for the checkout is exactly what `init
// --worktree-root` invites an operator to do right after upgrading.
func TestRecordedDirResolvesUnrecordedRunsToTheDefaultTree(t *testing.T) {
	p := paths.WithRoot(t.TempDir())

	for _, recorded := range []string{"", "  "} {
		if got, want := worktrees.RecordedDir(p, recorded, "repo1", "run1"), p.WorktreeDir("repo1", "run1"); got != want {
			t.Errorf("RecordedDir(%q) = %q, want the default placement %q", recorded, got, want)
		}
	}
}

// Validate is the layer internal/config cannot be: it knows where NM_HOME is,
// and every placement inside it collides with the daemon's own state. Under
// worktrees a run worktree is indistinguishable from the per-repository
// directories the default placement owns and sweeps; under logs a run's
// worktree IS its log directory (paths.RunLogDir), so removing the worktree
// would destroy the logs of the run that just finished.
func TestLayout_ValidateRefusesRootInsideAppState(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	checkout := filepath.Join(t.TempDir(), "src", "repo1")

	for name, root := range map[string]string{
		"the worktrees directory itself": p.WorktreesDir(),
		"a directory inside it":          filepath.Join(p.WorktreesDir(), "my-runs"),
		"a per-repository directory":     filepath.Join(p.WorktreesDir(), "repo1"),
		"the run log directory":          p.LogsDir(),
		"the gates directory":            p.ReposDir(),
		"NM_HOME itself":                 p.Root(),
	} {
		err := worktrees.New(p, map[string]string{checkout: root}).Validate()
		if err == nil {
			t.Errorf("%s (%q) was accepted; want a refusal", name, root)
			continue
		}
		if !namesPath(err, root) || !strings.Contains(err.Error(), "worktree_roots") {
			t.Errorf("%s: refusal %q names neither the setting nor the root", name, err)
		}
	}
}

// A root inside the checkout whose runs it holds is refused for a different
// reason: the run worktree is then an untracked directory in that checkout, so
// the checkout is dirty for as long as a run executes and guarded branch
// synchronization refuses to move it. `init --worktree-root` already refuses
// this; a hand-written config entry reaches the same rule here.
func TestLayout_ValidateRefusesRootInsideItsCheckout(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	checkout := filepath.Join(t.TempDir(), "src", "repo1")

	for _, root := range []string{
		filepath.Join(checkout, "runs"),
		filepath.Join(checkout, ".no-mistakes", "runs"),
	} {
		err := worktrees.New(p, map[string]string{checkout: root}).Validate()
		if err == nil {
			t.Errorf("root %q inside its own checkout was accepted; want a refusal", root)
			continue
		}
		if !namesPath(err, root) || !strings.Contains(err.Error(), "worktree_roots") {
			t.Errorf("refusal %q names neither the setting nor the root", err)
		}
	}
	// A sibling of the checkout is the normal case and must stay accepted.
	sibling := filepath.Join(filepath.Dir(checkout), "repo1-runs")
	if err := worktrees.New(p, map[string]string{checkout: sibling}).Validate(); err != nil {
		t.Errorf("root next to the checkout rejected: %v", err)
	}
}

// The same dirty-checkout consequence applies to a checkout that is not the one
// whose runs land there, so a root inside ANY known checkout is refused: every
// run placed there would block that checkout's branch synchronization, with
// nothing naming the cause. Known checkouts are the configured keys plus
// whatever the caller supplies (the daemon supplies every registered
// repository).
func TestLayout_ValidateRefusesRootInsideAnotherKnownCheckout(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	dir := t.TempDir()
	own := filepath.Join(dir, "src", "own")
	other := filepath.Join(dir, "src", "other")
	root := filepath.Join(other, "runs")

	// Known only because the other checkout also has an entry.
	both := worktrees.New(p, map[string]string{
		own:   root,
		other: filepath.Join(dir, "work", "other-runs"),
	})
	err := both.Validate()
	if err == nil {
		t.Fatalf("root %q inside the configured checkout %q was accepted; want a refusal", root, other)
	}
	if !namesPath(err, root) || !namesPath(err, other) {
		t.Errorf("refusal %q does not name both the root and the checkout it would dirty", err)
	}

	// Known only because the caller supplied it, which is how a registered
	// repository with no entry of its own reaches this rule.
	configured := worktrees.New(p, map[string]string{own: root})
	if err := configured.Validate(); err != nil {
		t.Fatalf("unknown checkout must not be judged: %v", err)
	}
	if err := configured.Validate(other); err == nil {
		t.Errorf("root %q inside the registered checkout %q was accepted; want a refusal", root, other)
	}
}

func TestLayout_ValidateAcceptsPlacementOutsideAppState(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	dir := t.TempDir()
	checkout := filepath.Join(dir, "src", "repo1")

	if err := worktrees.New(p, nil).Validate(); err != nil {
		t.Errorf("default placement rejected: %v", err)
	}
	layout := worktrees.New(p, map[string]string{checkout: filepath.Join(dir, "work", "repo1-runs")})
	if err := layout.Validate(); err != nil {
		t.Errorf("placement outside NM_HOME rejected: %v", err)
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

// Contains has to agree with the filesystem, not with spelling: on a
// case-insensitive volume - the macOS and Windows default - a differently-cased
// ancestor names the same directory, and a guard that compares names would wave
// through a worktree root that then collides with the daemon's own state (a run
// worktree that IS paths.RunLogDir, so removing it takes the run's logs).
func TestContainsFollowsTheFilesystemAcrossCaseVariantSpellings(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "nm-home")
	if err := os.MkdirAll(filepath.Join(home, "logs"), 0o755); err != nil {
		t.Fatal(err)
	}
	variantHome := filepath.Join(base, strings.ToUpper("nm-home"))
	variantChild := filepath.Join(variantHome, "logs")

	// Whether the two spellings are one directory is the volume's answer, not
	// ours, so the expectation comes from the volume.
	sameDirectory := false
	if a, err := os.Stat(home); err == nil {
		if b, err := os.Stat(variantHome); err == nil {
			sameDirectory = os.SameFile(a, b)
		}
	}

	if got := worktrees.Contains(home, variantChild); got != sameDirectory {
		t.Errorf("Contains(%q, %q) = %v, want %v (the volume reports the two spellings as the same directory: %v)",
			home, variantChild, got, sameDirectory, sameDirectory)
	}
	// A genuinely different sibling stays outside on every volume.
	if worktrees.Contains(home, filepath.Join(base, "nm-home-elsewhere", "logs")) {
		t.Error("a sibling directory was reported as inside NM_HOME")
	}
}

// The placement guard is what that agreement protects: a root under NM_HOME must
// be refused however the operator spelled the path to it.
func TestCheckPlacementRefusesCaseVariantAppStateRoot(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "nm-home")
	if err := os.MkdirAll(filepath.Join(home, "logs"), 0o755); err != nil {
		t.Fatal(err)
	}
	variantRoot := filepath.Join(base, strings.ToUpper("nm-home"), "logs")

	sameDirectory := false
	if a, err := os.Stat(home); err == nil {
		if b, err := os.Stat(filepath.Dir(variantRoot)); err == nil {
			sameDirectory = os.SameFile(a, b)
		}
	}
	if !sameDirectory {
		t.Skip("case-sensitive volume: the variant spelling is a genuinely different directory")
	}

	p := paths.WithRoot(home)
	checkout := filepath.Join(base, "src", "repo1")
	err := worktrees.CheckPlacement(p, checkout, variantRoot)
	if err == nil {
		t.Fatalf("root %q was accepted; it is NM_HOME's own log directory on this volume", variantRoot)
	}
	if !namesPath(err, variantRoot) {
		t.Errorf("refusal %q does not name the offending root", err)
	}
}

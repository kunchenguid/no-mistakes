// Package worktrees resolves where a repository's pipeline run worktrees
// live.
//
// By default a run worktree is created at <NM_HOME>/worktrees/<repoID>/<runID>,
// which is deliberately outside every checkout. That placement defeats
// directory-scoped toolchain configuration: mise, direnv and friends resolve
// their settings by path ancestry, so a worktree under NM_HOME inherits none
// of the configuration the operator applied to the directory their checkouts
// live in. The worktree_roots map in the global config lets an operator name
// the directory a repository's run worktrees are created in, and this package
// is the single seam every consumer of a worktree path goes through, so
// placement is decided in exactly one place.
//
// The package sits below internal/config, which validates worktree_roots (see
// ValidateWorktreeRoots there) before any layout is built from it.
package worktrees

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// Layout maps a repository to the directory holding its run worktrees.
type Layout struct {
	paths *paths.Paths
	// roots is keyed by the canonical form of a registered checkout path so a
	// symlinked or unnormalized config key still matches the recorded
	// repository path.
	roots map[string]string
}

// New returns the layout described by roots, a validated worktree_roots map
// keyed by registered checkout path. A nil or empty map yields the default
// layout. Validation has already rejected keys that collide once canonicalized,
// so this is a plain mapping with no conflict to resolve.
func New(p *paths.Paths, roots map[string]string) *Layout {
	l := &Layout{paths: p}
	if len(roots) == 0 {
		return l
	}
	l.roots = make(map[string]string, len(roots))
	for checkout, root := range roots {
		l.roots[Canonical(checkout)] = filepath.Clean(root)
	}
	return l
}

// Dir resolves where a NEW run's worktree belongs. It is the only placement
// decision this package makes from configuration, and it is made once, at run
// creation: the resolved directory is recorded on the run (runs.worktree_dir)
// and every later consumer reads it back through RecordedDir, so editing
// worktree_roots while a run is in flight cannot retarget that run.
func (l *Layout) Dir(repoID, workingPath, runID string) string {
	if root, ok := l.CustomRoot(workingPath); ok {
		return filepath.Join(root, runID)
	}
	return l.paths.WorktreeDir(repoID, runID)
}

// RecordedDir returns the worktree directory of an EXISTING run, and needs no
// configuration to do it.
//
// recorded is the run's runs.worktree_dir: the placement resolved by Dir when
// the run was created. Reading it back rather than re-deriving it is what makes
// an edit to worktree_roots inert for runs that already exist - resume, step
// diff, startup cleanup, process reaping and eject all keep addressing the
// directory the run was actually created in, so a mid-flight edit can neither
// strand a parked run nor point a removal at a path it never created.
//
// An empty value is the default placement, never a configured root. Such a row
// was written before this column existed, and the column shipped with
// worktree_roots itself, so a run that recorded nothing predates the setting and
// can only have run under <NM_HOME>/worktrees. Consulting the current
// configuration for it would send resume and cleanup to a directory that run
// never used, and adding an entry for the checkout would be enough to strand a
// parked run upgraded across that boundary.
//
// Because neither branch reads configuration, a caller that only needs to find
// an existing run's worktree keeps working while the global config is
// unreadable.
func RecordedDir(p *paths.Paths, recorded, repoID, runID string) string {
	if trimmed := strings.TrimSpace(recorded); trimmed != "" {
		return filepath.Clean(trimmed)
	}
	return p.WorktreeDir(repoID, runID)
}

// CheckPlacement reports why root cannot hold the run worktrees of the
// repository checked out at checkout, or nil when it can. It is the one owner of
// that policy, so `init --worktree-root` refuses exactly what the daemon refuses
// to start on: a divergence there would print an entry that stops the daemon
// once pasted, and every command starts the daemon.
//
// internal/config validates every worktree_roots entry it can judge on its own
// (absolute paths, two checkouts sharing a root, a root equal to its checkout),
// but it never learns where NM_HOME is, and it is not the owner of what a run
// worktree does to the checkout it belongs to. Those two are this policy:
//
// A root inside NM_HOME collides with the daemon's own state. Inside
// <NM_HOME>/worktrees the collision is total: those entries are the ULID-named
// per-repository directories the default placement owns and sweeps wholesale,
// and a run ID is a ULID too, so a repository directory holding another
// repository's live run worktrees looks exactly like one more leftover run.
// Elsewhere under NM_HOME it is just as destructive for being narrower -
// <NM_HOME>/logs makes a run's worktree its own log directory (paths.RunLogDir
// is <NM_HOME>/logs/<runID>), so removing the worktree at run end destroys the
// logs of the run that just finished. Nothing an operator can want lives under
// NM_HOME anyway: it carries none of the directory-scoped toolchain
// configuration the setting exists to reach.
//
// A root inside ANY checkout puts an untracked directory in that checkout for as
// long as a run is executing, which makes it dirty. A dirty checkout blocks the
// guarded branch synchronization in internal/branchsync, so nobody can take a
// validated branch back there while a run is in flight - the placement does not
// merely look odd, it stops the workflow it is part of. That is true whether the
// victim is the checkout whose own runs land there or an unrelated one, so the
// caller passes every checkout it knows: the one the root belongs to first, then
// the other known checkouts (config keys, and at daemon startup every registered
// repository). Passing none skips that half, for a caller that knows no checkout
// at all.
func CheckPlacement(p *paths.Paths, checkout, root string, otherCheckouts ...string) error {
	home := p.Root()
	if Contains(home, root) {
		return fmt.Errorf("%q is inside no-mistakes' own state directory (%q), where it would collide with the daemon's worktrees, logs, or gates; choose a directory outside it", root, home)
	}
	if strings.TrimSpace(checkout) != "" && Contains(checkout, root) {
		return fmt.Errorf("%q is inside the checkout whose runs it would hold, which leaves that checkout with an untracked run worktree while a run executes and blocks branch synchronization; choose a directory outside the checkout", root)
	}
	own := Canonical(checkout)
	others := append([]string(nil), otherCheckouts...)
	sort.Strings(others)
	for _, other := range others {
		if strings.TrimSpace(other) == "" || Canonical(other) == own {
			continue
		}
		if Contains(other, root) {
			return fmt.Errorf("%q is inside the checkout %q, which every run placed there would leave with an untracked run worktree, blocking that checkout's branch synchronization while the run executes; choose a directory outside every checkout", root, other)
		}
	}
	return nil
}

// Validate rejects every configured entry CheckPlacement refuses, judged against
// the configured checkouts plus known, whose members the caller supplies from
// outside this package (the daemon passes every registered repository). It is the
// whole-configuration gate: the daemon refuses to start while any entry is
// unusable, and `init --worktree-root` refuses to print one.
//
// Registering a checkout AROUND an existing root reaches the same unusable state
// from the other direction, so init refuses that registration itself (see
// assertCheckoutHoldsNoConfiguredWorktreeRoot); this gate is the backstop for a
// configuration that reached the file another way.
//
// A key matching no registered repository stays a startup warning, because it
// places nothing at all.
func (l *Layout) Validate(known ...string) error {
	checkouts := l.Checkouts()
	sort.Strings(checkouts)
	for _, checkout := range checkouts {
		if err := l.validateEntry(checkout, known); err != nil {
			return err
		}
	}
	return nil
}

// ValidateCheckout rejects only the entry that places one checkout's runs, and
// says nothing about the rest of the configuration.
//
// It is what a single run's creation asks, because the alternative punishes the
// wrong repository: the registered-checkout half of this policy depends on state
// that changes while the daemon runs (a checkout registered into the path of an
// existing root), so a whole-configuration check would start failing every push
// for every repository, naming an entry the pushed repository has nothing to do
// with. The daemon's startup gate stays whole-configuration, and refuses to come
// back up until the operator fixes it.
func (l *Layout) ValidateCheckout(workingPath string, known ...string) error {
	if _, ok := l.CustomRoot(workingPath); !ok {
		return nil
	}
	return l.validateEntry(Canonical(workingPath), known)
}

// validateEntry judges one canonical entry against every checkout this layout
// and its caller know about, so both gates apply the identical rule.
func (l *Layout) validateEntry(checkout string, known []string) error {
	root, ok := l.roots[checkout]
	if !ok {
		return nil
	}
	checkouts := l.Checkouts()
	protected := make([]string, 0, len(known)+len(checkouts))
	protected = append(protected, known...)
	protected = append(protected, checkouts...)
	if err := CheckPlacement(l.paths, checkout, root, protected...); err != nil {
		return fmt.Errorf("invalid worktree_roots[%q]: run worktree root %w", checkout, err)
	}
	return nil
}

// CustomRoot reports the configured worktree root for a checkout, if any. It
// answers what the configuration currently says, which is what startup
// reporting and `init --worktree-root` guidance need; where an existing run's
// worktree is is RecordedDir's question, not this one's.
func (l *Layout) CustomRoot(workingPath string) (string, bool) {
	if len(l.roots) == 0 || strings.TrimSpace(workingPath) == "" {
		return "", false
	}
	root, ok := l.roots[Canonical(workingPath)]
	return root, ok
}

// Checkouts returns the canonical checkout path of every configured entry.
// It exists so startup can report a key that matches no registered
// repository, which is otherwise a silent no-op.
func (l *Layout) Checkouts() []string {
	out := make([]string, 0, len(l.roots))
	for checkout := range l.roots {
		out = append(out, checkout)
	}
	return out
}

// Canonical resolves a path to the form used to compare two spellings of the
// same directory. macOS reports /private/var for a /var path, so a single
// spelling is not enough to recognize the same checkout in a config key and a
// repository record. It is the one canonicalization every worktree_roots
// consumer uses, so a path that matches in one place matches in all of them.
func Canonical(path string) string {
	cleaned := filepath.Clean(path)
	if abs, err := filepath.Abs(cleaned); err == nil {
		cleaned = abs
	}
	// EvalSymlinks fails outright on a path that does not exist yet, and a
	// configured worktree root usually does not until its first run. Resolve
	// the deepest existing ancestor and keep the remainder, so a root and the
	// checkout it belongs to are still compared in the same spelling.
	current, rest := cleaned, ""
	for {
		if resolved, err := filepath.EvalSymlinks(current); err == nil {
			return filepath.Clean(filepath.Join(resolved, rest))
		}
		parent := filepath.Dir(current)
		if parent == current {
			return cleaned
		}
		rest = filepath.Join(filepath.Base(current), rest)
		current = parent
	}
}

// Contains reports whether path is dir itself or sits below it. It is how the
// pathological placements are recognized: a worktree root inside the checkout
// whose runs it would hold, or inside the directory no-mistakes already owns.
//
// Comparing spellings answers this for paths that do not exist yet, which a
// configured worktree root usually does not. It is not the whole answer,
// because on a case-insensitive volume - the macOS and Windows default -
// <NM_HOME>/logs and <nm_home>/Logs are ONE directory that no spelling
// comparison equates, and every placement guard would wave through a root that
// then collides with the daemon's own state. So a spelling that says "outside"
// is checked against the filesystem, which is the authority on whether two
// names are one directory.
func Contains(dir, path string) bool {
	return containsBySpelling(dir, path) || containsByIdentity(dir, path)
}

func containsBySpelling(dir, path string) bool {
	rel, err := filepath.Rel(Canonical(dir), Canonical(path))
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel))
}

// containsByIdentity asks the filesystem whether any ancestor of path IS dir,
// which settles case-insensitive volumes and any other aliasing a name cannot
// express. It walks upward because the interesting paths do not exist yet: the
// leaf is a root that no run has created, and the component whose case differs
// is one of its ancestors.
//
// A dir that does not exist has nothing to be identical to, so the spelling
// answer stands alone for it.
func containsByIdentity(dir, path string) bool {
	dirInfo, err := os.Stat(dir)
	if err != nil {
		return false
	}
	current := Canonical(path)
	for {
		if info, err := os.Stat(current); err == nil && os.SameFile(dirInfo, info) {
			return true
		}
		parent := filepath.Dir(current)
		if parent == current {
			return false
		}
		current = parent
	}
}

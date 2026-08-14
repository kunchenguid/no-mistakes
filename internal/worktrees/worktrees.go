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
// ValidateRoots there) before any layout is built from it.
package worktrees

import (
	"fmt"
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

// RecordedDir returns the worktree directory of an EXISTING run.
//
// recorded is the run's runs.worktree_dir: the placement resolved by Dir when
// the run was created. Reading it back rather than re-deriving it is what
// makes an edit to worktree_roots inert for runs that already exist - resume,
// step diff, startup cleanup, process reaping and eject all keep addressing
// the directory the run was actually created in, so a mid-flight edit can
// neither strand a parked run nor point a removal at a path it never created.
//
// An empty value is a run recorded before placement was durable. Those are
// derived from the current layout, exactly as every consumer did before the
// column existed, which is the closest thing to the truth still available for
// them.
func (l *Layout) RecordedDir(recorded, repoID, workingPath, runID string) string {
	if trimmed := strings.TrimSpace(recorded); trimmed != "" {
		return filepath.Clean(trimmed)
	}
	return l.Dir(repoID, workingPath, runID)
}

// Validate rejects the two placements a worktree_roots entry can name that do
// not work, and that internal/config cannot judge: it validates every entry it
// can on its own (absolute paths, two checkouts sharing a root, a root equal to
// its checkout), but it never learns where NM_HOME is, and it is not the owner
// of what a run worktree does to the checkout it belongs to. The daemon refuses
// to start on either, and run creation refuses the same way.
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
// A root inside the checkout whose runs it holds puts an untracked directory in
// that checkout for as long as a run is executing, which makes it dirty. A
// dirty checkout blocks the guarded branch synchronization in
// internal/branchsync, so the operator cannot take a validated branch back
// while any run is in flight - the placement does not merely look odd, it
// stops the workflow it is part of. `init --worktree-root` already refuses it;
// this is the same rule for a hand-written entry.
//
// A key matching no registered repository stays a startup warning, because it
// places nothing at all.
func (l *Layout) Validate() error {
	if len(l.roots) == 0 {
		return nil
	}
	home := l.paths.Root()
	checkouts := l.Checkouts()
	sort.Strings(checkouts)
	for _, checkout := range checkouts {
		root := l.roots[checkout]
		if Contains(home, root) {
			return fmt.Errorf("invalid worktree_roots[%q]: run worktree root %q is inside no-mistakes' own state directory (%q), where it would collide with the daemon's worktrees, logs, or gates; choose a directory outside it", checkout, root, home)
		}
		if Contains(checkout, root) {
			return fmt.Errorf("invalid worktree_roots[%q]: run worktree root %q is inside the checkout whose runs it would hold, which leaves that checkout with an untracked run worktree while a run executes and blocks branch synchronization; choose a directory outside the checkout", checkout, root)
		}
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

// Contains reports whether path is dir itself or sits below it, comparing
// canonical forms. It is how the pathological placements are recognized: a
// worktree root inside the checkout whose runs it would hold, or inside the
// directory no-mistakes already owns.
func Contains(dir, path string) bool {
	rel, err := filepath.Rel(Canonical(dir), Canonical(path))
	if err != nil {
		return false
	}
	return rel == "." || (!strings.HasPrefix(rel, "..") && !filepath.IsAbs(rel))
}

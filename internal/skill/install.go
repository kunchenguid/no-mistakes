package skill

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// InstallBases are the user-level agent skill parent directories, relative to
// the user's home directory, that init populates. `~/.claude/skills` is Claude
// Code's personal-skill location (OpenCode reads it too); `~/.agents/skills`
// is the vendor-neutral user-level convention Codex, OpenCode, Rovo Dev, and
// Pi all read.
var InstallBases = []string{
	filepath.Join(".claude", "skills"),
	filepath.Join(".agents", "skills"),
}

// InstallUser installs the skill into the agent skill directories under the
// current user's home directory. It returns the home-relative paths written.
func InstallUser() ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve home directory: %w", err)
	}
	return Install(home)
}

// Install writes SKILL.md into each agent skills directory under root
// (normally the user's home directory), creating directories as needed. It
// returns the root-relative paths written so the caller can report them.
// Writing is idempotent: re-running overwrites with identical content
// (refreshing a stale SKILL.md from an older version).
//
// Users may consolidate the two bases with a symlink - `.claude/skills` ->
// `.agents/skills`, the whole `.claude` dir -> `.agents`, or the reverse. Install
// follows such links transparently, including when the symlinked target dir does
// not exist yet (a plain os.MkdirAll would fail with "file exists" on a dangling
// symlink). Both logical bases stay readable afterward via the link.
func Install(root string) ([]string, error) {
	content := []byte(Markdown())
	written := make([]string, 0, len(InstallBases))
	for _, base := range InstallBases {
		rel := filepath.Join(base, Name, "SKILL.md")
		path := filepath.Join(root, rel)
		// Resolve any symlink components to a real directory before creating
		// it, so a dangling symlink in the path does not collide with MkdirAll.
		realDir, err := resolveThroughSymlinks(filepath.Dir(path))
		if err != nil {
			return written, err
		}
		if err := os.MkdirAll(realDir, 0o755); err != nil {
			return written, err
		}
		if err := os.WriteFile(filepath.Join(realDir, "SKILL.md"), content, 0o644); err != nil {
			return written, err
		}
		written = append(written, rel)
	}
	return written, nil
}

// Vendored reports the repo-relative paths of legacy vendored skill copies
// under repoRoot. Older no-mistakes versions wrote SKILL.md into each
// initialized repo; init uses this to tell users those copies are no longer
// needed. It never modifies the repo.
func Vendored(repoRoot string) []string {
	var found []string
	for _, base := range InstallBases {
		rel := filepath.Join(base, Name, "SKILL.md")
		if _, err := os.Stat(filepath.Join(repoRoot, rel)); err == nil {
			found = append(found, rel)
		}
	}
	return found
}

// resolveThroughSymlinks walks dir component by component and rewrites the path
// through any symlink it encounters, even when the symlink's target does not
// exist yet. The result contains no symlink components, so os.MkdirAll on it
// will not trip over a dangling symlink. dir must be absolute.
func resolveThroughSymlinks(dir string) (string, error) {
	return resolveThroughSymlinksSeen(dir, make(map[string]struct{}))
}

func resolveThroughSymlinksSeen(dir string, seen map[string]struct{}) (string, error) {
	clean := filepath.Clean(dir)
	volume := filepath.VolumeName(clean)
	cur := volume + string(filepath.Separator)
	for _, part := range strings.Split(strings.TrimPrefix(clean, volume), string(filepath.Separator)) {
		if part == "" {
			continue
		}
		cur = filepath.Join(cur, part)
		info, err := os.Lstat(cur)
		if err != nil {
			// This component does not exist yet; nothing left to resolve.
			// Remaining parts are appended verbatim onto the resolved prefix.
			continue
		}
		if info.Mode()&os.ModeSymlink == 0 {
			continue
		}
		key := filepath.Clean(cur)
		if _, ok := seen[key]; ok {
			return "", fmt.Errorf("symlink cycle resolving %s", dir)
		}
		seen[key] = struct{}{}
		target, err := os.Readlink(cur)
		if err != nil {
			return "", err
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(cur), target)
		}
		// The target may itself be or contain symlinks; resolve recursively.
		if cur, err = resolveThroughSymlinksSeen(target, seen); err != nil {
			return "", err
		}
	}
	return cur, nil
}

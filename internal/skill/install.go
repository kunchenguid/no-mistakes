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

// InstallResult reports which logical user-level paths Install refreshed and
// which it left to an external manager because the per-skill directory is a
// symlink.
type InstallResult struct {
	Written []string
	Managed []string
}

// InstallUser installs the skill into the agent skill directories under the
// current user's home directory.
func InstallUser() (InstallResult, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return InstallResult{}, fmt.Errorf("resolve home directory: %w", err)
	}
	return Install(home)
}

// Install writes SKILL.md into each agent skills directory under root
// (normally the user's home directory), creating directories as needed. It
// reports both refreshed paths and externally managed per-skill symlinks.
// Writing is idempotent: re-running overwrites with identical content
// (refreshing a stale SKILL.md from an older version).
//
// Users may consolidate the two bases with a symlink - `.claude/skills` ->
// `.agents/skills`, the whole `.claude` dir -> `.agents`, or the reverse. Install
// follows such parent links transparently, including when the symlinked target
// dir does not exist yet (a plain os.MkdirAll would fail with "file exists" on
// a dangling symlink). A symlink at the individual `no-mistakes` directory is
// externally managed: Install verifies it is readable but never changes its
// target.
func Install(root string) (InstallResult, error) {
	content := []byte(Markdown())
	result := InstallResult{
		Written: make([]string, 0, len(InstallBases)),
		Managed: make([]string, 0, len(InstallBases)),
	}
	for _, base := range InstallBases {
		rel := filepath.Join(base, Name, "SKILL.md")
		// Resolve only the parent skill base. The final per-skill component is
		// inspected separately so an external-manager symlink is not followed
		// and overwritten.
		realBase, err := resolveThroughSymlinks(filepath.Join(root, base))
		if err != nil {
			return result, err
		}
		if err := os.MkdirAll(realBase, 0o755); err != nil {
			return result, err
		}
		skillDir := filepath.Join(realBase, Name)
		info, err := os.Lstat(skillDir)
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			if _, err := os.ReadFile(filepath.Join(skillDir, "SKILL.md")); err != nil {
				return result, fmt.Errorf("externally managed skill %s is not readable: %w", filepath.Join(base, Name), err)
			}
			result.Managed = append(result.Managed, filepath.Join(base, Name))
			continue
		}
		if err != nil && !os.IsNotExist(err) {
			return result, err
		}
		if err := os.MkdirAll(skillDir, 0o755); err != nil {
			return result, err
		}
		if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), content, 0o644); err != nil {
			return result, err
		}
		result.Written = append(result.Written, rel)
	}
	return result, nil
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

package branchsync

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/git"
)

func (s *Service) checkIgnoredCollisions(ctx context.Context, plan State) State {
	path, err := ignoredCollision(ctx, s.workDir(), plan.Local.Head, plan.Pipeline.PushedHead)
	if err != nil {
		return blockedPlan(plan, StateAmbiguousContext, "blocked_ignored_paths_unverified", "could not verify ignored local paths before synchronization; no files, HEAD, or index were changed: "+err.Error())
	}
	if path != "" {
		return blockedPlan(plan, StateDirty, "blocked_ignored_path_collision", fmt.Sprintf("incoming tracked paths would replace ignored local path %q; move this path outside the checkout and retry sync; no files, HEAD, or index were changed", path))
	}
	return plan
}

// Git treats ignored content as disposable during both reset and fast-forward.
// Compare Git's ignored entries with the paths the exact target will write.
// Both Git outputs are NUL-delimited: whitespace and quoted filenames are data.
func ignoredCollision(ctx context.Context, dir, from, to string) (string, error) {
	root, err := git.RunRaw(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	// CLI callers may invoke sync from a subdirectory. Every path below must
	// describe the whole invoking worktree, rather than only that directory.
	dir = strings.TrimSuffix(string(root), "\n")
	ignored, err := git.RunRaw(ctx, dir, "ls-files", "--others", "--ignored", "--exclude-standard", "--directory", "-z")
	if err != nil || len(ignored) == 0 {
		return "", err
	}
	incoming, err := git.RunRaw(ctx, dir, "diff", "--no-relative", "--no-ext-diff", "--no-renames", "--name-only", "--diff-filter=ACMT", "-z", from, to, "--")
	if err != nil {
		return "", err
	}
	for _, entry := range strings.Split(string(ignored), "\x00") {
		local := strings.TrimSuffix(entry, "/")
		if local == "" {
			continue
		}
		for _, target := range strings.Split(string(incoming), "\x00") {
			if target == "" {
				continue
			}
			if local == target || strings.HasPrefix(local, target+"/") {
				return local, nil
			}
			if !strings.HasPrefix(target, local+"/") {
				continue
			}
			// --directory collapses wholly ignored directories. An incoming
			// child may coexist with their unrelated contents, so examine only
			// its path components, without following any local symlink.
			prefix := local
			parts := strings.Split(strings.TrimPrefix(target, local+"/"), "/")
			for i := 0; ; i++ {
				info, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(prefix)))
				if os.IsNotExist(err) {
					break
				}
				if err != nil {
					return "", err
				}
				if !info.IsDir() || prefix == target {
					return prefix, nil
				}
				prefix += "/" + parts[i]
			}
		}
	}
	return "", nil
}

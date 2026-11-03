package safepath

import (
	"path/filepath"
	"sort"
	"strings"
)

// Run worktree placeholders. A run worktree lives at
// <NM_HOME>/worktrees/<repo-id>/<run-id>, where <repo-id> is a short hash of
// the absolute local checkout path: published, it lets anyone confirm a guessed
// account name and layout offline, and an NM_HOME outside the home directory
// publishes whole. Like Placeholder, neither token is angle-bracketed, because
// GitHub renders published .md evidence and drops unknown tags.
const (
	WorktreePlaceholder  = "{run-worktree}"
	WorktreesPlaceholder = "{worktrees}"
)

// RedactWorktrees rewrites the run's worktree root, and as a fallback any path
// into worktreesRoot, to the worktree placeholders. Call it before RedactText:
// once the home prefix is "~", these roots no longer match. Either argument
// may be empty.
func RedactWorktrees(text, runWorktree, worktreesRoot string) string {
	for _, root := range pathSpellings(runWorktree) {
		text = replacePathPrefix(text, root, func(string) (int, string) { return 0, WorktreePlaceholder })
	}
	for _, root := range pathSpellings(worktreesRoot) {
		// A single backslash separates only in text that spells the root
		// with single backslashes; after "/" or "\\" it starts a JSON escape.
		single := strings.Contains(root, `\`) && !strings.Contains(root, `\\`)
		text = replacePathPrefix(text, root, func(rest string) (int, string) {
			// <repo-id>/<run-id> names one run worktree; anything shorter
			// still drops the repo id.
			repoID := pathSegment(rest, single)
			if repoID == 0 {
				return 0, WorktreesPlaceholder
			}
			if runID := pathSegment(rest[repoID:], single); runID > 0 {
				return repoID + runID, WorktreePlaceholder
			}
			return repoID, WorktreesPlaceholder
		})
	}
	return text
}

// pathSegment returns the length of the separator plus path segment name at
// the start of rest, or 0 when rest does not start with one. A single
// backslash counts as a separator only when singleBackslash is set.
func pathSegment(rest string, singleBackslash bool) int {
	sep := 0
	switch {
	case strings.HasPrefix(rest, `\\`):
		sep = 2
	case strings.HasPrefix(rest, "/"), singleBackslash && strings.HasPrefix(rest, `\`):
		sep = 1
	default:
		return 0
	}
	n := sep
	for n < len(rest) && isPathNameByte(rest[n]) {
		n++
	}
	if n == sep {
		return 0
	}
	return n
}

// pathSpellings lists the spellings an absolute directory can appear as in
// captured output, longest first, the same set homeCandidates builds.
func pathSpellings(dir string) []string {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	add := func(candidate string) {
		if usableHomeCandidate(candidate) && !seen[candidate] {
			seen[candidate] = true
			out = append(out, candidate)
		}
	}
	dir = filepath.Clean(dir)
	addBothSeparatorSpellings(add, dir)
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		addBothSeparatorSpellings(add, filepath.Clean(resolved))
	}
	sort.SliceStable(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
}

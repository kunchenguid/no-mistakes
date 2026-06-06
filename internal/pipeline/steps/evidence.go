package steps

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/config"
)

func testEvidenceRoot() string {
	return filepath.Join(os.TempDir(), "no-mistakes-evidence")
}

func testEvidenceDir(runID string) string {
	return filepath.Join(testEvidenceRoot(), runID)
}

// resolveTestEvidenceDir picks where the test step writes evidence artifacts.
//
// By default (opt-out), evidence lives in a temporary directory keyed by run ID
// and is referenced only by local path. When the user opts in to storing
// evidence in the repo, it instead lands under a readable, branch-named
// directory inside the worktree so it is committed, pushed, and rendered
// directly on the PR. An absolute or escaping configured directory is rejected
// and falls back to the temporary location so evidence can never be written
// outside the worktree.
func resolveTestEvidenceDir(workDir, branch, runID string, ev config.Evidence) string {
	location := resolveTestEvidenceLocation(workDir, branch, runID, ev)
	return location.Dir
}

type testEvidenceLocation struct {
	Dir         string
	StoreInRepo bool
}

func resolveTestEvidenceLocation(workDir, branch, runID string, ev config.Evidence) testEvidenceLocation {
	if !ev.StoreInRepo {
		return testEvidenceLocation{Dir: testEvidenceDir(runID)}
	}
	sub, ok := safeRepoSubdir(ev.Dir)
	if !ok {
		return testEvidenceLocation{Dir: testEvidenceDir(runID)}
	}
	segments := evidenceBranchSlug(branch)
	if len(segments) == 0 {
		segments = []string{runID}
	}
	relParts := append([]string{sub}, segments...)
	rel := filepath.Join(relParts...)
	if repoPathHasSymlink(workDir, rel) {
		return testEvidenceLocation{Dir: testEvidenceDir(runID)}
	}
	parts := append([]string{workDir}, relParts...)
	return testEvidenceLocation{Dir: filepath.Join(parts...), StoreInRepo: true}
}

func repoPathHasSymlink(workDir, rel string) bool {
	clean := filepath.Clean(rel)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || filepath.IsAbs(clean) {
		return true
	}
	current := workDir
	for _, part := range strings.Split(clean, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			return false
		}
		if err != nil {
			return true
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return true
		}
	}
	return false
}

// safeRepoSubdir validates a configured evidence directory as a relative path
// that stays inside the repo worktree. It returns the cleaned, OS-native path
// and false when the directory is empty, absolute, or escapes the worktree.
func safeRepoSubdir(dir string) (string, bool) {
	dir = strings.TrimSpace(dir)
	if dir == "" || filepath.IsAbs(dir) || hasPathRootPrefix(dir) || hasWindowsDrivePrefix(dir) {
		return "", false
	}
	clean := filepath.Clean(filepath.FromSlash(dir))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", false
	}
	first, _, _ := strings.Cut(clean, string(filepath.Separator))
	if strings.EqualFold(first, ".git") {
		return "", false
	}
	return clean, true
}

func hasPathRootPrefix(path string) bool {
	return strings.HasPrefix(path, "/") || strings.HasPrefix(path, `\`)
}

func hasWindowsDrivePrefix(path string) bool {
	if len(path) < 2 || path[1] != ':' {
		return false
	}
	c := path[0]
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z'
}

// evidenceBranchSlug turns a branch name into readable, filesystem-safe path
// segments. Branch separators are preserved as nested directories; unsafe
// characters are replaced with dashes and traversal segments are dropped.
func evidenceBranchSlug(branch string) []string {
	var segments []string
	for _, raw := range strings.Split(branch, "/") {
		seg := sanitizeEvidenceSegment(raw)
		if seg == "" || seg == "." || seg == ".." {
			continue
		}
		segments = append(segments, seg)
	}
	return segments
}

// sanitizeEvidenceSegment keeps alphanumerics, dash, underscore, and dot,
// replacing every other rune with a dash, then collapses dash runs and trims
// leading/trailing dashes.
func sanitizeEvidenceSegment(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := b.String()
	for strings.Contains(out, "--") {
		out = strings.ReplaceAll(out, "--", "-")
	}
	return strings.Trim(out, "-")
}

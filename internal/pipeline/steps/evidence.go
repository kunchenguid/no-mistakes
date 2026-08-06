package steps

import (
	"os"
	"path/filepath"
	"strings"
)

func testEvidenceRoot() string {
	return filepath.Join(os.TempDir(), "no-mistakes-evidence")
}

// testEvidenceDir is where the test step writes a run's evidence artifacts.
//
// Evidence is always collected OUTSIDE the worktree, keyed by run ID, so a
// pipeline run can never commit artifacts into the branch it is validating.
// Publishing them (opt-in, see config.Evidence) copies the directory onto the
// repository's orphan evidence branch instead, which is what keeps screenshots
// and logs out of the default branch's history while still linking them from
// the PR.
func testEvidenceDir(runID string) string {
	return filepath.Join(testEvidenceRoot(), runID)
}

// evidenceBranchSlug turns a branch name into readable, filesystem-safe path
// segments used as the run's directory on the evidence branch. Branch
// separators are preserved as nested directories; unsafe characters are
// replaced with dashes and traversal segments are dropped.
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

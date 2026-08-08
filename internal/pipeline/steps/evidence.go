package steps

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
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

func taskBrowserRoot(sctx *pipeline.StepContext) string {
	repo := "repo"
	branch := ""
	if sctx != nil {
		if sctx.Repo != nil {
			if upstream := strings.TrimSpace(sctx.Repo.UpstreamURL); upstream != "" {
				repo = upstream
			} else if id := strings.TrimSpace(sctx.Repo.ID); id != "" {
				repo = id
			}
		}
		if sctx.Run != nil {
			branch = sctx.Run.Branch
		}
	}
	repoSum := sha256.Sum256([]byte(repo))
	branchSum := sha256.Sum256([]byte(branch))
	return filepath.Join(testEvidenceRoot(), "tasks", fmt.Sprintf("%x", repoSum[:6]), fmt.Sprintf("%x", branchSum[:6]))
}

func taskBrowserRuntimeDir(sctx *pipeline.StepContext) string {
	return filepath.Join(taskBrowserRoot(sctx), "runtime")
}

// reusableEvidenceDir is stable across reruns only while the complete
// non-ignored worktree tree is identical.
func reusableEvidenceDir(sctx *pipeline.StepContext) (string, error) {
	ctx := context.Background()
	if sctx != nil && sctx.Ctx != nil {
		ctx = sctx.Ctx
	}
	tree, err := pipeline.WorktreeTreeID(ctx, sctx.WorkDir)
	if err != nil {
		return "", err
	}
	return filepath.Join(taskBrowserRoot(sctx), "evidence", tree), nil
}

func browserReusePromptSection(sctx *pipeline.StepContext) string {
	evidenceDir, err := reusableEvidenceDir(sctx)
	if err != nil {
		evidenceDir = "unavailable for the current worktree"
	}
	return fmt.Sprintf(`

Reusable browser context:
- Reuse compatible browser evidence before recapturing it: %s
- Reuse task-local browser/runtime configuration instead of rebuilding an equivalent environment: %s
- Evidence is compatible only for the exact current worktree tree; runtime configuration remains scoped to this repository task and branch.
`, evidenceDir, taskBrowserRuntimeDir(sctx))
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

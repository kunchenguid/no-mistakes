package steps

import (
	"context"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/evidence"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

// ValidateRunPRBaseBranchName checks a per-run PR base branch name using the
// same Git ref rules as pr.base_branch in repo config.
func ValidateRunPRBaseBranchName(name string) (string, error) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return "", nil
	}
	if _, err := evidence.NormalizeBranch(trimmed); err != nil {
		return "", err
	}
	return trimmed, nil
}

// VerifyRemoteBranchExists reports whether origin has refs/heads/<branch>.
func VerifyRemoteBranchExists(ctx context.Context, workDir, branch string) error {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return nil
	}
	sha, err := git.LsRemote(ctx, workDir, "origin", "refs/heads/"+branch)
	if err != nil {
		return fmt.Errorf("look up remote branch %q: %w", branch, err)
	}
	if sha == "" {
		return fmt.Errorf("remote branch %q does not exist on origin", branch)
	}
	return nil
}

// runPRBaseBranch returns the per-run PR base branch override stored on the
// run record, or "" when unset.
func runPRBaseBranch(sctx *pipeline.StepContext) string {
	if sctx == nil || sctx.Run == nil || sctx.Run.PRBaseBranch == nil {
		return ""
	}
	return strings.TrimSpace(*sctx.Run.PRBaseBranch)
}

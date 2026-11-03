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
// run record, or "" when unset. It is the operator's TARGET intent and the only
// base the PR step retargets an existing pull request to.
func runPRBaseBranch(sctx *pipeline.StepContext) string {
	if sctx == nil || sctx.Run == nil || sctx.Run.PRBaseBranch == nil {
		return ""
	}
	return strings.TrimSpace(*sctx.Run.PRBaseBranch)
}

// observedBaseBranch returns the base branch the CI step observed on the pull
// request and aligned the run to, or "" when it has not observed one. It is
// SCOPING state: it never becomes a retarget request, because the forge moved
// the pull request itself and nothing in this run asked for that.
func observedBaseBranch(sctx *pipeline.StepContext) string {
	if sctx == nil || sctx.Run == nil || sctx.Run.ObservedBaseBranch == nil {
		return ""
	}
	return strings.TrimSpace(*sctx.Run.ObservedBaseBranch)
}

// targetPRBaseBranch resolves the branch this run means to target: the
// operator's per-run override, then the configured pr.base_branch, then the
// repository default. It deliberately ignores the observed base, because
// observation is not intent: a pull request the forge retargeted must not be
// moved back by the PR step.
func targetPRBaseBranch(sctx *pipeline.StepContext) string {
	branch := ""
	if runBase := runPRBaseBranch(sctx); runBase != "" {
		branch = runBase
	} else if sctx != nil && sctx.Config != nil && strings.TrimSpace(sctx.Config.PR.BaseBranch) != "" {
		branch = strings.TrimSpace(sctx.Config.PR.BaseBranch)
	} else if sctx != nil && sctx.Repo != nil {
		branch = strings.TrimSpace(sctx.Repo.DefaultBranch)
	}
	if branch == "" {
		branch = "main"
	}
	return branch
}

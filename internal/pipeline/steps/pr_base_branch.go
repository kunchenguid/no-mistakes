package steps

import (
	"context"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/evidence"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

// EffectiveIntegrationBase is the branch an integration remote is chosen for.
// A per-run override wins, then repository config, then the repository default,
// then main.
func EffectiveIntegrationBase(runBase, configuredBase, repoDefault string) string {
	if base := strings.TrimSpace(runBase); base != "" {
		return base
	}
	if base := strings.TrimSpace(configuredBase); base != "" {
		return base
	}
	if base := strings.TrimSpace(repoDefault); base != "" {
		return base
	}
	return "main"
}

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

// VerifyRemoteBranchExists reports whether remote has refs/heads/<branch>.
// An empty remote is origin, which is the historical integration remote.
func VerifyRemoteBranchExists(ctx context.Context, workDir, remote, branch string) error {
	remote = strings.TrimSpace(remote)
	if remote == "" {
		remote = "origin"
	}
	return verifyRemoteBranch(ctx, workDir, remote, remote, branch)
}

// VerifyIntegrationBranch checks branch on remote. When workDir does not have
// that remote configured, the URL is read from lookupDir (the working
// checkout) and queried from workDir, so a gate worktree that only has origin
// can still see another remote. The error names the remote, not the URL.
func VerifyIntegrationBranch(ctx context.Context, workDir, lookupDir, remote, branch string) error {
	remote = strings.TrimSpace(remote)
	if remote == "" {
		remote = "origin"
	}
	spec := remote
	if remote != "origin" {
		if _, err := git.GetRemoteURL(ctx, workDir, remote); err != nil {
			lookup := strings.TrimSpace(lookupDir)
			if lookup == "" {
				lookup = workDir
			}
			url, urlErr := git.GetRemoteURL(ctx, lookup, remote)
			if urlErr != nil {
				return fmt.Errorf("look up remote %q: %w", remote, urlErr)
			}
			spec = url
		}
	}
	return verifyRemoteBranch(ctx, workDir, spec, remote, branch)
}

func verifyRemoteBranch(ctx context.Context, workDir, remoteSpec, label, branch string) error {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return nil
	}
	sha, err := git.LsRemote(ctx, workDir, remoteSpec, "refs/heads/"+branch)
	if err != nil {
		return fmt.Errorf("look up remote branch %q: %w", branch, err)
	}
	if sha == "" {
		return fmt.Errorf("remote branch %q does not exist on %s", branch, label)
	}
	return nil
}

// NormalizeRemoteName accepts an empty name or a single git remote name.
func NormalizeRemoteName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", nil
	}
	if strings.ContainsAny(name, " \t\r\n\\") || strings.Contains(name, "..") || strings.Contains(name, "/") {
		return "", fmt.Errorf("invalid remote name %q", name)
	}
	return name, nil
}

// TrackingRemoteForBase returns the local branch's configured upstream remote
// when that upstream merge ref is exactly refs/heads/<baseBranch>. It returns
// "" when tracking is unset, does not name baseBranch, or the remote is not
// configured.
func TrackingRemoteForBase(ctx context.Context, workDir, headBranch, baseBranch string) string {
	headBranch = strings.TrimPrefix(strings.TrimSpace(headBranch), "refs/heads/")
	baseBranch = strings.TrimSpace(baseBranch)
	if workDir == "" || headBranch == "" || headBranch == "HEAD" || baseBranch == "" {
		return ""
	}
	remote, err := git.Run(ctx, workDir, "config", "--get", "branch."+headBranch+".remote")
	if err != nil {
		return ""
	}
	merge, err := git.Run(ctx, workDir, "config", "--get", "branch."+headBranch+".merge")
	if err != nil {
		return ""
	}
	remote = strings.TrimSpace(remote)
	if remote == "" || strings.TrimSpace(merge) != "refs/heads/"+baseBranch {
		return ""
	}
	if _, err := git.GetRemoteURL(ctx, workDir, remote); err != nil {
		return ""
	}
	return remote
}

// StoredIntegrationRemote is the remote name to persist for a run. An explicit
// name, including origin, is stored so it suppresses later tracking. With no
// explicit name, a non-origin upstream that already tracks baseBranch is
// stored; origin and unset stay empty and keep today's origin behavior.
func StoredIntegrationRemote(ctx context.Context, workDir, headBranch, baseBranch, explicit string) (string, error) {
	explicit, err := NormalizeRemoteName(explicit)
	if err != nil {
		return "", fmt.Errorf("--base-remote: %w", err)
	}
	if explicit != "" {
		if _, err := git.GetRemoteURL(ctx, workDir, explicit); err != nil {
			return "", fmt.Errorf("--base-remote: remote %q is not configured", explicit)
		}
		return explicit, nil
	}
	tracked := TrackingRemoteForBase(ctx, workDir, headBranch, baseBranch)
	if tracked == "" || tracked == "origin" {
		return "", nil
	}
	return tracked, nil
}

// ResolveIntegrationRemote is the remote a run uses for the base branch.
// Empty explicit input resolves through StoredIntegrationRemote and otherwise
// stays origin.
func ResolveIntegrationRemote(ctx context.Context, workDir, headBranch, baseBranch, explicit string) (string, error) {
	stored, err := StoredIntegrationRemote(ctx, workDir, headBranch, baseBranch, explicit)
	if err != nil {
		return "", err
	}
	if stored == "" {
		return "origin", nil
	}
	return stored, nil
}

// runPRBaseBranch returns the per-run PR base branch override stored on the
// run record, or "" when unset.
func runPRBaseBranch(sctx *pipeline.StepContext) string {
	if sctx == nil || sctx.Run == nil || sctx.Run.PRBaseBranch == nil {
		return ""
	}
	return strings.TrimSpace(*sctx.Run.PRBaseBranch)
}

// runBaseRemote returns the persisted integration remote, or "" when the run
// keeps the origin default. An explicit origin is stored as "origin" so it
// suppresses the tracking fallback.
func runBaseRemote(sctx *pipeline.StepContext) string {
	if sctx == nil || sctx.Run == nil || sctx.Run.BaseRemote == nil {
		return ""
	}
	return strings.TrimSpace(*sctx.Run.BaseRemote)
}

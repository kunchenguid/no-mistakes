package steps

import (
	"context"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/gate"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

// reviewWorkload returns the bounded change size (files + net lines) between
// base and head for local telemetry, or nil when the diff-stat cannot be
// computed (so the invocation records an unknown workload rather than a
// fabricated zero).
func reviewWorkload(ctx context.Context, workDir, base, head string) *agent.InvocationWorkload {
	files, lines, err := git.DiffStat(ctx, workDir, base, head)
	if err != nil {
		return nil
	}
	return &agent.InvocationWorkload{Files: files, Lines: lines}
}

// pipelineBaseBranch returns the configured PR target, falling back to the
// repository default branch exactly as before when pr.base_branch is unset.
// Config.PR is trusted-only; the daemon resolved its explicit branch from the
// upstream parent before constructing the executor.
func pipelineBaseBranch(sctx *pipeline.StepContext) string {
	if sctx != nil && sctx.Config != nil {
		if branch := strings.TrimSpace(sctx.Config.PR.BaseBranch); branch != "" {
			return branch
		}
	}
	if sctx != nil && sctx.Repo != nil {
		return strings.TrimSpace(sctx.Repo.DefaultBranch)
	}
	return ""
}

func pipelineBaseTarget(sctx *pipeline.StepContext) string {
	if sctx != nil && sctx.Config != nil && sctx.Config.PR.HasExplicitBaseBranch() {
		if sha := strings.TrimSpace(sctx.Config.PR.ResolvedBaseSHA); sha != "" {
			return sha
		}
	}
	return pipelineBaseBranch(sctx)
}

func resolvePipelineBranchBaseSHA(ctx context.Context, sctx *pipeline.StepContext) string {
	return resolveBranchBaseSHA(ctx, sctx.WorkDir, sctx.Run.BaseSHA, pipelineBaseTarget(sctx))
}

// resolveBaseSHA returns a usable base SHA for diff/log operations.
// When baseSHA is the zero ref (new branch push), it tries git merge-base
// against the intended PR base branch, falling back to the empty tree SHA.
func resolveBaseSHA(ctx context.Context, workDir, baseSHA, baseTarget string) string {
	if !git.IsZeroSHA(baseSHA) {
		return baseSHA
	}
	if mb := mergeBaseWithTarget(ctx, workDir, baseTarget); mb != "" {
		return mb
	}
	return git.EmptyTreeSHA
}

// resolveBranchBaseSHA returns the branch base commit relative to the intended
// PR base branch when possible. This keeps pipeline steps scoped to the full
// branch, not just the last pushed delta. If merge-base cannot be determined, it falls
// back to resolveBaseSHA.
func resolveBranchBaseSHA(ctx context.Context, workDir, fallbackBaseSHA, baseTarget string) string {
	if mb := mergeBaseWithTarget(ctx, workDir, baseTarget); mb != "" {
		return mb
	}
	return resolveBaseSHA(ctx, workDir, fallbackBaseSHA, baseTarget)
}

func resolveDefaultBranchTipSHA(ctx context.Context, workDir, upstreamURL, fallbackBaseSHA, baseBranch string) string {
	sha, _ := resolveDefaultBranchTip(ctx, workDir, upstreamURL, fallbackBaseSHA, baseBranch)
	return sha
}

func resolveRunDefaultBranchTip(ctx context.Context, sctx *pipeline.StepContext, fallbackBaseSHA, baseBranch string) (string, bool) {
	sha, resolved, _ := refreshRunBaseBranchTip(ctx, sctx, fallbackBaseSHA, baseBranch)
	return sha, resolved
}

func validateExplicitPRBase(ctx context.Context, sctx *pipeline.StepContext) error {
	if sctx == nil || sctx.Config == nil || !sctx.Config.PR.HasExplicitBaseBranch() {
		return nil
	}
	_, resolved, err := refreshRunBaseBranchTip(ctx, sctx, sctx.Run.BaseSHA, pipelineBaseBranch(sctx))
	if err != nil {
		return err
	}
	if !resolved {
		return fmt.Errorf("configured pr.base_branch %q could not be resolved", pipelineBaseBranch(sctx))
	}
	return nil
}

func refreshRunBaseBranchTip(ctx context.Context, sctx *pipeline.StepContext, fallbackBaseSHA, baseBranch string) (string, bool, error) {
	fallback := unresolvedDefaultBranchTip(ctx, sctx.WorkDir, fallbackBaseSHA, baseBranch)
	if strings.TrimSpace(baseBranch) == "" {
		return fallback, false, nil
	}
	explicit := sctx.Config != nil && sctx.Config.PR.HasExplicitBaseBranch()
	ref := "refs/remotes/origin/" + baseBranch
	var fetchErr error
	if explicit {
		ref = git.RunPRBaseMonitorRef(sctx.Run.ID)
		fetchErr = fetchRunUpstreamBranchToPrivateRef(ctx, sctx, baseBranch, ref)
	} else {
		fetchErr = fetchRunUpstreamBranch(ctx, sctx, baseBranch)
	}
	if fetchErr != nil {
		if explicit {
			return fallback, false, fmt.Errorf("configured pr.base_branch %q could not be fetched from the upstream repository after refresh: %w", baseBranch, fetchErr)
		}
		return fallback, false, fmt.Errorf("fetch upstream branch %q: %w", baseBranch, fetchErr)
	}
	sha, err := git.Run(ctx, sctx.WorkDir, "rev-parse", "--verify", ref+"^{commit}")
	if err != nil || strings.TrimSpace(sha) == "" {
		if err == nil {
			err = fmt.Errorf("empty commit")
		}
		if explicit {
			return fallback, false, fmt.Errorf("configured pr.base_branch %q did not resolve after refresh: %w", baseBranch, err)
		}
		return fallback, false, fmt.Errorf("resolve upstream branch %q after fetch: %w", baseBranch, err)
	}
	sha = strings.TrimSpace(sha)
	if explicit {
		mergeBase, mergeErr := git.Run(ctx, sctx.WorkDir, "merge-base", "HEAD", sha)
		if mergeErr != nil || strings.TrimSpace(mergeBase) == "" {
			if mergeErr != nil {
				return fallback, false, fmt.Errorf("configured pr.base_branch %q has no usable shared history with HEAD after refresh: %w", baseBranch, mergeErr)
			}
			return fallback, false, fmt.Errorf("configured pr.base_branch %q has no usable shared history with HEAD after refresh", baseBranch)
		}
		snapshot := strings.TrimSpace(sctx.Config.PR.ResolvedBaseSHA)
		if snapshot != "" {
			if _, ancestorErr := git.Run(ctx, sctx.WorkDir, "merge-base", "--is-ancestor", snapshot, sha); ancestorErr != nil {
				return fallback, false, fmt.Errorf("configured pr.base_branch %q moved behind or away from immutable run snapshot %s (refreshed tip %s): %w", baseBranch, snapshot, sha, ancestorErr)
			}
		}
	}
	return sha, true, nil
}

func resolveDefaultBranchTip(ctx context.Context, workDir, upstreamURL, fallbackBaseSHA, baseBranch string) (string, bool) {
	if strings.TrimSpace(baseBranch) != "" {
		remoteName := resolveUpstreamRemoteName(ctx, workDir, upstreamURL)
		if err := git.FetchRemoteBranch(ctx, workDir, remoteName, baseBranch); err != nil {
			return unresolvedDefaultBranchTip(ctx, workDir, fallbackBaseSHA, baseBranch), false
		}
		for _, ref := range []string{remoteName + "/" + baseBranch, baseBranch} {
			sha, err := git.Run(ctx, workDir, "rev-parse", "--verify", ref)
			if err == nil && strings.TrimSpace(sha) != "" {
				return strings.TrimSpace(sha), true
			}
		}
	}
	return resolveBaseSHA(ctx, workDir, fallbackBaseSHA, baseBranch), false
}

func unresolvedDefaultBranchTip(ctx context.Context, workDir, fallbackBaseSHA, baseBranch string) string {
	if !git.IsZeroSHA(fallbackBaseSHA) {
		return fallbackBaseSHA
	}
	sha, localErr := git.Run(ctx, workDir, "rev-parse", "--verify", baseBranch)
	if localErr == nil && strings.TrimSpace(sha) != "" {
		return strings.TrimSpace(sha)
	}
	return git.EmptyTreeSHA
}

func resolveUpstreamRemoteName(ctx context.Context, workDir, upstreamURL string) string {
	if strings.TrimSpace(upstreamURL) == "" {
		return "origin"
	}
	remotes, err := git.Run(ctx, workDir, "remote")
	if err != nil {
		return "origin"
	}
	for _, remote := range strings.Fields(remotes) {
		url, urlErr := git.GetRemoteURL(ctx, workDir, remote)
		if urlErr == nil && gate.SameRemoteRepository(url, upstreamURL) {
			return remote
		}
	}
	return "origin"
}

func mergeBaseWithBranch(ctx context.Context, workDir, baseBranch string) string {
	return mergeBaseWithTarget(ctx, workDir, baseBranch)
}

func mergeBaseWithTarget(ctx context.Context, workDir, target string) string {
	target = strings.TrimSpace(target)
	if target == "" {
		return ""
	}
	refs := []string{target}
	if !strings.HasPrefix(target, "refs/") && len(target) != 40 {
		refs = []string{"origin/" + target, target}
	}
	for _, ref := range refs {
		mb, err := git.Run(ctx, workDir, "merge-base", "HEAD", ref)
		if err == nil && strings.TrimSpace(mb) != "" {
			return strings.TrimSpace(mb)
		}
	}
	return ""
}

// lastFetchedBranchTip returns the commit the push branch's remote-tracking ref
// resolves to in the worktree - the exact remote head the rebase step last
// fetched and rebased against. It is the safe anchor for a force-with-lease: if
// the live remote has moved past it, the push must be treated as potentially
// discarding unseen work. Returns "" when no tracking ref exists (e.g. a brand
// new branch or a failed fetch), which makes the caller fall back to the
// content-incorporation check rather than trusting a stale value.
func lastFetchedBranchTip(ctx context.Context, workDir, branch string, fork bool) string {
	trackingRef := "refs/remotes/origin/" + branch
	if fork {
		trackingRef = forkBranchTrackingRef(branch)
	}
	sha, err := git.Run(ctx, workDir, "rev-parse", "--verify", "--quiet", trackingRef+"^{commit}")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(sha)
}

func normalizedBranchRef(ref string) string {
	if !strings.HasPrefix(ref, "refs/") {
		return "refs/heads/" + ref
	}
	return ref
}

// resolveUpstreamURL returns the upstream URL to push or query. Ordinarily it
// prefers the worktree's configured "origin" remote, which inherits any
// embedded credentials from the gate's bare repo. When run-start discovery
// verified a different current clone URL, it prefers that refreshed repo value
// instead. It also falls back to the repo record when origin cannot be read.
//
// This separation lets the database and logs store a redacted URL while the
// credential still reaches the git push/ls-remote argv that needs it.
func resolveUpstreamURL(sctx *pipeline.StepContext) string {
	if url, err := git.GetRemoteURL(sctx.Ctx, sctx.WorkDir, "origin"); err == nil && strings.TrimSpace(url) != "" {
		// A matching repository identity means origin may carry credentials that
		// the database intentionally omits. A different registration was refreshed
		// from the working clone at run start, so prefer it without rewriting
		// either clone or gate remote configuration.
		if sctx.Repo == nil || !sctx.Repo.URLsVerified || gate.SameRemoteRepository(url, sctx.Repo.UpstreamURL) {
			return url
		}
	}
	return sctx.Repo.UpstreamURL
}

func fetchRunUpstreamBranch(ctx context.Context, sctx *pipeline.StepContext, branch string) error {
	upstreamURL := resolveUpstreamURL(sctx)
	originURL, err := git.GetRemoteURL(ctx, sctx.WorkDir, "origin")
	if err == nil && upstreamURL == originURL {
		return git.FetchRemoteBranch(ctx, sctx.WorkDir, "origin", branch)
	}
	return git.FetchRemoteBranchToRef(ctx, sctx.WorkDir, upstreamURL, branch, "refs/remotes/origin/"+branch)
}

func fetchRunUpstreamBranchToPrivateRef(ctx context.Context, sctx *pipeline.StepContext, branch, localRef string) error {
	upstreamURL := resolveUpstreamURL(sctx)
	originURL, err := git.GetRemoteURL(ctx, sctx.WorkDir, "origin")
	if err == nil && upstreamURL == originURL {
		return git.FetchRemoteBranchToPrivateRef(ctx, sctx.WorkDir, "origin", branch, localRef)
	}
	return git.FetchRemoteBranchToPrivateRef(ctx, sctx.WorkDir, upstreamURL, branch, localRef)
}

// resolvePushURL returns the URL to push to: the fork when one is configured
// (fork-based contributions, Repo.ForkURL set), else the upstream selected by
// resolveUpstreamURL. A matching worktree origin can retain credentials outside
// the database; a different URL verified from the working clone at run start
// takes precedence without rewriting the worktree remote. Fork URLs carry no
// embedded credentials today, so the fork path uses the repo record directly.
// In both cases callers wrap the URL in safeurl.Redact before logging it.
func resolvePushURL(sctx *pipeline.StepContext) string {
	if sctx.Repo != nil && strings.TrimSpace(sctx.Repo.ForkURL) != "" {
		return sctx.Repo.ForkURL
	}
	return resolveUpstreamURL(sctx)
}

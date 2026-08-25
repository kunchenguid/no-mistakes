package steps

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/safeurl"
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

// resolveBaseSHA returns a usable base SHA for diff/log operations.
// When baseSHA is the zero ref (new branch push), it tries git merge-base
// against the default branch, falling back to the empty tree SHA.
func resolveBaseSHA(ctx context.Context, workDir, baseSHA, defaultBranch string) string {
	if !git.IsZeroSHA(baseSHA) {
		return baseSHA
	}
	if mb := mergeBaseWithDefaultBranch(ctx, workDir, defaultBranch); mb != "" {
		return mb
	}
	return git.EmptyTreeSHA
}

// resolveBranchBaseSHA returns the branch base commit relative to the default
// branch when possible. This keeps pipeline steps scoped to the full branch,
// not just the last pushed delta. If merge-base cannot be determined, it falls
// back to resolveBaseSHA.
func resolveBranchBaseSHA(ctx context.Context, workDir, fallbackBaseSHA, defaultBranch string) string {
	if mb := mergeBaseWithDefaultBranch(ctx, workDir, defaultBranch); mb != "" {
		return mb
	}
	return resolveBaseSHA(ctx, workDir, fallbackBaseSHA, defaultBranch)
}

func resolveDefaultBranchTipSHA(ctx context.Context, workDir, upstreamURL, fallbackBaseSHA, defaultBranch string) string {
	sha, _ := resolveDefaultBranchTip(ctx, workDir, upstreamURL, fallbackBaseSHA, defaultBranch)
	return sha
}

func resolveRunDefaultBranchTipSHA(ctx context.Context, sctx *pipeline.StepContext, fallbackBaseSHA, defaultBranch string) string {
	sha, _ := resolveRunDefaultBranchTip(ctx, sctx, fallbackBaseSHA, defaultBranch)
	return sha
}

func resolveRunDefaultBranchTip(ctx context.Context, sctx *pipeline.StepContext, fallbackBaseSHA, defaultBranch string) (string, bool) {
	if strings.TrimSpace(defaultBranch) != "" {
		if err := fetchRunUpstreamBranch(ctx, sctx, defaultBranch); err != nil {
			return unresolvedDefaultBranchTip(ctx, sctx.WorkDir, fallbackBaseSHA, defaultBranch), false
		}
		sha, err := git.Run(ctx, sctx.WorkDir, "rev-parse", "--verify", "origin/"+defaultBranch)
		if err == nil && strings.TrimSpace(sha) != "" {
			return strings.TrimSpace(sha), true
		}
	}
	return resolveBaseSHA(ctx, sctx.WorkDir, fallbackBaseSHA, defaultBranch), false
}

func resolveDefaultBranchTip(ctx context.Context, workDir, upstreamURL, fallbackBaseSHA, defaultBranch string) (string, bool) {
	if strings.TrimSpace(defaultBranch) != "" {
		remoteName := resolveUpstreamRemoteName(ctx, workDir, upstreamURL)
		if err := git.FetchRemoteBranch(ctx, workDir, remoteName, defaultBranch); err != nil {
			return unresolvedDefaultBranchTip(ctx, workDir, fallbackBaseSHA, defaultBranch), false
		}
		for _, ref := range []string{remoteName + "/" + defaultBranch, defaultBranch} {
			sha, err := git.Run(ctx, workDir, "rev-parse", "--verify", ref)
			if err == nil && strings.TrimSpace(sha) != "" {
				return strings.TrimSpace(sha), true
			}
		}
	}
	return resolveBaseSHA(ctx, workDir, fallbackBaseSHA, defaultBranch), false
}

func unresolvedDefaultBranchTip(ctx context.Context, workDir, fallbackBaseSHA, defaultBranch string) string {
	if !git.IsZeroSHA(fallbackBaseSHA) {
		return fallbackBaseSHA
	}
	sha, localErr := git.Run(ctx, workDir, "rev-parse", "--verify", defaultBranch)
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
		if urlErr == nil && strings.TrimSpace(url) == strings.TrimSpace(upstreamURL) {
			return remote
		}
	}
	return "origin"
}

func mergeBaseWithDefaultBranch(ctx context.Context, workDir, defaultBranch string) string {
	if strings.TrimSpace(defaultBranch) == "" {
		return ""
	}
	for _, ref := range []string{"origin/" + defaultBranch, defaultBranch} {
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
		// A matching redacted value means origin may carry credentials that the
		// database intentionally omits. A different registration was refreshed
		// from the working clone at run start, so prefer it without rewriting
		// either clone or gate remote configuration.
		if sctx.Repo == nil || !sctx.Repo.URLsVerified || safeurl.Redact(url) == sctx.Repo.UpstreamURL {
			return url
		}
	}
	return sctx.Repo.UpstreamURL
}

// fetchUpstreamTimeout bounds a single upstream fetch. Abort convergence alone
// does not rescue a fetch that hangs with nothing to cancel it: an SSH fetch can
// sit indefinitely on a dead connection while the run context stays live. The
// deadline is attributed with ErrFetchTimeout so the caller can say why it gave
// up rather than logging a bare context error.
// Overridable so tests can drive the deadline without waiting it out.
var fetchUpstreamTimeout = 120 * time.Second

// ErrFetchTimeout marks a fetch that exceeded fetchUpstreamTimeout.
var ErrFetchTimeout = errors.New("upstream fetch timed out")

func fetchRunUpstreamBranch(ctx context.Context, sctx *pipeline.StepContext, branch string) error {
	// Respect a deadline the caller already set rather than extending it.
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeoutCause(ctx, fetchUpstreamTimeout, ErrFetchTimeout)
		defer cancel()
	}

	err := fetchRunUpstreamBranchInner(ctx, sctx, branch)
	if err != nil && errors.Is(context.Cause(ctx), ErrFetchTimeout) {
		return fmt.Errorf("%w after %s: %v", ErrFetchTimeout, fetchUpstreamTimeout, err)
	}
	return err
}

func fetchRunUpstreamBranchInner(ctx context.Context, sctx *pipeline.StepContext, branch string) error {
	upstreamURL := resolveUpstreamURL(sctx)
	originURL, err := git.GetRemoteURL(ctx, sctx.WorkDir, "origin")
	if err == nil && upstreamURL == originURL {
		return git.FetchRemoteBranch(ctx, sctx.WorkDir, "origin", branch)
	}
	return git.FetchRemoteBranchToRef(ctx, sctx.WorkDir, upstreamURL, branch, "refs/remotes/origin/"+branch)
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

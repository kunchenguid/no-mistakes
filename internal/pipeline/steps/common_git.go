package steps

import (
	"context"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/safeurl"
)

// baseBranch returns the branch every base-relative pipeline operation is
// scoped to: rebase target, diff/review/test/document/lint scope, and PR base.
//
// It is the repository's trusted explicit base_branch when one is configured,
// otherwise the repository's default branch - so a repo with no explicit base
// behaves exactly as before. The value is validated by
// assertBaseBranchUsable before any step acts on it; this accessor is the
// single read point so a step cannot accidentally keep using DefaultBranch.
//
// Repo.DefaultBranch deliberately keeps its other jobs (trusted-config anchor
// in the daemon, telemetry branch role): the base a contributor merges into is
// not the same fact as the branch the maintainer's trusted config lives on.
func baseBranch(sctx *pipeline.StepContext) string {
	if sctx.Config != nil {
		if explicit := strings.TrimSpace(sctx.Config.BaseBranch); explicit != "" {
			return explicit
		}
	}
	if sctx.Repo == nil {
		return ""
	}
	return strings.TrimSpace(sctx.Repo.DefaultBranch)
}

// assertBaseBranchUsable fails closed when an explicit base is unsafe or would
// defeat the gate.
//
// Two rejections, both fatal to the run rather than merely logged:
//   - a syntactically unsafe or ambiguous ref (config.ValidateBaseBranch), so
//     nothing option-like or revision-operator-like ever reaches a git argv;
//   - a base equal to the run's own branch, which would make the reviewed diff
//     empty and open a self-targeting PR.
//
// A repo with no explicit base skips both checks and keeps current behavior.
func assertBaseBranchUsable(sctx *pipeline.StepContext) error {
	if sctx.Config == nil {
		return nil
	}
	explicit := strings.TrimSpace(sctx.Config.BaseBranch)
	if explicit == "" {
		return nil
	}
	if err := config.ValidateBaseBranch(explicit); err != nil {
		return fmt.Errorf("explicit base branch rejected: %w", err)
	}
	if sctx.Run != nil {
		branch := strings.TrimPrefix(strings.TrimSpace(sctx.Run.Branch), "refs/heads/")
		if branch != "" && branch == explicit {
			return fmt.Errorf("explicit base branch %q is the branch under validation; a branch cannot be its own base", explicit)
		}
	}
	return nil
}

// assertBaseBranchResolvable verifies an explicit base actually exists on the
// remote the run integrates with, and that it names exactly one ref. An
// unresolved or ambiguous base must stop the run before a rebase or PR rather
// than silently degrade to the default branch, which would rebase and open a
// PR against the wrong history.
//
// It is a no-op when no explicit base is configured: the default-branch path
// keeps its existing tolerant fetch-and-fall-back behavior.
func assertBaseBranchResolvable(ctx context.Context, sctx *pipeline.StepContext) error {
	if sctx.Config == nil {
		return nil
	}
	explicit := strings.TrimSpace(sctx.Config.BaseBranch)
	if explicit == "" {
		return nil
	}
	out, err := git.Run(ctx, sctx.WorkDir, "ls-remote", "--heads", resolveUpstreamURL(sctx), "refs/heads/"+explicit)
	if err != nil {
		return fmt.Errorf("could not resolve explicit base branch %q on the remote: %w", explicit, err)
	}
	var matches int
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) != "" {
			matches++
		}
	}
	switch {
	case matches == 0:
		return fmt.Errorf("explicit base branch %q does not exist on the remote", explicit)
	case matches > 1:
		return fmt.Errorf("explicit base branch %q is ambiguous on the remote (%d matching refs)", explicit, matches)
	}
	return nil
}

// PrepareExplicitBaseBranch validates, resolves, and fetches the trusted
// explicit integration base before pipeline step dispatch.
func PrepareExplicitBaseBranch(ctx context.Context, sctx *pipeline.StepContext) error {
	if err := assertBaseBranchUsable(sctx); err != nil {
		return err
	}
	if sctx.Config == nil || strings.TrimSpace(sctx.Config.BaseBranch) == "" {
		return nil
	}
	if err := assertBaseBranchResolvable(ctx, sctx); err != nil {
		return err
	}
	base := strings.TrimSpace(sctx.Config.BaseBranch)
	if err := fetchRunUpstreamBranch(ctx, sctx, base); err != nil {
		return fmt.Errorf("could not fetch explicit base branch %q: %w", base, err)
	}
	if _, err := git.ResolveRef(ctx, sctx.WorkDir, "refs/remotes/origin/"+base); err != nil {
		return fmt.Errorf("could not resolve fetched explicit base branch %q: %w", base, err)
	}
	return nil
}

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

func fetchRunUpstreamBranch(ctx context.Context, sctx *pipeline.StepContext, branch string) error {
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

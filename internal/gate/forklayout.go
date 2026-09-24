package gate

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/safeurl"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/scm/github"
	"github.com/kunchenguid/no-mistakes/internal/winproc"
)

// resolveForkParent is github.RepoParent wired to a real gh invocation. It is
// a package var so tests can substitute a fake without shelling out for real,
// matching ensureGateHooksPathIsolation above.
var resolveForkParent = func(ctx context.Context, remoteURL string) (parentSlug string, isFork bool, err error) {
	host := scm.ResolveHost(ctx, remoteURL)
	cmd := func(ctx context.Context, name string, args ...string) *exec.Cmd {
		c := exec.CommandContext(ctx, name, args...)
		winproc.Harden(c)
		return c
	}
	return github.RepoParent(ctx, cmd, host, remoteURL)
}

// refuseForkOriginMisrouting detects the fork layout `gh repo fork --clone`
// leaves behind - `origin` is the contributor's own fork and a separate
// `upstream` remote already names the real parent repository - and refuses
// loudly instead of letting a plain `no-mistakes init` (no --fork-url) treat
// the fork as the parent (issue #1178). Left undetected, that layout opens
// scratch PRs inside the contributor's fork and no attestation ever binds to
// their real PR against the parent.
//
// Detection is best-effort and fails OPEN, never closed: it only refuses when
// it has positive proof (a GitHub API "fork" + "parent" answer for origin that
// matches the upstream remote's own repo), and skips silently whenever it
// cannot get that proof - no "upstream" remote, either remote not on GitHub,
// gh unavailable/unauthenticated, offline, or origin genuinely not a fork of
// what upstream names. That mirrors every other init check: init has never
// required gh or network access, and a repo with an unrelated "upstream"
// remote (a template, not a fork parent) must keep initializing exactly as it
// always has.
//
// It only runs when fork routing is not already configured (no --fork-url on
// this call and no previously recorded fork), because a contributor who
// already named their fork has already told no-mistakes which remote is
// which; running it unconditionally would also refuse a deliberately
// configured origin=parent, --fork-url=fork setup that happens to also carry
// an unrelated "upstream" remote.
func refuseForkOriginMisrouting(ctx context.Context, absRoot string) error {
	// Read origin's literal configured URL rather than trusting a caller-
	// supplied value: InitWithFork resolves url.*.insteadOf rewrites for the
	// non-fork path (git.GetRemoteURL), which would hide the real GitHub host
	// this check needs behind whatever the rewrite points at.
	originURL, err := git.GetConfiguredRemoteURL(ctx, absRoot, "origin")
	if err != nil || strings.TrimSpace(originURL) == "" {
		return nil
	}
	if scm.DetectProviderContext(ctx, originURL) != scm.ProviderGitHub {
		return nil
	}
	hasUpstream, err := git.HasRemote(ctx, absRoot, "upstream")
	if err != nil || !hasUpstream {
		return nil
	}
	upstreamRemoteURL, err := git.GetConfiguredRemoteURL(ctx, absRoot, "upstream")
	if err != nil || strings.TrimSpace(upstreamRemoteURL) == "" {
		return nil
	}
	if scm.DetectProviderContext(ctx, upstreamRemoteURL) != scm.ProviderGitHub {
		return nil
	}
	// scm.ProviderGitHub covers any GitHub Enterprise host, not just
	// github.com, so an origin/upstream pair on different hosts must be
	// rejected explicitly - matching owner/name alone is not proof they name
	// the same repository.
	if !strings.EqualFold(scm.ResolveHost(ctx, originURL), scm.ResolveHost(ctx, upstreamRemoteURL)) {
		return nil
	}

	originOwner, originName, ok := splitRepoSlug(github.RepoSlug(originURL))
	if !ok {
		return nil
	}
	upstreamOwner, upstreamName, ok := splitRepoSlug(github.RepoSlug(upstreamRemoteURL))
	if !ok {
		return nil
	}
	// A genuine fork always keeps its parent's repo name; skip the network
	// call entirely for an unrelated "upstream" remote (a different project,
	// or a same-owner rename) rather than paying gh latency on every init.
	if strings.EqualFold(originOwner, upstreamOwner) || !strings.EqualFold(originName, upstreamName) {
		return nil
	}

	parentSlug, isFork, err := resolveForkParent(ctx, originURL)
	if err != nil || !isFork || strings.TrimSpace(parentSlug) == "" {
		return nil
	}
	parentOwner, parentName, ok := splitRepoSlug(parentSlug)
	if !ok || !strings.EqualFold(parentOwner, upstreamOwner) || !strings.EqualFold(parentName, upstreamName) {
		return nil
	}

	return fmt.Errorf(
		"origin (%s/%s) is a GitHub fork of %s, and your 'upstream' remote already points at that parent repository\n\n"+
			"no-mistakes needs 'origin' pointed at the parent repository, with your fork configured as a separate push target - "+
			"otherwise it opens pull requests and binds pipeline attestations inside your own fork instead of %s.\n\n"+
			"Fix:\n"+
			"  git remote set-url origin %s\n"+
			"  no-mistakes init --fork-url %s\n\n"+
			"See CONTRIBUTING.md for the full fork workflow.",
		originOwner, originName, parentSlug, parentSlug,
		safeurl.Redact(upstreamRemoteURL), safeurl.Redact(originURL))
}

// splitRepoSlug splits an "owner/name" slug, reporting ok=false for anything
// else (including the empty string RepoSlug returns for an unparseable URL).
func splitRepoSlug(slug string) (owner, name string, ok bool) {
	owner, name, found := strings.Cut(slug, "/")
	if !found || owner == "" || name == "" {
		return "", "", false
	}
	return owner, name, true
}

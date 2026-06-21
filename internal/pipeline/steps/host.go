package steps

import (
	"context"
	"fmt"
	"os/exec"

	"github.com/kunchenguid/no-mistakes/internal/bitbucket"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/scm/github"
	"github.com/kunchenguid/no-mistakes/internal/scm/gitlab"
)

// buildHost returns a scm.Host for the given provider, wired to sctx's
// working directory and environment. When the host cannot be constructed
// (unknown provider, missing Bitbucket config, etc) it returns nil and a
// human-readable skip reason suitable for logging.
func buildHost(sctx *pipeline.StepContext, provider scm.Provider) (scm.Host, string) {
	cmdFactory := func(_ context.Context, name string, args ...string) *exec.Cmd {
		return stepCmd(sctx, name, args...)
	}
	switch provider {
	case scm.ProviderGitHub:
		// Resolve the owner/name slug so gh commands carry --repo and work from
		// the daemon's fixed (non-repo) working directory. Fall back to the PR
		// URL when the upstream remote URL is unavailable.
		repo := github.RepoSlug(sctx.Repo.UpstreamURL)
		if repo == "" && sctx.Run.PRURL != nil {
			repo = github.RepoSlug(*sctx.Run.PRURL)
		}
		forkRepo := ""
		if sctx.Repo.ForkURL != "" {
			forkRepo = github.RepoSlug(sctx.Repo.ForkURL)
		}
		return github.NewWithFork(cmdFactory, func() bool { return stepCLIAvailable(sctx, provider) }, repo, forkRepo), ""
	case scm.ProviderGitLab:
		if sctx.Repo.ForkURL != "" {
			// Fork MR routing for GitLab is intentionally not half-wired.
			// The push step may use fork_url, but PR creation must skip until
			// GitLab source-project routing is implemented end to end.
			return nil, "fork PR routing for GitLab is not implemented"
		}
		return gitlab.New(cmdFactory, func() bool { return stepCLIAvailable(sctx, provider) }), ""
	case scm.ProviderBitbucket:
		if sctx.Repo.ForkURL != "" {
			// Fork PR routing for Bitbucket is intentionally not half-wired.
			// The API needs distinct source and destination repositories before
			// this provider can safely consume fork_url for PR creation.
			return nil, "fork PR routing for Bitbucket is not implemented"
		}
		client, err := bitbucket.NewClientFromEnv(sctx.Env)
		if err != nil {
			return nil, err.Error()
		}
		repo, err := resolveBitbucketRepoRef(sctx.Repo.UpstreamURL, sctx.Run.PRURL)
		if err != nil {
			return nil, err.Error()
		}
		return bitbucket.NewHost(client, repo), ""
	default:
		return nil, fmt.Sprintf("provider %s is not supported yet", provider)
	}
}

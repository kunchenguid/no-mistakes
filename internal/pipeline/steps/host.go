package steps

import (
	"context"
	"fmt"
	"os/exec"

	"github.com/kunchenguid/no-mistakes/internal/bitbucket"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/scm/azuredevops"
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
		// Resolve the slug so gh commands carry --repo and work from the
		// daemon's fixed (non-repo) working directory. For GitHub Enterprise
		// Server, HostPrefixedSlug returns "host/owner/name" which is the
		// format gh requires for --repo on GHE. Fall back to the PR URL when
		// the upstream remote URL is unavailable. The hostname also scopes
		// the auth-status check so a stale token on any other configured gh
		// host cannot make this repo look unauthenticated.
		host := scm.ExtractHost(sctx.Repo.UpstreamURL)
		repo := github.HostPrefixedSlug(sctx.Repo.UpstreamURL)
		if repo == "" && sctx.Run.PRURL != nil {
			repo = github.HostPrefixedSlug(*sctx.Run.PRURL)
			if host == "" {
				host = scm.ExtractHost(*sctx.Run.PRURL)
			}
		}
		forkRepo := ""
		if sctx.Repo.ForkURL != "" {
			// forkRepo is only used to extract the fork owner for --head owner:branch;
			// the plain slug (without host prefix) is correct here.
			forkRepo = github.RepoSlug(sctx.Repo.ForkURL)
		}
		return github.NewWithFork(cmdFactory, func() bool { return stepCLIAvailable(sctx, provider) }, host, repo, forkRepo), ""
	case scm.ProviderGitLab:
		if sctx.Repo.ForkURL != "" {
			// Fork MR routing for GitLab is intentionally not half-wired.
			// The push step may use fork_url, but PR creation must skip until
			// GitLab source-project routing is implemented end to end.
			return nil, "fork PR routing for GitLab is not implemented"
		}
		return gitlab.New(
			cmdFactory,
			func() bool { return stepCLIAvailable(sctx, provider) },
			scm.ExtractHost(sctx.Repo.UpstreamURL),
			gitlab.ProjectPath(sctx.Repo.UpstreamURL),
		), ""
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
	case scm.ProviderAzureDevOps:
		if sctx.Repo.ForkURL != "" {
			// Fork PR routing for Azure DevOps is intentionally not half-wired,
			// mirroring GitLab and Bitbucket: the push step may use fork_url, but
			// PR creation must skip until cross-repository routing is implemented
			// end to end.
			return nil, "fork PR routing for Azure DevOps is not implemented"
		}
		org, project, repo, ok := azuredevops.ParseRemote(sctx.Repo.UpstreamURL)
		if !ok && sctx.Run.PRURL != nil {
			org, project, repo, ok = azuredevops.ParseRemote(*sctx.Run.PRURL)
		}
		if !ok {
			return nil, "could not resolve Azure DevOps organization, project, and repository from the remote URL"
		}
		return azuredevops.New(cmdFactory, func() bool { return stepCLIAvailable(sctx, provider) }, org, project, repo), ""
	default:
		return nil, fmt.Sprintf("provider %s is not supported yet", provider)
	}
}

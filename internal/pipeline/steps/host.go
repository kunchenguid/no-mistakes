package steps

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

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
		host := scm.ResolveHost(sctx.Ctx, sctx.Repo.UpstreamURL)
		repo := github.HostPrefixedSlugForHost(sctx.Repo.UpstreamURL, host)
		if repo == "" && sctx.Run.PRURL != nil {
			prHost := scm.ResolveHost(sctx.Ctx, *sctx.Run.PRURL)
			repo = github.HostPrefixedSlugForHost(*sctx.Run.PRURL, prHost)
			if host == "" {
				host = prHost
			}
		}
		forkRepo := ""
		if sctx.Repo.ForkURL != "" {
			// forkRepo is only used to extract the fork owner for --head owner:branch;
			// the plain slug (without host prefix) is correct here.
			forkRepo = github.RepoSlug(sctx.Repo.ForkURL)
		}
		return github.NewWithFork(scmCLIFactory(sctx, cmdFactory), func() bool { return stepCLIAvailable(sctx, provider) }, host, repo, forkRepo), ""
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
			scm.ResolveHost(sctx.Ctx, sctx.Repo.UpstreamURL),
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

// scmCLIFactory applies the global scm settings to gh invocations, and only to
// gh: the GitLab and Bitbucket hosts are built with an unwrapped factory. The daemon
// execs gh directly from a fixed, non-repo working directory, so it never sees
// the login shell where a credential manager is wired up. When scm.cli_wrapper
// is set, gh runs under that wrapper (for example `op plugin run -- gh`) from
// the repo's own working path, so a wrapper that scopes credentials by
// directory hands back the identity belonging to that repo rather than a
// global default. When scm.gh_config_dir is set it replaces GH_CONFIG_DIR, so
// gh reads no stored accounts and its auth state is exactly the token the
// wrapper injected. Both unset returns base unchanged.
func scmCLIFactory(sctx *pipeline.StepContext, base github.CmdFactory) github.CmdFactory {
	if sctx.Config == nil {
		return base
	}
	wrapper := sctx.Config.SCM.CLIWrapper
	configDir := strings.TrimSpace(sctx.Config.SCM.GHConfigDir)
	if len(wrapper) == 0 && configDir == "" {
		return base
	}
	return func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if len(wrapper) > 0 {
			wrapped := make([]string, 0, len(wrapper)-1+1+len(args))
			wrapped = append(wrapped, wrapper[1:]...)
			wrapped = append(wrapped, name)
			wrapped = append(wrapped, args...)
			name, args = wrapper[0], wrapped
		}
		cmd := base(ctx, name, args...)
		// The wrapper resolves credentials relative to its working directory,
		// and gh addresses the repository through --repo rather than the cwd,
		// so pointing at the user's real checkout is safe and is what makes
		// directory-scoped credentials resolve correctly.
		if sctx.Repo != nil && sctx.Repo.WorkingPath != "" {
			cmd.Dir = sctx.Repo.WorkingPath
		}
		if configDir != "" {
			// A nil Env means "inherit"; appending to it would instead reduce
			// the child's environment to this single variable.
			if cmd.Env == nil {
				cmd.Env = os.Environ()
			}
			cmd.Env = append(cmd.Env, "GH_CONFIG_DIR="+configDir)
		}
		return cmd
	}
}

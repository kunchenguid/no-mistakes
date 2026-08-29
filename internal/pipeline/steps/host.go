package steps

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/bitbucket"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/scm/azuredevops"
	"github.com/kunchenguid/no-mistakes/internal/scm/forgejo"
	"github.com/kunchenguid/no-mistakes/internal/scm/gitea"
	"github.com/kunchenguid/no-mistakes/internal/scm/github"
	"github.com/kunchenguid/no-mistakes/internal/scm/gitlab"
)

// resolvedProvider returns the run-scoped provider selected by forge profile
// routing. Runs without a selected profile retain the legacy URL-based
// detection, including the PR URL fallback used during recovery.
func resolvedProvider(sctx *pipeline.StepContext) scm.Provider {
	if sctx.ForgeContext != nil {
		return sctx.ForgeContext.Provider
	}
	provider := detectProviderForStep(sctx, sctx.Repo.UpstreamURL)
	if provider == scm.ProviderUnknown && sctx.Run.PRURL != nil {
		provider = detectProviderForStep(sctx, *sctx.Run.PRURL)
	}
	return provider
}

func resolvedHost(sctx *pipeline.StepContext, remote string) string {
	if sctx.ForgeContext != nil && sctx.ForgeContext.Host != "" {
		return sctx.ForgeContext.Host
	}
	return scm.ResolveHost(sctx.Ctx, remote)
}

// buildHost returns a scm.Host for the given provider, wired to sctx's
// working directory and environment. When the host cannot be constructed
// (unknown provider, missing Bitbucket config, etc) it returns nil and a
// human-readable skip reason suitable for logging.
func buildHost(sctx *pipeline.StepContext, provider scm.Provider) (scm.Host, string) {
	cmdFactory := func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return stepCmdContext(sctx, ctx, name, args...)
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
		host := resolvedHost(sctx, sctx.Repo.UpstreamURL)
		repo := github.HostPrefixedSlugForHost(sctx.Repo.UpstreamURL, host)
		if repo == "" && sctx.Run.PRURL != nil {
			prHost := resolvedHost(sctx, *sctx.Run.PRURL)
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
			resolvedHost(sctx, sctx.Repo.UpstreamURL),
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
	case scm.ProviderForgejo:
		if sctx.Repo.ForkURL != "" {
			return nil, "fork PR routing for Forgejo is not implemented"
		}
		baseURL := forgejoBaseURLForStep(sctx)
		remote := sctx.Repo.UpstreamURL
		if strings.TrimSpace(remote) == "" && sctx.Run.PRURL != nil {
			remote = *sctx.Run.PRURL
		}
		resolvedBase, repo, err := forgejo.ResolveRemote(remote, baseURL, scm.ResolveHost(sctx.Ctx, remote))
		if err != nil {
			return nil, fmt.Sprintf("could not resolve Forgejo host and repository: %v", err)
		}
		executable := "forgejo-axi"
		if sctx.Config != nil && strings.TrimSpace(sctx.Config.ForgejoAXIPath) != "" {
			executable = strings.TrimSpace(sctx.Config.ForgejoAXIPath)
		}
		tokenEnv := forgejoTokenEnvForStep(sctx, resolvedBase)
		return forgejo.New(forgejo.Options{
			CommandFactory: cmdFactory,
			CLIAvailable:   func(name string) bool { return stepExecutableAvailable(sctx, name) },
			Executable:     executable,
			BaseURL:        resolvedBase,
			Repository:     repo,
			TokenEnv:       tokenEnv,
			Secrets:        forgejoTokenValuesForStep(sctx),
		}), ""
	case scm.ProviderGitea:
		if sctx.Repo.ForkURL != "" {
			// Fork PR routing for Gitea is intentionally not half-wired,
			// mirroring GitLab, Bitbucket, and Azure DevOps: cross-repository
			// routing needs distinct source/destination handling this
			// provider does not implement yet.
			return nil, "fork PR routing for Gitea is not implemented"
		}
		host := scm.ResolveHost(sctx.Ctx, sctx.Repo.UpstreamURL)
		repoSlug := scm.RepoPath(sctx.Repo.UpstreamURL)
		if repoSlug == "" {
			return nil, "could not resolve Gitea owner/repo from the remote URL"
		}
		// login comes from tea's own config.yml (see scm.ResolveGiteaLogin); an
		// empty login is tolerated here and surfaces as an actionable error
		// from Host.Available instead of failing host construction outright.
		login := scm.ResolveGiteaLogin(host)
		return gitea.New(cmdFactory, func() bool { return stepCLIAvailable(sctx, provider) }, host, login, repoSlug), ""
	default:
		return nil, fmt.Sprintf("provider %s is not supported yet", provider)
	}
}

// BuildHostForTest exposes buildHost to tests in other packages.
func BuildHostForTest(sctx *pipeline.StepContext, provider scm.Provider) (scm.Host, string) {
	return buildHost(sctx, provider)
}

func detectProviderForStep(sctx *pipeline.StepContext, remoteURL string) scm.Provider {
	return scm.DetectProviderContextWithForgejoBaseURL(sctx.Ctx, remoteURL, forgejoBaseURLForStep(sctx))
}

func forgejoBaseURLForStep(sctx *pipeline.StepContext) string {
	if value, ok := effectiveStepEnvValue(sctx, "FORGEJO_BASE_URL"); ok {
		return strings.TrimSpace(value)
	}
	return ""
}

func effectiveStepEnvValue(sctx *pipeline.StepContext, key string) (string, bool) {
	if sctx != nil {
		if value, ok := envValue(sctx.Env, key); ok {
			return value, true
		}
	}
	return os.LookupEnv(key)
}

// forgejoTokenEnvForStep mirrors forgejo-axi's documented host-key encoding so
// an explicit --base-url retains host-scoped token precedence.
func forgejoTokenEnvForStep(sctx *pipeline.StepContext, baseURL string) string {
	parsed, err := url.Parse(baseURL)
	if err == nil && parsed.Host != "" {
		var key strings.Builder
		key.WriteString("FORGEJO_TOKEN_")
		for _, char := range strings.ToUpper(parsed.Host) {
			if char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' {
				key.WriteRune(char)
			} else {
				fmt.Fprintf(&key, "_%X_", char)
			}
		}
		name := key.String()
		if value, ok := effectiveStepEnvValue(sctx, name); ok && value != "" {
			return name
		}
	}
	if value, ok := effectiveStepEnvValue(sctx, "FORGEJO_TOKEN"); ok && value != "" {
		return "FORGEJO_TOKEN"
	}
	return ""
}

func forgejoTokenValuesForStep(sctx *pipeline.StepContext) []string {
	env := os.Environ()
	if sctx != nil && len(sctx.Env) > 0 {
		env = mergeEnv(sctx.Env)
	}
	var secrets []string
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if ok && strings.HasPrefix(strings.ToUpper(key), "FORGEJO_TOKEN") && value != "" {
			secrets = append(secrets, value)
		}
	}
	return secrets
}

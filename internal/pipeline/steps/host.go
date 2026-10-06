package steps

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/bitbucket"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/safeurl"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/scm/azuredevops"
	"github.com/kunchenguid/no-mistakes/internal/scm/forgejo"
	"github.com/kunchenguid/no-mistakes/internal/scm/gitea"
	"github.com/kunchenguid/no-mistakes/internal/scm/github"
	"github.com/kunchenguid/no-mistakes/internal/scm/gitlab"
	"github.com/kunchenguid/no-mistakes/internal/scm/plugin"
)

// resolvedProvider returns the run-scoped provider selected by forge profile
// routing. Runs without a selected profile retain the legacy URL-based
// detection, including the PR URL fallback used during recovery. A provider
// plugin claiming the remote's host decides first: it is the operator's
// explicit machine-local choice, and a run whose forge profile also claims
// the repository never starts (config load refuses a shared literal host,
// forgecontext.RefuseProviderPluginOverlap one shared only after SSH alias
// resolution).
func resolvedProvider(sctx *pipeline.StepContext) scm.Provider {
	if name, ok := providerPluginForStep(sctx); ok {
		return scm.PluginProvider(name)
	}
	if sctx.ForgeContext != nil {
		return sctx.ForgeContext.Provider
	}
	provider := detectProviderForStep(sctx, sctx.Repo.UpstreamURL)
	if provider == scm.ProviderUnknown && sctx.Run.PRURL != nil {
		provider = detectProviderForStep(sctx, *sctx.Run.PRURL)
	}
	return provider
}

// providerPluginForStep returns the configured provider plugin that claims
// the run's upstream remote, falling back to the recorded PR URL like
// built-in detection does during recovery.
func providerPluginForStep(sctx *pipeline.StepContext) (string, bool) {
	if sctx.Config == nil || len(sctx.Config.ProviderPlugins) == 0 {
		return "", false
	}
	remote := providerPluginRemote(sctx)
	if remote == "" {
		return "", false
	}
	return sctx.Config.ProviderPlugins.Select(scm.ExtractHost(remote), func() string {
		return scm.ResolveHost(sctx.Ctx, remote)
	})
}

func providerPluginRemote(sctx *pipeline.StepContext) string {
	if remote := strings.TrimSpace(sctx.Repo.UpstreamURL); remote != "" {
		return remote
	}
	if sctx.Run.PRURL != nil {
		return strings.TrimSpace(*sctx.Run.PRURL)
	}
	return ""
}

// pluginContractBroken reports whether a provider error must fail the step
// rather than skip it or be retried like a transient read. A provider that
// says it cannot serve the repository (CLI missing, not authenticated) skips
// with that reason, which axi surfaces under run.automatic_skips and as
// passed-with-skips, and an ordinary failed read is polled again. A provider
// plugin that broke its contract (unreadable output, a protocol version this
// build does not speak, a mismatched PR identity, a timeout), in its status
// handshake or in any later call, is a broken operator-installed component:
// skipping would let a run read as passed-with-skips off an answer nothing
// validated, and polling it again only defers the failure to a timeout.
func pluginContractBroken(err error) bool {
	return errors.Is(err, plugin.ErrProtocol)
}

// pluginPollFailsStep is pluginContractBroken for the CI monitor's repeated
// polls and failed-log retrieval, minus a timeout (see plugin.ErrTimeout):
// the monitor already retries a failed poll and a repair already reports
// missing logs, and neither passes on one, so only a deterministic violation
// fails the step there. One-shot reads use pluginContractBroken.
func pluginPollFailsStep(err error) bool {
	return pluginContractBroken(err) && !errors.Is(err, plugin.ErrTimeout)
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
	if name, ok := provider.PluginName(); ok {
		return buildPluginHost(sctx, name, cmdFactory)
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
			forkRepo = github.RepoSlug(sctx.Repo.ForkURL)
		}
		draft := sctx.Config != nil && sctx.Config.Providers.GitHub.DraftPullRequests
		return github.NewWithFork(cmdFactory, func() bool { return stepCLIAvailable(sctx, provider) }, host, repo, forkRepo, draft), ""
	case scm.ProviderGitLab:
		if sctx.Repo.ForkURL != "" {
			// Fork MR routing for GitLab is intentionally not half-wired.
			// The push step may use fork_url, but PR creation must skip until
			// GitLab source-project routing is implemented end to end.
			return nil, "fork PR routing for GitLab is not implemented"
		}
		draft := sctx.Config != nil && sctx.Config.Providers.GitLab.DraftPullRequests
		return gitlab.NewWithDraft(
			cmdFactory,
			func() bool { return stepCLIAvailable(sctx, provider) },
			resolvedHost(sctx, sctx.Repo.UpstreamURL),
			gitlab.ProjectPath(sctx.Repo.UpstreamURL),
			draft,
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
		draft := sctx.Config != nil && sctx.Config.Providers.Bitbucket.DraftPullRequests
		return bitbucket.NewHost(client, repo, draft), ""
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
		draft := sctx.Config != nil && sctx.Config.Providers.AzureDevOps.DraftPullRequests
		return azuredevops.NewWithDraft(cmdFactory, func() bool { return stepCLIAvailable(sctx, provider) }, org, project, repo, draft), ""
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

// buildPluginHost wires an operator-configured provider plugin. The plugin
// runs through the same step command factory as built-in provider CLIs, so it
// inherits the run's environment (and any forge-profile overlay) and the
// worktree as its working directory.
func buildPluginHost(sctx *pipeline.StepContext, name string, cmdFactory plugin.CmdFactory) (scm.Host, string) {
	if sctx.Config == nil {
		return nil, fmt.Sprintf("provider plugin %q is not configured", name)
	}
	cfg, ok := sctx.Config.ProviderPlugins[name]
	if !ok {
		return nil, fmt.Sprintf("provider plugin %q is not configured", name)
	}
	if sctx.Repo.ForkURL != "" {
		// Fork PR routing is intentionally not half-wired, mirroring every
		// non-GitHub provider: a plugin gets one repository identity and
		// would otherwise open a self PR.
		return nil, fmt.Sprintf("fork PR routing for provider plugin %q is not implemented", name)
	}
	remote := providerPluginRemote(sctx)
	if remote == "" {
		return nil, fmt.Sprintf("provider plugin %q: no upstream remote or PR URL to identify the repository", name)
	}
	repoPath := scm.RepoPath(remote)
	if repoPath == "" {
		// Every returned PR URL is checked against this path, so a remote
		// without one could never yield a valid answer.
		return nil, fmt.Sprintf("provider plugin %q: could not resolve the repository path from the remote URL", name)
	}
	return plugin.New(plugin.Options{
		Name:       name,
		Executable: cfg.Command,
		Args:       cfg.Args,
		Repository: plugin.Repository{
			RemoteURL: safeurl.Redact(remote),
			Host:      scm.ResolveHost(sctx.Ctx, remote),
			RawHost:   scm.ExtractHost(remote),
			Path:      repoPath,
		},
		Timeout:             cfg.Timeout,
		DraftPullRequests:   cfg.DraftPullRequests,
		CommandFactory:      cmdFactory,
		ExecutableAvailable: func(executable string) bool { return stepExecutableAvailable(sctx, executable) },
	}), ""
}

// PublicationHost shares the pipeline's provider, repository and fork routing
// with explicit custody/publication operations. Unsupported routing refuses.
func PublicationHost(sctx *pipeline.StepContext) (scm.Host, string) {
	scopedCtx, scopedRepo := *sctx, *sctx.Repo
	scopedRepo.URLsVerified = true
	scopedCtx.Repo = &scopedRepo
	return buildHost(&scopedCtx, resolvedProvider(&scopedCtx))
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

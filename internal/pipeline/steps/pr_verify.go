package steps

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/forgecontext"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/scm/azuredevops"
	"github.com/kunchenguid/no-mistakes/internal/scm/forgejo"
	"github.com/kunchenguid/no-mistakes/internal/scm/origin"
)

// PRAppendixVerification reports the publication parser's ownership decision.
// A matching digest is an accidental-edit guard, not authentication or proof
// that the attestation describes the PR's current head.
type PRAppendixVerification struct {
	Verdict string `json:"verdict"`
	Reason  string `json:"reason"`
}

// VerifyPRAppendix never changes the supplied body. Publication remains the
// sole owner of marker parsing and legacy digest recovery.
func VerifyPRAppendix(body string) PRAppendixVerification {
	parts, err := parsePROwnedBody(body)
	switch {
	case !hasPRAppendixMarkers(body):
		reason := "no owned PR appendix is present"
		if err != nil {
			reason = err.Error()
		}
		return PRAppendixVerification{"absent", reason}
	case errors.Is(err, errPRAppendixMismatch):
		return PRAppendixVerification{"mismatch", err.Error()}
	case err != nil:
		return PRAppendixVerification{"malformed", err.Error()}
	case parts.managed:
		return PRAppendixVerification{"match", "owned PR appendix accepted by the publication parser"}
	default:
		return PRAppendixVerification{"absent", "no owned PR appendix is present"}
	}
}

type PRAppendixReader func(context.Context, string) (scm.PRContent, error)

// NewPRAppendixReader uses the publication provider adapters without opening a
// run, database, or daemon connection. Only the read interface escapes.
func NewPRAppendixReader(ctx context.Context, cfg *config.Config, repoURL string) (PRAppendixReader, error) {
	forge, err := forgecontext.Resolve(ctx, cfg.ForgeProfiles, repoURL, "")
	if err != nil {
		return nil, err
	}
	if err := forgecontext.RefuseProviderPluginOverlap(ctx, forge, cfg.ProviderPlugins, repoURL); err != nil {
		return nil, err
	}
	sctx := &pipeline.StepContext{
		Ctx: ctx, Config: cfg, ForgeContext: forge,
		Repo: &db.Repo{UpstreamURL: repoURL}, Run: &db.Run{},
	}
	provider := resolvedProvider(sctx)
	host, reason := buildHost(sctx, provider)
	if host == nil {
		return nil, fmt.Errorf("cannot read PR content: %s", reason)
	}
	if err := host.Available(ctx); err != nil {
		return nil, fmt.Errorf("cannot read PR content: %w", err)
	}
	reader, ok := host.(scm.PRRepositoryContentReader)
	if !ok {
		return nil, fmt.Errorf("provider does not support repository-bound numbered PR reads")
	}
	expected := scm.RepositoryIdentity{Host: resolvedHost(sctx, repoURL), Path: scm.RepoPath(repoURL)}
	remote, _ := url.Parse(repoURL)
	compareWebAuthority := !provider.IsPlugin() && remote != nil && (remote.Scheme == "http" || remote.Scheme == "https")
	if compareWebAuthority {
		remote.User = nil
		expected.Host = scm.RepositoryIdentityFromURL(remote.String()).Host
	}
	switch provider {
	case scm.ProviderAzureDevOps:
		org, project, repo, ok := azuredevops.ParseRemote(repoURL)
		if !ok {
			return nil, fmt.Errorf("cannot resolve requested Azure repository identity")
		}
		expected = azuredevops.RepositoryIdentity(org, project, repo)
	case scm.ProviderOrigin:
		expected = scm.RepositoryIdentity{Host: "origin.cursor.com", Path: origin.RepoSlug(repoURL)}
	case scm.ProviderForgejo:
		base, repo, err := forgejo.ResolveRemoteWithSSHDomain(repoURL, forgejoBaseURLForStep(sctx), scm.ResolveHost(ctx, repoURL), forgejoSSHDomainForStep(sctx))
		if err != nil {
			return nil, err
		}
		expected = scm.RepositoryIdentityFromURL(strings.TrimRight(base, "/") + "/" + repo)
	}
	if expected.Host == "" || expected.Path == "" {
		return nil, fmt.Errorf("requested repository identity is incomplete")
	}
	return func(ctx context.Context, number string) (scm.PRContent, error) {
		result, err := reader.GetPRRepositoryContent(ctx, number)
		if err != nil {
			return scm.PRContent{}, err
		}
		// This is the single gate between every adapter's structured read and
		// the parser. No content escapes on missing or mismatched identity.
		if result.Repository.Host == "" || result.Repository.Path == "" {
			return scm.PRContent{}, fmt.Errorf("PR read did not report a repository identity")
		}
		observedHost, expectedHost := result.Repository.Host, expected.Host
		if !compareWebAuthority {
			observedHost = scm.ExtractHost("https://" + observedHost)
			expectedHost = scm.ExtractHost("https://" + expectedHost)
		}
		if !strings.EqualFold(observedHost, expectedHost) || !strings.EqualFold(result.Repository.Path, expected.Path) {
			return scm.PRContent{}, fmt.Errorf("PR repository identity does not match --repo")
		}
		return result.Content, nil
	}, nil
}

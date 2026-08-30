package publication

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/forgecontext"
	"github.com/kunchenguid/no-mistakes/internal/runenv"
	"github.com/kunchenguid/no-mistakes/internal/scm"
)

const (
	defaultPublicationSessionResolveTimeout = 15 * time.Second
	maxPublicationSessionResolveTimeout     = 30 * time.Second
	maxGitHubTokenBytes                     = 64 << 10
)

// PublicationEnvironmentResolver returns the trusted, repository-scoped
// subprocess environment for one exact publication operation.
type PublicationEnvironmentResolver interface {
	ResolveEnvironment(ctx context.Context, publicationID, repositoryID string) (runenv.Overlay, error)
}

// GitHubV1Session is a transient, repository-scoped HTTP session. Routing
// ports consume it immediately and never persist it.
type GitHubV1Session struct {
	HTTPClient *http.Client
	APIBaseURL string
	Token      string
}

// GitHubV1SessionResolver lazily resolves a session for one exact publication
// and registered repository pair.
type GitHubV1SessionResolver interface {
	ResolveGitHubV1Session(ctx context.Context, publicationID, repositoryID string) (GitHubV1Session, error)
}

type GitHubV1RoutingPortOptions struct {
	DB       *db.DB
	Sessions GitHubV1SessionResolver
}

// GitHubV1RoutingPort implements both PRPort and CIPort without retaining a
// daemon-global credential or API endpoint.
type GitHubV1RoutingPort struct {
	db       *db.DB
	sessions GitHubV1SessionResolver
}

func NewGitHubV1RoutingPort(options GitHubV1RoutingPortOptions) (*GitHubV1RoutingPort, error) {
	if options.DB == nil {
		return nil, fmt.Errorf("GitHub routing port database is required")
	}
	if options.Sessions == nil {
		return nil, fmt.Errorf("GitHub routing port session resolver is required")
	}
	return &GitHubV1RoutingPort{db: options.DB, sessions: options.Sessions}, nil
}

func (p *GitHubV1RoutingPort) CreateExact(ctx context.Context, request PREffectRequest) error {
	port, err := p.portFor(ctx, request.PublicationID, request.RepositoryID)
	if err != nil {
		return err
	}
	return port.CreateExact(ctx, request)
}

func (p *GitHubV1RoutingPort) FindExact(ctx context.Context, query PRReconcileQuery) ([]PRObservation, error) {
	port, err := p.portFor(ctx, query.PublicationID, query.RepositoryID)
	if err != nil {
		return nil, err
	}
	return port.FindExact(ctx, query)
}

func (p *GitHubV1RoutingPort) ObserveExact(ctx context.Context, query CIQuery) (CIObservation, error) {
	repositoryID, err := p.boundRepositoryID(query.PublicationID, "")
	if err != nil {
		return CIObservation{}, err
	}
	port, err := p.portFor(ctx, query.PublicationID, repositoryID)
	if err != nil {
		return CIObservation{}, err
	}
	return port.ObserveExact(ctx, query)
}

func (p *GitHubV1RoutingPort) portFor(ctx context.Context, publicationID, repositoryID string) (*GitHubV1Port, error) {
	boundRepositoryID, err := p.boundRepositoryID(publicationID, repositoryID)
	if err != nil {
		return nil, err
	}
	session, err := p.sessions.ResolveGitHubV1Session(ctx, publicationID, boundRepositoryID)
	if err != nil {
		return nil, fmt.Errorf("resolve repository-scoped GitHub publication session: %w", err)
	}
	port, err := NewGitHubV1Port(GitHubV1PortOptions{
		DB: p.db, HTTPClient: session.HTTPClient, APIBaseURL: session.APIBaseURL, Token: session.Token,
	})
	if err != nil {
		return nil, fmt.Errorf("construct repository-scoped GitHub publication port: %w", err)
	}
	return port, nil
}

func (p *GitHubV1RoutingPort) boundRepositoryID(publicationID, requestedRepositoryID string) (string, error) {
	publicationRow, err := p.db.GetPublication(publicationID)
	if err != nil {
		return "", fmt.Errorf("load routed publication: %w", err)
	}
	if publicationRow == nil {
		return "", fmt.Errorf("routed publication is not registered")
	}
	parsed, err := ParseRequest(publicationRow.CanonicalRequest)
	if err != nil || parsed.PublicationID != publicationRow.PublicationID ||
		parsed.Request.Candidate.RepositoryID != publicationRow.RepoID ||
		parsed.Request.Candidate.HeadRef != publicationRow.CandidateRef ||
		parsed.Request.Candidate.BaseRef != publicationRow.BaseRef ||
		parsed.Request.Candidate.CommitSHA != publicationRow.HeadSHA ||
		parsed.Request.Candidate.BaseSHA != publicationRow.BaseSHA ||
		parsed.Request.Candidate.TreeSHA != publicationRow.TreeSHA {
		return "", fmt.Errorf("routed publication binding is inconsistent")
	}
	if requestedRepositoryID != "" && requestedRepositoryID != publicationRow.RepoID {
		return "", fmt.Errorf("routed publication repository does not match the exact binding")
	}
	repo, err := p.db.GetRepo(publicationRow.RepoID)
	if err != nil {
		return "", fmt.Errorf("load routed publication repository: %w", err)
	}
	if repo == nil || strings.TrimSpace(repo.ForkURL) != "" {
		return "", fmt.Errorf("routed publication requires one registered non-fork repository")
	}
	return publicationRow.RepoID, nil
}

// GitHubTokenCommand obtains the raw stdout of `gh auth token --hostname` in
// one trusted forge-profile environment.
type GitHubTokenCommand func(ctx context.Context, host string, environment runenv.Overlay) (string, error)

type ForgePublicationSessionResolverOptions struct {
	DB             *db.DB
	Profiles       config.ForgeProfiles
	HTTPClient     *http.Client
	TokenCommand   GitHubTokenCommand
	ResolveTimeout time.Duration
}

// ForgePublicationSessionResolver is both the Git transport environment
// resolver and the lazy GitHub HTTP session resolver used by daemon
// composition. It stores profiles, never resolved tokens.
type ForgePublicationSessionResolver struct {
	db             *db.DB
	profiles       config.ForgeProfiles
	client         *http.Client
	tokenCommand   GitHubTokenCommand
	resolveTimeout time.Duration
}

func NewForgePublicationSessionResolver(options ForgePublicationSessionResolverOptions) (*ForgePublicationSessionResolver, error) {
	if options.DB == nil {
		return nil, fmt.Errorf("publication session resolver database is required")
	}
	timeout := options.ResolveTimeout
	if timeout == 0 {
		timeout = defaultPublicationSessionResolveTimeout
	}
	if timeout < 0 || timeout > maxPublicationSessionResolveTimeout {
		return nil, fmt.Errorf("publication session resolve timeout must be positive and at most %s", maxPublicationSessionResolveTimeout)
	}
	client := &http.Client{Timeout: defaultPublicationSessionResolveTimeout}
	if options.HTTPClient != nil {
		cloned := *options.HTTPClient
		if cloned.Timeout <= 0 || cloned.Timeout > maxPublicationSessionResolveTimeout {
			cloned.Timeout = defaultPublicationSessionResolveTimeout
		}
		client = &cloned
	}
	command := options.TokenCommand
	if command == nil {
		command = defaultGitHubTokenCommand
	}
	profiles := make(config.ForgeProfiles, len(options.Profiles))
	for host, profile := range options.Profiles {
		profiles[host] = profile
	}
	return &ForgePublicationSessionResolver{
		db: options.DB, profiles: profiles, client: client, tokenCommand: command, resolveTimeout: timeout,
	}, nil
}

func (r *ForgePublicationSessionResolver) ResolveEnvironment(ctx context.Context, publicationID, repositoryID string) (runenv.Overlay, error) {
	_, forgeCtx, _, err := r.resolveRoute(ctx, publicationID, repositoryID)
	if err != nil {
		return runenv.Overlay{}, err
	}
	if forgeCtx == nil {
		return runenv.Overlay{}, nil
	}
	return forgeCtx.Environment.Clone(), nil
}

func (r *ForgePublicationSessionResolver) ResolveGitHubV1Session(ctx context.Context, publicationID, repositoryID string) (GitHubV1Session, error) {
	_, forgeCtx, host, err := r.resolveRoute(ctx, publicationID, repositoryID)
	if err != nil {
		return GitHubV1Session{}, err
	}
	environment := runenv.Overlay{}
	if forgeCtx != nil {
		environment = forgeCtx.Environment.Clone()
	}
	resolveCtx, cancel := context.WithTimeout(ctx, r.resolveTimeout)
	defer cancel()
	rawToken, commandErr := r.tokenCommand(resolveCtx, host, environment)
	if commandErr != nil {
		return GitHubV1Session{}, fmt.Errorf("resolve GitHub publication credential failed")
	}
	token, err := canonicalGitHubCommandToken(rawToken)
	if err != nil {
		return GitHubV1Session{}, err
	}
	return GitHubV1Session{
		HTTPClient: r.client, APIBaseURL: githubAPIBaseForHost(host), Token: token,
	}, nil
}

func (r *ForgePublicationSessionResolver) resolveRoute(
	ctx context.Context, publicationID, repositoryID string,
) (*db.Repo, *forgecontext.Context, string, error) {
	publicationRow, err := r.db.GetPublication(publicationID)
	if err != nil {
		return nil, nil, "", fmt.Errorf("load publication route: %w", err)
	}
	if publicationRow == nil || publicationRow.RepoID != repositoryID {
		return nil, nil, "", fmt.Errorf("publication route does not match a registered exact binding")
	}
	parsed, err := ParseRequest(publicationRow.CanonicalRequest)
	if err != nil || parsed.PublicationID != publicationRow.PublicationID ||
		parsed.Request.Candidate.RepositoryID != repositoryID ||
		parsed.Request.Candidate.HeadRef != publicationRow.CandidateRef ||
		parsed.Request.Candidate.BaseRef != publicationRow.BaseRef ||
		parsed.Request.Candidate.CommitSHA != publicationRow.HeadSHA ||
		parsed.Request.Candidate.BaseSHA != publicationRow.BaseSHA ||
		parsed.Request.Candidate.TreeSHA != publicationRow.TreeSHA {
		return nil, nil, "", fmt.Errorf("publication route binding is inconsistent")
	}
	repo, err := r.db.GetRepo(repositoryID)
	if err != nil {
		return nil, nil, "", fmt.Errorf("load publication route repository: %w", err)
	}
	if repo == nil {
		return nil, nil, "", fmt.Errorf("publication route repository is not registered")
	}
	if strings.TrimSpace(repo.ForkURL) != "" {
		return nil, nil, "", fmt.Errorf("factory publication v1 does not support fork routing")
	}
	registeredIdentity, registeredErr := canonicalEffectRemoteIdentity(repo.UpstreamURL)
	boundIdentity, boundErr := canonicalEffectRemoteIdentity(parsed.Request.Scopes.Push.RemoteIdentity)
	if registeredErr != nil || boundErr != nil || registeredIdentity != boundIdentity {
		return nil, nil, "", fmt.Errorf("publication route remote does not match the canonical request")
	}
	forgeCtx, err := forgecontext.Resolve(ctx, r.profiles, repo.UpstreamURL, repo.ForkURL)
	if err != nil {
		return nil, nil, "", fmt.Errorf("resolve publication forge profile: %w", err)
	}
	host := scm.ResolveHost(ctx, repo.UpstreamURL)
	provider := scm.DetectProviderStaticContext(ctx, repo.UpstreamURL)
	if forgeCtx != nil {
		host = forgeCtx.Host
		provider = forgeCtx.Provider
	}
	if provider != scm.ProviderGitHub || strings.TrimSpace(host) == "" {
		return nil, nil, "", fmt.Errorf("factory publication v1 requires an exact GitHub repository route")
	}
	return repo, forgeCtx, strings.ToLower(strings.TrimSpace(host)), nil
}

func githubAPIBaseForHost(host string) string {
	if strings.EqualFold(host, "github.com") {
		return "https://api.github.com"
	}
	return (&url.URL{Scheme: "https", Host: host, Path: "/api/v3"}).String()
}

func canonicalGitHubCommandToken(raw string) (string, error) {
	if len(raw) > maxGitHubTokenBytes {
		return "", fmt.Errorf("GitHub publication credential output exceeds its bound")
	}
	token := raw
	if strings.HasSuffix(token, "\r\n") {
		token = strings.TrimSuffix(token, "\r\n")
	} else if strings.HasSuffix(token, "\n") {
		token = strings.TrimSuffix(token, "\n")
	}
	if token == "" || strings.IndexFunc(token, func(char rune) bool { return char < 0x21 || char > 0x7e }) >= 0 {
		return "", fmt.Errorf("GitHub publication credential is missing or malformed")
	}
	return token, nil
}

func defaultGitHubTokenCommand(ctx context.Context, host string, environment runenv.Overlay) (string, error) {
	command := exec.CommandContext(ctx, "gh", "auth", "token", "--hostname", host)
	command.Env = environment.Apply(nil)
	stdout, err := command.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("prepare GitHub token command")
	}
	command.Stderr = io.Discard
	if err := command.Start(); err != nil {
		return "", fmt.Errorf("start GitHub token command")
	}
	raw, readErr := io.ReadAll(io.LimitReader(stdout, maxGitHubTokenBytes+1))
	if readErr != nil || len(raw) > maxGitHubTokenBytes {
		_ = command.Process.Kill()
		_ = command.Wait()
		return "", fmt.Errorf("read GitHub token command")
	}
	if err := command.Wait(); err != nil {
		return "", fmt.Errorf("GitHub token command failed")
	}
	return string(raw), nil
}

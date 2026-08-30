package publication

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/db"
	gitutil "github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/runenv"
)

// GitPushPortOptions binds the exact-push port to the existing repository
// registry. No configured worktree ref or HEAD is used as a push source.
type GitPushPortOptions struct {
	DB                  *db.DB
	EnvironmentResolver PublicationEnvironmentResolver
}

// GitPushPort publishes one immutable commit object to one exact full branch
// ref and observes that same remote ref without fetching or rewriting it.
type GitPushPort struct {
	db                  *db.DB
	environmentResolver PublicationEnvironmentResolver
}

func NewGitPushPort(options GitPushPortOptions) (*GitPushPort, error) {
	if options.DB == nil {
		return nil, fmt.Errorf("Git push port database is required")
	}
	return &GitPushPort{db: options.DB, environmentResolver: options.EnvironmentResolver}, nil
}

func (p *GitPushPort) PublishExact(ctx context.Context, request PushEffectRequest) error {
	publication, repo, remote, gitCtx, err := p.validateRequest(ctx, request)
	if err != nil {
		return err
	}
	if err := verifyRegisteredCandidate(gitCtx, repo.WorkingPath, publication.CandidateRef, publication.HeadSHA, publication.TreeSHA); err != nil {
		return fmt.Errorf("revalidate exact candidate before push: %w", err)
	}
	if _, err := gitutil.Run(gitCtx, repo.WorkingPath, "push", "--no-verify", remote, request.CommitSHA+":"+request.DestinationRef); err != nil {
		return fmt.Errorf("push immutable publication commit: %w", err)
	}
	return nil
}

func (p *GitPushPort) ObserveExact(ctx context.Context, request PushEffectRequest) (PushObservation, error) {
	_, repo, remote, gitCtx, err := p.validateRequest(ctx, request)
	if err != nil {
		return PushObservation{}, err
	}
	raw, err := gitutil.RunRaw(gitCtx, repo.WorkingPath, "ls-remote", "--refs", remote, request.DestinationRef)
	if err != nil {
		return PushObservation{}, fmt.Errorf("observe exact publication ref: %w", err)
	}
	var matches []string
	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		fields := bytes.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 2 || string(fields[1]) != request.DestinationRef || !isLowerHex(string(fields[0]), 40) {
			return PushObservation{}, fmt.Errorf("malformed exact publication ref observation")
		}
		matches = append(matches, string(fields[0]))
	}
	switch len(matches) {
	case 0:
		return PushObservation{}, nil
	case 1:
		return PushObservation{RemoteHeadSHA: matches[0]}, nil
	default:
		return PushObservation{}, fmt.Errorf("ambiguous exact publication ref observation")
	}
}

func (p *GitPushPort) validateRequest(ctx context.Context, request PushEffectRequest) (*db.Publication, *db.Repo, string, context.Context, error) {
	if !isLowerHex(request.PublicationID, 64) {
		return nil, nil, "", nil, fmt.Errorf("push publication ID is not a lowercase SHA-256")
	}
	if request.RepositoryID == "" {
		return nil, nil, "", nil, fmt.Errorf("push repository ID is required")
	}
	if !isLowerHex(request.CommitSHA, 40) {
		return nil, nil, "", nil, fmt.Errorf("push commit is not a full lowercase SHA")
	}
	if !isFullBranchRef(request.DestinationRef) {
		return nil, nil, "", nil, fmt.Errorf("push destination is not a full branch ref")
	}
	if !isLowerHex(request.EffectDigest, 64) {
		return nil, nil, "", nil, fmt.Errorf("push effect digest is not a lowercase SHA-256")
	}
	publication, err := p.db.GetPublication(request.PublicationID)
	if err != nil {
		return nil, nil, "", nil, fmt.Errorf("load push publication: %w", err)
	}
	if publication == nil {
		return nil, nil, "", nil, fmt.Errorf("push publication %s is not registered", request.PublicationID)
	}
	parsed, err := ParseRequest(publication.CanonicalRequest)
	if err != nil {
		return nil, nil, "", nil, fmt.Errorf("parse stored push publication: %w", err)
	}
	if parsed.PublicationID != publication.PublicationID ||
		request.RepositoryID != publication.RepoID ||
		request.CommitSHA != publication.HeadSHA ||
		request.RemoteIdentity != parsed.Request.Scopes.Push.RemoteIdentity ||
		request.DestinationRef != parsed.Request.Scopes.Push.DestinationRef {
		return nil, nil, "", nil, fmt.Errorf("push request does not match the exact publication binding")
	}
	effect, err := p.db.GetPublicationEffect(request.PublicationID, db.PublicationEffectPush)
	if err != nil {
		return nil, nil, "", nil, fmt.Errorf("load durable push effect: %w", err)
	}
	if effect == nil || effect.Binding.CandidateSHA != request.CommitSHA ||
		effect.Binding.RemoteIdentity != request.RemoteIdentity ||
		effect.Binding.DestinationRef != request.DestinationRef ||
		effect.Binding.HeadRef != parsed.Request.Candidate.HeadRef ||
		effect.Binding.BaseRef != "" || effect.Binding.DraftDigest != "" ||
		len(effect.PreparedPayload) != 0 || effect.Binding.EffectDigest != request.EffectDigest {
		return nil, nil, "", nil, fmt.Errorf("push request does not match the durable effect journal")
	}
	boundDigest := effect.Binding.EffectDigest
	recomputed := effect.Binding
	recomputed.EffectDigest = ""
	wantDigest, err := effectDigest(recomputed)
	if err != nil || boundDigest != wantDigest {
		return nil, nil, "", nil, fmt.Errorf("durable push effect digest is inconsistent")
	}
	repo, err := p.db.GetRepo(request.RepositoryID)
	if err != nil {
		return nil, nil, "", nil, fmt.Errorf("load push repository: %w", err)
	}
	if repo == nil {
		return nil, nil, "", nil, fmt.Errorf("push repository %s is not registered", request.RepositoryID)
	}
	remote := strings.TrimSpace(repo.PushURL())
	boundIdentity, err := canonicalEffectRemoteIdentity(remote)
	if err != nil {
		return nil, nil, "", nil, fmt.Errorf("resolve registered push remote: %w", err)
	}
	requestIdentity, err := canonicalEffectRemoteIdentity(request.RemoteIdentity)
	if err != nil {
		return nil, nil, "", nil, fmt.Errorf("resolve bound push remote identity: %w", err)
	}
	if requestIdentity != boundIdentity {
		return nil, nil, "", nil, fmt.Errorf("push remote identity does not match the registered remote")
	}
	gitCtx, err := p.resolveGitEnvironment(ctx, request.PublicationID, request.RepositoryID)
	if err != nil {
		return nil, nil, "", nil, err
	}
	resolvedRemote, err := gitutil.Run(gitCtx, repo.WorkingPath, "ls-remote", "--get-url", remote)
	if err != nil {
		return nil, nil, "", nil, fmt.Errorf("resolve effective Git push route: %w", err)
	}
	if strings.ContainsAny(resolvedRemote, "\r\n") {
		return nil, nil, "", nil, fmt.Errorf("effective Git push route is ambiguous")
	}
	resolvedIdentity, err := canonicalEffectRemoteIdentity(resolvedRemote)
	if err != nil || resolvedIdentity != requestIdentity {
		return nil, nil, "", nil, fmt.Errorf("effective Git push route does not match the exact remote identity")
	}
	return publication, repo, remote, gitCtx, nil
}

func (p *GitPushPort) resolveGitEnvironment(ctx context.Context, publicationID, repositoryID string) (context.Context, error) {
	if p.environmentResolver == nil {
		return isolatedGitContext(ctx), nil
	}
	overlay, err := p.environmentResolver.ResolveEnvironment(ctx, publicationID, repositoryID)
	if err != nil {
		return nil, fmt.Errorf("resolve trusted Git publication environment: %w", err)
	}
	return isolatedGitContextWithOverlay(ctx, overlay), nil
}

func isolatedGitContextWithOverlay(ctx context.Context, overlay runenv.Overlay) context.Context {
	overlay = overlay.Clone()
	if overlay.Set == nil {
		overlay.Set = make(map[string]string)
	}
	overlay.Set["GIT_NO_REPLACE_OBJECTS"] = "1"
	selectors := []string{
		"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_COMMON_DIR", "GIT_DIR", "GIT_INDEX_FILE",
		"GIT_NAMESPACE", "GIT_OBJECT_DIRECTORY", "GIT_PREFIX", "GIT_REPLACE_REF_BASE", "GIT_WORK_TREE",
	}
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "GIT_CONFIG_") {
			selectors = append(selectors, key)
		}
	}
	for key := range overlay.Set {
		if strings.HasPrefix(key, "GIT_CONFIG_") {
			selectors = append(selectors, key)
		}
	}
	for _, key := range selectors {
		delete(overlay.Set, key)
		overlay.Unset = append(overlay.Unset, key)
	}
	return gitutil.WithEnvironment(ctx, overlay)
}

func canonicalEffectRemoteIdentity(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed != raw || strings.IndexFunc(trimmed, func(r rune) bool { return r <= ' ' || r == 0x7f }) >= 0 {
		return "", fmt.Errorf("remote identity is empty or non-canonical")
	}
	if strings.Contains(trimmed, "://") {
		parsed, err := url.Parse(trimmed)
		if err != nil || parsed.Hostname() == "" || parsed.RawQuery != "" || parsed.Fragment != "" {
			return "", fmt.Errorf("remote URL is not an uncredentialed canonical URL")
		}
		switch strings.ToLower(parsed.Scheme) {
		case "http", "https":
			if parsed.User != nil {
				return "", fmt.Errorf("HTTP remote URL contains credentials")
			}
		case "ssh", "git":
			if parsed.User != nil {
				if _, hasPassword := parsed.User.Password(); hasPassword {
					return "", fmt.Errorf("remote URL contains a password")
				}
			}
		default:
			return "", fmt.Errorf("remote URL scheme is unsupported")
		}
		path := strings.Trim(strings.TrimSuffix(parsed.EscapedPath(), ".git"), "/")
		if path == "" {
			return "", fmt.Errorf("remote URL has no repository path")
		}
		return strings.ToLower(parsed.Hostname()) + "/" + strings.ToLower(path), nil
	}
	if colon := strings.IndexByte(trimmed, ':'); colon > 0 && !filepath.IsAbs(trimmed) {
		host := trimmed[:colon]
		if at := strings.LastIndexByte(host, '@'); at >= 0 {
			host = host[at+1:]
		}
		path := strings.Trim(strings.TrimSuffix(trimmed[colon+1:], ".git"), "/")
		if host == "" || path == "" || strings.ContainsAny(host+path, "?#") {
			return "", fmt.Errorf("scp-style remote is malformed")
		}
		return strings.ToLower(host) + "/" + strings.ToLower(path), nil
	}
	if !filepath.IsAbs(trimmed) {
		parts := strings.Split(strings.Trim(strings.TrimSuffix(trimmed, ".git"), "/"), "/")
		if len(parts) >= 3 && strings.Contains(parts[0], ".") {
			for _, part := range parts {
				if part == "" || part == "." || part == ".." {
					return "", fmt.Errorf("remote identity path is malformed")
				}
			}
			return strings.ToLower(strings.Join(parts, "/")), nil
		}
	}
	absolute, err := filepath.Abs(trimmed)
	if err != nil {
		return "", fmt.Errorf("resolve local remote path: %w", err)
	}
	return filepath.Clean(absolute), nil
}

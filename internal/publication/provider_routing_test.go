package publication

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/runenv"
)

type recordingPublicationEnvironmentResolver struct {
	mu      sync.Mutex
	calls   int
	overlay runenv.Overlay
}

func (r *recordingPublicationEnvironmentResolver) ResolveEnvironment(_ context.Context, _, _ string) (runenv.Overlay, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	return r.overlay.Clone(), nil
}

func (r *recordingPublicationEnvironmentResolver) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func TestGitPushPortUsesLazyTrustedEnvironmentForRoutePushAndObservation(t *testing.T) {
	fixture := newCandidatePortFixture(t, "push-routed-environment")
	remote := filepath.Join(t.TempDir(), "remote.git")
	candidateGit(t, "", "init", "--bare", remote)
	repoID, publicationID, effectDigest := bindLocalPushPublication(t, fixture, remote, "routed-environment")
	output := filepath.Join(t.TempDir(), "seen-environment")
	hook := "#!/bin/sh\nprintf '%s' \"$PUBLICATION_ROUTE_SENTINEL\" > \"$PUBLICATION_ROUTE_OUTPUT\"\nexit 0\n"
	if err := os.WriteFile(filepath.Join(remote, "hooks", "pre-receive"), []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	resolver := &recordingPublicationEnvironmentResolver{overlay: runenv.Overlay{Set: map[string]string{
		"PUBLICATION_ROUTE_SENTINEL": "profile-a", "PUBLICATION_ROUTE_OUTPUT": output,
	}}}
	port, err := NewGitPushPort(GitPushPortOptions{DB: fixture.database, EnvironmentResolver: resolver})
	if err != nil {
		t.Fatal(err)
	}
	if resolver.count() != 0 {
		t.Fatal("Git push construction eagerly resolved a forge environment")
	}
	request := PushEffectRequest{
		PublicationID: publicationID, RepositoryID: repoID, CommitSHA: fixture.headSHA,
		RemoteIdentity: remote, DestinationRef: fixture.parsed.Request.Candidate.HeadRef, EffectDigest: effectDigest,
	}
	if err := port.PublishExact(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	seen, err := os.ReadFile(output)
	if err != nil || string(seen) != "profile-a" {
		t.Fatalf("remote push environment = %q, %v", seen, err)
	}
	if _, err := port.ObserveExact(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if resolver.count() != 2 {
		t.Fatalf("environment resolver calls = %d, want one per push/observe operation", resolver.count())
	}
}

type recordingGitHubSessionResolver struct {
	mu      sync.Mutex
	calls   []string
	session GitHubV1Session
	err     error
}

func (r *recordingGitHubSessionResolver) ResolveGitHubV1Session(_ context.Context, publicationID, repositoryID string) (GitHubV1Session, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, publicationID+"/"+repositoryID)
	return r.session, r.err
}

func (r *recordingGitHubSessionResolver) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func TestGitHubV1RoutingPortResolvesOneLazySessionPerBoundOperation(t *testing.T) {
	stub := newGitHubV1Stub(t)
	fixture := newCandidatePortFixture(t, "github-routing")
	resolver := &recordingGitHubSessionResolver{session: GitHubV1Session{
		HTTPClient: stub.server.Client(), APIBaseURL: stub.server.URL,
	}}
	router, err := NewGitHubV1RoutingPort(GitHubV1RoutingPortOptions{DB: fixture.database, Sessions: resolver})
	if err != nil {
		t.Fatal(err)
	}
	if resolver.count() != 0 {
		t.Fatal("routing-port construction eagerly resolved credentials")
	}
	marker := reconciliationMarker(fixture.publication)
	draft, _ := finalizedPRDraft([]byte("routed"), marker)
	planTestPREffect(t, fixture, draft)
	query := PRReconcileQuery{
		PublicationID: fixture.publication.PublicationID, RepositoryID: fixture.repo.ID,
		BaseRef: fixture.parsed.Request.Candidate.BaseRef, HeadRef: fixture.parsed.Request.Candidate.HeadRef,
		CommitSHA: fixture.headSHA, Marker: marker, DraftSHA256: sha256HexBytes(draft),
	}
	if _, err := router.FindExact(context.Background(), query); err != nil {
		t.Fatal(err)
	}
	if resolver.count() != 1 {
		t.Fatalf("PR session resolver calls = %d", resolver.count())
	}
	if _, err := router.ObserveExact(context.Background(), CIQuery{PublicationID: fixture.publication.PublicationID, CommitSHA: fixture.headSHA}); err != nil {
		t.Fatal(err)
	}
	if resolver.count() != 2 {
		t.Fatalf("CI session resolver calls = %d", resolver.count())
	}
}

type tokenCall struct {
	host string
	env  runenv.Overlay
}

func addProviderRoutingPublication(t *testing.T, fixture *candidatePortFixture, id, upstream, hostIdentity, suffix string) (*db.Repo, *db.Publication) {
	t.Helper()
	repo, err := fixture.database.InsertRepoWithID(id, t.TempDir(), upstream, "main")
	if err != nil {
		t.Fatal(err)
	}
	request := fixture.parsed.Request
	request.Factory.RunID += "-" + suffix
	request.Candidate.RepositoryID = repo.ID
	request.Scopes.Push.RemoteIdentity = hostIdentity
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	row, _, _, err := fixture.database.CreateOrGetPublication(db.CreatePublicationInput{
		PublicationID: parsed.PublicationID, CanonicalRequest: parsed.CanonicalBytes,
		RepoID: repo.ID, CandidateRef: request.Candidate.HeadRef, BaseRef: request.Candidate.BaseRef,
		BaseSHA: request.Candidate.BaseSHA, HeadSHA: request.Candidate.CommitSHA, TreeSHA: request.Candidate.TreeSHA,
	})
	if err != nil {
		t.Fatal(err)
	}
	return repo, row
}

func writeGitHubProfile(t *testing.T, host, login string) string {
	t.Helper()
	dir := t.TempDir()
	raw := fmt.Sprintf("%s:\n  user: %s\n  users:\n    %s: {}\n", host, login, login)
	if err := os.WriteFile(filepath.Join(dir, "hosts.yml"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestForgePublicationSessionResolverSeparatesRepositoriesHostsAndProfiles(t *testing.T) {
	fixture := newCandidatePortFixture(t, "provider-sessions")
	_, enterprisePublication := addProviderRoutingPublication(
		t, fixture, "abcdef012345", "https://ghe.example/team/project.git", "ghe.example/team/project", "enterprise",
	)
	githubDir := writeGitHubProfile(t, "github.com", "github-bot")
	enterpriseDir := writeGitHubProfile(t, "ghe.example", "enterprise-bot")
	var mu sync.Mutex
	var calls []tokenCall
	resolver, err := NewForgePublicationSessionResolver(ForgePublicationSessionResolverOptions{
		DB: fixture.database,
		Profiles: config.ForgeProfiles{
			"github.com":  {GHConfigDir: githubDir, ExpectedLogin: "github-bot"},
			"ghe.example": {GHConfigDir: enterpriseDir, ExpectedLogin: "enterprise-bot"},
		},
		HTTPClient: &http.Client{},
		TokenCommand: func(_ context.Context, host string, environment runenv.Overlay) (string, error) {
			mu.Lock()
			calls = append(calls, tokenCall{host: host, env: environment.Clone()})
			mu.Unlock()
			return "token-for-" + host + "\n", nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 0 {
		t.Fatal("session-resolver construction invoked gh auth")
	}
	githubSession, err := resolver.ResolveGitHubV1Session(context.Background(), fixture.publication.PublicationID, fixture.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	enterpriseSession, err := resolver.ResolveGitHubV1Session(context.Background(), enterprisePublication.PublicationID, "abcdef012345")
	if err != nil {
		t.Fatal(err)
	}
	if githubSession.APIBaseURL != "https://api.github.com" || enterpriseSession.APIBaseURL != "https://ghe.example/api/v3" {
		t.Fatalf("API bases github=%q enterprise=%q", githubSession.APIBaseURL, enterpriseSession.APIBaseURL)
	}
	if githubSession.Token == enterpriseSession.Token || githubSession.Token == "" || enterpriseSession.Token == "" {
		t.Fatal("repository sessions shared or lost credentials")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 2 || calls[0].host != "github.com" || calls[0].env.Set["GH_CONFIG_DIR"] != githubDir ||
		calls[1].host != "ghe.example" || calls[1].env.Set["GH_CONFIG_DIR"] != enterpriseDir {
		t.Fatalf("token routing calls = %#v", calls)
	}
}

func TestForgePublicationSessionResolverFailsClosedWithoutLeakingCredentials(t *testing.T) {
	secret := "github_pat_MUST_NOT_LEAK"
	tests := map[string]struct {
		prepare func(*testing.T, *candidatePortFixture) (config.ForgeProfiles, string, string)
		command GitHubTokenCommand
	}{
		"wrong login": {
			prepare: func(t *testing.T, fixture *candidatePortFixture) (config.ForgeProfiles, string, string) {
				dir := writeGitHubProfile(t, "github.com", "active-user")
				return config.ForgeProfiles{"github.com": {GHConfigDir: dir, ExpectedLogin: "other-user"}}, fixture.publication.PublicationID, fixture.repo.ID
			},
		},
		"fork": {
			prepare: func(t *testing.T, fixture *candidatePortFixture) (config.ForgeProfiles, string, string) {
				if _, err := fixture.database.UpdateRepoForkURL(fixture.repo.ID, "https://github.com/fork/project.git"); err != nil {
					t.Fatal(err)
				}
				dir := writeGitHubProfile(t, "github.com", "bot")
				return config.ForgeProfiles{"github.com": {GHConfigDir: dir}}, fixture.publication.PublicationID, fixture.repo.ID
			},
		},
		"empty token": {
			prepare: func(t *testing.T, fixture *candidatePortFixture) (config.ForgeProfiles, string, string) {
				dir := writeGitHubProfile(t, "github.com", "bot")
				return config.ForgeProfiles{"github.com": {GHConfigDir: dir}}, fixture.publication.PublicationID, fixture.repo.ID
			},
			command: func(context.Context, string, runenv.Overlay) (string, error) { return "\n", nil },
		},
		"token command error": {
			prepare: func(t *testing.T, fixture *candidatePortFixture) (config.ForgeProfiles, string, string) {
				dir := writeGitHubProfile(t, "github.com", "bot")
				return config.ForgeProfiles{"github.com": {GHConfigDir: dir}}, fixture.publication.PublicationID, fixture.repo.ID
			},
			command: func(context.Context, string, runenv.Overlay) (string, error) { return "", errors.New(secret) },
		},
		"invalid token bytes": {
			prepare: func(t *testing.T, fixture *candidatePortFixture) (config.ForgeProfiles, string, string) {
				dir := writeGitHubProfile(t, "github.com", "bot")
				return config.ForgeProfiles{"github.com": {GHConfigDir: dir}}, fixture.publication.PublicationID, fixture.repo.ID
			},
			command: func(context.Context, string, runenv.Overlay) (string, error) { return secret + "\nextra\n", nil },
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			fixture := newCandidatePortFixture(t, "provider-refuse-"+strings.ReplaceAll(name, " ", "-"))
			profiles, publicationID, repoID := test.prepare(t, fixture)
			command := test.command
			if command == nil {
				command = func(context.Context, string, runenv.Overlay) (string, error) { return "unused", nil }
			}
			resolver, err := NewForgePublicationSessionResolver(ForgePublicationSessionResolverOptions{
				DB: fixture.database, Profiles: profiles, HTTPClient: &http.Client{}, TokenCommand: command,
			})
			if err != nil {
				t.Fatal(err)
			}
			if session, err := resolver.ResolveGitHubV1Session(context.Background(), publicationID, repoID); err == nil {
				t.Fatalf("unsafe provider session accepted: %#v", session)
			} else if strings.Contains(err.Error(), secret) {
				t.Fatalf("provider error leaked credential: %v", err)
			}
		})
	}
}

func TestForgePublicationSessionResolverRejectsWrongProviderAndBoundsTokenCommand(t *testing.T) {
	t.Run("wrong provider", func(t *testing.T) {
		fixture := newCandidatePortFixture(t, "provider-wrong-provider")
		_, row := addProviderRoutingPublication(t, fixture, "abcdef012345", "https://gitlab.com/team/project.git", "gitlab.com/team/project", "gitlab")
		glabDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(glabDir, "config.yml"), []byte("hosts:\n  gitlab.com:\n    user: bot\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		resolver, err := NewForgePublicationSessionResolver(ForgePublicationSessionResolverOptions{
			DB: fixture.database, Profiles: config.ForgeProfiles{"gitlab.com": {GLabConfigDir: glabDir}},
			TokenCommand: func(context.Context, string, runenv.Overlay) (string, error) { return "unused", nil },
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := resolver.ResolveGitHubV1Session(context.Background(), row.PublicationID, "abcdef012345"); err == nil {
			t.Fatal("GitLab repository received a GitHub publication session")
		}
	})

	t.Run("bounded token command", func(t *testing.T) {
		fixture := newCandidatePortFixture(t, "provider-timeout")
		dir := writeGitHubProfile(t, "github.com", "bot")
		resolver, err := NewForgePublicationSessionResolver(ForgePublicationSessionResolverOptions{
			DB: fixture.database, Profiles: config.ForgeProfiles{"github.com": {GHConfigDir: dir}}, ResolveTimeout: 20 * time.Millisecond,
			TokenCommand: func(ctx context.Context, _ string, _ runenv.Overlay) (string, error) {
				<-ctx.Done()
				return "", ctx.Err()
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		started := time.Now()
		if _, err := resolver.ResolveGitHubV1Session(context.Background(), fixture.publication.PublicationID, fixture.repo.ID); err == nil {
			t.Fatal("timed-out credential command succeeded")
		}
		if time.Since(started) > time.Second {
			t.Fatal("credential command was not bounded")
		}
	})
}

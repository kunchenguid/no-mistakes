package publication

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
)

func TestGitPushPortPublishesImmutableHToExactRefAndObservesRemote(t *testing.T) {
	fixture := newCandidatePortFixture(t, "push-exact")
	remote := filepath.Join(t.TempDir(), "remote.git")
	candidateGit(t, "", "init", "--bare", remote)
	repoID, publicationID, effectDigest := bindLocalPushPublication(t, fixture, remote, "exact")

	// Move the registered checkout to another branch after the request is
	// bound, while leaving the bound candidate ref at H. The port must still
	// publish immutable H, never mutable worktree HEAD.
	candidateGit(t, fixture.source, "checkout", "-b", "unrelated-after-binding")
	if err := os.WriteFile(filepath.Join(fixture.source, "after-binding.txt"), []byte("later\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	candidateGit(t, fixture.source, "add", "after-binding.txt")
	candidateGit(t, fixture.source, "commit", "-m", "after binding")
	laterSHA := candidateGit(t, fixture.source, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(fixture.source, ".git", "hooks", "pre-push"), []byte("#!/bin/sh\nexit 91\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	port, err := NewGitPushPort(GitPushPortOptions{DB: fixture.database})
	if err != nil {
		t.Fatalf("new Git push port: %v", err)
	}
	request := PushEffectRequest{
		PublicationID:  publicationID,
		RepositoryID:   repoID,
		CommitSHA:      fixture.headSHA,
		RemoteIdentity: remote,
		DestinationRef: fixture.parsed.Request.Candidate.HeadRef,
		EffectDigest:   effectDigest,
	}
	if err := port.PublishExact(context.Background(), request); err != nil {
		t.Fatalf("publish exact H: %v", err)
	}
	if got := candidateGit(t, remote, "rev-parse", request.DestinationRef); got != fixture.headSHA || got == laterSHA {
		t.Fatalf("remote ref = %s, want immutable H %s and not checkout HEAD %s", got, fixture.headSHA, laterSHA)
	}
	observation, err := port.ObserveExact(context.Background(), request)
	if err != nil {
		t.Fatalf("observe exact push: %v", err)
	}
	if observation.RemoteHeadSHA != fixture.headSHA {
		t.Fatalf("remote observation = %q, want %q", observation.RemoteHeadSHA, fixture.headSHA)
	}

	// A later remote move is observed as drift rather than rewritten or hidden.
	candidateGit(t, fixture.source, "push", "--no-verify", remote, "+"+laterSHA+":"+request.DestinationRef)
	observation, err = port.ObserveExact(context.Background(), request)
	if err != nil {
		t.Fatalf("observe drifted push: %v", err)
	}
	if observation.RemoteHeadSHA != laterSHA {
		t.Fatalf("drift observation = %q, want live remote %q", observation.RemoteHeadSHA, laterSHA)
	}
}

func TestGitPushPortRejectsUnboundRepositoryRemoteCommitAndRef(t *testing.T) {
	fixture := newCandidatePortFixture(t, "push-reject")
	remote := filepath.Join(t.TempDir(), "remote.git")
	candidateGit(t, "", "init", "--bare", remote)
	repoID, publicationID, effectDigest := bindLocalPushPublication(t, fixture, remote, "reject")
	port, err := NewGitPushPort(GitPushPortOptions{DB: fixture.database})
	if err != nil {
		t.Fatal(err)
	}
	valid := PushEffectRequest{
		PublicationID:  publicationID,
		RepositoryID:   repoID,
		CommitSHA:      fixture.headSHA,
		RemoteIdentity: remote,
		DestinationRef: fixture.parsed.Request.Candidate.HeadRef,
		EffectDigest:   effectDigest,
	}
	tests := []struct {
		name   string
		mutate func(*PushEffectRequest)
	}{
		{name: "repository", mutate: func(request *PushEffectRequest) { request.RepositoryID = "012345abcdef" }},
		{name: "remote", mutate: func(request *PushEffectRequest) { request.RemoteIdentity = remote + "-other" }},
		{name: "commit", mutate: func(request *PushEffectRequest) { request.CommitSHA = strings.Repeat("f", 40) }},
		{name: "ref", mutate: func(request *PushEffectRequest) { request.DestinationRef = "HEAD" }},
		{name: "malformed digest", mutate: func(request *PushEffectRequest) { request.EffectDigest = "bad" }},
		{name: "foreign digest", mutate: func(request *PushEffectRequest) { request.EffectDigest = strings.Repeat("b", 64) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := valid
			test.mutate(&request)
			if err := port.PublishExact(context.Background(), request); err == nil {
				t.Fatalf("PublishExact accepted unbound %s", test.name)
			}
			if got := candidateGit(t, remote, "for-each-ref", "--format=%(refname)"); got != "" {
				t.Fatalf("rejected %s request mutated remote refs: %q", test.name, got)
			}
		})
	}
	if err := os.WriteFile(filepath.Join(fixture.source, "candidate-ref-drift.txt"), []byte("drift\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	candidateGit(t, fixture.source, "add", "candidate-ref-drift.txt")
	candidateGit(t, fixture.source, "commit", "-m", "move candidate ref")
	if err := port.PublishExact(context.Background(), valid); err == nil {
		t.Fatal("PublishExact accepted a candidate ref that drifted away from H")
	}
	if got := candidateGit(t, remote, "for-each-ref", "--format=%(refname)"); got != "" {
		t.Fatalf("candidate-ref drift mutated remote refs: %q", got)
	}
}

func TestGitPushPortRejectsGitURLRewriteBeforeAnyRemoteEffect(t *testing.T) {
	fixture := newCandidatePortFixture(t, "push-url-rewrite")
	remote := filepath.Join(t.TempDir(), "rewritten.git")
	candidateGit(t, "", "init", "--bare", remote)
	boundRemote := "https://github.com/example/project.git"
	candidateGit(t, fixture.source, "config", "url."+remote+".insteadOf", boundRemote)
	repoID, publicationID, effectDigest := bindLocalPushPublication(t, fixture, boundRemote, "rewrite")
	port, err := NewGitPushPort(GitPushPortOptions{DB: fixture.database})
	if err != nil {
		t.Fatal(err)
	}
	request := PushEffectRequest{
		PublicationID:  publicationID,
		RepositoryID:   repoID,
		CommitSHA:      fixture.headSHA,
		RemoteIdentity: boundRemote,
		DestinationRef: fixture.parsed.Request.Candidate.HeadRef,
		EffectDigest:   effectDigest,
	}

	if err := port.PublishExact(context.Background(), request); err == nil {
		t.Fatal("Push accepted a Git insteadOf rewrite to a different effective remote")
	}
	if got := candidateGit(t, remote, "for-each-ref", "--format=%(refname)"); got != "" {
		t.Fatalf("rejected rewritten route mutated remote refs: %q", got)
	}
}

func bindLocalPushPublication(t *testing.T, fixture *candidatePortFixture, remote, suffix string) (string, string, string) {
	t.Helper()
	if err := fixture.database.DeleteRepo(fixture.repo.ID); err != nil {
		t.Fatalf("replace fixture repository binding: %v", err)
	}
	repo, err := fixture.database.InsertRepo(fixture.source, remote, "main")
	if err != nil {
		t.Fatalf("register local push repository: %v", err)
	}
	request := fixture.parsed.Request
	request.Factory.RunID += "-local-push-" + suffix
	request.Candidate.RepositoryID = repo.ID
	request.Scopes.Push.RemoteIdentity = remote
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseRequest(raw)
	if err != nil {
		t.Fatalf("parse local push publication: %v", err)
	}
	publicationRow, _, created, err := fixture.database.CreateOrGetPublication(db.CreatePublicationInput{
		PublicationID:    parsed.PublicationID,
		CanonicalRequest: parsed.CanonicalBytes,
		RepoID:           repo.ID,
		CandidateRef:     request.Candidate.HeadRef,
		BaseRef:          request.Candidate.BaseRef,
		BaseSHA:          request.Candidate.BaseSHA,
		HeadSHA:          request.Candidate.CommitSHA,
		TreeSHA:          request.Candidate.TreeSHA,
	})
	if err != nil {
		t.Fatalf("create local push publication: %v", err)
	}
	if !created {
		t.Fatal("local push publication was not created")
	}
	binding := db.PublicationEffectBinding{
		CandidateSHA:   request.Candidate.CommitSHA,
		RemoteIdentity: request.Scopes.Push.RemoteIdentity,
		DestinationRef: request.Scopes.Push.DestinationRef,
		HeadRef:        request.Candidate.HeadRef,
	}
	binding.EffectDigest, err = effectDigest(binding)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.database.PlanPublicationEffect(db.PlanPublicationEffectInput{
		PublicationID: publicationRow.PublicationID,
		Kind:          db.PublicationEffectPush,
		Binding:       binding,
	}); err != nil {
		t.Fatalf("plan local push effect: %v", err)
	}
	return repo.ID, publicationRow.PublicationID, binding.EffectDigest
}

type githubV1Stub struct {
	t            *testing.T
	server       *httptest.Server
	mu           sync.Mutex
	requests     []githubV1RecordedRequest
	pulls        any
	checks       any
	statuses     any
	statusHeader http.Header
	statusPages  map[string]any
}

type githubV1RecordedRequest struct {
	Method string
	Path   string
	Query  url.Values
	Body   []byte
}

func newGitHubV1Stub(t *testing.T) *githubV1Stub {
	t.Helper()
	stub := &githubV1Stub{
		t:        t,
		pulls:    []any{},
		checks:   map[string]any{"total_count": 0, "check_runs": []any{}},
		statuses: map[string]any{"sha": strings.Repeat("a", 40), "state": "pending", "total_count": 0, "statuses": []any{}},
	}
	stub.server = httptest.NewServer(http.HandlerFunc(stub.serveHTTP))
	t.Cleanup(stub.server.Close)
	return stub
}

func (s *githubV1Stub) serveHTTP(w http.ResponseWriter, request *http.Request) {
	body, err := io.ReadAll(request.Body)
	if err != nil {
		s.t.Errorf("read stub request: %v", err)
	}
	s.mu.Lock()
	s.requests = append(s.requests, githubV1RecordedRequest{
		Method: request.Method,
		Path:   request.URL.Path,
		Query:  request.URL.Query(),
		Body:   body,
	})
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case request.Method == http.MethodPost && request.URL.Path == "/repos/example/project/pulls":
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"number":17}`))
	case request.Method == http.MethodGet && request.URL.Path == "/repos/example/project/pulls":
		_ = json.NewEncoder(w).Encode(s.pulls)
	case request.Method == http.MethodGet && strings.Contains(request.URL.Path, "/check-runs"):
		_ = json.NewEncoder(w).Encode(s.checks)
	case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/status"):
		for name, values := range s.statusHeader {
			for _, value := range values {
				w.Header().Add(name, value)
			}
		}
		statuses := s.statuses
		if s.statusPages != nil {
			page := request.URL.Query().Get("page")
			if page == "" {
				page = "1"
			}
			statuses = s.statusPages[page]
		}
		_ = json.NewEncoder(w).Encode(statuses)
	default:
		s.t.Errorf("unexpected GitHub v1 request: %s %s", request.Method, request.URL.String())
		http.Error(w, `{"message":"unexpected"}`, http.StatusNotFound)
	}
}

func (s *githubV1Stub) snapshotRequests() []githubV1RecordedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]githubV1RecordedRequest(nil), s.requests...)
}

func newGitHubV1TestPort(t *testing.T, suffix string, stub *githubV1Stub) (*candidatePortFixture, *GitHubV1Port) {
	t.Helper()
	fixture := newCandidatePortFixture(t, suffix)
	port, err := NewGitHubV1Port(GitHubV1PortOptions{
		DB:         fixture.database,
		HTTPClient: stub.server.Client(),
		APIBaseURL: stub.server.URL,
	})
	if err != nil {
		t.Fatalf("new GitHub v1 port: %v", err)
	}
	return fixture, port
}

func githubPullSnapshot(number int, body, baseRef, headRef, headSHA, fullName string) map[string]any {
	return map[string]any{
		"number": number,
		"body":   body,
		"base": map[string]any{
			"ref":  baseRef,
			"repo": map[string]any{"full_name": fullName},
		},
		"head": map[string]any{
			"ref":  headRef,
			"sha":  headSHA,
			"repo": map[string]any{"full_name": fullName},
		},
	}
}

func planTestPREffect(t *testing.T, fixture *candidatePortFixture, draft []byte) string {
	t.Helper()
	draftDigest := sha256.Sum256(draft)
	binding := db.PublicationEffectBinding{
		CandidateSHA:   fixture.publication.HeadSHA,
		RemoteIdentity: fixture.parsed.Request.Scopes.Push.RemoteIdentity,
		DestinationRef: fixture.parsed.Request.Scopes.Push.DestinationRef,
		BaseRef:        fixture.parsed.Request.Scopes.PR.BaseRef,
		HeadRef:        fixture.parsed.Request.Scopes.PR.HeadRef,
		DraftDigest:    hex.EncodeToString(draftDigest[:]),
	}
	var err error
	binding.EffectDigest, err = effectDigest(binding)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.database.PlanPublicationEffect(db.PlanPublicationEffectInput{
		PublicationID:   fixture.publication.PublicationID,
		Kind:            db.PublicationEffectPR,
		Binding:         binding,
		PreparedPayload: draft,
	}); err != nil {
		t.Fatalf("plan test PR effect: %v", err)
	}
	return binding.EffectDigest
}

func TestGitHubV1PRPortCreatesDeterministicBoundDraftAndFindsExactSnapshots(t *testing.T) {
	stub := newGitHubV1Stub(t)
	fixture, port := newGitHubV1TestPort(t, "github-pr", stub)
	marker := reconciliationMarker(fixture.publication)
	draft, err := finalizedPRDraft([]byte("Publication body"), marker)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(draft)
	effectDigest := planTestPREffect(t, fixture, draft)
	request := PREffectRequest{
		PublicationID: fixture.publication.PublicationID,
		RepositoryID:  fixture.repo.ID,
		BaseRef:       "refs/heads/main",
		HeadRef:       fixture.parsed.Request.Candidate.HeadRef,
		CommitSHA:     fixture.headSHA,
		Marker:        marker,
		Draft:         draft,
		DraftSHA256:   hex.EncodeToString(digest[:]),
		EffectDigest:  effectDigest,
	}
	if err := port.CreateExact(context.Background(), request); err != nil {
		t.Fatalf("create exact PR: %v", err)
	}
	recorded := stub.snapshotRequests()
	if len(recorded) != 1 || recorded[0].Method != http.MethodPost {
		t.Fatalf("PR create calls = %#v", recorded)
	}
	var payload struct {
		Title string `json:"title"`
		Body  string `json:"body"`
		Base  string `json:"base"`
		Head  string `json:"head"`
		Draft bool   `json:"draft"`
	}
	if err := json.Unmarshal(recorded[0].Body, &payload); err != nil {
		t.Fatalf("decode create payload: %v", err)
	}
	if payload.Title != "Factory publication "+fixture.publication.PublicationID || payload.Body != string(draft) ||
		payload.Base != "main" || payload.Head != strings.TrimPrefix(request.HeadRef, "refs/heads/") || payload.Draft {
		t.Fatalf("unbound or nondeterministic PR payload: %+v", payload)
	}

	stub.pulls = []any{githubPullSnapshot(17, string(draft), "main", strings.TrimPrefix(request.HeadRef, "refs/heads/"), fixture.headSHA, "example/project")}
	query := PRReconcileQuery{
		PublicationID: fixture.publication.PublicationID,
		RepositoryID:  fixture.repo.ID,
		BaseRef:       request.BaseRef,
		HeadRef:       request.HeadRef,
		CommitSHA:     request.CommitSHA,
		Marker:        marker,
		DraftSHA256:   request.DraftSHA256,
	}
	observations, err := port.FindExact(context.Background(), query)
	if err != nil {
		t.Fatalf("find exact PR: %v", err)
	}
	if matches := exactPRMatches(observations, query); len(matches) != 1 || matches[0].Number != "17" {
		t.Fatalf("exact PR matches = %#v, observations %#v", matches, observations)
	}
	requests := stub.snapshotRequests()
	last := requests[len(requests)-1]
	if last.Method != http.MethodGet || last.Query.Get("base") != "main" || last.Query.Get("head") != "example:"+strings.TrimPrefix(request.HeadRef, "refs/heads/") {
		t.Fatalf("PR lookup was not exact repo/base/head: %#v", last)
	}
}

func TestGitHubV1PRPortRejectsDraftDriftAndPreservesZeroOrMultipleExactMatches(t *testing.T) {
	stub := newGitHubV1Stub(t)
	fixture, port := newGitHubV1TestPort(t, "github-pr-drift", stub)
	marker := reconciliationMarker(fixture.publication)
	draft, _ := finalizedPRDraft([]byte("Body"), marker)
	digest := sha256.Sum256(draft)
	effectDigest := planTestPREffect(t, fixture, draft)
	request := PREffectRequest{
		PublicationID: fixture.publication.PublicationID,
		RepositoryID:  fixture.repo.ID,
		BaseRef:       "refs/heads/main",
		HeadRef:       fixture.parsed.Request.Candidate.HeadRef,
		CommitSHA:     fixture.headSHA,
		Marker:        marker,
		Draft:         draft,
		DraftSHA256:   hex.EncodeToString(digest[:]),
		EffectDigest:  effectDigest,
	}
	for name, mutate := range map[string]func(*PREffectRequest){
		"raw bytes": func(value *PREffectRequest) { value.Draft = append(value.Draft, 'x') },
		"marker":    func(value *PREffectRequest) { value.Marker = "<!-- foreign -->" },
		"head":      func(value *PREffectRequest) { value.CommitSHA = strings.Repeat("f", 40) },
		"repo":      func(value *PREffectRequest) { value.RepositoryID = "012345abcdef" },
		"effect":    func(value *PREffectRequest) { value.EffectDigest = strings.Repeat("b", 64) },
	} {
		t.Run(name, func(t *testing.T) {
			changed := request
			changed.Draft = append([]byte(nil), request.Draft...)
			mutate(&changed)
			before := len(stub.snapshotRequests())
			if err := port.CreateExact(context.Background(), changed); err == nil {
				t.Fatalf("CreateExact accepted %s drift", name)
			}
			if after := len(stub.snapshotRequests()); after != before {
				t.Fatalf("rejected %s drift reached GitHub: %d -> %d calls", name, before, after)
			}
		})
	}

	query := PRReconcileQuery{
		PublicationID: fixture.publication.PublicationID,
		RepositoryID:  fixture.repo.ID,
		BaseRef:       request.BaseRef,
		HeadRef:       request.HeadRef,
		CommitSHA:     request.CommitSHA,
		Marker:        marker,
		DraftSHA256:   request.DraftSHA256,
	}
	exact := githubPullSnapshot(1, string(draft), "main", strings.TrimPrefix(request.HeadRef, "refs/heads/"), fixture.headSHA, "example/project")
	stub.pulls = []any{}
	observations, err := port.FindExact(context.Background(), query)
	if err != nil || len(exactPRMatches(observations, query)) != 0 {
		t.Fatalf("zero-match reconcile = %#v, %v", observations, err)
	}
	for name, snapshot := range map[string]map[string]any{
		"repository": githubPullSnapshot(1, string(draft), "main", strings.TrimPrefix(request.HeadRef, "refs/heads/"), fixture.headSHA, "other/project"),
		"base":       githubPullSnapshot(1, string(draft), "other", strings.TrimPrefix(request.HeadRef, "refs/heads/"), fixture.headSHA, "example/project"),
		"head ref":   githubPullSnapshot(1, string(draft), "main", "other", fixture.headSHA, "example/project"),
		"head SHA":   githubPullSnapshot(1, string(draft), "main", strings.TrimPrefix(request.HeadRef, "refs/heads/"), strings.Repeat("f", 40), "example/project"),
		"marker":     githubPullSnapshot(1, "body without marker", "main", strings.TrimPrefix(request.HeadRef, "refs/heads/"), fixture.headSHA, "example/project"),
		"raw digest": githubPullSnapshot(1, string(draft)+"drift", "main", strings.TrimPrefix(request.HeadRef, "refs/heads/"), fixture.headSHA, "example/project"),
	} {
		t.Run("snapshot "+name, func(t *testing.T) {
			stub.pulls = []any{snapshot}
			observed, err := port.FindExact(context.Background(), query)
			if err != nil {
				t.Fatal(err)
			}
			if matches := exactPRMatches(observed, query); len(matches) != 0 {
				t.Fatalf("%s drift produced exact matches: %#v", name, matches)
			}
		})
	}
	stub.pulls = []any{exact, githubPullSnapshot(2, string(draft), "main", strings.TrimPrefix(request.HeadRef, "refs/heads/"), fixture.headSHA, "example/project")}
	observations, err = port.FindExact(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	if matches := exactPRMatches(observations, query); len(matches) != 2 {
		t.Fatalf("ambiguous exact observations collapsed to %#v", matches)
	}
}

func TestGitHubV1PortRequiresConfidentialCredentialTransportAndNeverLeaksToken(t *testing.T) {
	fixture := newCandidatePortFixture(t, "github-transport-policy")
	secret := "github_pat_DO_NOT_LEAK_123"
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Fatal("rejected plaintext configuration reached the HTTP transport")
	}))
	t.Cleanup(plain.Close)
	_, err := NewGitHubV1Port(GitHubV1PortOptions{
		DB:         fixture.database,
		HTTPClient: plain.Client(),
		APIBaseURL: plain.URL,
		Token:      secret,
	})
	if err == nil {
		t.Fatal("GitHub port accepted a token over plaintext HTTP")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("plaintext rejection leaked token: %v", err)
	}
	_, err = NewGitHubV1Port(GitHubV1PortOptions{
		DB:         fixture.database,
		HTTPClient: plain.Client(),
		APIBaseURL: "http://example.com",
	})
	if err == nil {
		t.Fatal("GitHub port accepted plaintext HTTP for a non-loopback host")
	}

	var seenURL, seenAuthorization string
	tlsServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		seenURL = request.URL.String()
		seenAuthorization = request.Header.Get("Authorization")
		http.Error(w, secret, http.StatusInternalServerError)
	}))
	t.Cleanup(tlsServer.Close)
	port, err := NewGitHubV1Port(GitHubV1PortOptions{
		DB:         fixture.database,
		HTTPClient: tlsServer.Client(),
		APIBaseURL: tlsServer.URL,
		Token:      secret,
	})
	if err != nil {
		t.Fatalf("GitHub port rejected HTTPS credential transport: %v", err)
	}
	marker := reconciliationMarker(fixture.publication)
	draft, _ := finalizedPRDraft([]byte("transport policy"), marker)
	planTestPREffect(t, fixture, draft)
	draftDigest := sha256.Sum256(draft)
	_, err = port.FindExact(context.Background(), PRReconcileQuery{
		PublicationID: fixture.publication.PublicationID,
		RepositoryID:  fixture.repo.ID,
		BaseRef:       fixture.parsed.Request.Candidate.BaseRef,
		HeadRef:       fixture.parsed.Request.Candidate.HeadRef,
		CommitSHA:     fixture.headSHA,
		Marker:        marker,
		DraftSHA256:   hex.EncodeToString(draftDigest[:]),
	})
	if err == nil {
		t.Fatal("expected injected HTTPS provider failure")
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(seenURL, secret) {
		t.Fatalf("GitHub token leaked through error or URL: err=%v url=%q", err, seenURL)
	}
	if seenAuthorization != "Bearer "+secret {
		t.Fatalf("HTTPS request did not carry the injected bearer credential")
	}
}

func TestGitHubV1CIPortRequiresOneLiveExactPRAndNonEmptyAllPassAtH(t *testing.T) {
	tests := map[string]struct {
		prHead      string
		checks      any
		statuses    any
		wantAllPass bool
	}{
		"check pass": {
			checks:      map[string]any{"total_count": 1, "check_runs": []any{map[string]any{"name": "test", "head_sha": "HEAD", "status": "completed", "conclusion": "success"}}},
			statuses:    map[string]any{"sha": "HEAD", "state": "success", "total_count": 0, "statuses": []any{}},
			wantAllPass: true,
		},
		"status pass": {
			checks:      map[string]any{"total_count": 0, "check_runs": []any{}},
			statuses:    map[string]any{"sha": "HEAD", "state": "success", "total_count": 1, "statuses": []any{map[string]any{"context": "legacy", "state": "success"}}},
			wantAllPass: true,
		},
		"empty": {
			checks:   map[string]any{"total_count": 0, "check_runs": []any{}},
			statuses: map[string]any{"sha": "HEAD", "state": "pending", "total_count": 0, "statuses": []any{}},
		},
		"pending": {
			checks:   map[string]any{"total_count": 1, "check_runs": []any{map[string]any{"name": "test", "head_sha": "HEAD", "status": "in_progress", "conclusion": nil}}},
			statuses: map[string]any{"sha": "HEAD", "state": "pending", "total_count": 0, "statuses": []any{}},
		},
		"cancelled": {
			checks:   map[string]any{"total_count": 1, "check_runs": []any{map[string]any{"name": "test", "head_sha": "HEAD", "status": "completed", "conclusion": "cancelled"}}},
			statuses: map[string]any{"sha": "HEAD", "state": "success", "total_count": 0, "statuses": []any{}},
		},
		"skipped": {
			checks:   map[string]any{"total_count": 1, "check_runs": []any{map[string]any{"name": "test", "head_sha": "HEAD", "status": "completed", "conclusion": "skipped"}}},
			statuses: map[string]any{"sha": "HEAD", "state": "success", "total_count": 0, "statuses": []any{}},
		},
		"unknown": {
			checks:   map[string]any{"total_count": 1, "check_runs": []any{map[string]any{"name": "test", "head_sha": "HEAD", "status": "completed", "conclusion": "mystery"}}},
			statuses: map[string]any{"sha": "HEAD", "state": "success", "total_count": 0, "statuses": []any{}},
		},
		"failed": {
			checks:   map[string]any{"total_count": 1, "check_runs": []any{map[string]any{"name": "test", "head_sha": "HEAD", "status": "completed", "conclusion": "failure"}}},
			statuses: map[string]any{"sha": "HEAD", "state": "success", "total_count": 0, "statuses": []any{}},
		},
		"malformed": {
			checks:   map[string]any{"total_count": 1, "check_runs": []any{map[string]any{"head_sha": "HEAD", "status": "completed", "conclusion": "success"}}},
			statuses: map[string]any{"sha": "HEAD", "state": "success", "total_count": 0, "statuses": []any{}},
		},
		"check head drift": {
			checks:   map[string]any{"total_count": 1, "check_runs": []any{map[string]any{"name": "test", "head_sha": "ffffffffffffffffffffffffffffffffffffffff", "status": "completed", "conclusion": "success"}}},
			statuses: map[string]any{"sha": "HEAD", "state": "success", "total_count": 0, "statuses": []any{}},
		},
		"status head drift": {
			checks:   map[string]any{"total_count": 0, "check_runs": []any{}},
			statuses: map[string]any{"sha": "ffffffffffffffffffffffffffffffffffffffff", "state": "success", "total_count": 1, "statuses": []any{map[string]any{"context": "legacy", "state": "success"}}},
		},
		"PR head drift": {
			prHead:   strings.Repeat("f", 40),
			checks:   map[string]any{"total_count": 1, "check_runs": []any{map[string]any{"name": "test", "head_sha": "HEAD", "status": "completed", "conclusion": "success"}}},
			statuses: map[string]any{"sha": "HEAD", "state": "success", "total_count": 0, "statuses": []any{}},
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			stub := newGitHubV1Stub(t)
			fixture, port := newGitHubV1TestPort(t, "github-ci-"+strings.ReplaceAll(name, " ", "-"), stub)
			head := test.prHead
			if head == "" {
				head = fixture.headSHA
			}
			replaceHead := func(value any) any {
				raw, _ := json.Marshal(value)
				raw = []byte(strings.ReplaceAll(string(raw), `"HEAD"`, fmt.Sprintf("%q", fixture.headSHA)))
				var replaced any
				_ = json.Unmarshal(raw, &replaced)
				return replaced
			}
			stub.checks = replaceHead(test.checks)
			stub.statuses = replaceHead(test.statuses)
			stub.pulls = []any{githubPullSnapshot(17, "body", "main", strings.TrimPrefix(fixture.parsed.Request.Candidate.HeadRef, "refs/heads/"), head, "example/project")}

			observation, err := port.ObserveExact(context.Background(), CIQuery{
				PublicationID: fixture.publication.PublicationID,
				CommitSHA:     fixture.headSHA,
			})
			if err != nil {
				t.Fatalf("observe CI: %v", err)
			}
			if got := exactCIPassed(observation, fixture.headSHA); got != test.wantAllPass {
				t.Fatalf("exactCIPassed(%+v) = %v, want %v", observation, got, test.wantAllPass)
			}
			for _, request := range stub.snapshotRequests() {
				if request.Method != http.MethodGet {
					t.Fatalf("CI issued mutating/rerun request: %#v", request)
				}
				if strings.Contains(request.Path, "rerun") || strings.Contains(request.Path, "dispatch") {
					t.Fatalf("CI invoked rerun/fix surface: %#v", request)
				}
			}
		})
	}
}

func TestGitHubV1CIPortRejectsTruncatedCommitStatusPagination(t *testing.T) {
	stub := newGitHubV1Stub(t)
	fixture, port := newGitHubV1TestPort(t, "github-ci-status-pagination", stub)
	stub.pulls = []any{githubPullSnapshot(
		17,
		"body",
		"main",
		strings.TrimPrefix(fixture.parsed.Request.Candidate.HeadRef, "refs/heads/"),
		fixture.headSHA,
		"example/project",
	)}
	stub.checks = map[string]any{"total_count": 0, "check_runs": []any{}}
	stub.statusPages = map[string]any{
		"1": map[string]any{
			"sha": fixture.headSHA, "total_count": 1,
			"statuses": []any{map[string]any{"context": "first-page-pass", "state": "success"}},
		},
		"2": map[string]any{
			"sha": fixture.headSHA, "total_count": 2,
			"statuses": []any{map[string]any{"context": "second-page-pending", "state": "pending"}},
		},
	}
	statusPath := "/repos/example/project/commits/" + fixture.headSHA + "/status"
	stub.statusHeader = http.Header{
		"Link": {"<" + stub.server.URL + statusPath + "?per_page=100&page=2>; rel=\"next\""},
	}

	observation, err := port.ObserveExact(context.Background(), CIQuery{
		PublicationID: fixture.publication.PublicationID,
		CommitSHA:     fixture.headSHA,
	})
	if err == nil {
		t.Fatalf("paginated status observation was silently accepted: %+v", observation)
	}
	if exactCIPassed(observation, fixture.headSHA) {
		t.Fatalf("paginated status observation became green: %+v", observation)
	}

	var statusRequests []githubV1RecordedRequest
	for _, request := range stub.snapshotRequests() {
		if request.Path == statusPath {
			statusRequests = append(statusRequests, request)
		}
	}
	if len(statusRequests) != 1 {
		t.Fatalf("commit-status requests = %#v, want one bounded first-page request", statusRequests)
	}
	if got := statusRequests[0].Query.Get("per_page"); got != "100" {
		t.Fatalf("commit-status per_page = %q, want 100", got)
	}
	if got := statusRequests[0].Query.Get("page"); got != "1" {
		t.Fatalf("commit-status page = %q, want 1", got)
	}
}

func TestGitHubV1CIPortCommitStatusCompletenessIsFailClosed(t *testing.T) {
	stub := newGitHubV1Stub(t)
	fixture, port := newGitHubV1TestPort(t, "github-ci-status-completeness", stub)
	stub.pulls = []any{githubPullSnapshot(
		17,
		"body",
		"main",
		strings.TrimPrefix(fixture.parsed.Request.Candidate.HeadRef, "refs/heads/"),
		fixture.headSHA,
		"example/project",
	)}
	stub.checks = map[string]any{"total_count": 0, "check_runs": []any{}}
	complete := map[string]any{
		"sha": fixture.headSHA, "total_count": 1,
		"statuses": []any{map[string]any{"context": "complete-pass", "state": "success"}},
	}

	tests := map[string]struct {
		statuses any
		header   http.Header
		wantPass bool
	}{
		"single complete page": {statuses: complete, wantPass: true},
		"missing total count": {
			statuses: map[string]any{
				"sha":      fixture.headSHA,
				"statuses": []any{map[string]any{"context": "pass", "state": "success"}},
			},
		},
		"count smaller than payload": {
			statuses: map[string]any{
				"sha": fixture.headSHA, "total_count": 0,
				"statuses": []any{map[string]any{"context": "pass", "state": "success"}},
			},
		},
		"count larger than payload": {
			statuses: map[string]any{
				"sha": fixture.headSHA, "total_count": 2,
				"statuses": []any{map[string]any{"context": "pass", "state": "success"}},
			},
		},
		"negative count": {
			statuses: map[string]any{"sha": fixture.headSHA, "total_count": -1, "statuses": []any{}},
		},
		"empty Link header": {
			statuses: complete,
			header:   http.Header{"Link": {""}},
		},
		"malformed Link header": {
			statuses: complete,
			header:   http.Header{"Link": {"not-a-link"}},
		},
		"unquoted next relation": {
			statuses: complete,
			header:   http.Header{"Link": {"<https://api.invalid/status?page=2>; rel=next"}},
		},
		"unexpected last relation": {
			statuses: complete,
			header:   http.Header{"Link": {"<https://api.invalid/status?page=1>; rel=\"last\""}},
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			stub.statusPages = nil
			stub.statuses = test.statuses
			stub.statusHeader = test.header.Clone()
			observation, err := port.ObserveExact(context.Background(), CIQuery{
				PublicationID: fixture.publication.PublicationID,
				CommitSHA:     fixture.headSHA,
			})
			if test.wantPass {
				if err != nil {
					t.Fatalf("complete status observation: %v", err)
				}
				if !exactCIPassed(observation, fixture.headSHA) {
					t.Fatalf("complete status observation did not pass: %+v", observation)
				}
				return
			}
			if err == nil {
				t.Fatalf("ambiguous status observation was accepted: %+v", observation)
			}
			if exactCIPassed(observation, fixture.headSHA) {
				t.Fatalf("ambiguous status observation became green: %+v", observation)
			}
		})
	}
}

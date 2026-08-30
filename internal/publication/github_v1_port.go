package publication

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/kunchenguid/no-mistakes/internal/db"
)

const githubV1MaxResponseBytes = 8 << 20

// GitHubV1PortOptions inject the exact HTTP boundary. Tests use httptest;
// production wiring supplies an authenticated stdlib client and API base.
type GitHubV1PortOptions struct {
	DB         *db.DB
	HTTPClient *http.Client
	APIBaseURL string
	Token      string
}

// GitHubV1Port implements only the closed publication PR and read-only CI
// surfaces. It intentionally exposes no rerun, dispatch, fix, or merge method.
type GitHubV1Port struct {
	db      *db.DB
	client  *http.Client
	baseURL string
	token   string
}

type githubV1Repository struct {
	owner    string
	name     string
	fullName string
}

func NewGitHubV1Port(options GitHubV1PortOptions) (*GitHubV1Port, error) {
	if options.DB == nil {
		return nil, fmt.Errorf("GitHub v1 port database is required")
	}
	if options.HTTPClient == nil {
		return nil, fmt.Errorf("GitHub v1 HTTP client is required")
	}
	base, err := url.Parse(strings.TrimSpace(options.APIBaseURL))
	if err != nil || base.Scheme == "" || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, fmt.Errorf("GitHub v1 API base URL is invalid")
	}
	if base.Scheme != "https" && base.Scheme != "http" {
		return nil, fmt.Errorf("GitHub v1 API base URL must use HTTP or HTTPS")
	}
	token := options.Token
	if token != strings.TrimSpace(token) || strings.IndexFunc(token, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		return nil, fmt.Errorf("GitHub v1 token is not canonical")
	}
	if base.Scheme == "http" {
		host := base.Hostname()
		ip := net.ParseIP(host)
		loopback := strings.EqualFold(host, "localhost") || (ip != nil && ip.IsLoopback())
		if token != "" || !loopback {
			return nil, fmt.Errorf("GitHub v1 plaintext HTTP is allowed only tokenless on loopback")
		}
	}
	client := *options.HTTPClient
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &GitHubV1Port{
		db:      options.DB,
		client:  &client,
		baseURL: strings.TrimRight(base.String(), "/"),
		token:   token,
	}, nil
}

func (p *GitHubV1Port) CreateExact(ctx context.Context, request PREffectRequest) error {
	publication, repo, err := p.validatePRBinding(request.PublicationID, request.RepositoryID, request.BaseRef, request.HeadRef, request.CommitSHA)
	if err != nil {
		return err
	}
	if request.Marker != reconciliationMarker(publication) || bytes.Count(request.Draft, []byte(request.Marker)) != 1 {
		return fmt.Errorf("PR draft does not contain exactly one bound reconciliation marker")
	}
	if !utf8.Valid(request.Draft) || len(request.Draft) == 0 {
		return fmt.Errorf("PR draft must be non-empty UTF-8")
	}
	if sha256HexBytes(request.Draft) != request.DraftSHA256 || !isLowerHex(request.DraftSHA256, sha256.Size*2) {
		return fmt.Errorf("PR draft raw-byte SHA-256 does not match its binding")
	}
	if !isLowerHex(request.EffectDigest, sha256.Size*2) {
		return fmt.Errorf("PR effect digest is not a lowercase SHA-256")
	}
	if err := p.validateDurablePREffect(publication, request.BaseRef, request.HeadRef, request.CommitSHA, request.Marker, request.DraftSHA256, request.EffectDigest, request.Draft); err != nil {
		return err
	}
	payload := struct {
		Title string `json:"title"`
		Body  string `json:"body"`
		Base  string `json:"base"`
		Head  string `json:"head"`
		Draft bool   `json:"draft"`
	}{
		Title: "Factory publication " + publication.PublicationID,
		Body:  string(request.Draft),
		Base:  shortBranchRef(request.BaseRef),
		Head:  shortBranchRef(request.HeadRef),
		Draft: false,
	}
	var response struct {
		Number int64 `json:"number"`
	}
	if _, err := p.doJSON(ctx, http.MethodPost, p.repoPath(repo, "pulls"), nil, payload, &response); err != nil {
		return fmt.Errorf("create exact GitHub PR: %w", err)
	}
	if response.Number <= 0 {
		return fmt.Errorf("GitHub PR create returned no canonical PR number")
	}
	return nil
}

func (p *GitHubV1Port) FindExact(ctx context.Context, query PRReconcileQuery) ([]PRObservation, error) {
	publication, repo, err := p.validatePRBinding(query.PublicationID, query.RepositoryID, query.BaseRef, query.HeadRef, query.CommitSHA)
	if err != nil {
		return nil, err
	}
	if query.Marker != reconciliationMarker(publication) {
		return nil, fmt.Errorf("PR reconciliation marker does not match the publication")
	}
	if !isLowerHex(query.DraftSHA256, sha256.Size*2) {
		return nil, fmt.Errorf("PR reconciliation draft digest is invalid")
	}
	if err := p.validateDurablePREffect(publication, query.BaseRef, query.HeadRef, query.CommitSHA, query.Marker, query.DraftSHA256, "", nil); err != nil {
		return nil, err
	}
	pulls, err := p.listPulls(ctx, repo, query.BaseRef, query.HeadRef)
	if err != nil {
		return nil, err
	}
	observations := make([]PRObservation, 0, len(pulls))
	for _, pull := range pulls {
		observation := PRObservation{
			BaseRef:     fullBranchRef(pull.Base.Ref),
			HeadRef:     fullBranchRef(pull.Head.Ref),
			HeadSHA:     pull.Head.SHA,
			DraftSHA256: sha256HexBytes([]byte(pull.Body)),
		}
		if pull.Number > 0 {
			observation.Number = strconv.FormatInt(pull.Number, 10)
		}
		if strings.EqualFold(pull.Base.Repo.FullName, repo.fullName) && strings.EqualFold(pull.Head.Repo.FullName, repo.fullName) && pull.Number > 0 {
			observation.RepositoryID = query.RepositoryID
		}
		if bytes.Count([]byte(pull.Body), []byte(query.Marker)) == 1 {
			observation.Marker = query.Marker
		}
		observations = append(observations, observation)
	}
	return observations, nil
}

func (p *GitHubV1Port) ObserveExact(ctx context.Context, query CIQuery) (CIObservation, error) {
	publication, parsed, repo, err := p.loadPublicationBinding(query.PublicationID)
	if err != nil {
		return CIObservation{}, err
	}
	if query.CommitSHA != publication.HeadSHA || !isLowerHex(query.CommitSHA, 40) {
		return CIObservation{}, fmt.Errorf("CI query is not bound to exact publication H")
	}
	pulls, err := p.listPulls(ctx, repo, parsed.Request.Candidate.BaseRef, parsed.Request.Candidate.HeadRef)
	if err != nil {
		return CIObservation{}, err
	}
	matching := make([]githubV1Pull, 0, len(pulls))
	for _, pull := range pulls {
		if pull.Number > 0 && strings.EqualFold(pull.Base.Repo.FullName, repo.fullName) && strings.EqualFold(pull.Head.Repo.FullName, repo.fullName) &&
			fullBranchRef(pull.Base.Ref) == parsed.Request.Candidate.BaseRef &&
			fullBranchRef(pull.Head.Ref) == parsed.Request.Candidate.HeadRef {
			matching = append(matching, pull)
		}
	}
	if len(matching) == 0 {
		return CIObservation{}, nil
	}
	if len(matching) > 1 {
		return CIObservation{HeadSHA: query.CommitSHA}, nil
	}
	liveHead := matching[0].Head.SHA
	if liveHead != query.CommitSHA {
		return CIObservation{HeadSHA: liveHead}, nil
	}

	checks, err := p.readCheckRuns(ctx, repo, query.CommitSHA)
	if err != nil {
		return CIObservation{}, err
	}
	statuses, statusHead, err := p.readCommitStatuses(ctx, repo, query.CommitSHA)
	if err != nil {
		return CIObservation{}, err
	}
	if statusHead != query.CommitSHA {
		return CIObservation{HeadSHA: statusHead, Checks: append(checks, statuses...)}, nil
	}
	return CIObservation{HeadSHA: query.CommitSHA, Checks: append(checks, statuses...)}, nil
}

type githubV1Pull struct {
	Number int64  `json:"number"`
	Body   string `json:"body"`
	Base   struct {
		Ref  string `json:"ref"`
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"base"`
	Head struct {
		Ref  string `json:"ref"`
		SHA  string `json:"sha"`
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
}

func (p *GitHubV1Port) listPulls(ctx context.Context, repo githubV1Repository, baseRef, headRef string) ([]githubV1Pull, error) {
	query := url.Values{
		"state":    {"open"},
		"base":     {shortBranchRef(baseRef)},
		"head":     {repo.owner + ":" + shortBranchRef(headRef)},
		"per_page": {"100"},
	}
	var pulls []githubV1Pull
	header, err := p.doJSON(ctx, http.MethodGet, p.repoPath(repo, "pulls"), query, nil, &pulls)
	if err != nil {
		return nil, fmt.Errorf("list exact GitHub PR candidates: %w", err)
	}
	if strings.Contains(header.Get("Link"), `rel="next"`) {
		return nil, fmt.Errorf("GitHub PR observation is incomplete due to pagination")
	}
	return pulls, nil
}

func (p *GitHubV1Port) readCheckRuns(ctx context.Context, repo githubV1Repository, commitSHA string) ([]CICheck, error) {
	var response struct {
		TotalCount *int `json:"total_count"`
		CheckRuns  []struct {
			Name       *string `json:"name"`
			HeadSHA    *string `json:"head_sha"`
			Status     *string `json:"status"`
			Conclusion *string `json:"conclusion"`
		} `json:"check_runs"`
	}
	header, err := p.doJSON(ctx, http.MethodGet, p.repoPath(repo, "commits", commitSHA, "check-runs"), url.Values{"per_page": {"100"}}, nil, &response)
	if err != nil {
		return nil, fmt.Errorf("observe GitHub check runs: %w", err)
	}
	if response.TotalCount == nil || *response.TotalCount != len(response.CheckRuns) || strings.Contains(header.Get("Link"), `rel="next"`) {
		return nil, fmt.Errorf("GitHub check-run observation is malformed or incomplete")
	}
	checks := make([]CICheck, 0, len(response.CheckRuns))
	for _, run := range response.CheckRuns {
		name, head, status, conclusion := deref(run.Name), deref(run.HeadSHA), deref(run.Status), deref(run.Conclusion)
		checks = append(checks, CICheck{Name: name, HeadSHA: head, Status: githubCheckStatus(status, conclusion)})
	}
	return checks, nil
}

func (p *GitHubV1Port) readCommitStatuses(ctx context.Context, repo githubV1Repository, commitSHA string) ([]CICheck, string, error) {
	var response struct {
		SHA        *string `json:"sha"`
		TotalCount *int    `json:"total_count"`
		Statuses   []struct {
			Context *string `json:"context"`
			State   *string `json:"state"`
		} `json:"statuses"`
	}
	header, err := p.doJSON(ctx, http.MethodGet, p.repoPath(repo, "commits", commitSHA, "status"), url.Values{
		"per_page": {"100"},
		"page":     {"1"},
	}, nil, &response)
	if err != nil {
		return nil, "", fmt.Errorf("observe GitHub commit statuses: %w", err)
	}
	// Page 1 cannot legitimately carry any pagination Link when the complete
	// result fits in the requested bound. Reject every present Link value,
	// including malformed or empty variants, instead of trying to infer that a
	// partially understood header is safe.
	if response.TotalCount == nil || *response.TotalCount < 0 || *response.TotalCount != len(response.Statuses) || len(header.Values("Link")) != 0 {
		return nil, "", fmt.Errorf("GitHub commit-status observation is malformed or incomplete")
	}
	head := deref(response.SHA)
	checks := make([]CICheck, 0, len(response.Statuses))
	for _, status := range response.Statuses {
		checks = append(checks, CICheck{Name: deref(status.Context), HeadSHA: head, Status: githubCommitStatus(deref(status.State))})
	}
	return checks, head, nil
}

func githubCheckStatus(status, conclusion string) CICheckStatus {
	switch status {
	case "queued", "in_progress", "waiting", "requested", "pending":
		return CICheckPending
	case "completed":
		switch conclusion {
		case "success":
			return CICheckPass
		case "failure", "timed_out", "action_required", "startup_failure":
			return CICheckFail
		case "cancelled":
			return CICheckCancelled
		case "neutral", "skipped":
			return CICheckSkipped
		default:
			return CICheckUnknown
		}
	default:
		return CICheckUnknown
	}
}

func githubCommitStatus(state string) CICheckStatus {
	switch state {
	case "success":
		return CICheckPass
	case "pending":
		return CICheckPending
	case "failure", "error":
		return CICheckFail
	default:
		return CICheckUnknown
	}
}

func (p *GitHubV1Port) validatePRBinding(publicationID, repositoryID, baseRef, headRef, commitSHA string) (*db.Publication, githubV1Repository, error) {
	publication, parsed, repo, err := p.loadPublicationBinding(publicationID)
	if err != nil {
		return nil, githubV1Repository{}, err
	}
	if repositoryID != publication.RepoID || baseRef != parsed.Request.Candidate.BaseRef ||
		headRef != parsed.Request.Candidate.HeadRef || commitSHA != publication.HeadSHA {
		return nil, githubV1Repository{}, fmt.Errorf("GitHub PR request does not match the exact publication binding")
	}
	return publication, repo, nil
}

func (p *GitHubV1Port) validateDurablePREffect(publication *db.Publication, baseRef, headRef, commitSHA, marker, draftDigest, requestEffectDigest string, requestDraft []byte) error {
	parsed, err := ParseRequest(publication.CanonicalRequest)
	if err != nil {
		return fmt.Errorf("parse durable PR publication: %w", err)
	}
	effect, err := p.db.GetPublicationEffect(publication.PublicationID, db.PublicationEffectPR)
	if err != nil {
		return fmt.Errorf("load durable PR effect: %w", err)
	}
	if effect == nil || effect.Binding.CandidateSHA != commitSHA ||
		effect.Binding.RemoteIdentity != parsed.Request.Scopes.Push.RemoteIdentity ||
		effect.Binding.DestinationRef != parsed.Request.Scopes.Push.DestinationRef ||
		effect.Binding.BaseRef != baseRef || effect.Binding.HeadRef != headRef ||
		effect.Binding.DraftDigest != draftDigest || !isLowerHex(effect.Binding.EffectDigest, sha256.Size*2) {
		return fmt.Errorf("PR request does not match the durable effect journal")
	}
	if requestEffectDigest != "" && effect.Binding.EffectDigest != requestEffectDigest {
		return fmt.Errorf("PR effect digest does not match the durable effect journal")
	}
	recomputed := effect.Binding
	recomputed.EffectDigest = ""
	wantDigest, err := effectDigest(recomputed)
	if err != nil || wantDigest != effect.Binding.EffectDigest {
		return fmt.Errorf("durable PR effect digest is inconsistent")
	}
	if len(effect.PreparedPayload) == 0 || sha256HexBytes(effect.PreparedPayload) != effect.Binding.DraftDigest ||
		bytes.Count(effect.PreparedPayload, []byte(marker)) != 1 {
		return fmt.Errorf("durable PR prepared payload is inconsistent")
	}
	if requestDraft != nil && !bytes.Equal(requestDraft, effect.PreparedPayload) {
		return fmt.Errorf("PR draft bytes do not match the durable prepared payload")
	}
	return nil
}

func (p *GitHubV1Port) loadPublicationBinding(publicationID string) (*db.Publication, ParsedRequest, githubV1Repository, error) {
	if !isLowerHex(publicationID, sha256.Size*2) {
		return nil, ParsedRequest{}, githubV1Repository{}, fmt.Errorf("GitHub publication ID is invalid")
	}
	publication, err := p.db.GetPublication(publicationID)
	if err != nil {
		return nil, ParsedRequest{}, githubV1Repository{}, err
	}
	if publication == nil {
		return nil, ParsedRequest{}, githubV1Repository{}, fmt.Errorf("publication %s is not registered", publicationID)
	}
	parsed, err := ParseRequest(publication.CanonicalRequest)
	if err != nil {
		return nil, ParsedRequest{}, githubV1Repository{}, fmt.Errorf("parse stored publication request: %w", err)
	}
	if parsed.PublicationID != publication.PublicationID || parsed.Request.Candidate.RepositoryID != publication.RepoID ||
		parsed.Request.Candidate.CommitSHA != publication.HeadSHA || parsed.Request.Candidate.TreeSHA != publication.TreeSHA {
		return nil, ParsedRequest{}, githubV1Repository{}, fmt.Errorf("stored GitHub publication binding is inconsistent")
	}
	record, err := p.db.GetRepo(publication.RepoID)
	if err != nil {
		return nil, ParsedRequest{}, githubV1Repository{}, err
	}
	if record == nil {
		return nil, ParsedRequest{}, githubV1Repository{}, fmt.Errorf("GitHub publication repository is not registered")
	}
	repo, err := parseGitHubV1Repository(record.UpstreamURL)
	if err != nil {
		return nil, ParsedRequest{}, githubV1Repository{}, err
	}
	return publication, parsed, repo, nil
}

func parseGitHubV1Repository(raw string) (githubV1Repository, error) {
	trimmed := strings.TrimSpace(raw)
	var repositoryPath string
	if strings.Contains(trimmed, "://") {
		parsed, err := url.Parse(trimmed)
		if err != nil || parsed.Hostname() == "" || parsed.RawQuery != "" || parsed.Fragment != "" {
			return githubV1Repository{}, fmt.Errorf("registered GitHub remote is invalid")
		}
		if parsed.User != nil {
			if parsed.Scheme != "ssh" {
				return githubV1Repository{}, fmt.Errorf("registered GitHub remote contains credentials")
			}
			if _, hasPassword := parsed.User.Password(); hasPassword {
				return githubV1Repository{}, fmt.Errorf("registered GitHub remote contains a password")
			}
		}
		repositoryPath = parsed.Path
	} else {
		colon := strings.IndexByte(trimmed, ':')
		if colon <= 0 || colon == len(trimmed)-1 {
			return githubV1Repository{}, fmt.Errorf("registered remote is not a GitHub repository URL")
		}
		repositoryPath = trimmed[colon+1:]
	}
	parts := strings.Split(strings.Trim(strings.TrimSuffix(repositoryPath, ".git"), "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || parts[0] == "." || parts[0] == ".." || parts[1] == "." || parts[1] == ".." {
		return githubV1Repository{}, fmt.Errorf("registered GitHub remote must identify exactly owner/repository")
	}
	return githubV1Repository{owner: parts[0], name: parts[1], fullName: strings.ToLower(parts[0] + "/" + parts[1])}, nil
}

func (p *GitHubV1Port) repoPath(repo githubV1Repository, parts ...string) string {
	segments := []string{"repos", repo.owner, repo.name}
	segments = append(segments, parts...)
	for index := range segments {
		segments[index] = url.PathEscape(segments[index])
	}
	return "/" + strings.Join(segments, "/")
}

func (p *GitHubV1Port) doJSON(ctx context.Context, method, path string, query url.Values, body, output any) (http.Header, error) {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encode GitHub request: %w", err)
		}
		reader = bytes.NewReader(raw)
	}
	endpoint := p.baseURL + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return nil, fmt.Errorf("create GitHub request: %w", err)
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if p.token != "" {
		request.Header.Set("Authorization", "Bearer "+p.token)
	}
	response, err := p.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("execute GitHub request: %w", err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, githubV1MaxResponseBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read GitHub response: %w", err)
	}
	if len(raw) > githubV1MaxResponseBytes {
		return nil, fmt.Errorf("GitHub response exceeds %d bytes", githubV1MaxResponseBytes)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("GitHub returned HTTP %d", response.StatusCode)
	}
	if output != nil {
		if len(raw) == 0 {
			return nil, fmt.Errorf("GitHub returned an empty JSON response")
		}
		if err := json.Unmarshal(raw, output); err != nil {
			return nil, fmt.Errorf("decode GitHub response: %w", err)
		}
	}
	return response.Header.Clone(), nil
}

func shortBranchRef(ref string) string {
	return strings.TrimPrefix(ref, "refs/heads/")
}

func fullBranchRef(ref string) string {
	if ref == "" {
		return ""
	}
	return "refs/heads/" + ref
}

func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

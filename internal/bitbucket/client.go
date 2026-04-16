package bitbucket

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	defaultAPIBaseURL = "https://api.bitbucket.org"
	envEmail          = "NO_MISTAKES_BITBUCKET_EMAIL"
	envToken          = "NO_MISTAKES_BITBUCKET_API_TOKEN"
	envAPIBaseURL     = "NO_MISTAKES_BITBUCKET_API_BASE_URL"
)

type RepoRef struct {
	Workspace string
	RepoSlug  string
}

type PullRequest struct {
	ID               int
	URL              string
	State            string
	SourceCommitHash string
}

type CommitStatus struct {
	Name        string `json:"name"`
	State       string `json:"state"`
	Description string `json:"description"`
	URL         string `json:"url"`
}

type Pipeline struct {
	UUID string `json:"uuid"`
}

type PipelineStep struct {
	UUID  string `json:"uuid"`
	State struct {
		Name   string `json:"name"`
		Result struct {
			Name string `json:"name"`
		} `json:"result"`
	} `json:"state"`
}

type Client struct {
	baseURL    string
	email      string
	token      string
	httpClient *http.Client
}

func NewClientFromEnv(env []string) (*Client, error) {
	email := lookupEnv(env, envEmail)
	if strings.TrimSpace(email) == "" {
		return nil, fmt.Errorf("missing %s", envEmail)
	}
	token := lookupEnv(env, envToken)
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("missing %s", envToken)
	}
	baseURL := lookupEnv(env, envAPIBaseURL)
	if strings.TrimSpace(baseURL) == "" {
		baseURL = defaultAPIBaseURL
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		email:   email,
		token:   token,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}, nil
}

func ParseRepoRef(raw string) (RepoRef, error) {
	trimmed := strings.TrimSpace(raw)
	trimmed = strings.TrimSuffix(trimmed, ".git")

	if strings.HasPrefix(trimmed, "git@bitbucket.org:") {
		path := strings.TrimPrefix(trimmed, "git@bitbucket.org:")
		return parseRepoPath(path)
	}

	parsed, err := url.Parse(trimmed)
	if err != nil {
		return RepoRef{}, fmt.Errorf("parse bitbucket repo URL: %w", err)
	}
	host := strings.ToLower(parsed.Hostname())
	if host != "bitbucket.org" && !strings.HasSuffix(host, ".bitbucket.org") {
		return RepoRef{}, fmt.Errorf("unsupported Bitbucket host %q", parsed.Host)
	}
	return parseRepoPath(strings.TrimPrefix(parsed.Path, "/"))
}

func parseRepoPath(path string) (RepoRef, error) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) < 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return RepoRef{}, fmt.Errorf("invalid Bitbucket repository path %q", path)
	}
	return RepoRef{Workspace: parts[0], RepoSlug: parts[1]}, nil
}

func (c *Client) FindOpenPRBySourceBranch(ctx context.Context, repo RepoRef, branch string) (*PullRequest, error) {
	query := url.Values{}
	query.Set("q", fmt.Sprintf(`source.branch.name=%q AND state=%q`, branch, "OPEN"))

	var response struct {
		Values []bitbucketPullRequest `json:"values"`
	}
	if err := c.doJSON(ctx, http.MethodGet, repoPRPath(repo), query, nil, &response); err != nil {
		return nil, err
	}
	if len(response.Values) == 0 {
		return nil, nil
	}
	return response.Values[0].toPullRequest(), nil
}

func (c *Client) CreatePR(ctx context.Context, repo RepoRef, sourceBranch, destBranch, title, body string) (*PullRequest, error) {
	requestBody := map[string]any{
		"title":       title,
		"description": body,
		"source": map[string]any{
			"branch": map[string]string{"name": sourceBranch},
		},
		"destination": map[string]any{
			"branch": map[string]string{"name": destBranch},
		},
	}
	var response bitbucketPullRequest
	if err := c.doJSON(ctx, http.MethodPost, repoPRPath(repo), nil, requestBody, &response); err != nil {
		return nil, err
	}
	return response.toPullRequest(), nil
}

func (c *Client) UpdatePR(ctx context.Context, repo RepoRef, prID int, title, body string) (*PullRequest, error) {
	requestBody := map[string]any{
		"title":       title,
		"description": body,
	}
	var response bitbucketPullRequest
	if err := c.doJSON(ctx, http.MethodPut, fmt.Sprintf("%s/%d", repoPRPath(repo), prID), nil, requestBody, &response); err != nil {
		return nil, err
	}
	return response.toPullRequest(), nil
}

func (c *Client) GetPR(ctx context.Context, repo RepoRef, prID int) (*PullRequest, error) {
	var response bitbucketPullRequest
	if err := c.doJSON(ctx, http.MethodGet, fmt.Sprintf("%s/%d", repoPRPath(repo), prID), nil, nil, &response); err != nil {
		return nil, err
	}
	return response.toPullRequest(), nil
}

func (c *Client) ListPRStatuses(ctx context.Context, repo RepoRef, prID int) ([]CommitStatus, error) {
	next := fmt.Sprintf("%s/%d/statuses", repoPRPath(repo), prID)
	statuses := make([]CommitStatus, 0)
	for next != "" {
		var response struct {
			Values []CommitStatus `json:"values"`
			Next   string         `json:"next"`
		}
		if err := c.doJSONPathOrURL(ctx, http.MethodGet, next, nil, &response); err != nil {
			return nil, err
		}
		statuses = append(statuses, response.Values...)
		if response.Next != "" {
			validatedNext, err := c.validatePaginationURL(response.Next)
			if err != nil {
				return nil, err
			}
			next = validatedNext
			continue
		}
		next = response.Next
	}
	return statuses, nil
}

func (c *Client) ListPipelinesByCommit(ctx context.Context, repo RepoRef, commitSHA string) ([]Pipeline, error) {
	query := url.Values{}
	query.Set("target.commit.hash", commitSHA)
	query.Set("sort", "-created_on")

	var response struct {
		Values []Pipeline `json:"values"`
	}
	if err := c.doJSON(ctx, http.MethodGet, fmt.Sprintf("/2.0/repositories/%s/%s/pipelines", repo.Workspace, repo.RepoSlug), query, nil, &response); err != nil {
		return nil, err
	}
	return response.Values, nil
}

func (c *Client) ListPipelineSteps(ctx context.Context, repo RepoRef, pipelineUUID string) ([]PipelineStep, error) {
	var response struct {
		Values []PipelineStep `json:"values"`
	}
	path := fmt.Sprintf("/2.0/repositories/%s/%s/pipelines/%s/steps", repo.Workspace, repo.RepoSlug, pipelineUUID)
	if err := c.doJSON(ctx, http.MethodGet, path, nil, nil, &response); err != nil {
		return nil, err
	}
	return response.Values, nil
}

func (c *Client) GetStepLog(ctx context.Context, repo RepoRef, pipelineUUID, stepUUID string) (string, error) {
	path := fmt.Sprintf("/2.0/repositories/%s/%s/pipelines/%s/steps/%s/log", repo.Workspace, repo.RepoSlug, pipelineUUID, stepUUID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return "", fmt.Errorf("build Bitbucket request: %w", err)
	}
	req.SetBasicAuth(c.email, c.token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("Bitbucket GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("Bitbucket GET %s: status %d: %s", path, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read Bitbucket step log: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
}

type bitbucketPullRequest struct {
	ID     int    `json:"id"`
	State  string `json:"state"`
	Source struct {
		Commit struct {
			Hash string `json:"hash"`
		} `json:"commit"`
	} `json:"source"`
	Links struct {
		HTML struct {
			Href string `json:"href"`
		} `json:"html"`
	} `json:"links"`
}

func (pr bitbucketPullRequest) toPullRequest() *PullRequest {
	return &PullRequest{
		ID:               pr.ID,
		URL:              strings.TrimSpace(pr.Links.HTML.Href),
		State:            strings.TrimSpace(pr.State),
		SourceCommitHash: strings.TrimSpace(pr.Source.Commit.Hash),
	}
}

func (c *Client) validatePaginationURL(rawURL string) (string, error) {
	base, err := url.Parse(c.baseURL)
	if err != nil {
		return "", fmt.Errorf("parse Bitbucket base URL: %w", err)
	}
	nextURL, err := url.Parse(rawURL)
	if err != nil {
		return "", fmt.Errorf("parse Bitbucket pagination URL: %w", err)
	}
	if !nextURL.IsAbs() {
		return rawURL, nil
	}
	if !strings.EqualFold(nextURL.Scheme, base.Scheme) || !strings.EqualFold(nextURL.Host, base.Host) {
		return "", fmt.Errorf("reject cross-origin Bitbucket pagination URL %q", rawURL)
	}
	return rawURL, nil
}

func (c *Client) doJSON(ctx context.Context, method, path string, query url.Values, requestBody any, responseBody any) error {
	endpoint := c.baseURL + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	return c.doJSONPathOrURL(ctx, method, endpoint, requestBody, responseBody)
}

func (c *Client) doJSONPathOrURL(ctx context.Context, method, pathOrURL string, requestBody any, responseBody any) error {
	var bodyReader io.Reader = http.NoBody
	if requestBody != nil {
		payload, err := json.Marshal(requestBody)
		if err != nil {
			return fmt.Errorf("marshal Bitbucket request body: %w", err)
		}
		bodyReader = bytes.NewReader(payload)
	}

	endpoint := pathOrURL
	requestLabel := pathOrURL
	if !strings.HasPrefix(pathOrURL, "http://") && !strings.HasPrefix(pathOrURL, "https://") {
		endpoint = c.baseURL + pathOrURL
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bodyReader)
	if err != nil {
		return fmt.Errorf("build Bitbucket request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.SetBasicAuth(c.email, c.token)
	if requestBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("Bitbucket %s %s: %w", method, requestLabel, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("Bitbucket %s %s: status %d: %s", method, requestLabel, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if responseBody == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(responseBody); err != nil {
		return fmt.Errorf("decode Bitbucket response: %w", err)
	}
	return nil
}

func repoPRPath(repo RepoRef) string {
	return fmt.Sprintf("/2.0/repositories/%s/%s/pullrequests", repo.Workspace, repo.RepoSlug)
}

func lookupEnv(env []string, key string) string {
	prefix := key + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return strings.TrimPrefix(entry, prefix)
		}
	}
	return ""
}

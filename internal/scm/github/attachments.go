package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// User-attachments upload contract, pinned against cli/cli v2.99.0
// (internal/attachments/client.go and userasset.go).
//
// This is not a documented REST or GraphQL API. gh itself already depends on
// the same unofficial HTTP endpoint, and Homebrew still lagged 2.99.0 when
// this shipped, so no-mistakes posts the request from Go rather than requiring
// `gh --attach` on PATH. The request shape is:
//
//	POST {uploadsPrefix}user-attachments/assets
//	  ?name=<basename>&content_type=<mime>&repository_id=<numeric repo id>
//	Authorization: Bearer <token>
//	Content-Type: application/octet-stream
//	Accept: application/vnd.github+json
//
// uploadsPrefix is https://uploads.github.com/ on github.com and
// https://uploads.<host>/ on GHEC (*.ghe.com). GHES is refused client-side:
// gh's checkHost rejects auth.IsEnterprise hosts, and GitHub has not published
// this endpoint for Enterprise Server.
//
// Response JSON is {"url":"https://github.com/user-attachments/assets/<uuid>"}.
//
// Credential class is an allowlist matching gh: OAuth (gho_), classic PAT
// (ghp_), fine-grained PAT (github_pat_). Installation / Actions tokens (ghs_)
// and GitHub App user-to-server tokens (ghu_) are refused before the request;
// reporters have 404'd the endpoint with GITHUB_TOKEN even with contents:write
// (cli/cli#14309). Permission allowlist is ADMIN/MAINTAIN/WRITE; READ/TRIAGE
// 404. Write access is required to upload; repository access is required to
// view a private asset.
//
// Client-side file rules, also matching gh: extension only (png, jpg, jpeg,
// gif, webp, svg, mp4, mov, webm), case-insensitive; regular non-empty files
// only; images at most 10 MiB; videos at most 100 MiB (the server still
// enforces plan limits). Videos render as a bare URL so GitHub shows a player;
// images become ![alt](url).

const (
	maxUserAssetImageBytes int64 = 10 * 1024 * 1024
	maxUserAssetVideoBytes int64 = 100 * 1024 * 1024
	userAssetUploadTimeout       = 60 * time.Second
)

const userAssetRepoQuery = `query($owner:String!,$name:String!){repository(owner:$owner,name:$name){databaseId viewerPermission}}`

var (
	userAssetContentTypes = []struct {
		ext         string
		contentType string
		video       bool
	}{
		{".png", "image/png", false},
		{".jpg", "image/jpeg", false},
		{".jpeg", "image/jpeg", false},
		{".gif", "image/gif", false},
		{".webp", "image/webp", false},
		{".svg", "image/svg+xml", false},
		{".mp4", "video/mp4", true},
		{".mov", "video/quicktime", true},
		{".webm", "video/webm", true},
	}
	userAssetUUID = regexp.MustCompile(`(?i)^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	uploadPerms   = map[string]bool{
		"ADMIN":    true,
		"MAINTAIN": true,
		"WRITE":    true,
	}
)

// UserAsset describes a local file that passed gh's attach rules.
type UserAsset struct {
	Path        string
	ContentType string
	Size        int64
	Video       bool
	fileInfo    fs.FileInfo
}

// SupportsUserAttachments reports whether host can serve the unofficial
// user-attachments upload endpoint. Empty host is github.com (the Host
// constructor leaves it empty for github.com remotes). GHEC tenants (*.ghe.com)
// are included; GitHub Enterprise Server is not.
func SupportsUserAttachments(host string) bool {
	h := normalizeGitHubHost(host)
	if h == "" || h == "github.com" || h == "github.localhost" || h == "garage.github.com" {
		return true
	}
	return h == "ghe.com" || strings.HasSuffix(h, ".ghe.com")
}

func normalizeGitHubHost(host string) string {
	return strings.ToLower(strings.TrimSpace(host))
}

func userAssetUploadPrefix(host string) string {
	h := normalizeGitHubHost(host)
	if h == "github.localhost" {
		return "http://uploads.github.localhost/"
	}
	if h == "" || h == "github.com" {
		return "https://uploads.github.com/"
	}
	return "https://uploads." + h + "/"
}

// ValidateUserAsset applies gh 2.99.0's client-side attach rules. A failure
// here must not produce a user-attachments URL.
func ValidateUserAsset(path string) (UserAsset, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return UserAsset{}, errors.New("empty attachment path")
	}
	info, err := os.Stat(path)
	if err != nil {
		if pathErr, ok := err.(*fs.PathError); ok {
			return UserAsset{}, fmt.Errorf("%s: %w", path, pathErr.Err)
		}
		return UserAsset{}, err
	}
	if info.IsDir() {
		return UserAsset{}, fmt.Errorf("%s is a directory", path)
	}
	if !info.Mode().IsRegular() {
		return UserAsset{}, fmt.Errorf("%s is not a regular file", path)
	}
	if info.Size() == 0 {
		return UserAsset{}, fmt.Errorf("%s is empty", path)
	}
	contentType, video, err := userAssetContentType(path)
	if err != nil {
		return UserAsset{}, err
	}
	limit, kind := maxUserAssetImageBytes, "images"
	if video {
		limit, kind = maxUserAssetVideoBytes, "videos"
	}
	if info.Size() > limit {
		return UserAsset{}, fmt.Errorf("%s: %s must be at most %.1f MB", path, kind, float64(limit)/(1024*1024))
	}
	return UserAsset{Path: path, ContentType: contentType, Size: info.Size(), Video: video, fileInfo: info}, nil
}

func userAssetContentType(path string) (string, bool, error) {
	ext := strings.ToLower(filepath.Ext(path))
	for _, t := range userAssetContentTypes {
		if t.ext == ext {
			return t.contentType, t.video, nil
		}
	}
	supported := make([]string, len(userAssetContentTypes))
	for i, t := range userAssetContentTypes {
		supported[i] = strings.TrimPrefix(t.ext, ".")
	}
	return "", false, fmt.Errorf("%s is not a supported file type (supported: %s)", path, strings.Join(supported, ", "))
}

type githubTokenClass int

const (
	githubTokenRejected githubTokenClass = iota
	githubTokenAllowed
)

func classifyGitHubToken(token string) (githubTokenClass, string) {
	token = strings.TrimSpace(token)
	if token == "" {
		return githubTokenRejected, "no GitHub token available"
	}
	switch {
	case strings.HasPrefix(token, "ghs_"):
		return githubTokenRejected, "GitHub App installation and Actions tokens cannot upload user-attachments"
	case strings.HasPrefix(token, "ghu_"):
		return githubTokenRejected, "GitHub App user-to-server tokens cannot upload user-attachments"
	case strings.HasPrefix(token, "gho_"), strings.HasPrefix(token, "ghp_"), strings.HasPrefix(token, "github_pat_"):
		return githubTokenAllowed, ""
	default:
		return githubTokenRejected, "unsupported GitHub authentication type for user-attachments"
	}
}

// UserAssetClient posts one validated file to the unofficial upload endpoint.
type UserAssetClient struct {
	HTTP         *http.Client
	UploadPrefix string
	Token        string
	RepositoryID int64
	acceptedHost string
}

func (c *UserAssetClient) httpClient() *http.Client {
	if c != nil && c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: userAssetUploadTimeout}
}

// UploadFile sends asset and returns the user-attachments URL. The URL is
// checked against gh's response shape before it is returned so a surprising
// payload cannot become a dead link in a PR body.
func (c *UserAssetClient) UploadFile(ctx context.Context, asset UserAsset) (string, error) {
	if c == nil {
		return "", errors.New("user-attachments client is not configured")
	}
	if c.RepositoryID <= 0 {
		return "", errors.New("could not determine which repository to attach files to")
	}
	prefix := strings.TrimSpace(c.UploadPrefix)
	if prefix == "" {
		return "", errors.New("user-attachments upload prefix is empty")
	}

	body, err := os.Open(asset.Path)
	if err != nil {
		return "", err
	}
	defer body.Close()
	openedInfo, err := body.Stat()
	if err != nil {
		return "", err
	}
	if !openedInfo.Mode().IsRegular() || openedInfo.Size() != asset.Size || asset.fileInfo == nil || !os.SameFile(asset.fileInfo, openedInfo) {
		return "", fmt.Errorf("%s changed after attachment validation", asset.Path)
	}

	endpoint, err := url.Parse(prefix)
	if err != nil {
		return "", err
	}
	endpoint = endpoint.JoinPath("user-attachments", "assets")
	query := endpoint.Query()
	query.Set("name", filepath.Base(asset.Path))
	query.Set("content_type", asset.ContentType)
	query.Set("repository_id", strconv.FormatInt(c.RepositoryID, 10))
	endpoint.RawQuery = query.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), body)
	if err != nil {
		return "", err
	}
	req.ContentLength = asset.Size
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+c.Token)

	httpClient := *c.httpClient()
	httpClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", fmt.Errorf("user-attachments upload HTTP %d", resp.StatusCode)
	}
	var parsed struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(payload, &parsed); err != nil {
		return "", fmt.Errorf("decode user-attachments response: %w", err)
	}
	clean, err := sanitizeUserAttachmentURL(parsed.URL, c.acceptedHost)
	if err != nil {
		return "", err
	}
	return clean, nil
}

func sanitizeUserAttachmentURL(raw, host string) (string, error) {
	clean := strings.TrimSpace(raw)
	parsed, err := url.ParseRequestURI(clean)
	if err != nil || parsed.Host == "" {
		return "", errors.New("user-attachments response was not an absolute URL")
	}
	if !strings.EqualFold(parsed.Scheme, "https") && !(strings.EqualFold(parsed.Scheme, "http") && strings.EqualFold(parsed.Hostname(), "github.localhost")) {
		return "", errors.New("user-attachments response used an unexpected URL scheme")
	}
	if !userAttachmentHostOK(parsed.Hostname(), host) {
		return "", fmt.Errorf("user-attachments response host %q is not a GitHub attachments host", parsed.Hostname())
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) != 3 || parts[0] != "user-attachments" || parts[1] != "assets" || !userAssetUUID.MatchString(parts[2]) {
		return "", errors.New("user-attachments response path was not /user-attachments/assets/<uuid>")
	}
	parsed.Fragment = ""
	parsed.RawQuery = ""
	return parsed.String(), nil
}

func userAttachmentHostOK(got, expected string) bool {
	got = strings.ToLower(got)
	expected = normalizeGitHubHost(expected)
	if expected == "" || expected == "github.com" {
		return got == "github.com"
	}
	if expected == "github.localhost" {
		return got == "github.localhost"
	}
	return got == expected || got == "github.com"
}

// UploadUserAsset validates path and uploads it as a GitHub user-attachment
// against this Host's repository. Callers must treat any error as fail-closed:
// keep today's PR rendering rather than inventing a URL.
func (h *Host) UploadUserAsset(ctx context.Context, path string) (string, error) {
	if h == nil {
		return "", errors.New("GitHub host is not configured")
	}
	if !SupportsUserAttachments(h.host) {
		return "", errors.New("attaching files is not supported on GitHub Enterprise Server")
	}
	asset, err := ValidateUserAsset(path)
	if err != nil {
		return "", err
	}
	token, err := h.authToken(ctx)
	if err != nil {
		return "", err
	}
	if class, reason := classifyGitHubToken(token); class != githubTokenAllowed {
		return "", errors.New(reason)
	}
	repoID, permission, err := h.userAssetRepo(ctx)
	if err != nil {
		return "", err
	}
	if permission == "" {
		return "", errors.New("could not determine your permission on the repository to attach files")
	}
	if !uploadPerms[permission] {
		return "", errors.New("attaching files requires write access to the repository")
	}
	client := &UserAssetClient{
		HTTP:         h.assetHTTP,
		UploadPrefix: h.assetUploadPrefix,
		Token:        token,
		RepositoryID: repoID,
		acceptedHost: h.host,
	}
	if client.UploadPrefix == "" {
		client.UploadPrefix = userAssetUploadPrefix(h.host)
	}
	if client.HTTP == nil {
		client.HTTP = &http.Client{Timeout: userAssetUploadTimeout}
	}
	return client.UploadFile(ctx, asset)
}

func (h *Host) authToken(ctx context.Context) (string, error) {
	args := []string{"auth", "token"}
	if h.host != "" {
		args = append(args, "--hostname", h.host)
	}
	cmd := h.cmd(ctx, "gh", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail != "" {
			return "", fmt.Errorf("gh auth token: %s", detail)
		}
		return "", fmt.Errorf("gh auth token: %w", err)
	}
	return strings.TrimSpace(stdout.String()), nil
}

func (h *Host) userAssetRepo(ctx context.Context) (int64, string, error) {
	repo := h.repoSlug()
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return 0, "", fmt.Errorf("resolve GitHub repository for user-attachments: invalid repository %q", repo)
	}
	args := []string{"api"}
	if h.host != "" {
		args = append(args, "--hostname", h.host)
	}
	args = append(args, "graphql", "-f", "query="+userAssetRepoQuery,
		"-F", "owner="+parts[0], "-F", "name="+parts[1])
	out, err := h.cmd(ctx, "gh", args...).CombinedOutput()
	if err != nil {
		return 0, "", fmt.Errorf("gh api repository for user-attachments: %s: %w", strings.TrimSpace(string(out)), err)
	}
	var response struct {
		Data struct {
			Repository *struct {
				DatabaseID       int64  `json:"databaseId"`
				ViewerPermission string `json:"viewerPermission"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out, &response); err != nil {
		return 0, "", fmt.Errorf("parse repository for user-attachments: %w", err)
	}
	if response.Data.Repository == nil {
		return 0, "", errors.New("user-attachments repository lookup returned no repository")
	}
	return response.Data.Repository.DatabaseID, strings.ToUpper(strings.TrimSpace(response.Data.Repository.ViewerPermission)), nil
}

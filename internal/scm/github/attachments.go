package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
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
// the same unofficial HTTP endpoint. no-mistakes sends the request through
// `gh api` so gh can authenticate it without exporting the raw credential to
// this process. The request shape is:
//
//	gh api --hostname <host> --method POST \
//	  --header "Content-Type: application/octet-stream" \
//	  --header "Accept: application/vnd.github+json" \
//	  --input - \
//	  {uploadsPrefix}user-attachments/assets
//	    ?name=<basename>&content_type=<mime>&repository_id=<numeric repo id>
//
// uploadsPrefix is https://uploads.github.com/ on github.com and
// https://uploads.<host>/ on GHEC (*.ghe.com). GHES is refused client-side:
// gh's checkHost rejects auth.IsEnterprise hosts, and GitHub has not published
// this endpoint for Enterprise Server.
//
// Response JSON is {"url":"https://github.com/user-attachments/assets/<uuid>"}.
//
// gh owns authentication; the endpoint rejects unsupported credential classes.
// The repository permission allowlist here is ADMIN/MAINTAIN/WRITE; READ/TRIAGE
// 404 at the endpoint. Write access is required to upload; repository access is
// required to view a private asset.
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
	return h.uploadUserAsset(ctx, asset, repoID)
}

// uploadUserAsset delegates the authenticated request to gh. In particular,
// this must not call `gh auth token` or populate an authorization header in
// no-mistakes: credential custody stays with gh and its configured keychain or
// credential provider.
func (h *Host) uploadUserAsset(ctx context.Context, asset UserAsset, repositoryID int64) (string, error) {
	if repositoryID <= 0 {
		return "", errors.New("could not determine which repository to attach files to")
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

	endpoint, err := url.Parse(userAssetUploadPrefix(h.host))
	if err != nil {
		return "", err
	}
	endpoint = endpoint.JoinPath("user-attachments", "assets")
	query := endpoint.Query()
	query.Set("name", filepath.Base(asset.Path))
	query.Set("content_type", asset.ContentType)
	query.Set("repository_id", strconv.FormatInt(repositoryID, 10))
	endpoint.RawQuery = query.Encode()

	args := []string{"api"}
	if h.host != "" {
		args = append(args, "--hostname", h.host)
	}
	args = append(args,
		"--method", "POST",
		"--header", "Accept: application/vnd.github+json",
		"--header", "Content-Type: application/octet-stream",
		"--header", fmt.Sprintf("Content-Length: %d", asset.Size),
		"--input", "-",
		endpoint.String(),
	)
	uploadCtx, cancel := context.WithTimeout(ctx, userAssetUploadTimeout)
	defer cancel()
	cmd := h.cmd(uploadCtx, "gh", args...)
	cmd.Stdin = body
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(uploadCtx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("gh api user-attachments upload timed out: %w", uploadCtx.Err())
		}
		detail := strings.TrimSpace(stderr.String())
		if strings.Contains(detail, "HTTP 404") {
			return "", errors.New("gh api user-attachments upload: GitHub returned HTTP 404; verify repository write access and use a credential type supported by GitHub user-attachments")
		}
		if detail != "" {
			return "", fmt.Errorf("gh api user-attachments upload: %s", detail)
		}
		return "", fmt.Errorf("gh api user-attachments upload: %w", err)
	}
	var response struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		return "", fmt.Errorf("parse gh api user-attachments response: %w", err)
	}
	return sanitizeUserAttachmentURL(response.URL, h.host)
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

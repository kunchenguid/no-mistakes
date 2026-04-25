package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

type releaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

type releaseResponse struct {
	TagName    string         `json:"tag_name"`
	Draft      bool           `json:"draft"`
	Prerelease bool           `json:"prerelease"`
	Assets     []releaseAsset `json:"assets"`
}

type releasePlan struct {
	CurrentVersion  string
	LatestVersion   string
	UpdateAvailable bool
	ArchiveName     string
	Archive         releaseAsset
	Checksums       releaseAsset
}

func pickReleaseAssets(app, tag string, assets []releaseAsset, platform platformSpec) (releaseAsset, releaseAsset, error) {
	archiveName := releaseArchiveName(app, tag, platform)
	var archive releaseAsset
	var checksums releaseAsset
	for _, asset := range assets {
		switch asset.Name {
		case archiveName:
			archive = asset
		case checksumsAssetName:
			checksums = asset
		}
	}
	if archive.Name == "" {
		return releaseAsset{}, releaseAsset{}, fmt.Errorf("release asset not found: %s", archiveName)
	}
	if checksums.Name == "" {
		return releaseAsset{}, releaseAsset{}, fmt.Errorf("release asset not found: %s", checksumsAssetName)
	}
	return archive, checksums, nil
}

func (u *updater) checkLatest(ctx context.Context) (*releasePlan, error) {
	release, err := u.fetchLatestRelease(ctx)
	if err != nil {
		return nil, err
	}
	plan := &releasePlan{
		CurrentVersion: u.currentVersion,
		LatestVersion:  release.TagName,
	}
	cmp, err := compareVersions(u.currentVersion, release.TagName)
	if err != nil {
		return nil, err
	}
	if cmp >= 0 {
		return plan, nil
	}
	archive, checksums, err := pickReleaseAssets(u.appName, release.TagName, release.Assets, u.platform)
	if err != nil {
		return nil, err
	}
	plan.UpdateAvailable = true
	plan.ArchiveName = releaseArchiveName(u.appName, release.TagName, u.platform)
	plan.Archive = archive
	plan.Checksums = checksums
	return plan, nil
}

func (u *updater) fetchLatestRelease(ctx context.Context) (*releaseResponse, error) {
	if u.httpClient == nil {
		u.httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	if u.includePrereleases {
		return u.fetchLatestReleaseIncludingPrereleases(ctx)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(u.apiBaseURL, "/")+"/repos/"+u.repo+"/releases/latest", nil)
	if err != nil {
		return nil, fmt.Errorf("build release request: %w", err)
	}
	resp, err := u.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch latest release: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch latest release: unexpected status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAPIResponseSize+1))
	if err != nil {
		return nil, fmt.Errorf("read latest release: %w", err)
	}
	if len(body) > maxAPIResponseSize {
		return nil, fmt.Errorf("latest release response exceeds %d bytes", maxAPIResponseSize)
	}
	var release releaseResponse
	if err := json.Unmarshal(body, &release); err != nil {
		return nil, fmt.Errorf("parse latest release: %w", err)
	}
	if release.TagName == "" {
		return nil, fmt.Errorf("latest release missing tag_name")
	}
	return &release, nil
}

// fetchLatestReleaseIncludingPrereleases finds the highest-semver release including
// prereleases. The unauthenticated /releases listing endpoint can lag minutes behind
// reality at GitHub's edge, so we cross-reference it with /tags (fresher in practice)
// and fall through to fetching the specific tag's release directly when the listing
// hasn't caught up yet.
func (u *updater) fetchLatestReleaseIncludingPrereleases(ctx context.Context) (*releaseResponse, error) {
	listed, listErr := u.fetchAllReleases(ctx)
	tags, tagsErr := u.fetchTagNames(ctx)
	if listErr != nil && tagsErr != nil {
		return nil, listErr
	}

	byTag := make(map[string]*releaseResponse, len(listed))
	for i := range listed {
		r := &listed[i]
		if r.Draft || r.TagName == "" {
			continue
		}
		byTag[r.TagName] = r
	}

	type candidate struct {
		name string
		ver  semVersion
	}
	seen := make(map[string]struct{})
	var candidates []candidate
	add := func(name string) {
		if name == "" {
			return
		}
		if _, ok := seen[name]; ok {
			return
		}
		seen[name] = struct{}{}
		v, err := parseVersion(name)
		if err != nil {
			return
		}
		candidates = append(candidates, candidate{name: name, ver: v})
	}
	for _, name := range tags {
		add(name)
	}
	for i := range listed {
		if listed[i].Draft {
			continue
		}
		add(listed[i].TagName)
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("no releases found")
	}
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].ver.compare(candidates[j].ver) > 0
	})

	const maxAttempts = 5
	var lastErr error
	for i, c := range candidates {
		if i >= maxAttempts {
			break
		}
		if r, ok := byTag[c.name]; ok {
			return r, nil
		}
		r, err := u.fetchReleaseByTag(ctx, c.name)
		if err != nil {
			lastErr = err
			continue
		}
		if r.Draft {
			continue
		}
		return r, nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("no releases found")
}

func (u *updater) fetchAllReleases(ctx context.Context) ([]releaseResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(u.apiBaseURL, "/")+"/repos/"+u.repo+"/releases", nil)
	if err != nil {
		return nil, fmt.Errorf("build releases request: %w", err)
	}
	resp, err := u.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch releases: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch releases: unexpected status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAPIResponseSize+1))
	if err != nil {
		return nil, fmt.Errorf("read releases: %w", err)
	}
	if len(body) > maxAPIResponseSize {
		return nil, fmt.Errorf("releases response exceeds %d bytes", maxAPIResponseSize)
	}
	var releases []releaseResponse
	if err := json.Unmarshal(body, &releases); err != nil {
		return nil, fmt.Errorf("parse releases: %w", err)
	}
	return releases, nil
}

func (u *updater) fetchTagNames(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(u.apiBaseURL, "/")+"/repos/"+u.repo+"/tags?per_page=100", nil)
	if err != nil {
		return nil, fmt.Errorf("build tags request: %w", err)
	}
	resp, err := u.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch tags: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch tags: unexpected status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAPIResponseSize+1))
	if err != nil {
		return nil, fmt.Errorf("read tags: %w", err)
	}
	if len(body) > maxAPIResponseSize {
		return nil, fmt.Errorf("tags response exceeds %d bytes", maxAPIResponseSize)
	}
	var tags []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(body, &tags); err != nil {
		return nil, fmt.Errorf("parse tags: %w", err)
	}
	names := make([]string, 0, len(tags))
	for _, t := range tags {
		if t.Name != "" {
			names = append(names, t.Name)
		}
	}
	return names, nil
}

func (u *updater) fetchReleaseByTag(ctx context.Context, tag string) (*releaseResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(u.apiBaseURL, "/")+"/repos/"+u.repo+"/releases/tags/"+tag, nil)
	if err != nil {
		return nil, fmt.Errorf("build release-by-tag request: %w", err)
	}
	resp, err := u.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch release for tag %s: %w", tag, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("no release for tag %s", tag)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch release for tag %s: unexpected status %d", tag, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAPIResponseSize+1))
	if err != nil {
		return nil, fmt.Errorf("read release for tag %s: %w", tag, err)
	}
	if len(body) > maxAPIResponseSize {
		return nil, fmt.Errorf("release response for tag %s exceeds %d bytes", tag, maxAPIResponseSize)
	}
	var release releaseResponse
	if err := json.Unmarshal(body, &release); err != nil {
		return nil, fmt.Errorf("parse release for tag %s: %w", tag, err)
	}
	return &release, nil
}

func (u *updater) downloadAsset(ctx context.Context, assetURL string, limit int64) ([]byte, error) {
	if err := ensureHTTPS(assetURL); err != nil {
		return nil, err
	}
	if u.httpClient == nil {
		u.httpClient = &http.Client{Timeout: 5 * time.Minute}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, assetURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build download request: %w", err)
	}
	resp, err := u.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download asset: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download asset: unexpected status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read asset: %w", err)
	}
	if int64(len(body)) > limit {
		return nil, fmt.Errorf("download exceeds %d bytes", limit)
	}
	return body, nil
}

func parseChecksums(data []byte) (map[string]string, error) {
	checksums := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) != 2 {
			return nil, fmt.Errorf("parse checksums: malformed line %q", line)
		}
		checksums[parts[1]] = parts[0]
	}
	return checksums, nil
}

func verifyChecksum(data []byte, want string) error {
	sum := sha256.Sum256(data)
	got := hex.EncodeToString(sum[:])
	if got != want {
		return fmt.Errorf("checksum mismatch: got %s want %s", got, want)
	}
	return nil
}

func ensureHTTPS(rawURL string) error {
	if allowInsecureDownloads {
		return nil
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("parse url: %w", err)
	}
	if u.Scheme != "https" {
		return fmt.Errorf("reject non-https url: %s", rawURL)
	}
	return nil
}

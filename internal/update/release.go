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

func (u *updater) fetchLatestReleaseIncludingPrereleases(ctx context.Context) (*releaseResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(u.apiBaseURL, "/")+"/repos/"+u.repo+"/releases", nil)
	if err != nil {
		return nil, fmt.Errorf("build release request: %w", err)
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
	var best *releaseResponse
	var bestVer semVersion
	for i := range releases {
		r := &releases[i]
		if r.Draft || r.TagName == "" {
			continue
		}
		v, err := parseVersion(r.TagName)
		if err != nil {
			continue
		}
		if best == nil || v.compare(bestVer) > 0 {
			best = r
			bestVer = v
		}
	}
	if best == nil {
		return nil, fmt.Errorf("no releases found")
	}
	return best, nil
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

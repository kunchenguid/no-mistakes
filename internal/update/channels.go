package update

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

const (
	channelsReleaseTag = "channels"
	channelsAssetName  = "channels.json"
	channelsSchemaV1   = 1
)

func defaultManifestURL(repo string) string {
	return "https://github.com/" + repo + "/releases/download/" + channelsReleaseTag + "/" + channelsAssetName
}

// channelsManifest is the release-asset document the updater reads over the
// GitHub download CDN. It names the current stable and beta (highest-semver
// including prereleases) heads so version discovery does not use the rate-limited
// REST API.
type channelsManifest struct {
	SchemaVersion int              `json:"schema_version"`
	Stable        *releaseResponse `json:"stable,omitempty"`
	Beta          *releaseResponse `json:"beta,omitempty"`
}

func parseChannelsManifest(data []byte) (*channelsManifest, error) {
	var m channelsManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse channel manifest: %w", err)
	}
	if m.SchemaVersion != channelsSchemaV1 {
		return nil, fmt.Errorf("channel manifest schema_version %d is unsupported", m.SchemaVersion)
	}
	return &m, nil
}

func (m *channelsManifest) channel(includePrereleases bool) (*releaseResponse, error) {
	if m == nil {
		return nil, fmt.Errorf("channel manifest is empty")
	}
	release := m.Stable
	if includePrereleases {
		release = m.Beta
	}
	if release == nil || release.TagName == "" {
		if includePrereleases {
			return nil, fmt.Errorf("channel manifest missing beta release")
		}
		return nil, fmt.Errorf("channel manifest missing stable release")
	}
	return release, nil
}

// EncodeChannelsManifest builds the CDN channel document from GitHub REST
// payloads. latestStableJSON is the body of GET /repos/{repo}/releases/latest
// (empty when that endpoint 404s). allReleasesJSON is the array from
// GET /repos/{repo}/releases. Beta is the highest-semver non-draft release,
// including prereleases, matching the updater's --beta selection.
func EncodeChannelsManifest(latestStableJSON, allReleasesJSON []byte) ([]byte, error) {
	var all []releaseResponse
	if len(bytes.TrimSpace(allReleasesJSON)) > 0 {
		if err := json.Unmarshal(allReleasesJSON, &all); err != nil {
			return nil, fmt.Errorf("parse releases list: %w", err)
		}
	}
	manifest := channelsManifest{SchemaVersion: channelsSchemaV1}
	if release := highestRelease(all, true); release != nil {
		copied := *release
		manifest.Beta = &copied
	}
	manifest.Stable = decodeLatestStable(latestStableJSON)
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode channel manifest: %w", err)
	}
	data = append(data, '\n')
	return data, nil
}

func decodeLatestStable(latestStableJSON []byte) *releaseResponse {
	if len(bytes.TrimSpace(latestStableJSON)) == 0 {
		return nil
	}
	var release releaseResponse
	if err := json.Unmarshal(latestStableJSON, &release); err != nil {
		return nil
	}
	if !usableChannelRelease(release, false) {
		return nil
	}
	return &release
}

func highestRelease(releases []releaseResponse, includePrereleases bool) *releaseResponse {
	var best *releaseResponse
	var bestVer semVersion
	for i := range releases {
		r := releases[i]
		if !usableChannelRelease(r, includePrereleases) {
			continue
		}
		v, err := parseVersion(r.TagName)
		if err != nil {
			continue
		}
		if best == nil || v.compare(bestVer) > 0 {
			copied := r
			best = &copied
			bestVer = v
		}
	}
	return best
}

func usableChannelRelease(r releaseResponse, includePrereleases bool) bool {
	if r.Draft || r.TagName == "" || r.TagName == channelsReleaseTag {
		return false
	}
	if !includePrereleases && r.Prerelease {
		return false
	}
	return true
}

func (u *updater) fetchReleaseFromManifest(ctx context.Context) (*releaseResponse, error) {
	if err := ensureHTTPS(u.manifestURL); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.manifestURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build channel manifest request: %w", err)
	}
	resp, err := u.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch channel manifest: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetch channel manifest: unexpected status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxAPIResponseSize+1))
	if err != nil {
		return nil, fmt.Errorf("read channel manifest: %w", err)
	}
	if len(body) > maxAPIResponseSize {
		return nil, fmt.Errorf("channel manifest exceeds %d bytes", maxAPIResponseSize)
	}
	manifest, err := parseChannelsManifest(body)
	if err != nil {
		return nil, err
	}
	return manifest.channel(u.includePrereleases)
}

package update

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDefaultManifestURLUsesReleaseAssetCDN(t *testing.T) {
	got := defaultManifestURL("kunchenguid/no-mistakes")
	if strings.Contains(got, "api.github.com") {
		t.Fatalf("manifest URL %q must not use the GitHub REST API host", got)
	}
	want := "https://github.com/kunchenguid/no-mistakes/releases/download/channels/channels.json"
	if got != want {
		t.Fatalf("defaultManifestURL = %q, want %q", got, want)
	}
}

func TestEncodeChannelsManifestDistinguishesStableAndHighestSemverBeta(t *testing.T) {
	latestStable := []byte(`{
		"tag_name":"v1.2.3",
		"draft":false,
		"prerelease":false,
		"assets":[{"name":"checksums.txt","browser_download_url":"https://github.com/example/checksums-stable"}]
	}`)
	all := []byte(`[
		{"tag_name":"channels","draft":false,"prerelease":true,"assets":[{"name":"channels.json","browser_download_url":"https://github.com/example/channels.json"}]},
		{"tag_name":"v1.3.0-beta.1","draft":false,"prerelease":true,"assets":[{"name":"checksums.txt","browser_download_url":"https://github.com/example/checksums-beta1"}]},
		{"tag_name":"v1.3.0-beta.2","draft":false,"prerelease":true,"assets":[{"name":"checksums.txt","browser_download_url":"https://github.com/example/checksums-beta2"}]},
		{"tag_name":"v1.2.3","draft":false,"prerelease":false,"assets":[{"name":"checksums.txt","browser_download_url":"https://github.com/example/checksums-stable"}]},
		{"tag_name":"v1.4.0-draft","draft":true,"prerelease":true,"assets":[{"name":"checksums.txt","browser_download_url":"https://github.com/example/checksums-draft"}]}
	]`)

	data, err := EncodeChannelsManifest(latestStable, all)
	if err != nil {
		t.Fatalf("EncodeChannelsManifest error = %v", err)
	}
	manifest, err := parseChannelsManifest(data)
	if err != nil {
		t.Fatalf("parseChannelsManifest error = %v", err)
	}
	if manifest.Stable == nil || manifest.Stable.TagName != "v1.2.3" {
		t.Fatalf("stable = %+v, want v1.2.3", manifest.Stable)
	}
	if manifest.Beta == nil || manifest.Beta.TagName != "v1.3.0-beta.2" {
		t.Fatalf("beta = %+v, want v1.3.0-beta.2 (highest semver, skipping draft and channels tag)", manifest.Beta)
	}

	stable, err := manifest.channel(false)
	if err != nil {
		t.Fatalf("stable channel error = %v", err)
	}
	if stable.TagName != "v1.2.3" {
		t.Fatalf("stable channel tag = %q", stable.TagName)
	}
	beta, err := manifest.channel(true)
	if err != nil {
		t.Fatalf("beta channel error = %v", err)
	}
	if beta.TagName != "v1.3.0-beta.2" {
		t.Fatalf("beta channel tag = %q", beta.TagName)
	}
}

func TestEncodeChannelsManifestBetaCanBeStableWhenStableIsNewest(t *testing.T) {
	latestStable := []byte(`{"tag_name":"v2.0.0","draft":false,"prerelease":false,"assets":[]}`)
	all := []byte(`[
		{"tag_name":"v2.0.0","draft":false,"prerelease":false,"assets":[]},
		{"tag_name":"v1.9.0-beta.1","draft":false,"prerelease":true,"assets":[]}
	]`)

	data, err := EncodeChannelsManifest(latestStable, all)
	if err != nil {
		t.Fatalf("EncodeChannelsManifest error = %v", err)
	}
	manifest, err := parseChannelsManifest(data)
	if err != nil {
		t.Fatalf("parseChannelsManifest error = %v", err)
	}
	if manifest.Beta == nil || manifest.Beta.TagName != "v2.0.0" {
		t.Fatalf("beta = %+v, want v2.0.0 because a newer stable outranks older prereleases", manifest.Beta)
	}
}

func TestEncodeChannelsManifestOmitsUnusableLatestStable(t *testing.T) {
	data, err := EncodeChannelsManifest([]byte(`{"tag_name":"channels","draft":false,"prerelease":true,"assets":[]}`), []byte(`[
		{"tag_name":"v1.2.3","draft":false,"prerelease":false,"assets":[]}
	]`))
	if err != nil {
		t.Fatalf("EncodeChannelsManifest error = %v", err)
	}
	manifest, err := parseChannelsManifest(data)
	if err != nil {
		t.Fatalf("parseChannelsManifest error = %v", err)
	}
	if manifest.Stable == nil || manifest.Stable.TagName != "v1.2.3" {
		t.Fatalf("stable = %+v, want computed v1.2.3 after rejecting the channels tag", manifest.Stable)
	}
}

func TestFetchLatestRelease_ManifestPathDoesNotCallGitHubRESTAPI(t *testing.T) {
	allowInsecureDownloads = true
	t.Cleanup(func() { allowInsecureDownloads = false })

	var apiHits []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/repos/") {
			apiHits = append(apiHits, r.URL.Path)
			http.Error(w, "rate limited", http.StatusForbidden)
			return
		}
		if r.URL.Path != "/releases/download/channels/channels.json" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Fatalf("manifest fetch sent Authorization %q, want anonymous", got)
		}
		fmt.Fprint(w, `{
			"schema_version":1,
			"stable":{"tag_name":"v1.2.3","prerelease":false,"assets":[{"name":"checksums.txt","browser_download_url":"https://example.com/stable"}]},
			"beta":{"tag_name":"v1.3.0-beta.2","prerelease":true,"assets":[{"name":"checksums.txt","browser_download_url":"https://example.com/beta"}]}
		}`)
	}))
	defer server.Close()

	t.Setenv("GITHUB_TOKEN", "must-not-be-sent-on-manifest")
	t.Setenv("GH_TOKEN", "")

	u := &updater{
		repo:           "kunchenguid/no-mistakes",
		currentVersion: "v1.2.2",
		apiBaseURL:     server.URL,
		manifestURL:    server.URL + "/releases/download/channels/channels.json",
		httpClient:     server.Client(),
	}

	stable, err := u.fetchLatestRelease(context.Background())
	if err != nil {
		t.Fatalf("stable fetchLatestRelease error = %v", err)
	}
	if stable.TagName != "v1.2.3" {
		t.Fatalf("stable tag = %q, want v1.2.3", stable.TagName)
	}

	u.includePrereleases = true
	beta, err := u.fetchLatestRelease(context.Background())
	if err != nil {
		t.Fatalf("beta fetchLatestRelease error = %v", err)
	}
	if beta.TagName != "v1.3.0-beta.2" {
		t.Fatalf("beta tag = %q, want v1.3.0-beta.2", beta.TagName)
	}
	if len(apiHits) != 0 {
		t.Fatalf("REST API paths hit on the manifest happy path: %v", apiHits)
	}
}

func TestFetchLatestRelease_ManifestSucceedsWhenRESTAPIReturns403(t *testing.T) {
	allowInsecureDownloads = true
	t.Cleanup(func() { allowInsecureDownloads = false })

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/repos/") {
			http.Error(w, "API rate limit exceeded", http.StatusForbidden)
			return
		}
		fmt.Fprint(w, `{"schema_version":1,"stable":{"tag_name":"v1.2.3","assets":[]},"beta":{"tag_name":"v1.3.0-beta.1","assets":[]}}`)
	}))
	defer server.Close()

	u := &updater{
		repo:        "kunchenguid/no-mistakes",
		apiBaseURL:  server.URL,
		manifestURL: server.URL + "/releases/download/channels/channels.json",
		httpClient:  server.Client(),
	}
	release, err := u.fetchLatestRelease(context.Background())
	if err != nil {
		t.Fatalf("fetchLatestRelease should ignore REST 403 when the manifest is reachable, error = %v", err)
	}
	if release.TagName != "v1.2.3" {
		t.Fatalf("tag = %q, want v1.2.3", release.TagName)
	}
}

func TestFetchLatestRelease_FallsBackToRESTWhenManifestMissing(t *testing.T) {
	allowInsecureDownloads = true
	t.Cleanup(func() { allowInsecureDownloads = false })

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/releases/download/channels/channels.json":
			http.NotFound(w, r)
		case "/repos/kunchenguid/no-mistakes/releases/latest":
			fmt.Fprint(w, `{"tag_name":"v1.2.3","assets":[]}`)
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	defer server.Close()

	u := &updater{
		repo:        "kunchenguid/no-mistakes",
		apiBaseURL:  server.URL,
		manifestURL: server.URL + "/releases/download/channels/channels.json",
		httpClient:  server.Client(),
	}
	release, err := u.fetchLatestRelease(context.Background())
	if err != nil {
		t.Fatalf("fetchLatestRelease error = %v", err)
	}
	if release.TagName != "v1.2.3" {
		t.Fatalf("tag = %q, want v1.2.3 from REST fallback", release.TagName)
	}
}

func TestFetchLatestRelease_FallbackUsesEnvToken(t *testing.T) {
	allowInsecureDownloads = true
	t.Cleanup(func() { allowInsecureDownloads = false })

	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing.json" {
			http.NotFound(w, r)
			return
		}
		gotAuth = r.Header.Get("Authorization")
		fmt.Fprint(w, `{"tag_name":"v1.2.3","assets":[]}`)
	}))
	defer server.Close()

	u := &updater{
		repo:        "kunchenguid/no-mistakes",
		apiBaseURL:  server.URL,
		manifestURL: server.URL + "/missing.json",
		httpClient:  server.Client(),
	}
	t.Setenv("GITHUB_TOKEN", "fallback-token")
	t.Setenv("GH_TOKEN", "")
	if _, err := u.fetchLatestRelease(context.Background()); err != nil {
		t.Fatalf("fetchLatestRelease error = %v", err)
	}
	if got, want := gotAuth, "Bearer fallback-token"; got != want {
		t.Fatalf("REST fallback Authorization = %q, want %q", got, want)
	}
}

func TestParseChannelsManifestRejectsUnknownSchema(t *testing.T) {
	if _, err := parseChannelsManifest([]byte(`{"schema_version":2,"stable":{"tag_name":"v1.0.0"}}`)); err == nil {
		t.Fatal("parseChannelsManifest should reject unsupported schema_version")
	}
}

func TestCheckLatest_ManifestStableAndBetaSelectDifferentHeads(t *testing.T) {
	allowInsecureDownloads = true
	t.Cleanup(func() { allowInsecureDownloads = false })

	stableArchive := "no-mistakes-v1.2.3-darwin-arm64.tar.gz"
	betaArchive := "no-mistakes-v1.3.0-beta.2-darwin-arm64.tar.gz"
	manifest := channelsManifest{
		SchemaVersion: 1,
		Stable: &releaseResponse{
			TagName:    "v1.2.3",
			Prerelease: false,
			Assets: []releaseAsset{
				{Name: stableArchive, BrowserDownloadURL: "https://example.com/stable.tgz"},
				{Name: checksumsAssetName, BrowserDownloadURL: "https://example.com/stable-sum"},
			},
		},
		Beta: &releaseResponse{
			TagName:    "v1.3.0-beta.2",
			Prerelease: true,
			Assets: []releaseAsset{
				{Name: betaArchive, BrowserDownloadURL: "https://example.com/beta.tgz"},
				{Name: checksumsAssetName, BrowserDownloadURL: "https://example.com/beta-sum"},
			},
		},
	}
	payload, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}

	var apiHits int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/repos/") {
			apiHits++
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if _, writeErr := w.Write(payload); writeErr != nil {
			t.Fatalf("write manifest: %v", writeErr)
		}
	}))
	defer server.Close()

	base := &updater{
		appName:        "no-mistakes",
		repo:           "kunchenguid/no-mistakes",
		currentVersion: "v1.2.2",
		platform:       platformSpec{GOOS: "darwin", GOARCH: "arm64"},
		apiBaseURL:     server.URL,
		manifestURL:    server.URL + "/channels.json",
		httpClient:     server.Client(),
	}

	stablePlan, err := base.checkLatest(context.Background())
	if err != nil {
		t.Fatalf("stable checkLatest error = %v", err)
	}
	if stablePlan.LatestVersion != "v1.2.3" {
		t.Fatalf("stable LatestVersion = %q", stablePlan.LatestVersion)
	}
	if stablePlan.ArchiveName != stableArchive {
		t.Fatalf("stable ArchiveName = %q", stablePlan.ArchiveName)
	}

	base.includePrereleases = true
	betaPlan, err := base.checkLatest(context.Background())
	if err != nil {
		t.Fatalf("beta checkLatest error = %v", err)
	}
	if betaPlan.LatestVersion != "v1.3.0-beta.2" {
		t.Fatalf("beta LatestVersion = %q", betaPlan.LatestVersion)
	}
	if betaPlan.ArchiveName != betaArchive {
		t.Fatalf("beta ArchiveName = %q", betaPlan.ArchiveName)
	}
	if apiHits != 0 {
		t.Fatalf("REST API hit %d times on the manifest path", apiHits)
	}
}

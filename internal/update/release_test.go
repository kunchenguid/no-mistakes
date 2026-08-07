package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestApplyGitHubAuth(t *testing.T) {
	t.Run("no token set leaves request anonymous", func(t *testing.T) {
		t.Setenv("GITHUB_TOKEN", "")
		t.Setenv("GH_TOKEN", "")
		req, err := http.NewRequest(http.MethodGet, "https://example.com", nil)
		if err != nil {
			t.Fatalf("NewRequest error = %v", err)
		}
		applyGitHubAuth(req)
		if got := req.Header.Get("Authorization"); got != "" {
			t.Fatalf("Authorization header = %q, want empty", got)
		}
	})

	t.Run("GITHUB_TOKEN populates Authorization header", func(t *testing.T) {
		t.Setenv("GITHUB_TOKEN", "gh-token-value")
		t.Setenv("GH_TOKEN", "")
		req, err := http.NewRequest(http.MethodGet, "https://example.com", nil)
		if err != nil {
			t.Fatalf("NewRequest error = %v", err)
		}
		applyGitHubAuth(req)
		if got, want := req.Header.Get("Authorization"), "Bearer gh-token-value"; got != want {
			t.Fatalf("Authorization header = %q, want %q", got, want)
		}
	})

	t.Run("GH_TOKEN is used when GITHUB_TOKEN is absent", func(t *testing.T) {
		t.Setenv("GITHUB_TOKEN", "")
		t.Setenv("GH_TOKEN", "fallback-token-value")
		req, err := http.NewRequest(http.MethodGet, "https://example.com", nil)
		if err != nil {
			t.Fatalf("NewRequest error = %v", err)
		}
		applyGitHubAuth(req)
		if got, want := req.Header.Get("Authorization"), "Bearer fallback-token-value"; got != want {
			t.Fatalf("Authorization header = %q, want %q", got, want)
		}
	})

	t.Run("GITHUB_TOKEN takes precedence over GH_TOKEN", func(t *testing.T) {
		t.Setenv("GITHUB_TOKEN", "primary-token-value")
		t.Setenv("GH_TOKEN", "fallback-token-value")
		req, err := http.NewRequest(http.MethodGet, "https://example.com", nil)
		if err != nil {
			t.Fatalf("NewRequest error = %v", err)
		}
		applyGitHubAuth(req)
		if got, want := req.Header.Get("Authorization"), "Bearer primary-token-value"; got != want {
			t.Fatalf("Authorization header = %q, want %q", got, want)
		}
	})
}

func TestFetchLatestRelease_SendsAuthorizationHeaderFromEnvToken(t *testing.T) {
	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		fmt.Fprint(w, `{"tag_name":"v1.2.3","assets":[]}`)
	}))
	defer server.Close()

	u := &updater{
		appName:        "no-mistakes",
		repo:           "kunchenguid/no-mistakes",
		currentVersion: "v1.2.2",
		apiBaseURL:     server.URL,
		httpClient:     server.Client(),
	}

	t.Run("token present", func(t *testing.T) {
		t.Setenv("GITHUB_TOKEN", "env-token-value")
		t.Setenv("GH_TOKEN", "")
		if _, err := u.fetchLatestRelease(context.Background()); err != nil {
			t.Fatalf("fetchLatestRelease error = %v", err)
		}
		if want := "Bearer env-token-value"; gotAuth != want {
			t.Fatalf("Authorization header = %q, want %q", gotAuth, want)
		}
	})

	t.Run("no token present", func(t *testing.T) {
		t.Setenv("GITHUB_TOKEN", "")
		t.Setenv("GH_TOKEN", "")
		if _, err := u.fetchLatestRelease(context.Background()); err != nil {
			t.Fatalf("fetchLatestRelease error = %v", err)
		}
		if gotAuth != "" {
			t.Fatalf("Authorization header = %q, want empty", gotAuth)
		}
	})
}

func TestDownloadAsset_SendsAuthorizationHeaderFromEnvToken(t *testing.T) {
	allowInsecureDownloads = true
	t.Cleanup(func() { allowInsecureDownloads = false })

	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		fmt.Fprint(w, "asset-bytes")
	}))
	defer server.Close()

	u := &updater{
		httpClient: server.Client(),
	}

	t.Setenv("GITHUB_TOKEN", "download-token-value")
	t.Setenv("GH_TOKEN", "")
	if _, err := u.downloadAsset(context.Background(), server.URL, 1<<20); err != nil {
		t.Fatalf("downloadAsset error = %v", err)
	}
	if want := "Bearer download-token-value"; gotAuth != want {
		t.Fatalf("Authorization header = %q, want %q", gotAuth, want)
	}
}

func TestPickReleaseAssets(t *testing.T) {
	assets := []releaseAsset{
		{Name: "no-mistakes-v1.2.3-darwin-arm64.tar.gz", BrowserDownloadURL: "https://example.com/archive"},
		{Name: "checksums.txt", BrowserDownloadURL: "https://example.com/checksums"},
		{Name: "ignored.txt", BrowserDownloadURL: "https://example.com/ignored"},
	}

	archive, checksums, err := pickReleaseAssets("no-mistakes", "v1.2.3", assets, platformSpec{GOOS: "darwin", GOARCH: "arm64"})
	if err != nil {
		t.Fatalf("pickReleaseAssets error = %v", err)
	}
	if archive.Name != "no-mistakes-v1.2.3-darwin-arm64.tar.gz" {
		t.Fatalf("archive = %q", archive.Name)
	}
	if checksums.Name != "checksums.txt" {
		t.Fatalf("checksums = %q", checksums.Name)
	}

	_, _, err = pickReleaseAssets("no-mistakes", "v1.2.3", assets[:1], platformSpec{GOOS: "darwin", GOARCH: "arm64"})
	if err == nil {
		t.Fatal("pickReleaseAssets should fail when checksums asset is missing")
	}
}

func TestParseChecksums(t *testing.T) {
	archiveName := "no-mistakes-v1.2.3-darwin-arm64.tar.gz"
	data := []byte("abc123  no-mistakes-v1.2.3-darwin-arm64.tar.gz\ndef456  other.tar.gz\n")

	checksums, err := parseChecksums(data)
	if err != nil {
		t.Fatalf("parseChecksums error = %v", err)
	}
	if checksums[archiveName] != "abc123" {
		t.Fatalf("checksum = %q", checksums[archiveName])
	}

	if _, err := parseChecksums([]byte("bad-line")); err == nil {
		t.Fatal("parseChecksums should reject malformed input")
	}
}

func TestVerifyChecksum(t *testing.T) {
	payload := []byte("archive-bytes")
	hash := sha256.Sum256(payload)
	want := hex.EncodeToString(hash[:])

	if err := verifyChecksum(payload, want); err != nil {
		t.Fatalf("verifyChecksum error = %v", err)
	}
	if err := verifyChecksum(payload, stringsRepeat("0", 64)); err == nil {
		t.Fatal("verifyChecksum should fail on mismatch")
	}
}

func TestEnsureHTTPS(t *testing.T) {
	if err := ensureHTTPS("https://example.com/release"); err != nil {
		t.Fatalf("ensureHTTPS rejected https url: %v", err)
	}
	if err := ensureHTTPS("http://example.com/release"); err == nil {
		t.Fatal("ensureHTTPS should reject http urls")
	}
}

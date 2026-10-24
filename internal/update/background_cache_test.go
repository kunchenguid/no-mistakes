package update

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/paths"
)

func TestBackgroundCacheFreshness(t *testing.T) {
	allowInsecureDownloads = true
	t.Cleanup(func() { allowInsecureDownloads = false })
	t.Setenv(noUpdateCheckEnv, "")
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name, current, latest string
		age                   time.Duration
		invocations, fetches  int
		notice                bool
	}{
		{"prerelease ahead of legacy stable", "v1.97.2", "v1.84.0", time.Hour, 2, 0, false},
		{"semver prerelease ahead of legacy stable", "v1.98.0-beta.1", "v1.84.0", time.Hour, 2, 0, false},
		{"installed equals answer", "v1.84.0", "v1.84.0", time.Hour, 2, 0, false},
		{"pending update is noticed and refreshed", "v1.83.0", "v1.84.0", time.Hour, 1, 1, true},
		{"expired answer refreshes once", "v1.84.0", "v1.84.0", cacheTTL + time.Second, 2, 1, false},
		{"expired prerelease answer refreshes once", "v1.100.2", "v1.84.0", cacheTTL + time.Second, 2, 1, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			fetches := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fetches++
				fmt.Fprint(w, backgroundChannelsFixture)
			}))
			defer server.Close()
			cachePath := filepath.Join(t.TempDir(), "update-check.json")
			if err := writeCache(cachePath, &checkCache{CheckedAt: now.Add(-tt.age), LatestVersion: tt.latest}); err != nil {
				t.Fatal(err)
			}
			stderr := new(bytes.Buffer)
			u := &updater{
				appName: "no-mistakes", currentVersion: tt.current,
				platform:    platformSpec{GOOS: "darwin", GOARCH: "arm64"},
				manifestURL: server.URL, httpClient: server.Client(), cachePath: cachePath,
				stderr: stderr, noColor: true, now: func() time.Time { return now },
			}
			u.spawnBackground = func(version string) error {
				if version != tt.current {
					t.Fatalf("background version = %q, want %q", version, tt.current)
				}
				// Complete the child synchronously so a second invocation sees its cache.
				if err := u.refreshCache(context.Background()); err != nil {
					t.Fatal(err)
				}
				return nil
			}
			for range tt.invocations {
				u.maybeNotifyAndCheck([]string{"status"})
			}
			if fetches != tt.fetches {
				t.Errorf("manifest fetches = %d, want %d", fetches, tt.fetches)
			}
			wantNotice := ""
			if tt.notice {
				wantNotice = fmt.Sprintf("A new version of no-mistakes is available: %s -> %s\nRun \"no-mistakes update\" to update\n", tt.current, tt.latest)
			}
			if stderr.String() != wantNotice {
				t.Errorf("notice = %q, want %q", stderr.String(), wantNotice)
			}
		})
	}
}

func TestBackgroundAndExplicitUpdateKeepChannelAnswersSeparate(t *testing.T) {
	allowInsecureDownloads = true
	t.Cleanup(func() { allowInsecureDownloads = false })
	t.Setenv(noUpdateCheckEnv, "")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, backgroundChannelsFixture)
	}))
	defer server.Close()
	cachePath := filepath.Join(t.TempDir(), "update-check.json")
	u := &updater{
		appName: "no-mistakes", currentVersion: "v1.97.2",
		platform:    platformSpec{GOOS: "darwin", GOARCH: "arm64"},
		manifestURL: server.URL, httpClient: server.Client(), cachePath: cachePath,
		now: time.Now,
	}
	if err := u.refreshCache(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := u.cachedLatestVersion(); got != "v1.100.2" {
		t.Errorf("background prerelease answer = %q, want v1.100.2", got)
	}
	// The explicit command still defaults to stable, even on a beta install.
	if err := u.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := u.cachedLatestVersion(); got != "v1.100.2" {
		t.Errorf("explicit stable update clobbered beta answer: %q", got)
	}
	// Exercise --beta without installing anything or touching a daemon.
	u.includePrereleases = true
	u.currentVersion = "v1.100.2"
	if err := u.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	stable := readCache(cachePath)
	beta := readCache(filepath.Join(filepath.Dir(cachePath), "update-check-beta.json"))
	if stable == nil || stable.LatestVersion != "v1.84.0" {
		t.Errorf("stable cache = %#v", stable)
	}
	if beta == nil || beta.LatestVersion != "v1.100.2" {
		t.Errorf("beta cache = %#v", beta)
	}
	u.currentVersion = "v1.84.0"
	u.includePrereleases = false
	if err := u.refreshCache(context.Background()); err != nil {
		t.Fatal(err)
	}
	beta = readCache(filepath.Join(filepath.Dir(cachePath), "update-check-beta.json"))
	if beta == nil || beta.LatestVersion != "v1.100.2" {
		t.Errorf("stable background refresh clobbered beta answer: %#v", beta)
	}
	if got := u.cachedLatestVersion(); got != "" {
		t.Errorf("stable install consulted beta answer: %q", got)
	}
	// A semver prerelease stays on beta even when stable is ahead of it.
	u.currentVersion = "v1.84.0-beta.1"
	if err := u.refreshCache(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := u.cachedLatestVersion(); got != "v1.100.2" {
		t.Errorf("semver prerelease consulted stable answer: %q", got)
	}
}

const backgroundChannelsFixture = `{"schema_version":1,"stable":{"tag_name":"v1.84.0","assets":[{"name":"no-mistakes-v1.84.0-darwin-arm64.tar.gz","browser_download_url":"https://example.com/stable"},{"name":"checksums.txt","browser_download_url":"https://example.com/checksums"}]},"beta":{"tag_name":"v1.100.2","prerelease":true,"assets":[{"name":"no-mistakes-v1.100.2-darwin-arm64.tar.gz","browser_download_url":"https://example.com/archive"},{"name":"checksums.txt","browser_download_url":"https://example.com/checksums"}]}}`

func TestBackgroundCacheExemptions(t *testing.T) {
	for _, tt := range []struct {
		name, version, optOut string
		args                  []string
		disabled, nix         bool
	}{
		{name: "long version query", args: []string{"--version"}},
		{name: "short version query", args: []string{"-v"}},
		{name: "explicit update", args: []string{"update", "--beta"}},
		{name: "background child", args: []string{backgroundFlag, "v1.83.0"}},
		{name: "development build", version: "dev"},
		{name: "self-update unavailable", nix: true},
		{name: "background disabled", disabled: true},
		{name: "existing environment opt-out", optOut: "1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(noUpdateCheckEnv, tt.optOut)
			version := tt.version
			if version == "" {
				version = "v1.83.0"
			}
			cachePath := filepath.Join(t.TempDir(), "update-check.json")
			if err := writeCache(cachePath, &checkCache{CheckedAt: time.Now().Add(-2 * cacheTTL), LatestVersion: "v1.84.0"}); err != nil {
				t.Fatal(err)
			}
			stderr := new(bytes.Buffer)
			u := &updater{
				currentVersion: version, cachePath: cachePath, stderr: stderr, now: time.Now,
				disableBackground: tt.disabled, nixStoreInstall: tt.nix,
				spawnBackground: func(string) error {
					t.Fatal("exempt invocation spawned a background refresh")
					return nil
				},
			}
			u.maybeNotifyAndCheck(tt.args)
			if stderr.Len() != 0 {
				t.Fatalf("exempt invocation printed a notice: %q", stderr.String())
			}
		})
	}
}

type updateCheckTransport func(*http.Request) (*http.Response, error)

func (f updateCheckTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestBackgroundChildUsesRunningBuildChannel(t *testing.T) {
	t.Setenv("NM_HOME", t.TempDir())
	// The recursion guard must not suppress the child's requested refresh.
	t.Setenv(noUpdateCheckEnv, "1")
	previous := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = previous })
	fetches := 0
	http.DefaultTransport = updateCheckTransport(func(req *http.Request) (*http.Response, error) {
		fetches++
		if req.URL.String() != defaultManifestURL(repoName) {
			t.Fatalf("unexpected update URL: %s", req.URL)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(backgroundChannelsFixture)), Header: make(http.Header)}, nil
	})
	handled, err := MaybeHandleBackgroundCheck([]string{backgroundFlag, "v1.100.2"})
	if !handled || err != nil {
		t.Fatalf("background child: handled=%v, err=%v", handled, err)
	}
	if fetches != 1 {
		t.Fatalf("background child fetched %d times, want 1", fetches)
	}
	p, err := paths.New()
	if err != nil {
		t.Fatal(err)
	}
	beta := readCache(filepath.Join(filepath.Dir(p.UpdateCheckFile()), "update-check-beta.json"))
	if beta == nil || beta.LatestVersion != "v1.100.2" {
		t.Fatalf("background child beta cache = %#v", beta)
	}
}

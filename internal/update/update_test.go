package update

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/paths"
)

func TestCompareVersions(t *testing.T) {
	tests := []struct {
		name    string
		a       string
		b       string
		wantCmp int
	}{
		{name: "equal with v prefix", a: "v1.2.3", b: "v1.2.3", wantCmp: 0},
		{name: "equal without v prefix", a: "1.2.3", b: "1.2.3", wantCmp: 0},
		{name: "patch newer", a: "v1.2.4", b: "v1.2.3", wantCmp: 1},
		{name: "patch older", a: "v1.2.3", b: "v1.2.4", wantCmp: -1},
		{name: "minor newer", a: "v1.3.0", b: "v1.2.9", wantCmp: 1},
		{name: "major newer", a: "v2.0.0", b: "v1.9.9", wantCmp: 1},
		{name: "missing patch treated as zero", a: "v1.2", b: "v1.2.0", wantCmp: 0},
		{name: "missing minor and patch treated as zero", a: "v1", b: "v1.0.0", wantCmp: 0},
		{name: "prerelease less than release", a: "v1.0.0-beta", b: "v1.0.0", wantCmp: -1},
		{name: "prerelease lexical compare", a: "v1.0.0-beta", b: "v1.0.0-rc1", wantCmp: -1},
		{name: "release greater than prerelease", a: "v1.0.0", b: "v1.0.0-rc1", wantCmp: 1},
		{name: "numeric prerelease compare", a: "v1.0.0-2", b: "v1.0.0-10", wantCmp: -1},
		{name: "build metadata ignored", a: "v1.2.3+abc", b: "v1.2.3+def", wantCmp: 0},
		{name: "prerelease with build metadata", a: "v1.2.3-rc1+abc", b: "v1.2.3-rc1+def", wantCmp: 0},
		{name: "different prerelease lengths", a: "v1.2.3-alpha.1", b: "v1.2.3-alpha", wantCmp: 1},
		{name: "numeric prerelease less than string", a: "v1.2.3-1", b: "v1.2.3-alpha", wantCmp: -1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := compareVersions(tt.a, tt.b)
			if err != nil {
				t.Fatalf("compareVersions(%q, %q) error = %v", tt.a, tt.b, err)
			}
			if got != tt.wantCmp {
				t.Fatalf("compareVersions(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.wantCmp)
			}
		})
	}
}

func TestCompareVersionsRejectsInvalid(t *testing.T) {
	if _, err := compareVersions("dev", "v1.2.3"); err == nil {
		t.Fatal("compareVersions should reject non-semver input")
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

func TestCacheRoundTripAndStaleness(t *testing.T) {
	path := filepath.Join(t.TempDir(), "update-check.json")
	now := time.Date(2026, 4, 9, 12, 0, 0, 0, time.UTC)
	entry := &checkCache{CheckedAt: now, LatestVersion: "v1.2.3"}

	if err := writeCache(path, entry); err != nil {
		t.Fatalf("writeCache error = %v", err)
	}

	loaded := readCache(path)
	if loaded == nil {
		t.Fatal("readCache returned nil")
	}
	if !loaded.CheckedAt.Equal(now) {
		t.Fatalf("CheckedAt = %v, want %v", loaded.CheckedAt, now)
	}
	if loaded.LatestVersion != "v1.2.3" {
		t.Fatalf("LatestVersion = %q", loaded.LatestVersion)
	}

	if cacheStale(loaded, "v1.2.2", now.Add(23*time.Hour)) {
		t.Fatal("cache should be fresh before ttl")
	}
	if !cacheStale(loaded, "v1.2.2", now.Add(25*time.Hour)) {
		t.Fatal("cache should be stale after ttl")
	}
	if !cacheStale(loaded, "v1.2.3", now.Add(time.Hour)) {
		t.Fatal("cache should be stale when current version catches up")
	}
	if !cacheStale(nil, "v1.2.2", now) {
		t.Fatal("nil cache should be stale")
	}

	badPath := filepath.Join(t.TempDir(), "corrupt.json")
	if err := os.WriteFile(badPath, []byte("{bad json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := readCache(badPath); got != nil {
		t.Fatal("corrupt cache should return nil")
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

func TestExtractBinaryFromTarGz(t *testing.T) {
	archive := makeTarGz(t, map[string][]byte{
		"nested/no-mistakes": []byte("binary-bytes"),
	})

	binary, err := extractBinaryFromTarGz(archive, "no-mistakes")
	if err != nil {
		t.Fatalf("extractBinaryFromTarGz error = %v", err)
	}
	if string(binary) != "binary-bytes" {
		t.Fatalf("binary = %q", string(binary))
	}

	_, err = extractBinaryFromTarGz(makeTarGz(t, map[string][]byte{"nested/other": []byte("x")}), "no-mistakes")
	if err == nil {
		t.Fatal("extractBinaryFromTarGz should fail when binary is missing")
	}
}

func TestExtractBinaryFromZip(t *testing.T) {
	archive := makeZip(t, map[string][]byte{
		"nested/no-mistakes.exe": []byte("binary-bytes"),
	})

	binary, err := extractBinaryFromZip(archive, "no-mistakes.exe")
	if err != nil {
		t.Fatalf("extractBinaryFromZip error = %v", err)
	}
	if string(binary) != "binary-bytes" {
		t.Fatalf("binary = %q", string(binary))
	}

	_, err = extractBinaryFromZip(makeZip(t, map[string][]byte{"nested/other.exe": []byte("x")}), "no-mistakes.exe")
	if err == nil {
		t.Fatal("extractBinaryFromZip should fail when binary is missing")
	}
}

func TestUpdaterCheckLatestAndRefreshCache(t *testing.T) {
	allowInsecureDownloads = true
	t.Cleanup(func() { allowInsecureDownloads = false })

	tests := []struct {
		name        string
		platform    platformSpec
		archiveName string
	}{
		{
			name:        "darwin tarball",
			platform:    platformSpec{GOOS: "darwin", GOARCH: "arm64"},
			archiveName: "no-mistakes-v1.2.3-darwin-arm64.tar.gz",
		},
		{
			name:        "windows zip",
			platform:    platformSpec{GOOS: "windows", GOARCH: "amd64"},
			archiveName: "no-mistakes-v1.2.3-windows-amd64.zip",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/repos/kunchenguid/no-mistakes/releases/latest" {
					t.Fatalf("unexpected path %q", r.URL.Path)
				}
				fmt.Fprintf(w, `{"tag_name":"v1.2.3","assets":[{"name":%q,"browser_download_url":"http://example.com/archive"},{"name":"checksums.txt","browser_download_url":"http://example.com/checksums"}]}`,
					tt.archiveName,
				)
			}))
			defer server.Close()

			cachePath := filepath.Join(t.TempDir(), "update-check.json")
			u := &updater{
				appName:        "no-mistakes",
				repo:           "kunchenguid/no-mistakes",
				currentVersion: "v1.2.2",
				platform:       tt.platform,
				apiBaseURL:     server.URL,
				httpClient:     server.Client(),
				cachePath:      cachePath,
				now:            func() time.Time { return time.Date(2026, 4, 9, 12, 0, 0, 0, time.UTC) },
			}

			plan, err := u.checkLatest(context.Background())
			if err != nil {
				t.Fatalf("checkLatest error = %v", err)
			}
			if !plan.UpdateAvailable {
				t.Fatal("expected update to be available")
			}
			if plan.LatestVersion != "v1.2.3" {
				t.Fatalf("LatestVersion = %q", plan.LatestVersion)
			}
			if plan.ArchiveName != tt.archiveName {
				t.Fatalf("ArchiveName = %q, want %q", plan.ArchiveName, tt.archiveName)
			}
			if plan.Archive.Name != tt.archiveName {
				t.Fatalf("Archive.Name = %q, want %q", plan.Archive.Name, tt.archiveName)
			}

			if err := u.refreshCache(context.Background()); err != nil {
				t.Fatalf("refreshCache error = %v", err)
			}
			cache := readCache(cachePath)
			if cache == nil || cache.LatestVersion != "v1.2.3" {
				t.Fatalf("cache = %#v", cache)
			}
		})
	}
}

func TestUpdaterRunReplacesExecutable(t *testing.T) {
	allowInsecureDownloads = true
	t.Cleanup(func() { allowInsecureDownloads = false })

	archiveName := "no-mistakes-v1.2.3-darwin-arm64.tar.gz"
	archive := makeTarGz(t, map[string][]byte{
		"bin/no-mistakes": []byte("new-binary"),
	})
	sum := sha256.Sum256(archive)
	checksums := fmt.Sprintf("%s  %s\n", hex.EncodeToString(sum[:]), archiveName)

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/kunchenguid/no-mistakes/releases/latest":
			fmt.Fprintf(w, `{"tag_name":"v1.2.3","assets":[{"name":%q,"browser_download_url":%q},{"name":"checksums.txt","browser_download_url":%q}]}`,
				archiveName,
				server.URL+"/archive",
				server.URL+"/checksums",
			)
		case "/archive":
			w.Write(archive)
		case "/checksums":
			fmt.Fprint(w, checksums)
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	defer server.Close()

	execPath := filepath.Join(t.TempDir(), "no-mistakes")
	if err := os.WriteFile(execPath, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	stdout := new(bytes.Buffer)
	u := &updater{
		appName:        "no-mistakes",
		repo:           "kunchenguid/no-mistakes",
		currentVersion: "v1.2.2",
		platform:       platformSpec{GOOS: "darwin", GOARCH: "arm64"},
		apiBaseURL:     server.URL,
		httpClient:     server.Client(),
		executablePath: execPath,
		stdout:         stdout,
		now:            func() time.Time { return time.Date(2026, 4, 9, 12, 0, 0, 0, time.UTC) },
	}

	if err := u.run(context.Background()); err != nil {
		t.Fatalf("run error = %v", err)
	}
	content, err := os.ReadFile(execPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "new-binary" {
		t.Fatalf("executable content = %q", string(content))
	}
	if !strings.Contains(stdout.String(), "updated no-mistakes") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestUpdaterRunResetsDaemonAfterUpdate(t *testing.T) {
	allowInsecureDownloads = true
	t.Cleanup(func() { allowInsecureDownloads = false })

	archiveName := "no-mistakes-v1.2.3-darwin-arm64.tar.gz"
	archive := makeTarGz(t, map[string][]byte{
		"bin/no-mistakes": []byte("new-binary"),
	})
	sum := sha256.Sum256(archive)
	checksums := fmt.Sprintf("%s  %s\n", hex.EncodeToString(sum[:]), archiveName)

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/kunchenguid/no-mistakes/releases/latest":
			fmt.Fprintf(w, `{"tag_name":"v1.2.3","assets":[{"name":%q,"browser_download_url":%q},{"name":"checksums.txt","browser_download_url":%q}]}`,
				archiveName,
				server.URL+"/archive",
				server.URL+"/checksums",
			)
		case "/archive":
			w.Write(archive)
		case "/checksums":
			fmt.Fprint(w, checksums)
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	defer server.Close()

	execPath := filepath.Join(t.TempDir(), "no-mistakes")
	if err := os.WriteFile(execPath, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	resetCalled := false
	u := &updater{
		appName:        "no-mistakes",
		repo:           "kunchenguid/no-mistakes",
		currentVersion: "v1.2.2",
		platform:       platformSpec{GOOS: "darwin", GOARCH: "arm64"},
		apiBaseURL:     server.URL,
		httpClient:     server.Client(),
		executablePath: execPath,
		now:            func() time.Time { return time.Date(2026, 4, 9, 12, 0, 0, 0, time.UTC) },
		resetDaemon: func() error {
			resetCalled = true
			return nil
		},
	}

	if err := u.run(context.Background()); err != nil {
		t.Fatalf("run error = %v", err)
	}
	if !resetCalled {
		t.Fatal("expected daemon reset after successful update")
	}
}

func TestUpdaterRunFailsWhenDaemonResetFails(t *testing.T) {
	allowInsecureDownloads = true
	t.Cleanup(func() { allowInsecureDownloads = false })

	archiveName := "no-mistakes-v1.2.3-darwin-arm64.tar.gz"
	archive := makeTarGz(t, map[string][]byte{
		"bin/no-mistakes": []byte("new-binary"),
	})
	sum := sha256.Sum256(archive)
	checksums := fmt.Sprintf("%s  %s\n", hex.EncodeToString(sum[:]), archiveName)

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/kunchenguid/no-mistakes/releases/latest":
			fmt.Fprintf(w, `{"tag_name":"v1.2.3","assets":[{"name":%q,"browser_download_url":%q},{"name":"checksums.txt","browser_download_url":%q}]}`,
				archiveName,
				server.URL+"/archive",
				server.URL+"/checksums",
			)
		case "/archive":
			w.Write(archive)
		case "/checksums":
			fmt.Fprint(w, checksums)
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	defer server.Close()

	execPath := filepath.Join(t.TempDir(), "no-mistakes")
	if err := os.WriteFile(execPath, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	stdout := new(bytes.Buffer)
	stderr := new(bytes.Buffer)
	u := &updater{
		appName:        "no-mistakes",
		repo:           "kunchenguid/no-mistakes",
		currentVersion: "v1.2.2",
		platform:       platformSpec{GOOS: "darwin", GOARCH: "arm64"},
		apiBaseURL:     server.URL,
		httpClient:     server.Client(),
		executablePath: execPath,
		stdout:         stdout,
		stderr:         stderr,
		now:            func() time.Time { return time.Date(2026, 4, 9, 12, 0, 0, 0, time.UTC) },
		resetDaemon: func() error {
			return fmt.Errorf("boom")
		},
	}

	err := u.run(context.Background())
	if err == nil {
		t.Fatal("run should fail when daemon reset fails")
	}
	if !strings.Contains(err.Error(), "failed to reset daemon") {
		t.Fatalf("run error = %v", err)
	}
	content, err := os.ReadFile(execPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "new-binary" {
		t.Fatalf("executable content = %q", string(content))
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestUpdaterRunFailsWhenDaemonResetLeavesDaemonOffline(t *testing.T) {
	allowInsecureDownloads = true
	t.Cleanup(func() { allowInsecureDownloads = false })

	archiveName := "no-mistakes-v1.2.3-darwin-arm64.tar.gz"
	archive := makeTarGz(t, map[string][]byte{
		"bin/no-mistakes": []byte("new-binary"),
	})
	sum := sha256.Sum256(archive)
	checksums := fmt.Sprintf("%s  %s\n", hex.EncodeToString(sum[:]), archiveName)

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/kunchenguid/no-mistakes/releases/latest":
			fmt.Fprintf(w, `{"tag_name":"v1.2.3","assets":[{"name":%q,"browser_download_url":%q},{"name":"checksums.txt","browser_download_url":%q}]}`,
				archiveName,
				server.URL+"/archive",
				server.URL+"/checksums",
			)
		case "/archive":
			w.Write(archive)
		case "/checksums":
			fmt.Fprint(w, checksums)
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	defer server.Close()

	execPath := filepath.Join(t.TempDir(), "no-mistakes")
	if err := os.WriteFile(execPath, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	stdout := new(bytes.Buffer)
	u := &updater{
		appName:        "no-mistakes",
		repo:           "kunchenguid/no-mistakes",
		currentVersion: "v1.2.2",
		platform:       platformSpec{GOOS: "darwin", GOARCH: "arm64"},
		apiBaseURL:     server.URL,
		httpClient:     server.Client(),
		executablePath: execPath,
		stdout:         stdout,
		now:            func() time.Time { return time.Date(2026, 4, 9, 12, 0, 0, 0, time.UTC) },
		resetDaemon: func() error {
			return &daemonResetError{err: errors.New("start daemon: boom"), daemonOffline: true}
		},
	}

	err := u.run(context.Background())
	if err == nil {
		t.Fatal("run should fail when daemon reset leaves daemon offline")
	}
	if !strings.Contains(err.Error(), "daemon is offline") {
		t.Fatalf("run error = %v", err)
	}
	content, readErr := os.ReadFile(execPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(content) != "new-binary" {
		t.Fatalf("executable content = %q", string(content))
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func TestUpdaterRunFailsWhenDaemonUsesDifferentExecutable(t *testing.T) {
	allowInsecureDownloads = true
	t.Cleanup(func() { allowInsecureDownloads = false })

	archiveName := "no-mistakes-v1.2.3-darwin-arm64.tar.gz"
	archive := makeTarGz(t, map[string][]byte{
		"bin/no-mistakes": []byte("new-binary"),
	})
	sum := sha256.Sum256(archive)
	checksums := fmt.Sprintf("%s  %s\n", hex.EncodeToString(sum[:]), archiveName)

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/kunchenguid/no-mistakes/releases/latest":
			fmt.Fprintf(w, `{"tag_name":"v1.2.3","assets":[{"name":%q,"browser_download_url":%q},{"name":"checksums.txt","browser_download_url":%q}]}`,
				archiveName,
				server.URL+"/archive",
				server.URL+"/checksums",
			)
		case "/archive":
			w.Write(archive)
		case "/checksums":
			fmt.Fprint(w, checksums)
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	defer server.Close()

	execDir := t.TempDir()
	execPath := filepath.Join(execDir, "no-mistakes")
	if err := os.WriteFile(execPath, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	otherExecPath := filepath.Join(execDir, "other-no-mistakes")
	if err := os.WriteFile(otherExecPath, []byte("other-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	origDaemonIsRunning := daemonIsRunning
	origDaemonExecutablePath := daemonExecutablePath
	t.Cleanup(func() {
		daemonIsRunning = origDaemonIsRunning
		daemonExecutablePath = origDaemonExecutablePath
	})

	checks := 0
	daemonIsRunning = func(*paths.Paths) (bool, error) {
		checks++
		return true, nil
	}
	daemonExecutablePath = func(*paths.Paths) (string, error) {
		return otherExecPath, nil
	}

	resetCalled := false
	u := &updater{
		appName:        "no-mistakes",
		repo:           "kunchenguid/no-mistakes",
		currentVersion: "v1.2.2",
		platform:       platformSpec{GOOS: "darwin", GOARCH: "arm64"},
		apiBaseURL:     server.URL,
		httpClient:     server.Client(),
		executablePath: execPath,
		now:            func() time.Time { return time.Date(2026, 4, 9, 12, 0, 0, 0, time.UTC) },
		resetDaemon: func() error {
			resetCalled = true
			return nil
		},
		paths: paths.WithRoot(t.TempDir()),
	}

	err := u.run(context.Background())
	if err == nil {
		t.Fatal("run should fail when daemon uses a different executable")
	}
	if !strings.Contains(err.Error(), "daemon is running from") {
		t.Fatalf("run error = %v", err)
	}
	if checks == 0 {
		t.Fatal("expected daemon health check before update")
	}
	if resetCalled {
		t.Fatal("reset daemon should not run when executables mismatch")
	}
	content, readErr := os.ReadFile(execPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(content) != "old-binary" {
		t.Fatalf("executable content = %q", string(content))
	}
}

func TestUpdaterRunFailsWhenDaemonExecutableCannotBeResolved(t *testing.T) {
	allowInsecureDownloads = true
	t.Cleanup(func() { allowInsecureDownloads = false })

	archiveName := "no-mistakes-v1.2.3-darwin-arm64.tar.gz"
	archive := makeTarGz(t, map[string][]byte{
		"bin/no-mistakes": []byte("new-binary"),
	})
	sum := sha256.Sum256(archive)
	checksums := fmt.Sprintf("%s  %s\n", hex.EncodeToString(sum[:]), archiveName)

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/kunchenguid/no-mistakes/releases/latest":
			fmt.Fprintf(w, `{"tag_name":"v1.2.3","assets":[{"name":%q,"browser_download_url":%q},{"name":"checksums.txt","browser_download_url":%q}]}`,
				archiveName,
				server.URL+"/archive",
				server.URL+"/checksums",
			)
		case "/archive":
			w.Write(archive)
		case "/checksums":
			fmt.Fprint(w, checksums)
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	defer server.Close()

	execDir := t.TempDir()
	execPath := filepath.Join(execDir, "no-mistakes")
	if err := os.WriteFile(execPath, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	origDaemonIsRunning := daemonIsRunning
	origDaemonExecutablePath := daemonExecutablePath
	t.Cleanup(func() {
		daemonIsRunning = origDaemonIsRunning
		daemonExecutablePath = origDaemonExecutablePath
	})

	checks := 0
	daemonIsRunning = func(*paths.Paths) (bool, error) {
		checks++
		return true, nil
	}
	daemonExecutablePath = func(*paths.Paths) (string, error) {
		return "", errors.New("pid lookup failed")
	}

	resetCalled := false
	u := &updater{
		appName:        "no-mistakes",
		repo:           "kunchenguid/no-mistakes",
		currentVersion: "v1.2.2",
		platform:       platformSpec{GOOS: "darwin", GOARCH: "arm64"},
		apiBaseURL:     server.URL,
		httpClient:     server.Client(),
		executablePath: execPath,
		now:            func() time.Time { return time.Date(2026, 4, 9, 12, 0, 0, 0, time.UTC) },
		resetDaemon: func() error {
			resetCalled = true
			return nil
		},
		paths: paths.WithRoot(t.TempDir()),
	}

	err := u.run(context.Background())
	if err == nil {
		t.Fatal("run should fail when daemon executable cannot be resolved")
	}
	if !strings.Contains(err.Error(), "cannot determine daemon executable path") {
		t.Fatalf("run error = %v", err)
	}
	if checks == 0 {
		t.Fatal("expected daemon health check before update")
	}
	if resetCalled {
		t.Fatal("reset daemon should not run when daemon executable cannot be resolved")
	}
}

func TestDefaultResetDaemonReportsOfflineWhenRestartFails(t *testing.T) {
	origIsRunning := daemonIsRunning
	origStop := daemonStop
	origStart := daemonStart
	t.Cleanup(func() {
		daemonIsRunning = origIsRunning
		daemonStop = origStop
		daemonStart = origStart
	})

	checks := 0
	daemonIsRunning = func(*paths.Paths) (bool, error) {
		checks++
		if checks == 1 {
			return true, nil
		}
		return false, nil
	}
	daemonStop = func(*paths.Paths) error { return nil }
	daemonStart = func(*paths.Paths) error { return errors.New("boom") }

	err := defaultResetDaemon(&paths.Paths{})
	if err == nil {
		t.Fatal("defaultResetDaemon should fail when restart fails")
	}
	var resetErr *daemonResetError
	if !errors.As(err, &resetErr) {
		t.Fatalf("expected daemonResetError, got %T", err)
	}
	if !resetErr.daemonOffline {
		t.Fatal("expected daemon to be marked offline")
	}
	if checks < 2 {
		t.Fatalf("expected follow-up daemon check, got %d checks", checks)
	}
	if !strings.Contains(err.Error(), "start daemon") {
		t.Fatalf("error = %v", err)
	}
}

func TestRunningDaemonExecutablePathUsesPIDFile(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := os.WriteFile(p.PIDFile(), []byte(fmt.Sprintf("%d", os.Getpid())), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := runningDaemonExecutablePath(p)
	if err != nil {
		t.Fatalf("runningDaemonExecutablePath error = %v", err)
	}
	want, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if got != resolveExecutablePath(want) {
		t.Fatalf("runningDaemonExecutablePath = %q, want %q", got, resolveExecutablePath(want))
	}
}

func TestRunningDaemonExecutablePathHandlesExecutablePathsWithSpaces(t *testing.T) {
	if os.Getenv("NO_MISTAKES_TEST_CHILD") == "1" {
		time.Sleep(10 * time.Second)
		return
	}

	originalPath, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	originalInfo, err := os.Stat(originalPath)
	if err != nil {
		t.Fatal(err)
	}
	binary, err := os.ReadFile(originalPath)
	if err != nil {
		t.Fatal(err)
	}

	dir := filepath.Join(t.TempDir(), "dir with spaces")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	copyPath := filepath.Join(dir, "no mistakes test binary")
	if err := os.WriteFile(copyPath, binary, originalInfo.Mode().Perm()); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(copyPath, "-test.run=^TestRunningDaemonExecutablePathHandlesExecutablePathsWithSpaces$")
	cmd.Env = append(os.Environ(), "NO_MISTAKES_TEST_CHILD=1")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	p := paths.WithRoot(t.TempDir())
	if err := os.WriteFile(p.PIDFile(), []byte(fmt.Sprintf("%d", cmd.Process.Pid)), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := runningDaemonExecutablePath(p)
	if err != nil {
		t.Fatalf("runningDaemonExecutablePath error = %v", err)
	}
	if got != resolveExecutablePath(copyPath) {
		t.Fatalf("runningDaemonExecutablePath = %q, want %q", got, resolveExecutablePath(copyPath))
	}
}

func TestDefaultResetDaemonRecoversWhenHealthCheckErrors(t *testing.T) {
	origIsRunning := daemonIsRunning
	origStop := daemonStop
	origStart := daemonStart
	t.Cleanup(func() {
		daemonIsRunning = origIsRunning
		daemonStop = origStop
		daemonStart = origStart
	})

	stopCalled := false
	startCalled := false
	daemonIsRunning = func(*paths.Paths) (bool, error) { return false, errors.New("health check failed") }
	daemonStop = func(*paths.Paths) error {
		stopCalled = true
		return nil
	}
	daemonStart = func(*paths.Paths) error {
		startCalled = true
		return nil
	}

	if err := defaultResetDaemon(&paths.Paths{}); err != nil {
		t.Fatalf("defaultResetDaemon error = %v", err)
	}
	if !stopCalled {
		t.Fatal("expected stop to be attempted after health-check error")
	}
	if !startCalled {
		t.Fatal("expected start to be attempted after health-check error")
	}
}

func TestDefaultResetDaemonNoopWhenDaemonOfflineAndNoArtifacts(t *testing.T) {
	origIsRunning := daemonIsRunning
	origStop := daemonStop
	origStart := daemonStart
	t.Cleanup(func() {
		daemonIsRunning = origIsRunning
		daemonStop = origStop
		daemonStart = origStart
	})

	p := paths.WithRoot(t.TempDir())
	stopCalled := false
	startCalled := false
	daemonIsRunning = func(*paths.Paths) (bool, error) { return false, nil }
	daemonStop = func(*paths.Paths) error {
		stopCalled = true
		return nil
	}
	daemonStart = func(*paths.Paths) error {
		startCalled = true
		return nil
	}

	if err := defaultResetDaemon(p); err != nil {
		t.Fatalf("defaultResetDaemon error = %v", err)
	}
	if stopCalled {
		t.Fatal("expected stop to be skipped when daemon is offline without artifacts")
	}
	if startCalled {
		t.Fatal("expected start to be skipped when daemon is offline without artifacts")
	}
}

func TestDefaultResetDaemonRecoversWhenDaemonArtifactsRemain(t *testing.T) {
	origIsRunning := daemonIsRunning
	origStop := daemonStop
	origStart := daemonStart
	t.Cleanup(func() {
		daemonIsRunning = origIsRunning
		daemonStop = origStop
		daemonStart = origStart
	})

	p := paths.WithRoot(t.TempDir())
	if err := os.WriteFile(p.Socket(), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}

	stopCalled := false
	startCalled := false
	daemonIsRunning = func(*paths.Paths) (bool, error) { return false, nil }
	daemonStop = func(*paths.Paths) error {
		stopCalled = true
		return nil
	}
	daemonStart = func(*paths.Paths) error {
		startCalled = true
		return nil
	}

	if err := defaultResetDaemon(p); err != nil {
		t.Fatalf("defaultResetDaemon error = %v", err)
	}
	if !stopCalled {
		t.Fatal("expected stop to be attempted when daemon artifacts remain")
	}
	if !startCalled {
		t.Fatal("expected start to be attempted when daemon artifacts remain")
	}
}

func TestDefaultResetDaemonDoesNotReportOfflineWhenRestartLeavesDaemonRunning(t *testing.T) {
	origIsRunning := daemonIsRunning
	origStop := daemonStop
	origStart := daemonStart
	t.Cleanup(func() {
		daemonIsRunning = origIsRunning
		daemonStop = origStop
		daemonStart = origStart
	})

	checks := 0
	daemonIsRunning = func(*paths.Paths) (bool, error) {
		checks++
		if checks == 1 {
			return true, nil
		}
		return true, nil
	}
	daemonStop = func(*paths.Paths) error { return nil }
	daemonStart = func(*paths.Paths) error { return errors.New("daemon already running") }

	err := defaultResetDaemon(&paths.Paths{})
	if err == nil {
		t.Fatal("defaultResetDaemon should fail when restart fails")
	}
	var resetErr *daemonResetError
	if !errors.As(err, &resetErr) {
		t.Fatalf("expected daemonResetError, got %T", err)
	}
	if resetErr.daemonOffline {
		t.Fatal("expected daemon to stay online")
	}
	if checks < 2 {
		t.Fatalf("expected follow-up daemon check, got %d checks", checks)
	}
}

func TestReplaceExecutableDarwinRequiresAtomicReplace(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("darwin-specific behavior")
	}

	dir := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	execPath := filepath.Join(dir, "no-mistakes")
	if err := os.WriteFile(execPath, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chmod(dir, 0o755)
	})

	err := replaceExecutable(execPath, []byte("new-binary"))
	if err == nil {
		t.Fatal("replaceExecutable should fail when atomic replacement is unavailable on darwin")
	}
	if !strings.Contains(err.Error(), "reinstall") {
		t.Fatalf("replaceExecutable error = %v", err)
	}
	content, readErr := os.ReadFile(execPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(content) != "old-binary" {
		t.Fatalf("executable content = %q", string(content))
	}
}

func TestUpdaterMaybeNotifyAndCheck(t *testing.T) {
	cachePath := filepath.Join(t.TempDir(), "update-check.json")
	if err := writeCache(cachePath, &checkCache{
		CheckedAt:     time.Date(2026, 4, 8, 12, 0, 0, 0, time.UTC),
		LatestVersion: "v1.2.3",
	}); err != nil {
		t.Fatal(err)
	}

	stderr := new(bytes.Buffer)
	spawned := false
	u := &updater{
		appName:        "no-mistakes",
		currentVersion: "v1.2.2",
		cachePath:      cachePath,
		stderr:         stderr,
		now:            func() time.Time { return time.Date(2026, 4, 9, 13, 0, 0, 0, time.UTC) },
		spawnBackground: func(currentVersion string) error {
			spawned = true
			if currentVersion != "v1.2.2" {
				t.Fatalf("currentVersion = %q", currentVersion)
			}
			return nil
		},
	}

	u.maybeNotifyAndCheck([]string{"status"})

	if !strings.Contains(stderr.String(), "A new version of no-mistakes is available: v1.2.2 -> v1.2.3") {
		t.Fatalf("stderr = %q", stderr.String())
	}
	if !spawned {
		t.Fatal("expected stale cache to trigger background refresh")
	}

	stderr.Reset()
	spawned = false
	u.maybeNotifyAndCheck([]string{"update"})
	if stderr.Len() != 0 {
		t.Fatalf("update command should not notify, got %q", stderr.String())
	}
	if spawned {
		t.Fatal("update command should not spawn background refresh")
	}
}

func TestUpdaterCachedLatestVersion(t *testing.T) {
	cachePath := filepath.Join(t.TempDir(), "update-check.json")
	if err := writeCache(cachePath, &checkCache{
		CheckedAt:     time.Date(2026, 4, 8, 12, 0, 0, 0, time.UTC),
		LatestVersion: "v1.2.3",
	}); err != nil {
		t.Fatal(err)
	}

	u := &updater{
		currentVersion: "v1.2.2",
		cachePath:      cachePath,
	}

	if got := u.cachedLatestVersion(); got != "v1.2.3" {
		t.Fatalf("cachedLatestVersion() = %q, want %q", got, "v1.2.3")
	}

	u.currentVersion = "v1.2.3"
	if got := u.cachedLatestVersion(); got != "" {
		t.Fatalf("cachedLatestVersion() = %q, want empty when already current", got)
	}
}

func stringsRepeat(s string, count int) string {
	buf := bytes.NewBuffer(nil)
	for i := 0; i < count; i++ {
		buf.WriteString(s)
	}
	return buf.String()
}

func makeTarGz(t *testing.T, files map[string][]byte) []byte {
	t.Helper()

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, data := range files {
		hdr := &tar.Header{Name: name, Mode: 0o755, Size: int64(len(data))}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func makeZip(t *testing.T, files map[string][]byte) []byte {
	t.Helper()

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, data := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

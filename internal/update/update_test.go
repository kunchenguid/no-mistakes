package update

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/paths"
)

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

func TestUpdaterRunSkipsDaemonExecutableCheckWhenAlreadyUpToDate(t *testing.T) {
	allowInsecureDownloads = true
	t.Cleanup(func() { allowInsecureDownloads = false })

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/kunchenguid/no-mistakes/releases/latest":
			fmt.Fprint(w, `{"tag_name":"v1.2.2","assets":[]}`)
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	defer server.Close()

	execPath := filepath.Join(t.TempDir(), "no-mistakes")
	if err := os.WriteFile(execPath, []byte("current-binary"), 0o755); err != nil {
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
		paths:          paths.WithRoot(t.TempDir()),
	}

	if err := u.run(context.Background()); err != nil {
		t.Fatalf("run error = %v", err)
	}
	if checks != 0 {
		t.Fatalf("expected no daemon executable check when already up to date, got %d checks", checks)
	}
	if !strings.Contains(stdout.String(), "already up to date") {
		t.Fatalf("stdout = %q", stdout.String())
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

func TestUpdaterCheckLatestBetaUsesReleasesList(t *testing.T) {
	allowInsecureDownloads = true
	t.Cleanup(func() { allowInsecureDownloads = false })

	archiveName := "no-mistakes-v1.3.0-beta.1-darwin-arm64.tar.gz"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/kunchenguid/no-mistakes/releases" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		fmt.Fprintf(w, `[
			{"tag_name":"v1.3.0-beta.1","draft":false,"prerelease":true,"assets":[{"name":%q,"browser_download_url":"http://example.com/archive"},{"name":"checksums.txt","browser_download_url":"http://example.com/checksums"}]},
			{"tag_name":"v1.2.3","draft":false,"prerelease":false,"assets":[]},
			{"tag_name":"v1.4.0-draft","draft":true,"prerelease":true,"assets":[]}
		]`, archiveName)
	}))
	defer server.Close()

	u := &updater{
		appName:            "no-mistakes",
		repo:               "kunchenguid/no-mistakes",
		currentVersion:     "v1.2.3",
		platform:           platformSpec{GOOS: "darwin", GOARCH: "arm64"},
		apiBaseURL:         server.URL,
		httpClient:         server.Client(),
		cachePath:          filepath.Join(t.TempDir(), "update-check.json"),
		now:                func() time.Time { return time.Date(2026, 4, 22, 12, 0, 0, 0, time.UTC) },
		includePrereleases: true,
	}

	plan, err := u.checkLatest(context.Background())
	if err != nil {
		t.Fatalf("checkLatest error = %v", err)
	}
	if !plan.UpdateAvailable {
		t.Fatal("expected update to be available")
	}
	if plan.LatestVersion != "v1.3.0-beta.1" {
		t.Fatalf("LatestVersion = %q", plan.LatestVersion)
	}
	if plan.ArchiveName != archiveName {
		t.Fatalf("ArchiveName = %q", plan.ArchiveName)
	}
}

func TestUpdaterCheckLatestBetaPicksHighestSemver(t *testing.T) {
	allowInsecureDownloads = true
	t.Cleanup(func() { allowInsecureDownloads = false })

	archiveName := "no-mistakes-v1.3.0-beta.2-darwin-arm64.tar.gz"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/kunchenguid/no-mistakes/releases" {
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
		fmt.Fprintf(w, `[
			{"tag_name":"v1.3.0-beta.1","draft":false,"prerelease":true,"assets":[]},
			{"tag_name":"v1.3.0-beta.2","draft":false,"prerelease":true,"assets":[{"name":%q,"browser_download_url":"http://example.com/archive"},{"name":"checksums.txt","browser_download_url":"http://example.com/checksums"}]},
			{"tag_name":"v1.2.3","draft":false,"prerelease":false,"assets":[]}
		]`, archiveName)
	}))
	defer server.Close()

	u := &updater{
		appName:            "no-mistakes",
		repo:               "kunchenguid/no-mistakes",
		currentVersion:     "v1.2.3",
		platform:           platformSpec{GOOS: "darwin", GOARCH: "arm64"},
		apiBaseURL:         server.URL,
		httpClient:         server.Client(),
		cachePath:          filepath.Join(t.TempDir(), "update-check.json"),
		now:                func() time.Time { return time.Date(2026, 4, 22, 12, 0, 0, 0, time.UTC) },
		includePrereleases: true,
	}

	plan, err := u.checkLatest(context.Background())
	if err != nil {
		t.Fatalf("checkLatest error = %v", err)
	}
	if plan.LatestVersion != "v1.3.0-beta.2" {
		t.Fatalf("LatestVersion = %q", plan.LatestVersion)
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

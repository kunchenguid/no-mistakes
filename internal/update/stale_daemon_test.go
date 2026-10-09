package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// A daemon that exited but left its socket and PID file behind is cleared by
// update's first daemon probe; update must still start the new daemon.
func TestUpdaterRunStartsDaemonAfterProbeClearsStaleArtifacts(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix socket setup is platform-specific")
	}
	allowInsecureDownloads = true
	t.Cleanup(func() { allowInsecureDownloads = false })

	archiveName := "no-mistakes-v1.2.3-darwin-arm64.tar.gz"
	archive := makeTarGz(t, map[string][]byte{"bin/no-mistakes": []byte("new-binary")})
	sum := sha256.Sum256(archive)
	checksums := fmt.Sprintf("%s  %s\n", hex.EncodeToString(sum[:]), archiveName)
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/channels.json":
			fmt.Fprintf(w, `{"schema_version":1,"stable":{"tag_name":"v1.2.3","assets":[{"name":%q,"browser_download_url":%q},{"name":"checksums.txt","browser_download_url":%q}]}}`,
				archiveName, server.URL+"/archive", server.URL+"/checksums")
		case "/archive":
			w.Write(archive)
		case "/checksums":
			fmt.Fprint(w, checksums)
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	defer server.Close()

	// A refused socket plus a PID file whose process start time no longer
	// matches: the recorded daemon is gone.
	root, err := os.MkdirTemp("", "utest")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	p := paths.WithRoot(root)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: p.Socket(), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	ln.SetUnlinkOnClose(false)
	ln.Close()
	if err := os.WriteFile(p.PIDFile(), []byte(fmt.Sprintf(`{"pid":%d,"started_at":"2000-01-01T00:00:00Z"}`, os.Getpid())), 0o644); err != nil {
		t.Fatal(err)
	}

	origStop, origStart := daemonStop, daemonStart
	t.Cleanup(func() { daemonStop, daemonStart = origStop, origStart })
	daemonStop = func(*paths.Paths) error { return nil }
	startCalled := false
	daemonStart = func(*paths.Paths) error {
		startCalled = true
		return nil
	}

	execPath := filepath.Join(t.TempDir(), "no-mistakes")
	if err := os.WriteFile(execPath, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	u := &updater{
		appName:        "no-mistakes",
		currentVersion: "v1.2.2",
		platform:       platformSpec{GOOS: "darwin", GOARCH: "arm64"},
		manifestURL:    server.URL + "/channels.json",
		httpClient:     server.Client(),
		executablePath: execPath,
		now:            func() time.Time { return time.Date(2026, 4, 9, 12, 0, 0, 0, time.UTC) },
		paths:          p,
		resetDaemon: func(daemonExpected bool) error {
			return defaultResetDaemon(p, daemonExpected)
		},
	}

	if err := u.run(context.Background()); err != nil {
		t.Fatalf("run error = %v", err)
	}
	if daemonArtifactsExist(p) {
		t.Fatal("expected the stale socket and PID file to be cleared")
	}
	if !startCalled {
		t.Fatal("expected update to start the new daemon after clearing a dead daemon's artifacts")
	}
}

package update

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Issue #1691: with runs active, update installs the new version and lets the
// daemon restart once they finish, instead of refusing or failing them.

func TestUpdaterRunWithActiveRunsInstallsAndSchedulesTheRestart(t *testing.T) {
	u, execPath, stdout, stderr := newActiveRunUpdater(t)
	resetCalled := false
	u.resetDaemon = func(bool) error { resetCalled = true; return nil }
	u.restartStatus = func() (*ipc.RestartStatusResult, error) { return &ipc.RestartStatusResult{}, nil }
	scheduled := false
	u.scheduleRestart = func() (*ipc.RestartStatusResult, error) {
		// The restart is scheduled only once the new version is in place:
		// the daemon restarts through this file.
		if content, _ := os.ReadFile(execPath); string(content) != "new-binary" {
			t.Fatalf("restart scheduled before the new binary was installed; executable = %q", content)
		}
		scheduled = true
		return &ipc.RestartStatusResult{Pending: true, WaitingRuns: 1}, nil
	}

	if err := u.run(context.Background()); err != nil {
		t.Fatalf("update with active runs should install and schedule, got %v\nstderr: %s", err, stderr)
	}
	if !scheduled {
		t.Fatal("update should schedule the daemon restart")
	}
	if resetCalled {
		t.Fatal("update must not restart the daemon immediately while a run is active")
	}
	if !strings.Contains(stdout.String(), "updated no-mistakes from v1.2.2 to v1.2.3; the daemon restarts after 1 active run finishes") {
		t.Fatalf("stdout should say what the restart waits for, got %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "will finish on the current version") || !strings.Contains(stderr.String(), "feature-a") {
		t.Fatalf("stderr should list the runs that finish first, got %q", stderr.String())
	}
}

func TestUpdaterRunWithActiveRunsRefusesWhenTheDaemonIsTooOldToWait(t *testing.T) {
	u, execPath, _, _ := newActiveRunUpdater(t)
	u.resetDaemon = func(bool) error { t.Fatal("reset must not run"); return nil }
	u.restartStatus = func() (*ipc.RestartStatusResult, error) { return nil, daemon.ErrRestartWhenIdleUnsupported }
	u.scheduleRestart = func() (*ipc.RestartStatusResult, error) { t.Fatal("schedule must not run"); return nil, nil }

	err := u.run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "refusing update") || !strings.Contains(err.Error(), "too old to wait") {
		t.Fatalf("update should refuse and name the old daemon, got %v", err)
	}
	if content, _ := os.ReadFile(execPath); string(content) != "old-binary" {
		t.Fatalf("a refused update must leave the executable alone, got %q", content)
	}
}

func TestUpdaterRunWithActiveRunsReportsAFailedSchedule(t *testing.T) {
	u, execPath, _, _ := newActiveRunUpdater(t)
	u.resetDaemon = func(bool) error { t.Fatal("reset must not run"); return nil }
	u.restartStatus = func() (*ipc.RestartStatusResult, error) { return &ipc.RestartStatusResult{}, nil }
	u.scheduleRestart = func() (*ipc.RestartStatusResult, error) { return nil, fmt.Errorf("connect to daemon: refused") }

	err := u.run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "updated no-mistakes to v1.2.3, but could not schedule the daemon restart") || !strings.Contains(err.Error(), "daemon restart --when-idle") {
		t.Fatalf("update should report the installed version and the way to finish, got %v", err)
	}
	if content, _ := os.ReadFile(execPath); string(content) != "new-binary" {
		t.Fatalf("executable = %q, want the installed new binary", content)
	}
}

// newActiveRunUpdater returns an updater for a v1.2.2 -> v1.2.3 update served
// by a local release server, with one run executing in its database.
func newActiveRunUpdater(t *testing.T) (*updater, string, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
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
			_, _ = w.Write(archive)
		case "/checksums":
			fmt.Fprint(w, checksums)
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)

	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	repo, err := database.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "feature-a", "aaa111", "000")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	execPath := filepath.Join(t.TempDir(), "no-mistakes")
	if err := os.WriteFile(execPath, []byte("old-binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	stdout, stderr := new(bytes.Buffer), new(bytes.Buffer)
	return &updater{
		appName:        "no-mistakes",
		currentVersion: "v1.2.2",
		platform:       platformSpec{GOOS: "darwin", GOARCH: "arm64"},
		manifestURL:    server.URL + "/channels.json",
		httpClient:     server.Client(),
		executablePath: execPath,
		stdout:         stdout,
		stderr:         stderr,
		now:            func() time.Time { return time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC) },
		paths:          p,
	}, execPath, stdout, stderr
}

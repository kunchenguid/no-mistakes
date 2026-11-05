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

	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/lifecycle"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Issue #1691: with --when-idle and runs active, update installs the new
// version and lets the daemon restart once they finish, instead of refusing or
// failing them. The maintainer's triage asked for it opt-in: a plain update
// still refuses.

func TestUpdaterRunWithActiveRunsInstallsAndSchedulesTheRestart(t *testing.T) {
	u, execPath, stdout, stderr := newActiveRunUpdater(t)
	u.whenIdle = true
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
	u.whenIdle = true
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
	u.whenIdle = true
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

func TestUpdaterRunWithActiveRunsStillRefusesWithoutWhenIdle(t *testing.T) {
	u, execPath, _, _ := newActiveRunUpdater(t)
	u.resetDaemon = func(bool) error { t.Fatal("reset must not run"); return nil }
	u.restartStatus = func() (*ipc.RestartStatusResult, error) { return &ipc.RestartStatusResult{}, nil }
	u.scheduleRestart = func() (*ipc.RestartStatusResult, error) { t.Fatal("schedule must not run"); return nil, nil }

	err := u.run(context.Background())
	if err == nil || err.Error() != "refusing update because 1 active pipeline runs are in progress; pass --force to stop/restart the daemon anyway" {
		t.Fatalf("a plain update should refuse exactly as before, got %v", err)
	}
	if content, _ := os.ReadFile(execPath); string(content) != "old-binary" {
		t.Fatalf("a refused update must leave the executable alone, got %q", content)
	}
}

// Greptile on #1697: a run can start while the archive downloads, so with
// --when-idle an idle daemon is restarted through the scheduled restart too,
// never by an immediate reset that would stop that run.
func TestUpdaterRunWhenIdleWithNoActiveRunStillSchedulesTheRestart(t *testing.T) {
	u, _, stdout, _ := newActiveRunUpdater(t)
	finishActiveRuns(t, u.paths)
	u.whenIdle = true
	u.resetDaemon = func(bool) error {
		t.Fatal("an immediate reset could stop a run started during the download")
		return nil
	}
	u.restartStatus = func() (*ipc.RestartStatusResult, error) { return &ipc.RestartStatusResult{}, nil }
	scheduled := false
	u.scheduleRestart = func() (*ipc.RestartStatusResult, error) {
		scheduled = true
		return &ipc.RestartStatusResult{Pending: true, WaitingRuns: 1}, nil
	}

	if err := u.run(context.Background()); err != nil {
		t.Fatalf("update --when-idle should install and schedule, got %v", err)
	}
	if !scheduled {
		t.Fatal("update --when-idle should schedule the restart even when no run was active at the start")
	}
	if !strings.Contains(stdout.String(), "the daemon restarts after 1 active run finishes") {
		t.Fatalf("stdout should report the run the restart now waits for, got %q", stdout.String())
	}
}

// A failed probe leaves a daemon that may be running and may start a run
// during the download, so an immediate reset is not a safe fallback.
func TestUpdaterRunWhenIdleWithNoActiveRunRefusesAFailedProbe(t *testing.T) {
	u, execPath, _, _ := newActiveRunUpdater(t)
	finishActiveRuns(t, u.paths)
	u.whenIdle = true
	u.resetDaemon = func(bool) error { t.Fatal("reset must not run"); return nil }
	u.restartStatus = func() (*ipc.RestartStatusResult, error) { return nil, fmt.Errorf("read: unexpected EOF") }
	u.scheduleRestart = func() (*ipc.RestartStatusResult, error) { t.Fatal("schedule must not run"); return nil, nil }

	err := u.run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "unexpected EOF") || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("update should refuse, name the error and offer --force, got %v", err)
	}
	if content, _ := os.ReadFile(execPath); string(content) != "old-binary" {
		t.Fatalf("a refused update must leave the executable alone, got %q", content)
	}
}

// With no daemon listening there is nothing to protect: the plain path applies.
func TestUpdaterRunWhenIdleWithNoActiveRunAndNoDaemonResetsImmediately(t *testing.T) {
	stubDaemonAlive(t, false)
	u, _, _, _ := newActiveRunUpdater(t)
	finishActiveRuns(t, u.paths)
	u.whenIdle = true
	u.restartStatus = func() (*ipc.RestartStatusResult, error) {
		return nil, fmt.Errorf("connect to daemon: %w", daemon.ErrDaemonNotReachable)
	}
	u.scheduleRestart = func() (*ipc.RestartStatusResult, error) { t.Fatal("schedule must not run"); return nil, nil }
	reset := false
	u.resetDaemon = func(bool) error { reset = true; return nil }

	if err := u.run(context.Background()); err != nil {
		t.Fatalf("update should proceed when no daemon is reachable, got %v", err)
	}
	if !reset {
		t.Fatal("update should use the plain reset when there is no daemon to protect")
	}
}

// Greptile on #1697: the --when-idle probe asks whether the daemon is running,
// which clears a dead daemon's stale socket and PID file. The update must
// still restart that daemon rather than report success with it stopped.
func TestUpdaterRunWhenIdleRestartsADeadDaemonWhoseFilesTheProbeCleared(t *testing.T) {
	u, _, _, _ := newActiveRunUpdater(t)
	finishActiveRuns(t, u.paths)
	if err := os.WriteFile(u.paths.PIDFile(), []byte("999999\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	orig := daemonIsRunning
	t.Cleanup(func() { daemonIsRunning = orig })
	daemonIsRunning = func(p *paths.Paths) (bool, error) {
		// Stands in for daemon.IsRunning finding the PID dead and removing
		// its stale files.
		_ = os.Remove(p.PIDFile())
		_ = os.Remove(p.Socket())
		return false, nil
	}
	u.whenIdle = true
	u.restartStatus = func() (*ipc.RestartStatusResult, error) {
		return nil, fmt.Errorf("connect to daemon: %w", daemon.ErrDaemonNotReachable)
	}
	u.scheduleRestart = func() (*ipc.RestartStatusResult, error) { t.Fatal("schedule must not run"); return nil, nil }
	var expected *bool
	u.resetDaemon = func(daemonExpected bool) error { expected = &daemonExpected; return nil }

	if err := u.run(context.Background()); err != nil {
		t.Fatalf("update should proceed with no live daemon, got %v", err)
	}
	if expected == nil {
		t.Fatal("update should reset the daemon")
	}
	if !*expected {
		t.Fatal("reset daemonExpected = false, want true so the dead daemon is started again")
	}
}

// Greptile on #1697: a refused connection alone does not prove the daemon is
// gone. One that is running but slow to accept could start a run during the
// download, so the update refuses instead of restarting it at once.
func TestUpdaterRunWhenIdleRefusesAnUnreachableDaemonThatIsStillRunning(t *testing.T) {
	stubDaemonAlive(t, true)
	u, execPath, _, _ := newActiveRunUpdater(t)
	finishActiveRuns(t, u.paths)
	u.whenIdle = true
	u.restartStatus = func() (*ipc.RestartStatusResult, error) {
		return nil, fmt.Errorf("connect to daemon: %w", daemon.ErrDaemonNotReachable)
	}
	u.scheduleRestart = func() (*ipc.RestartStatusResult, error) { t.Fatal("schedule must not run"); return nil, nil }
	u.resetDaemon = func(bool) error { t.Fatal("reset must not run"); return nil }

	err := u.run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "--force") {
		t.Fatalf("update should refuse and offer --force, got %v", err)
	}
	if content, _ := os.ReadFile(execPath); string(content) != "old-binary" {
		t.Fatalf("a refused update must leave the executable alone, got %q", content)
	}
}

func stubDaemonAlive(t *testing.T, alive bool) {
	t.Helper()
	orig := daemonIsRunning
	t.Cleanup(func() { daemonIsRunning = orig })
	daemonIsRunning = func(*paths.Paths) (bool, error) { return alive, nil }
}

func TestRestartStatusWrapsAnUnreachableSocket(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	if _, err := daemon.RestartStatus(p); !errors.Is(err, daemon.ErrDaemonNotReachable) {
		t.Fatalf("a dial failure should be ErrDaemonNotReachable, got %v", err)
	}
}

// The daemon restarts through its own executable, so a daemon started from
// another binary would miss the update. With runs active that is refused; with
// none, the plain takeover path (replace the daemon now) applies as without
// --when-idle.
func stubDaemonFromAnotherBinary(t *testing.T) {
	t.Helper()
	origRunning, origPath := daemonIsRunning, daemonExecutablePath
	t.Cleanup(func() { daemonIsRunning, daemonExecutablePath = origRunning, origPath })
	daemonIsRunning = func(*paths.Paths) (bool, error) { return true, nil }
	daemonExecutablePath = func(*paths.Paths) (string, error) { return "/opt/other/no-mistakes", nil }
}

func TestUpdaterRunWhenIdleRefusesADaemonFromAnotherBinaryWhileRunsAreActive(t *testing.T) {
	stubDaemonFromAnotherBinary(t)
	u, execPath, _, _ := newActiveRunUpdater(t)
	u.whenIdle = true
	u.assumeYes = true
	u.restartStatus = func() (*ipc.RestartStatusResult, error) { return &ipc.RestartStatusResult{}, nil }
	u.scheduleRestart = func() (*ipc.RestartStatusResult, error) { t.Fatal("schedule must not run"); return nil, nil }
	u.resetDaemon = func(bool) error { t.Fatal("reset must not run"); return nil }

	err := u.run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "with active runs") {
		t.Fatalf("update should refuse, got %v", err)
	}
	if content, _ := os.ReadFile(execPath); string(content) != "old-binary" {
		t.Fatalf("a refused update must leave the executable alone, got %q", content)
	}
}

func TestUpdaterRunWhenIdleWithNoActiveRunFallsBackToTheTakeoverReset(t *testing.T) {
	stubDaemonFromAnotherBinary(t)
	u, _, stdout, _ := newActiveRunUpdater(t)
	finishActiveRuns(t, u.paths)
	u.whenIdle = true
	u.assumeYes = true
	u.restartStatus = func() (*ipc.RestartStatusResult, error) { return &ipc.RestartStatusResult{}, nil }
	u.scheduleRestart = func() (*ipc.RestartStatusResult, error) { t.Fatal("schedule must not run"); return nil, nil }
	reset := false
	u.resetDaemon = func(bool) error { reset = true; return nil }

	if err := u.run(context.Background()); err != nil {
		t.Fatalf("update should take over the daemon when no run is active, got %v", err)
	}
	if !reset {
		t.Fatal("update should reset the daemon through the plain takeover path")
	}
	if !strings.Contains(stdout.String(), "updated no-mistakes from v1.2.2 to v1.2.3") {
		t.Fatalf("stdout = %q", stdout.String())
	}
}

func finishActiveRuns(t *testing.T, p *paths.Paths) {
	t.Helper()
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	runs, err := lifecycle.ActiveRuns(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, run := range runs {
		if err := database.UpdateRunStatus(run.ID, types.RunCompleted); err != nil {
			t.Fatal(err)
		}
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

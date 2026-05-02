package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

type failingRenameError string

func (e failingRenameError) Error() string { return string(e) }

func writeDaemonPIDRecord(t *testing.T, path string, record daemonPIDFile) {
	t.Helper()
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestStopNotRunningRemovesStaleArtifacts(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "dtest")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	p := paths.WithRoot(tmpDir)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	const pid = 424242
	if err := os.WriteFile(p.PIDFile(), []byte(fmt.Sprintf("%d", pid)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.Socket(), []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}

	originalUsesRegularFile := daemonEndpointUsesRegularFile
	daemonEndpointUsesRegularFile = func() bool { return true }
	defer func() {
		daemonEndpointUsesRegularFile = originalUsesRegularFile
	}()
	originalProcessRunning := daemonProcessRunning
	processRunningChecks := 0
	daemonProcessRunning = func(checkPID int) (bool, error) {
		if checkPID != pid {
			t.Fatalf("processRunning pid = %d, want %d", checkPID, pid)
		}
		processRunningChecks++
		return false, nil
	}
	defer func() {
		daemonProcessRunning = originalProcessRunning
	}()

	if err := Stop(p); err != nil {
		t.Fatalf("stop should succeed when daemon is not running: %v", err)
	}
	if processRunningChecks == 0 {
		t.Fatal("expected stale-artifact detection to check process state")
	}
	if _, err := os.Stat(p.PIDFile()); !os.IsNotExist(err) {
		t.Fatalf("expected stale pid file to be removed, got err=%v", err)
	}
	if _, err := os.Stat(p.Socket()); !os.IsNotExist(err) {
		t.Fatalf("expected stale socket file to be removed, got err=%v", err)
	}
}

func TestWaitForDaemonStopKeepsArtifactsWhenKillFails(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "dtest")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	p := paths.WithRoot(tmpDir)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.PIDFile(), []byte("999999"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.Socket(), []byte("still-there"), 0o644); err != nil {
		t.Fatal(err)
	}

	originalHealthCheck := daemonHealthCheck
	daemonHealthCheck = func(*paths.Paths) (bool, error) {
		return true, nil
	}
	defer func() {
		daemonHealthCheck = originalHealthCheck
	}()

	started := time.Now()
	err = waitForDaemonStop(p)
	if err == nil {
		t.Fatal("expected waitForDaemonStop to fail when kill fails")
	}
	if time.Since(started) < 5*time.Second {
		t.Fatalf("waitForDaemonStop returned too early after %v", time.Since(started))
	}
	if _, err := os.Stat(p.PIDFile()); err != nil {
		t.Fatalf("expected pid file to remain after failed kill, got err=%v", err)
	}
	if _, err := os.Stat(p.Socket()); err != nil {
		t.Fatalf("expected socket file to remain after failed kill, got err=%v", err)
	}
}

func TestStopDetachedDaemonFallsBackToPIDWhenSocketIsBroken(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix socket setup is platform-specific")
	}

	tmpDir, err := os.MkdirTemp("", "dtest")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	p := paths.WithRoot(tmpDir)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	const pid = 424242
	startedAt := time.Date(2026, 4, 20, 10, 0, 0, 0, time.UTC)
	writeDaemonPIDRecord(t, p.PIDFile(), daemonPIDFile{PID: pid, StartedAt: startedAt})
	ln, err := net.Listen("unix", p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	originalDial := daemonDial
	daemonDial = func(string) (*ipc.Client, error) {
		return nil, fmt.Errorf("transient ipc failure")
	}
	defer func() {
		daemonDial = originalDial
	}()
	originalProcessRunning := daemonProcessRunning
	runningChecks := 0
	daemonProcessRunning = func(checkPID int) (bool, error) {
		if checkPID != pid {
			t.Fatalf("processRunning pid = %d, want %d", checkPID, pid)
		}
		runningChecks++
		return runningChecks == 1, nil
	}
	defer func() {
		daemonProcessRunning = originalProcessRunning
	}()
	originalProcessStartTime := daemonProcessStartTime
	daemonProcessStartTime = func(checkPID int) (time.Time, error) {
		if checkPID != pid {
			t.Fatalf("processStartTime pid = %d, want %d", checkPID, pid)
		}
		return startedAt, nil
	}
	defer func() {
		daemonProcessStartTime = originalProcessStartTime
	}()
	originalKillPID := daemonKillPID
	killedPID := 0
	daemonKillPID = func(killPID int) error {
		killedPID = killPID
		return nil
	}
	defer func() {
		daemonKillPID = originalKillPID
	}()

	if err := stopDetachedDaemon(p); err != nil {
		t.Fatalf("expected stopDetachedDaemon to stop live pid when IPC dial fails, got %v", err)
	}
	if killedPID != pid {
		t.Fatalf("expected pid fallback to kill pid %d, got %d", pid, killedPID)
	}
	if runningChecks == 0 {
		t.Fatal("expected pid fallback to check process state")
	}
	if _, statErr := os.Stat(p.PIDFile()); !os.IsNotExist(statErr) {
		t.Fatalf("expected pid file to be removed after PID fallback, got err=%v", statErr)
	}
	if _, statErr := os.Stat(p.Socket()); !os.IsNotExist(statErr) {
		t.Fatalf("expected socket file to be removed after PID fallback, got err=%v", statErr)
	}
}

func TestStopDetachedDaemonRejectsStalePIDFallback(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix socket setup is platform-specific")
	}

	tmpDir, err := os.MkdirTemp("", "dtest")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	p := paths.WithRoot(tmpDir)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	writeDaemonPIDRecord(t, p.PIDFile(), daemonPIDFile{PID: os.Getpid(), StartedAt: time.Now().Add(-24 * time.Hour)})
	ln, err := net.Listen("unix", p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	originalDial := daemonDial
	daemonDial = func(string) (*ipc.Client, error) {
		return nil, fmt.Errorf("transient ipc failure")
	}
	defer func() {
		daemonDial = originalDial
	}()
	originalKillPID := daemonKillPID
	killCalled := false
	daemonKillPID = func(int) error {
		killCalled = true
		return nil
	}
	defer func() {
		daemonKillPID = originalKillPID
	}()

	err = stopDetachedDaemon(p)
	if err == nil {
		t.Fatal("expected stale pid fallback to fail")
	}
	if killCalled {
		t.Fatal("expected stale pid fallback to avoid killing the process")
	}
	if _, statErr := os.Stat(p.PIDFile()); statErr != nil {
		t.Fatalf("expected pid file to remain after rejected pid fallback, got err=%v", statErr)
	}
	if _, statErr := os.Stat(p.Socket()); statErr != nil {
		t.Fatalf("expected socket file to remain after rejected pid fallback, got err=%v", statErr)
	}
}

func TestStopDetachedDaemonRejectsUnrelatedLiveProcessPIDFallback(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix socket setup is platform-specific")
	}

	tmpDir, err := os.MkdirTemp("", "dtest")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	p := paths.WithRoot(tmpDir)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	writeDaemonPIDRecord(t, p.PIDFile(), daemonPIDFile{PID: os.Getpid(), StartedAt: time.Now()})
	ln, err := net.Listen("unix", p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	originalDial := daemonDial
	daemonDial = func(string) (*ipc.Client, error) {
		return nil, fmt.Errorf("transient ipc failure")
	}
	defer func() {
		daemonDial = originalDial
	}()
	originalProcessStartTime := daemonProcessStartTime
	daemonProcessStartTime = func(checkPID int) (time.Time, error) {
		if checkPID != os.Getpid() {
			t.Fatalf("processStartTime pid = %d, want %d", checkPID, os.Getpid())
		}
		return time.Now().Add(-time.Hour), nil
	}
	defer func() {
		daemonProcessStartTime = originalProcessStartTime
	}()
	originalKillPID := daemonKillPID
	killCalled := false
	daemonKillPID = func(int) error {
		killCalled = true
		return nil
	}
	defer func() {
		daemonKillPID = originalKillPID
	}()

	err = stopDetachedDaemon(p)
	if err == nil {
		t.Fatal("expected unrelated live process pid fallback to fail")
	}
	if killCalled {
		t.Fatal("expected unrelated live process pid fallback to avoid killing the process")
	}
	if _, statErr := os.Stat(p.PIDFile()); statErr != nil {
		t.Fatalf("expected pid file to remain after rejected pid fallback, got err=%v", statErr)
	}
	if _, statErr := os.Stat(p.Socket()); statErr != nil {
		t.Fatalf("expected socket file to remain after rejected pid fallback, got err=%v", statErr)
	}
}

func TestValidateDaemonPIDFallback_RejectsLegacyPIDFileForReusedPID(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "dtest")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	p := paths.WithRoot(tmpDir)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.PIDFile(), []byte(fmt.Sprintf("%d", os.Getpid())), 0o644); err != nil {
		t.Fatal(err)
	}

	old := daemonProcessStartTime
	daemonProcessStartTime = func(checkPID int) (time.Time, error) {
		if checkPID != os.Getpid() {
			t.Fatalf("processStartTime pid = %d, want %d", checkPID, os.Getpid())
		}
		return time.Now().Add(-time.Hour), nil
	}
	t.Cleanup(func() {
		daemonProcessStartTime = old
	})

	err = validateDaemonPIDFallback(p, os.Getpid())
	if err == nil {
		t.Fatal("expected legacy pid fallback to reject reused pid")
	}
}

func TestValidateDaemonPIDFallback_RejectsLegacyPIDFileTouchedNearLivePID(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "dtest")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	p := paths.WithRoot(tmpDir)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.PIDFile(), []byte(fmt.Sprintf("%d", os.Getpid())), 0o644); err != nil {
		t.Fatal(err)
	}
	mtime := time.Date(2026, 4, 20, 10, 0, 0, 0, time.UTC)
	if err := os.Chtimes(p.PIDFile(), mtime, mtime); err != nil {
		t.Fatal(err)
	}

	oldStartTime := daemonProcessStartTime
	oldHealth := daemonHealthCheck
	daemonProcessStartTime = func(checkPID int) (time.Time, error) {
		if checkPID != os.Getpid() {
			t.Fatalf("processStartTime pid = %d, want %d", checkPID, os.Getpid())
		}
		return mtime.Add(time.Second), nil
	}
	daemonHealthCheck = func(*paths.Paths) (bool, error) {
		return false, nil
	}
	t.Cleanup(func() {
		daemonProcessStartTime = oldStartTime
		daemonHealthCheck = oldHealth
	})

	err = validateDaemonPIDFallback(p, os.Getpid())
	if err == nil {
		t.Fatal("expected legacy pid fallback to reject timestamp-only matches")
	}
}

func TestWaitForProcessExitRetriesTransientInspectionErrors(t *testing.T) {
	originalProcessRunning := daemonProcessRunning
	checks := 0
	daemonProcessRunning = func(pid int) (bool, error) {
		if pid != 4242 {
			t.Fatalf("daemonProcessRunning pid = %d, want 4242", pid)
		}
		checks++
		switch checks {
		case 1:
			return false, fmt.Errorf("transient failure")
		case 2:
			return true, nil
		default:
			return false, nil
		}
	}
	t.Cleanup(func() {
		daemonProcessRunning = originalProcessRunning
	})

	waitForProcessExit(4242, 50*time.Millisecond)

	if checks < 3 {
		t.Fatalf("waitForProcessExit stopped after %d checks, want at least 3", checks)
	}
}

func TestCurrentDaemonPIDRecord_UsesProcessStartTime(t *testing.T) {
	want := time.Date(2026, 4, 20, 10, 0, 0, 0, time.UTC)
	now := want.Add(10 * time.Second)

	record, err := currentDaemonPIDRecord(func(pid int) (time.Time, error) {
		if pid != os.Getpid() {
			t.Fatalf("processStartTime pid = %d, want %d", pid, os.Getpid())
		}
		return want, nil
	}, func() time.Time {
		return now
	})
	if err != nil {
		t.Fatalf("currentDaemonPIDRecord returned error: %v", err)
	}
	if record.PID != os.Getpid() {
		t.Fatalf("record pid = %d, want %d", record.PID, os.Getpid())
	}
	if !record.StartedAt.Equal(want) {
		t.Fatalf("record started_at = %v, want %v", record.StartedAt, want)
	}
}

func TestWriteDaemonPIDFile_LeavesExistingFileUntouchedOnRenameFailure(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "daemon.pid")
	original := []byte("old-data")
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatal(err)
	}

	oldRename := renameDaemonPIDFile
	renameDaemonPIDFile = func(_, _ string) error {
		return failingRenameError("rename failed")
	}
	t.Cleanup(func() {
		renameDaemonPIDFile = oldRename
	})

	err := writeDaemonPIDFile(path, daemonPIDFile{PID: 12345, StartedAt: time.Date(2026, 4, 20, 10, 0, 0, 0, time.UTC)})
	if err == nil {
		t.Fatal("expected writeDaemonPIDFile to fail when rename fails")
	}
	data, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("read pid file: %v", readErr)
	}
	if string(data) != string(original) {
		t.Fatalf("pid file changed after failed atomic write: got %q want %q", string(data), string(original))
	}
	matches, globErr := filepath.Glob(filepath.Join(tmpDir, "daemon.pid.tmp-*"))
	if globErr != nil {
		t.Fatalf("glob temp files: %v", globErr)
	}
	if len(matches) != 0 {
		t.Fatalf("expected temp files to be cleaned up, got %v", matches)
	}
}

func TestStopDetachedDaemonRemovesArtifactsForDeadPID(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix socket setup is platform-specific")
	}

	tmpDir, err := os.MkdirTemp("", "dtest")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	p := paths.WithRoot(tmpDir)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.PIDFile(), []byte("999999"), 0o644); err != nil {
		t.Fatal(err)
	}
	fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	addr := &syscall.SockaddrUnix{Name: p.Socket()}
	if err := syscall.Bind(fd, addr); err != nil {
		_ = syscall.Close(fd)
		t.Fatal(err)
	}
	if err := syscall.Close(fd); err != nil {
		t.Fatal(err)
	}

	originalDial := daemonDial
	daemonDial = func(string) (*ipc.Client, error) {
		return nil, fmt.Errorf("transient ipc failure")
	}
	defer func() {
		daemonDial = originalDial
	}()

	if err := stopDetachedDaemon(p); err != nil {
		t.Fatalf("expected stopDetachedDaemon to clean stale artifacts, got %v", err)
	}
	if _, statErr := os.Stat(p.PIDFile()); !os.IsNotExist(statErr) {
		t.Fatalf("expected stale pid file to be removed, got err=%v", statErr)
	}
	if _, statErr := os.Stat(p.Socket()); !os.IsNotExist(statErr) {
		t.Fatalf("expected stale socket file to be removed, got err=%v", statErr)
	}
}

func TestStaleDaemonArtifactsKeepsPIDForLiveProcessWithoutSocket(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "dtest")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	p := paths.WithRoot(tmpDir)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.PIDFile(), []byte(fmt.Sprintf("%d", os.Getpid())), 0o644); err != nil {
		t.Fatal(err)
	}

	stale, err := staleDaemonArtifacts(p)
	if err != nil {
		t.Fatalf("staleDaemonArtifacts returned error: %v", err)
	}
	if stale {
		t.Fatal("expected live pid without socket to be treated as non-stale")
	}
}

func TestStaleDaemonArtifactsKeepsRegularEndpointFileForLiveProcess(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "dtest")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	p := paths.WithRoot(tmpDir)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.PIDFile(), []byte(fmt.Sprintf("%d", os.Getpid())), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.Socket(), []byte("127.0.0.1:1234\ntoken\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	original := daemonEndpointUsesRegularFile
	daemonEndpointUsesRegularFile = func() bool { return true }
	defer func() {
		daemonEndpointUsesRegularFile = original
	}()

	stale, err := staleDaemonArtifacts(p)
	if err != nil {
		t.Fatalf("staleDaemonArtifacts returned error: %v", err)
	}
	if stale {
		t.Fatal("expected live pid with regular endpoint file to be treated as non-stale")
	}
}

func TestStopDetachedDaemonKeepsArtifactsWhenPIDMissingButDaemonLooksLive(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix socket setup is platform-specific")
	}

	tmpDir, err := os.MkdirTemp("", "dtest")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	p := paths.WithRoot(tmpDir)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	originalDial := daemonDial
	daemonDial = func(string) (*ipc.Client, error) {
		return nil, fmt.Errorf("transient ipc failure")
	}
	defer func() {
		daemonDial = originalDial
	}()
	err = stopDetachedDaemon(p)
	if err == nil {
		t.Fatal("expected stopDetachedDaemon to fail without a pid file")
	}
	if _, statErr := os.Stat(p.Socket()); statErr != nil {
		t.Fatalf("expected socket file to remain when daemon looks live, got err=%v", statErr)
	}
	if _, statErr := os.Stat(p.PIDFile()); !os.IsNotExist(statErr) {
		t.Fatalf("expected pid file to remain missing, got err=%v", statErr)
	}
}

func TestStaleDaemonArtifactsRejectsNonPositivePID(t *testing.T) {
	tests := []struct {
		name string
		pid  string
	}{
		{name: "zero", pid: "0"},
		{name: "negative", pid: "-1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir, err := os.MkdirTemp("", "dtest")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(tmpDir)

			p := paths.WithRoot(tmpDir)
			if err := p.EnsureDirs(); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p.PIDFile(), []byte(tt.pid), 0o644); err != nil {
				t.Fatal(err)
			}

			called := false
			original := daemonProcessRunning
			daemonProcessRunning = func(int) (bool, error) {
				called = true
				return false, nil
			}
			defer func() {
				daemonProcessRunning = original
			}()

			_, err = staleDaemonArtifacts(p)
			if err == nil {
				t.Fatal("expected invalid pid error")
			}
			if !strings.Contains(err.Error(), "invalid") {
				t.Fatalf("error = %q, want invalid pid error", err)
			}
			if called {
				t.Fatal("expected invalid pid to avoid process probe")
			}
		})
	}
}

func TestReadPIDNoFile(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "dtest")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	p := paths.WithRoot(tmpDir)
	_, err = ReadPID(p)
	if err == nil {
		t.Error("expected error when no PID file")
	}
}

func TestWaitForDaemonStopDoesNotTreatHealthCheckErrorsAsStopped(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "dtest")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	p := paths.WithRoot(tmpDir)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.PIDFile(), []byte("999999"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.Socket(), []byte("still-there"), 0o644); err != nil {
		t.Fatal(err)
	}

	originalHealthCheck := daemonHealthCheck
	daemonHealthCheck = func(*paths.Paths) (bool, error) {
		return false, errors.New("transient ipc failure")
	}
	defer func() {
		daemonHealthCheck = originalHealthCheck
	}()

	started := time.Now()
	err = waitForDaemonStop(p)
	if err == nil {
		t.Fatal("expected waitForDaemonStop to fail when health checks only error")
	}
	if time.Since(started) < 5*time.Second {
		t.Fatalf("waitForDaemonStop returned too early after %v", time.Since(started))
	}
	if _, err := os.Stat(p.PIDFile()); err != nil {
		t.Fatalf("expected pid file to remain after health-check errors, got err=%v", err)
	}
	if _, err := os.Stat(p.Socket()); err != nil {
		t.Fatalf("expected socket file to remain after health-check errors, got err=%v", err)
	}
}

func TestWaitForDaemonStopRejectsStalePIDBeforeKill(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "dtest")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	p := paths.WithRoot(tmpDir)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	writeDaemonPIDRecord(t, p.PIDFile(), daemonPIDFile{PID: os.Getpid(), StartedAt: time.Now().Add(-24 * time.Hour)})
	if err := os.WriteFile(p.Socket(), []byte("still-there"), 0o644); err != nil {
		t.Fatal(err)
	}

	originalHealthCheck := daemonHealthCheck
	daemonHealthCheck = func(*paths.Paths) (bool, error) {
		return true, nil
	}
	defer func() {
		daemonHealthCheck = originalHealthCheck
	}()
	originalProcessStartTime := daemonProcessStartTime
	daemonProcessStartTime = func(checkPID int) (time.Time, error) {
		if checkPID != os.Getpid() {
			t.Fatalf("processStartTime pid = %d, want %d", checkPID, os.Getpid())
		}
		return time.Now(), nil
	}
	defer func() {
		daemonProcessStartTime = originalProcessStartTime
	}()
	originalKillPID := daemonKillPID
	killCalled := false
	daemonKillPID = func(int) error {
		killCalled = true
		return nil
	}
	defer func() {
		daemonKillPID = originalKillPID
	}()

	started := time.Now()
	err = waitForDaemonStop(p)
	if err == nil {
		t.Fatal("expected waitForDaemonStop to fail for stale pid")
	}
	if time.Since(started) < 5*time.Second {
		t.Fatalf("waitForDaemonStop returned too early after %v", time.Since(started))
	}
	if killCalled {
		t.Fatal("expected stale pid to avoid killing the process")
	}
	if _, err := os.Stat(p.PIDFile()); err != nil {
		t.Fatalf("expected pid file to remain after rejected kill, got err=%v", err)
	}
	if _, err := os.Stat(p.Socket()); err != nil {
		t.Fatalf("expected socket file to remain after rejected kill, got err=%v", err)
	}
}

func TestReadPIDInvalid(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "dtest")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	p := paths.WithRoot(tmpDir)
	os.WriteFile(filepath.Join(tmpDir, "daemon.pid"), []byte("notanumber"), 0o644)
	_, err = ReadPID(p)
	if err == nil {
		t.Error("expected error for invalid PID content")
	}
}

func TestReadPIDRejectsNonPositiveValues(t *testing.T) {
	tests := []struct {
		name string
		pid  string
	}{
		{name: "zero", pid: "0"},
		{name: "negative", pid: "-1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpDir, err := os.MkdirTemp("", "dtest")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(tmpDir)

			p := paths.WithRoot(tmpDir)
			if err := os.WriteFile(filepath.Join(tmpDir, "daemon.pid"), []byte(tt.pid), 0o644); err != nil {
				t.Fatal(err)
			}

			_, err = ReadPID(p)
			if err == nil {
				t.Fatal("expected invalid pid error")
			}
			if !strings.Contains(err.Error(), "invalid") {
				t.Fatalf("error = %q, want invalid pid error", err)
			}
		})
	}
}

package daemon

import (
	"net"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// staleDaemonRoot leaves what an exited daemon leaves behind: a socket file
// that refuses connections and a PID file naming its process.
func staleDaemonRoot(t *testing.T, pidFile []byte) *paths.Paths {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("unix socket setup is platform-specific")
	}
	tmpDir, err := os.MkdirTemp("", "dtest")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(tmpDir) })
	p := paths.WithRoot(tmpDir)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: p.Socket(), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	ln.SetUnlinkOnClose(false)
	ln.Close()
	if err := os.WriteFile(p.PIDFile(), pidFile, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func fakeDaemonProcess(t *testing.T, running bool, startedAt time.Time) {
	t.Helper()
	oldRunning, oldStart := daemonProcessRunning, daemonProcessStartTime
	daemonProcessRunning = func(int) (bool, error) { return running, nil }
	daemonProcessStartTime = func(int) (time.Time, error) { return startedAt, nil }
	t.Cleanup(func() { daemonProcessRunning, daemonProcessStartTime = oldRunning, oldStart })
}

func artifactsExist(p *paths.Paths) (socket, pid bool) {
	_, socketErr := os.Stat(p.Socket())
	_, pidErr := os.Stat(p.PIDFile())
	return socketErr == nil, pidErr == nil
}

func TestIsRunningRefusedSocketWithGoneDaemonIsNotRunning(t *testing.T) {
	started := time.Date(2026, 4, 20, 10, 0, 0, 0, time.UTC)
	record := []byte(`{"pid":424242,"started_at":"2026-04-20T10:00:00Z"}`)
	for name, process := range map[string]struct {
		running   bool
		startedAt time.Time
	}{
		"dead pid":            {running: false},
		"start-time mismatch": {running: true, startedAt: started.Add(time.Hour)},
	} {
		t.Run(name, func(t *testing.T) {
			p := staleDaemonRoot(t, record)
			fakeDaemonProcess(t, process.running, process.startedAt)

			alive, err := IsRunning(p)
			if err != nil || alive {
				t.Fatalf("IsRunning = (%v, %v), want (false, nil)", alive, err)
			}
			if socket, pid := artifactsExist(p); socket || pid {
				t.Fatalf("stale artifacts remain: socket=%v pid=%v", socket, pid)
			}
		})
	}
}

func TestIsRunningRefusedSocketWithLiveOrUnverifiableDaemonStillRefuses(t *testing.T) {
	started := time.Date(2026, 4, 20, 10, 0, 0, 0, time.UTC)
	record := []byte(`{"pid":424242,"started_at":"2026-04-20T10:00:00Z"}`)
	for name, tc := range map[string]struct {
		pidFile  []byte
		running  bool
		holdLock bool
	}{
		"live pid":                     {pidFile: record, running: true},
		"unreadable pid file":          {pidFile: []byte("notanumber")},
		"live legacy pid without time": {pidFile: []byte("424242"), running: true},
		"singleton lock held":          {pidFile: record, holdLock: true},
	} {
		t.Run(name, func(t *testing.T) {
			p := staleDaemonRoot(t, tc.pidFile)
			fakeDaemonProcess(t, tc.running, started)
			if tc.holdLock {
				lock, err := acquireSingletonLock(p)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(lock.Release)
			}

			alive, err := IsRunning(p)
			if err == nil || alive {
				t.Fatalf("IsRunning = (%v, %v), want a connect error", alive, err)
			}
			if socket, pid := artifactsExist(p); !socket || !pid {
				t.Fatalf("artifacts removed: socket=%v pid=%v", socket, pid)
			}
		})
	}
}

// The update path stops the old daemon and waits for it; a daemon that exited
// but left its socket and PID file behind must count as stopped.
func TestWaitForDaemonStopAcceptsExitedDaemonThatLeftItsArtifacts(t *testing.T) {
	started := time.Date(2026, 4, 20, 10, 0, 0, 0, time.UTC)
	p := staleDaemonRoot(t, []byte(`{"pid":424242,"started_at":"2026-04-20T10:00:00Z"}`))
	fakeDaemonProcess(t, false, started)
	t.Setenv("NM_TEST_DAEMON_STOP_TIMEOUT", "1s")
	oldKill := daemonKillPID
	daemonKillPID = func(pid int) error { t.Fatalf("killed pid %d of an exited daemon", pid); return nil }
	t.Cleanup(func() { daemonKillPID = oldKill })

	if err := waitForDaemonStop(p, daemonInstance{pid: 424242, startedAt: started}); err != nil {
		t.Fatalf("waitForDaemonStop = %v, want nil", err)
	}
	if socket, pid := artifactsExist(p); socket || pid {
		t.Fatalf("stale artifacts remain: socket=%v pid=%v", socket, pid)
	}
}

// A probe inspecting a stale PID must not hold the singleton lock, or a
// daemon starting at that moment would refuse to run.
func TestIsRunningStaleProbeDoesNotBlockAStartingDaemon(t *testing.T) {
	p := staleDaemonRoot(t, []byte(`{"pid":424242,"started_at":"2026-04-20T10:00:00Z"}`))
	oldRunning := daemonProcessRunning
	daemonProcessRunning = func(int) (bool, error) {
		lock, err := acquireSingletonLock(p)
		if err != nil {
			t.Errorf("daemon starting during inspection: %v", err)
			return false, nil
		}
		lock.Release()
		return false, nil
	}
	t.Cleanup(func() { daemonProcessRunning = oldRunning })

	if alive, err := IsRunning(p); err != nil || alive {
		t.Fatalf("IsRunning = (%v, %v), want (false, nil)", alive, err)
	}
}

// A daemon that started between the inspection and the removal rewrote the
// PID file; its artifacts stay.
func TestIsRunningStaleProbeKeepsArtifactsOfADaemonThatJustStarted(t *testing.T) {
	p := staleDaemonRoot(t, []byte(`{"pid":424242,"started_at":"2026-04-20T10:00:00Z"}`))
	oldRunning := daemonProcessRunning
	daemonProcessRunning = func(int) (bool, error) {
		if err := os.WriteFile(p.PIDFile(), []byte(`{"pid":515151,"started_at":"2026-04-20T11:00:00Z"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		return false, nil
	}
	t.Cleanup(func() { daemonProcessRunning = oldRunning })

	if alive, err := IsRunning(p); err == nil || alive {
		t.Fatalf("IsRunning = (%v, %v), want a connect error", alive, err)
	}
	if socket, pid := artifactsExist(p); !socket || !pid {
		t.Fatalf("artifacts removed: socket=%v pid=%v", socket, pid)
	}
}

// A probe holds the singleton lock while it re-checks and removes a dead
// daemon's artifacts; a daemon starting in that window waits it out instead of
// exiting as "already running".
func TestStartingDaemonWaitsOutTheCleanupLockWindow(t *testing.T) {
	p := staleDaemonRoot(t, []byte(`{"pid":424242,"started_at":"2026-04-20T10:00:00Z"}`))
	cleanupHolder, err := os.OpenFile(p.LockFile(), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if err := tryLockFile(cleanupHolder); err != nil {
		cleanupHolder.Close()
		t.Fatal(err)
	}
	released := make(chan struct{})
	go func() {
		defer close(released)
		time.Sleep(50 * time.Millisecond)
		_ = unlockFile(cleanupHolder)
		_ = cleanupHolder.Close()
	}()

	lock, err := acquireSingletonLock(p)
	<-released
	if err != nil {
		t.Fatalf("starting daemon refused during the cleanup window: %v", err)
	}
	lock.Release()
}

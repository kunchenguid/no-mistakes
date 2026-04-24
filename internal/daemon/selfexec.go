package daemon

import (
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

var daemonHealthCheck = daemonIsRunningViaIPC
var daemonDial = ipc.Dial
var daemonProcessRunning = processRunning
var daemonProcessStartTime = processStartTime
var daemonKillPID = killPID
var daemonEndpointUsesRegularFile = func() bool { return runtime.GOOS == "windows" }

func daemonStartTimeout() time.Duration {
	return durationFromEnv("NM_TEST_DAEMON_START_TIMEOUT", 5*time.Second)
}

func daemonStartPollInterval() time.Duration {
	return durationFromEnv("NM_TEST_DAEMON_START_POLL_INTERVAL", 100*time.Millisecond)
}

func durationFromEnv(name string, fallback time.Duration) time.Duration {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	d, err := time.ParseDuration(value)
	if err != nil || d <= 0 {
		return fallback
	}
	return d
}

// Start installs or refreshes the managed daemon service when supported and
// starts it, falling back to a detached re-exec with NM_DAEMON=1 when managed
// startup is unavailable or fails. It waits up to 5 seconds for the daemon to
// become responsive on the IPC socket.
//
// When the daemon is already running, Start refreshes the installed service
// definition and reloads the service manager if the on-disk definition is
// stale (e.g., after a binary upgrade that changed the plist/unit). This is
// what lets users pick up env-var changes (see #143 for the PATH fix) with
// a plain `daemon start` instead of a manual stop + start.
func Start(p *paths.Paths) error {
	if err := p.EnsureDirs(); err != nil {
		return err
	}
	if alive, _ := daemonHealthCheck(p); alive {
		reloaded, err := reinstallManagedServiceIfChanged(p)
		if err != nil {
			return err
		}
		if reloaded {
			return nil
		}
		return fmt.Errorf("daemon already running")
	}
	if managed, err := installManagedService(p); err == nil {
		if managed {
			if err := startManagedDaemon(p); err == nil {
				return nil
			} else if err := stopManagedFallback(p); err != nil {
				return err
			}
		}
	} else if alive, _ := daemonHealthCheck(p); alive {
		return nil
	}
	return startDetachedDaemon(p)
}

// reinstallManagedServiceIfChanged refreshes the managed daemon service and
// reloads it through launchctl/systemctl when the on-disk service definition
// differs from what the current binary would generate. Returns true when a
// reload actually happened. Called from Start() so that `daemon start` after
// a binary upgrade re-applies the new service definition without forcing
// users to run `daemon restart` explicitly. No-op on Windows and when the
// service manager is bypassed (i.e., under `go test`).
func reinstallManagedServiceIfChanged(p *paths.Paths) (bool, error) {
	if serviceManagerBypassed() {
		return false, nil
	}
	exe, err := serviceExecutablePath()
	if err != nil {
		return false, fmt.Errorf("resolve executable: %w", err)
	}
	home, err := serviceUserHomeDir()
	if err != nil {
		return false, fmt.Errorf("resolve user home: %w", err)
	}

	var installPath, wanted string
	renderedExecutable := exe
	switch runtimeGOOS {
	case "darwin":
		installPath = launchAgentPath(p)
	case "linux":
		installPath = systemdUserServicePath(p)
	default:
		return false, nil
	}

	existing, readErr := os.ReadFile(installPath)
	if readErr == nil {
		switch runtimeGOOS {
		case "darwin":
			if existingExe, ok := launchAgentExecutable(existing); ok {
				renderedExecutable = existingExe
			}
		case "linux":
			if existingExe, ok := systemdUnitExecutable(existing); ok {
				renderedExecutable = existingExe
			}
		}
	}
	switch runtimeGOOS {
	case "darwin":
		wanted = renderLaunchAgent(renderedExecutable, p, home)
	case "linux":
		wanted = renderSystemdUnit(renderedExecutable, p, home)
	}
	switch {
	case readErr == nil && string(existing) == wanted:
		return false, nil
	case os.IsNotExist(readErr):
		return false, nil
	case readErr != nil && !os.IsNotExist(readErr):
		return false, fmt.Errorf("read managed service definition: %w", readErr)
	}
	restoreMode := os.FileMode(0o644)
	if info, err := os.Stat(installPath); err == nil {
		restoreMode = info.Mode().Perm()
	}
	restoreOnFailure := func(cause error) (bool, error) {
		if err := os.WriteFile(installPath, existing, restoreMode); err != nil {
			return false, fmt.Errorf("%w; restore managed service definition: %v", cause, err)
		}
		return false, cause
	}

	if _, err := installManagedServiceWithExecutable(p, renderedExecutable); err != nil {
		return restoreOnFailure(err)
	}
	if err := stopCurrentDaemonBeforeManagedRestart(p); err != nil {
		return restoreOnFailure(err)
	}
	if _, err := restartManagedService(p); err != nil {
		return restoreOnFailure(err)
	}
	if err := waitForDaemonStart(p, 0, time.Time{}); err != nil {
		return restoreOnFailure(err)
	}
	return true, nil
}

func stopCurrentDaemonBeforeManagedRestart(p *paths.Paths) error {
	if managed, err := stopManagedService(p); managed && err != nil {
		if alive, _ := daemonHealthCheck(p); !alive {
			return nil
		}
		if detachedErr := stopDetachedDaemon(p); detachedErr != nil {
			return fmt.Errorf("stop managed daemon before restart: %w; detached shutdown: %v", err, detachedErr)
		}
		return nil
	}
	if alive, _ := daemonHealthCheck(p); alive {
		if err := stopDetachedDaemon(p); err != nil {
			return fmt.Errorf("stop existing daemon before managed restart: %w", err)
		}
	}
	return nil
}

func stopManagedFallback(p *paths.Paths) error {
	managed, err := stopManagedService(p)
	if !managed {
		return nil
	}
	if err == nil {
		if runtimeGOOS == "darwin" {
			if err := removeLaunchAgent(p); err != nil {
				return fmt.Errorf("remove launch agent before detached fallback: %w", err)
			}
		}
		return nil
	}
	if alive, _ := daemonHealthCheck(p); alive {
		return fmt.Errorf("managed daemon is still running: %w", err)
	}
	return fmt.Errorf("stop managed daemon before detached fallback: %w", err)
}

func startDetachedDaemon(p *paths.Paths) error {
	cleanupDaemonArtifacts(p)

	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve executable: %w", err)
	}

	logFile, err := os.OpenFile(p.DaemonLog(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open daemon log: %w", err)
	}
	defer logFile.Close()

	cmd := exec.Command(exe)
	cmd.Env = upsertEnv(os.Environ(), "NM_HOME", p.Root())
	cmd.Env = upsertEnv(cmd.Env, "NM_DAEMON", "1")
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	// Detach from parent process group so daemon survives CLI exit.
	setSysProcAttr(cmd)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start daemon: %w", err)
	}

	pid := cmd.Process.Pid
	startedAt, err := daemonProcessStartTime(pid)
	if err != nil {
		if cleanupErr := cleanupStartedDaemonProcess(cmd.Process); cleanupErr != nil {
			return fmt.Errorf("inspect daemon process %d: %w; cleanup daemon child: %v", pid, err, cleanupErr)
		}
		return fmt.Errorf("inspect daemon process %d: %w", pid, err)
	}
	slog.Info("daemon process started", "pid", pid, "log", p.DaemonLog())

	if err := waitForDaemonStartWithProcess(p, cmd.Process, pid, startedAt); err != nil {
		return err
	}

	// Release the child so it's not reaped when we exit.
	if err := cmd.Process.Release(); err != nil {
		return fmt.Errorf("release daemon process: %w", err)
	}
	return nil
}

func startManagedDaemon(p *paths.Paths) error {
	if _, err := startManagedService(p); err != nil {
		if alive, _ := daemonHealthCheck(p); alive {
			return nil
		}
		return err
	}
	return waitForDaemonStart(p, 0, time.Time{})
}

func waitForDaemonStart(p *paths.Paths, pid int, startedAt time.Time) error {
	return waitForDaemonStartWithProcess(p, nil, pid, startedAt)
}

func waitForDaemonStartWithProcess(p *paths.Paths, proc *os.Process, pid int, startedAt time.Time) error {
	// Poll for the daemon to become responsive.
	timeout := daemonStartTimeout()
	pollInterval := daemonStartPollInterval()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if alive, _ := daemonHealthCheck(p); alive {
			slog.Info("daemon is responsive", "pid", pid)
			return nil
		}
		time.Sleep(pollInterval)
	}

	// Kill the child so it can't race with rollback work (e.g. SQLite writes)
	// after the caller gives up on it. Skip when pid is 0 (managed service).
	if pid > 0 {
		var cleanupErr error
		if proc != nil {
			cleanupErr = cleanupStartedDaemonProcess(proc)
		} else {
			cleanupErr = killTimedOutDaemonPID(pid, startedAt)
		}
		if cleanupErr != nil {
			return fmt.Errorf("daemon started but did not become responsive within %v: cleanup daemon child %d: %w", timeout, pid, cleanupErr)
		}
		if proc == nil && !startedAt.IsZero() {
			waitForProcessExit(pid, timeout)
		}
	}

	return fmt.Errorf("daemon started but did not become responsive within %v", timeout)
}

func cleanupStartedDaemonProcess(proc *os.Process) error {
	if proc == nil {
		return nil
	}
	if err := proc.Kill(); err != nil {
		if _, waitErr := proc.Wait(); waitErr == nil {
			return nil
		}
		return fmt.Errorf("kill daemon child: %w", err)
	}
	if _, err := proc.Wait(); err != nil {
		return fmt.Errorf("wait daemon child exit: %w", err)
	}
	return nil
}

func killTimedOutDaemonPID(pid int, startedAt time.Time) error {
	if pid <= 0 || startedAt.IsZero() {
		return nil
	}
	currentStartTime, err := daemonProcessStartTime(pid)
	if err != nil {
		return fmt.Errorf("inspect daemon pid %d before kill: %w", pid, err)
	}
	if !currentStartTime.Equal(startedAt) {
		return fmt.Errorf("daemon pid %d no longer matches original process", pid)
	}
	return daemonKillPID(pid)
}

func waitForProcessExit(pid int, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		running, err := daemonProcessRunning(pid)
		if err == nil && !running {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// IsRunning checks if the daemon is alive by sending a health check via IPC.
func IsRunning(p *paths.Paths) (bool, error) {
	return daemonHealthCheck(p)
}

func daemonIsRunningViaIPC(p *paths.Paths) (bool, error) {
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		return false, nil
	}
	defer client.Close()

	var result ipc.HealthResult
	if err := client.Call(ipc.MethodHealth, &ipc.HealthParams{}, &result); err != nil {
		return false, err
	}
	return result.Status == "ok", nil
}

// Stop sends a shutdown request to the running daemon and waits for it to exit.
func Stop(p *paths.Paths) error {
	if managed, err := stopManagedService(p); managed {
		if err != nil {
			if alive, _ := daemonHealthCheck(p); !alive {
				return nil
			}
			if detachedErr := stopDetachedDaemon(p); detachedErr != nil {
				return fmt.Errorf("%w; detached shutdown: %v", err, detachedErr)
			}
			return nil
		}
		return waitForDaemonStop(p)
	}
	return stopDetachedDaemon(p)
}

func stopDetachedDaemon(p *paths.Paths) error {
	client, err := daemonDial(p.Socket())
	if err != nil {
		stale, staleErr := staleDaemonArtifacts(p)
		if staleErr != nil {
			return staleErr
		}
		if stale {
			cleanupDaemonArtifacts(p)
			return nil
		}
		if killErr := stopDetachedDaemonByPID(p); killErr != nil {
			return fmt.Errorf("dial daemon: %w; pid fallback: %v", err, killErr)
		}
		return nil
	}
	defer client.Close()

	var result ipc.ShutdownResult
	if err := client.Call(ipc.MethodShutdown, &ipc.ShutdownParams{}, &result); err != nil {
		return fmt.Errorf("shutdown request: %w", err)
	}
	return waitForDaemonStop(p)
}

func stopDetachedDaemonByPID(p *paths.Paths) error {
	pid, err := ReadPID(p)
	if err != nil {
		return err
	}
	if err := validateDaemonPIDFallback(p, pid); err != nil {
		return err
	}
	if err := daemonKillPID(pid); err != nil {
		return fmt.Errorf("kill daemon pid %d: %w", pid, err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		running, err := daemonProcessRunning(pid)
		if err != nil {
			return err
		}
		if !running {
			cleanupDaemonArtifacts(p)
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}

	return fmt.Errorf("daemon pid %d still running after kill", pid)
}

func validateDaemonPIDFallback(p *paths.Paths, pid int) error {
	if pid <= 0 {
		return fmt.Errorf("invalid daemon pid %d", pid)
	}
	record, err := readDaemonPIDFile(p.PIDFile())
	if err != nil {
		return fmt.Errorf("read pid file: %w", err)
	}
	if record.PID != pid {
		return fmt.Errorf("daemon pid %d does not match pid file instance", pid)
	}
	startTime, err := daemonProcessStartTime(pid)
	if err != nil {
		return fmt.Errorf("inspect daemon pid %d: %w", pid, err)
	}
	matches, err := daemonPIDRecordMatchesProcess(p, record, startTime)
	if err != nil {
		return fmt.Errorf("validate daemon pid %d: %w", pid, err)
	}
	if !matches {
		return fmt.Errorf("daemon pid %d does not match pid file instance", pid)
	}
	return nil
}

func killPID(pid int) error {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return fmt.Errorf("find process: %w", err)
	}
	return proc.Kill()
}

func staleDaemonArtifacts(p *paths.Paths) (bool, error) {
	info, err := os.Stat(p.Socket())
	missingSocket := os.IsNotExist(err)
	if err != nil && !missingSocket {
		return false, fmt.Errorf("stat daemon socket: %w", err)
	}
	if err == nil && info.Mode()&os.ModeSocket == 0 && !daemonEndpointUsesRegularFile() {
		return true, nil
	}
	pid, err := ReadPID(p)
	if err != nil {
		if os.IsNotExist(err) {
			if missingSocket {
				return true, nil
			}
			if daemonEndpointUsesRegularFile() {
				return false, nil
			}
			alive, err := daemonSocketAcceptingConnections(p.Socket())
			if err != nil {
				return false, err
			}
			return !alive, nil
		}
		return false, err
	}
	running, err := daemonProcessRunning(pid)
	if err != nil {
		return false, err
	}
	if missingSocket && running {
		return false, nil
	}
	return !running, nil
}

func daemonSocketAcceptingConnections(path string) (bool, error) {
	conn, err := net.DialTimeout("unix", path, 200*time.Millisecond)
	if err != nil {
		return false, nil
	}
	defer conn.Close()
	return true, nil
}

func waitForDaemonStop(p *paths.Paths) error {
	// Wait for daemon to actually stop (socket becomes unavailable).
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		alive, err := daemonHealthCheck(p)
		if err == nil && !alive {
			cleanupDaemonArtifacts(p)
			slog.Info("daemon stopped gracefully")
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Try to kill by PID as last resort.
	if pid, err := ReadPID(p); err == nil {
		if err := validateDaemonPIDFallback(p, pid); err != nil {
			return err
		}
		slog.Warn("daemon did not stop gracefully, killing", "pid", pid)
		if err := daemonKillPID(pid); err != nil {
			return fmt.Errorf("kill daemon pid %d: %w", pid, err)
		}

		killDeadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(killDeadline) {
			running, err := daemonProcessRunning(pid)
			if err != nil {
				return err
			}
			if !running {
				cleanupDaemonArtifacts(p)
				slog.Warn("daemon killed after shutdown timeout", "pid", pid)
				return nil
			}
			time.Sleep(100 * time.Millisecond)
		}
		return fmt.Errorf("daemon pid %d still running after kill", pid)
	}

	return fmt.Errorf("daemon did not stop within timeout")
}

func cleanupDaemonArtifacts(p *paths.Paths) {
	_ = os.Remove(p.Socket())
	_ = os.Remove(p.PIDFile())
}

func upsertEnv(env []string, key, value string) []string {
	prefix := key + "="
	updated := false
	result := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			if !updated {
				result = append(result, prefix+value)
				updated = true
			}
			continue
		}
		result = append(result, entry)
	}
	if !updated {
		result = append(result, prefix+value)
	}
	return result
}

// EnsureDaemon starts the daemon if it's not already running.
func EnsureDaemon(p *paths.Paths) error {
	if alive, _ := daemonHealthCheck(p); alive {
		return nil
	}
	return Start(p)
}

// ReadPID reads the daemon PID from the PID file.
func ReadPID(p *paths.Paths) (int, error) {
	record, err := readDaemonPIDFile(p.PIDFile())
	if err != nil {
		return 0, err
	}
	return record.PID, nil
}

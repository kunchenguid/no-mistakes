package daemon

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

var daemonHealthCheck = daemonIsRunningViaIPC

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

// Start forks a new daemon process by re-executing the current binary
// with NM_DAEMON=1. It waits up to 5 seconds for the daemon to become
// responsive on the IPC socket.
func Start(p *paths.Paths) error {
	if err := p.EnsureDirs(); err != nil {
		return err
	}
	if alive, _ := daemonHealthCheck(p); alive {
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

func stopManagedFallback(p *paths.Paths) error {
	managed, err := stopManagedService(p)
	if !managed || err == nil {
		return nil
	}
	if alive, _ := daemonHealthCheck(p); alive {
		return fmt.Errorf("managed daemon is still running: %w", err)
	}
	return fmt.Errorf("stop managed daemon before detached fallback: %w", err)
}

func startDetachedDaemon(p *paths.Paths) error {
	// Clean up stale socket/pid files
	os.Remove(p.Socket())
	os.Remove(p.PIDFile())

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
	cmd.Env = append(os.Environ(), "NM_DAEMON=1")
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	// Detach from parent process group so daemon survives CLI exit.
	setSysProcAttr(cmd)

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start daemon: %w", err)
	}

	pid := cmd.Process.Pid
	slog.Info("daemon process started", "pid", pid, "log", p.DaemonLog())

	// Release the child so it's not reaped when we exit.
	if err := cmd.Process.Release(); err != nil {
		return fmt.Errorf("release daemon process: %w", err)
	}
	return waitForDaemonStart(p, pid)
}

func startManagedDaemon(p *paths.Paths) error {
	if _, err := startManagedService(p); err != nil {
		if alive, _ := daemonHealthCheck(p); alive {
			return nil
		}
		return err
	}
	return waitForDaemonStart(p, 0)
}

func waitForDaemonStart(p *paths.Paths, pid int) error {
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

	return fmt.Errorf("daemon started but did not become responsive within %v", timeout)
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
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		return nil
	}
	defer client.Close()

	var result ipc.ShutdownResult
	if err := client.Call(ipc.MethodShutdown, &ipc.ShutdownParams{}, &result); err != nil {
		return fmt.Errorf("shutdown request: %w", err)
	}
	return waitForDaemonStop(p)
}

func waitForDaemonStop(p *paths.Paths) error {
	// Wait for daemon to actually stop (socket becomes unavailable).
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if alive, _ := daemonHealthCheck(p); !alive {
			slog.Info("daemon stopped gracefully")
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Try to kill by PID as last resort.
	if pid, err := ReadPID(p); err == nil {
		if proc, err := os.FindProcess(pid); err == nil {
			slog.Warn("daemon did not stop gracefully, killing", "pid", pid)
			proc.Kill()
		}
	}

	return nil
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
	data, err := os.ReadFile(p.PIDFile())
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(string(data))
	if err != nil {
		return 0, fmt.Errorf("invalid pid file: %w", err)
	}
	return pid, nil
}

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

// Start forks a new daemon process by re-executing the current binary
// with NM_DAEMON=1. It waits up to 5 seconds for the daemon to become
// responsive on the IPC socket.
func Start(p *paths.Paths) error {
	if alive, _ := IsRunning(p); alive {
		return fmt.Errorf("daemon already running")
	}

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

	// Poll for the daemon to become responsive.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if alive, _ := IsRunning(p); alive {
			slog.Info("daemon is responsive", "pid", pid)
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}

	return fmt.Errorf("daemon started but did not become responsive within 5s")
}

// IsRunning checks if the daemon is alive by sending a health check via IPC.
func IsRunning(p *paths.Paths) (bool, error) {
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
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		return fmt.Errorf("daemon not running")
	}
	defer client.Close()

	var result ipc.ShutdownResult
	if err := client.Call(ipc.MethodShutdown, &ipc.ShutdownParams{}, &result); err != nil {
		return fmt.Errorf("shutdown request: %w", err)
	}

	// Wait for daemon to actually stop (socket becomes unavailable).
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if alive, _ := IsRunning(p); !alive {
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
	if alive, _ := IsRunning(p); alive {
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

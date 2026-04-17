package update

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

type daemonResetError struct {
	err           error
	daemonOffline bool
}

func (e *daemonResetError) Error() string {
	return e.err.Error()
}

func (e *daemonResetError) Unwrap() error {
	return e.err
}

func (u *updater) ensureDaemonUsesCurrentExecutable() error {
	if u == nil || u.paths == nil || u.executablePath == "" {
		return nil
	}
	alive, err := daemonIsRunning(u.paths)
	if err != nil || !alive {
		return nil
	}
	runningPath, err := daemonExecutablePath(u.paths)
	if err != nil || runningPath == "" {
		if err != nil {
			return fmt.Errorf("cannot determine daemon executable path: %w", err)
		}
		return errors.New("cannot determine daemon executable path")
	}
	currentPath := resolveExecutablePath(u.executablePath)
	runningPath = resolveExecutablePath(runningPath)
	if executablePathsMatch(currentPath, runningPath) {
		return nil
	}
	return fmt.Errorf("daemon is running from %s, but update is running from %s; run update using the same binary that started the daemon, or restart the daemon from this binary first", runningPath, currentPath)
}

func executablePathsMatch(a, b string) bool {
	if currentGOOS != "windows" {
		return a == b
	}
	a = filepath.Clean(strings.ReplaceAll(a, `\`, "/"))
	b = filepath.Clean(strings.ReplaceAll(b, `\`, "/"))
	return strings.EqualFold(a, b)
}

func defaultResetDaemon(p *paths.Paths) error {
	if p == nil {
		return nil
	}
	alive, err := daemonIsRunning(p)
	if err == nil && !alive && !daemonArtifactsExist(p) {
		return nil
	}
	if err := daemonStop(p); err != nil {
		return fmt.Errorf("stop daemon: %w", err)
	}
	if err := daemonStart(p); err != nil {
		running, checkErr := daemonIsRunning(p)
		offline := checkErr == nil && !running
		return &daemonResetError{err: fmt.Errorf("start daemon: %w", err), daemonOffline: offline}
	}
	return nil
}

func daemonArtifactsExist(p *paths.Paths) bool {
	for _, path := range []string{p.Socket(), p.PIDFile()} {
		if _, err := os.Stat(path); err == nil {
			return true
		}
	}
	return false
}

func runningDaemonExecutablePath(p *paths.Paths) (string, error) {
	if p == nil {
		return "", fmt.Errorf("resolve daemon executable: nil paths")
	}
	pid, err := daemon.ReadPID(p)
	if err != nil {
		return "", fmt.Errorf("resolve daemon executable: %w", err)
	}
	path, err := executablePathForPID(pid)
	if err != nil {
		return "", fmt.Errorf("resolve daemon executable: %w", err)
	}
	if path == "" {
		return "", fmt.Errorf("resolve daemon executable: empty process command")
	}
	return resolveExecutablePath(path), nil
}

func executablePathForPID(pid int) (string, error) {
	if currentGOOS == "linux" {
		return os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "exe"))
	}
	if currentGOOS == "windows" {
		return windowsExecutablePathForPID(pid)
	}
	cmd := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "comm=")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func resolveExecutablePath(path string) string {
	if path == "" {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return path
}

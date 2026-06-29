//go:build !windows

package daemon

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func setSysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

// listDaemonProcesses enumerates running processes via ps and returns the ones
// that look like `no-mistakes daemon run --root <root>`. It powers the
// pgrep-style collision detection in reconcileCollidingDaemons. `ps` is used
// (rather than /proc) so the same approach works on Linux and macOS.
func listDaemonProcesses() ([]daemonProcessInfo, error) {
	cmd := exec.Command(psExecutable(), "-ww", "-eo", "pid=,command=")
	env := upsertEnv(os.Environ(), "LC_ALL", "C")
	cmd.Env = upsertEnv(env, "LANG", "C")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("enumerate processes: %w", err)
	}
	return parseDaemonProcessOutput(string(out), splitUnixProcessLine), nil
}

// splitUnixProcessLine parses one line of `ps -eo pid=,command=` output into a
// pid and the trailing command line.
func splitUnixProcessLine(line string) (int, string, bool) {
	trimmed := strings.TrimLeft(line, " \t")
	if trimmed == "" {
		return 0, "", false
	}
	sep := strings.IndexAny(trimmed, " \t")
	if sep < 0 {
		return 0, "", false
	}
	pid, err := strconv.Atoi(trimmed[:sep])
	if err != nil || pid <= 0 {
		return 0, "", false
	}
	return pid, strings.TrimLeft(trimmed[sep:], " \t"), true
}

func processRunning(pid int) (bool, error) {
	err := syscall.Kill(pid, 0)
	if err == nil {
		state, err := processState(pid)
		if err != nil {
			return false, err
		}
		if strings.HasPrefix(state, "Z") {
			return false, nil
		}
		return true, nil
	}
	if errors.Is(err, syscall.ESRCH) {
		return false, nil
	}
	if errors.Is(err, syscall.EPERM) {
		return true, nil
	}
	return false, err
}

func processState(pid int) (string, error) {
	cmd := processStateCommand(pid)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func processStateCommand(pid int) *exec.Cmd {
	cmd := exec.Command(psExecutable(), "-p", fmt.Sprintf("%d", pid), "-o", "stat=")
	env := upsertEnv(os.Environ(), "LC_ALL", "C")
	cmd.Env = upsertEnv(env, "LANG", "C")
	return cmd
}

func processStartTime(pid int) (time.Time, error) {
	if pid <= 0 {
		return time.Time{}, fmt.Errorf("invalid pid %d", pid)
	}
	cmd := processStartTimeCommand(pid)
	out, err := cmd.Output()
	if err != nil {
		return time.Time{}, err
	}
	startedAt := strings.TrimSpace(string(out))
	if startedAt == "" {
		return time.Time{}, fmt.Errorf("missing process start time")
	}
	parsed, err := parseProcessStartTime(startedAt, time.Local)
	if err != nil {
		return time.Time{}, err
	}
	return parsed, nil
}

func processStartTimeCommand(pid int) *exec.Cmd {
	cmd := exec.Command(psExecutable(), "-p", fmt.Sprintf("%d", pid), "-o", "lstart=")
	env := upsertEnv(os.Environ(), "LC_ALL", "C")
	cmd.Env = upsertEnv(env, "LANG", "C")
	return cmd
}

func psExecutable() string {
	if path, err := exec.LookPath("ps"); err == nil {
		return path
	}
	if _, err := os.Stat("/bin/ps"); err == nil {
		return "/bin/ps"
	}
	return "ps"
}

func parseProcessStartTime(value string, loc *time.Location) (time.Time, error) {
	if loc == nil {
		loc = time.Local
	}
	return time.ParseInLocation("Mon Jan 2 15:04:05 2006", value, loc)
}

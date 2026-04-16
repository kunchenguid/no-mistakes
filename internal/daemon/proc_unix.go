//go:build !windows

package daemon

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

func setSysProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
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

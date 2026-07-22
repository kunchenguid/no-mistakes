//go:build unix

package e2edaemon

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

func processCommandLine(pid int) (string, error) {
	if pid <= 0 {
		return "", fmt.Errorf("invalid pid")
	}
	cmd := exec.Command(psPath(), "-p", strconv.Itoa(pid), "-ww", "-o", "command=")
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func psPath() string {
	if p, err := exec.LookPath("ps"); err == nil {
		return p
	}
	if _, err := os.Stat("/bin/ps"); err == nil {
		return "/bin/ps"
	}
	return "ps"
}

func FindDaemonsForRoot(nmHome string) ([]int, error) {
	cmd := exec.Command(psPath(), "-ww", "-eo", "pid=,command=")
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("enumerate processes: %w", err)
	}
	var pids []int
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		sep := strings.IndexAny(line, " \t")
		if sep < 0 {
			continue
		}
		pid, err := strconv.Atoi(line[:sep])
		if err != nil || pid <= 0 {
			continue
		}
		if commandMatchesDaemonRoot(strings.TrimSpace(line[sep:]), nmHome) {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

func processAliveOS(pid int) (bool, error) {
	err := syscall.Kill(pid, 0)
	if err == nil {
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

func signalProcessOS(pid int, sig os.Signal) error {
	s, ok := sig.(syscall.Signal)
	if !ok {
		s = syscall.SIGTERM
	}
	return syscall.Kill(pid, s)
}

func terminateDaemonPID(pid int) error {
	_ = signalProcessOS(pid, syscall.SIGTERM)
	if waitProcessExit(pid, 2*time.Second) {
		return nil
	}
	_ = signalProcessOS(pid, syscall.SIGKILL)
	if waitProcessExit(pid, 2*time.Second) {
		return nil
	}
	return fmt.Errorf("pid %d still alive after SIGKILL", pid)
}

//go:build !windows

package main

import (
	"bufio"
	"errors"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestTerminateCmdStopsProcessGroup(t *testing.T) {
	t.Helper()

	cmd := exec.Command("sh", "-c", "sleep 30 & echo $! && wait")
	cmd.SysProcAttr = newProcAttr()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start process group: %v", err)
	}

	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
		t.Fatalf("read child pid: %v", err)
	}
	childPID, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_, _ = cmd.Process.Wait()
		t.Fatalf("parse child pid %q: %v", line, err)
	}

	if err := terminateCmd(cmd, 500*time.Millisecond); err != nil {
		t.Fatalf("terminateCmd: %v", err)
	}
	if waitForProcessExit(childPID, time.Second) {
		t.Fatalf("child process %d still running after group termination", childPID)
	}
}

func waitForProcessExit(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		running, err := processRunning(pid)
		if err != nil {
			return true
		}
		if !running {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
	running, err := processRunning(pid)
	return err != nil || running
}

func processRunning(pid int) (bool, error) {
	err := syscall.Kill(pid, 0)
	if err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return false, nil
		}
		return false, err
	}
	cmd := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid))
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return false, nil
		}
		return false, err
	}
	state := strings.TrimSpace(string(out))
	if state == "" || strings.HasPrefix(state, "Z") {
		return false, nil
	}
	return true, nil
}

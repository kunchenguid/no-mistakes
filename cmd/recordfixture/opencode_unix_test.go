//go:build !windows

package main

import (
	"bufio"
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
	if processExists(childPID) {
		t.Fatalf("child process %d still running after group termination", childPID)
	}
}

func processExists(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

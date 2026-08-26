//go:build unix

package e2edaemon

import (
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func detachTestProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}

func TestProcessAliveTreatsExitedZombieAsDead(t *testing.T) {
	cmd := exec.Command("sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatalf("start child: %v", err)
	}
	defer func() { _ = cmd.Wait() }()

	deadline := time.Now().Add(2 * time.Second)
	for {
		out, err := exec.Command(psPath(), "-p", stringPID(cmd.Process.Pid), "-o", "stat=").Output()
		if err == nil && strings.HasPrefix(strings.TrimSpace(string(out)), "Z") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("child pid %d did not become a zombie", cmd.Process.Pid)
		}
		time.Sleep(10 * time.Millisecond)
	}

	alive, err := ProcessAlive(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("ProcessAlive(%d): %v", cmd.Process.Pid, err)
	}
	if alive {
		t.Fatalf("ProcessAlive(%d) = true for an exited zombie, want false", cmd.Process.Pid)
	}
}

func stringPID(pid int) string {
	return strconv.Itoa(pid)
}

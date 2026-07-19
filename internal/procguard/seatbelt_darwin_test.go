//go:build darwin

package procguard

import (
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestSeatbeltSignalProfile_BlocksCrossProcessSignal is the evidence for the
// documented OS-native hardening layer. It proves that a process launched under
// the Seatbelt profile cannot signal an out-of-scope process even via an
// absolute-path /bin/kill - closing the residual the PATH-interposition guard
// cannot (builtin kill, /bin/kill, raw syscalls). It is the kernel-enforced
// upper bound of the boundary this package documents.
func TestSeatbeltSignalProfile_BlocksCrossProcessSignal(t *testing.T) {
	if _, err := exec.LookPath("sandbox-exec"); err != nil {
		t.Skip("sandbox-exec not available")
	}

	// Out-of-scope sentinel: a plain process, NOT launched under the sandbox. A
	// background Wait reaps it so a killed-but-unreaped zombie cannot masquerade
	// as alive under kill -0.
	sentinel := exec.Command("sleep", "60")
	if err := sentinel.Start(); err != nil {
		t.Fatalf("start sentinel: %v", err)
	}
	pid := sentinel.Process.Pid
	reaped := make(chan struct{})
	go func() { _, _ = sentinel.Process.Wait(); close(reaped) }()
	defer func() { _ = sentinel.Process.Kill() }()

	isReaped := func() bool {
		select {
		case <-reaped:
			return true
		default:
			return false
		}
	}

	profile := SeatbeltSignalProfile()

	// Under the profile, /bin/kill of the out-of-scope sentinel is denied.
	out, err := exec.Command("sandbox-exec", "-p", profile, "/bin/kill", "-TERM", fmt.Sprintf("%d", pid)).CombinedOutput()
	if err == nil {
		t.Fatalf("sandboxed kill unexpectedly succeeded; sentinel signalable. output=%q", out)
	}
	t.Logf("sandboxed kill denied as expected: %s", strings.TrimSpace(string(out)))
	time.Sleep(150 * time.Millisecond)
	if isReaped() {
		t.Fatalf("sentinel died under the Seatbelt profile; signal was not blocked")
	}

	// Control: without the sandbox the same signal succeeds (the vulnerability).
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		t.Fatalf("unguarded kill failed: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if isReaped() {
			return // sentinel gone, as expected
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("sentinel survived an unguarded SIGTERM")
}

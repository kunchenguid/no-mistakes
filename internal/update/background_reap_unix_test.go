//go:build unix

package update

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// directChildren returns the pids of this process's direct children, whether
// they are still running or waiting for their exit status to be collected.
func directChildren(t *testing.T) map[int]bool {
	t.Helper()
	cmd := exec.Command("ps", "-eo", "pid=,ppid=")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("ps: %v", err)
	}
	self := os.Getpid()
	observer := cmd.Process.Pid
	children := map[int]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		pid, pidErr := strconv.Atoi(fields[0])
		ppid, ppidErr := strconv.Atoi(fields[1])
		if pidErr != nil || ppidErr != nil || ppid != self || pid == observer {
			continue
		}
		children[pid] = true
	}
	return children
}

// TestSpawnBackgroundLeavesNoUnreapedChild pins the process-lifecycle contract
// of the startup update check. The check is deliberately fire-and-forget, so
// the CLI must not block on it, but the CLI is still the child's only possible
// wait owner and must collect its exit status.
//
// Without that, every no-mistakes CLI process carries a permanent <defunct>
// child. A short-lived command hides it; a run-following `axi run` or
// `axi respond` lives for tens of minutes, so an operator triaging that process
// sees a zero-CPU process whose only child is defunct and reads it as wedged.
func TestSpawnBackgroundLeavesNoUnreapedChild(t *testing.T) {
	preexisting := directChildren(t)

	// The child re-execs this test binary with the background flag, which the
	// test binary's flag parsing rejects, so it exits promptly. That is all
	// this test needs: an exited child whose exit status someone must collect.
	if err := defaultSpawnBackground("v9.9.9"); err != nil {
		t.Fatalf("defaultSpawnBackground: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	var remaining []int
	for {
		remaining = remaining[:0]
		for pid := range directChildren(t) {
			if !preexisting[pid] {
				remaining = append(remaining, pid)
			}
		}
		if len(remaining) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("background update-check child %v did not exit and get reaped", remaining)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

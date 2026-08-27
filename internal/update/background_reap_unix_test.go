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

// zombieChildren returns the pids of this process's direct children whose exit
// status nobody has collected yet. The kernel keeps such an entry alive until
// its parent waits on it, so a helper the CLI starts and never waits on stays
// attached to the CLI for the CLI's whole lifetime.
func zombieChildren(t *testing.T) map[int]bool {
	t.Helper()
	cmd := exec.Command("ps", "-eo", "pid=,ppid=,stat=")
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("ps: %v", err)
	}
	self := os.Getpid()
	zombies := map[int]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		pid, pidErr := strconv.Atoi(fields[0])
		ppid, ppidErr := strconv.Atoi(fields[1])
		if pidErr != nil || ppidErr != nil || ppid != self {
			continue
		}
		if strings.HasPrefix(fields[2], "Z") {
			zombies[pid] = true
		}
	}
	return zombies
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
	preexisting := zombieChildren(t)

	// The child re-execs this test binary with the background flag, which the
	// test binary's flag parsing rejects, so it exits promptly. That is all
	// this test needs: an exited child whose exit status someone must collect.
	if err := defaultSpawnBackground("v9.9.9"); err != nil {
		t.Fatalf("defaultSpawnBackground: %v", err)
	}

	// Give the child time to fork, exec and exit before judging the result, so
	// a clean read means "reaped" rather than "has not exited yet".
	time.Sleep(500 * time.Millisecond)

	deadline := time.Now().Add(5 * time.Second)
	var leaked []int
	for {
		leaked = leaked[:0]
		for pid := range zombieChildren(t) {
			if !preexisting[pid] {
				leaked = append(leaked, pid)
			}
		}
		if len(leaked) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("background update-check child %v was never reaped: it stays a defunct child of the CLI process for the CLI's whole lifetime", leaked)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

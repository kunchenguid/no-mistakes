//go:build unix

package procguard

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestMain runs the package tests, except when re-executed in "helper" mode: a
// helper process is a stand-in host process (an out-of-scope watcher or an
// in-scope worker) that carries a unique marker in its command line so `pkill
// -f <marker>` can match it, then blocks until it is signalled. We check the env
// before flag parsing so the marker can be an arbitrary positional arg.
func TestMain(m *testing.M) {
	if os.Getenv("NM_PROCGUARD_HELPER") != "" {
		// Block on timers (not select{}, which trips Go's deadlock detector when
		// no other goroutine is runnable). An unhandled SIGTERM/SIGKILL still
		// terminates us, which is how the guard/test reaps this stand-in.
		for {
			time.Sleep(time.Hour)
		}
	}
	os.Exit(m.Run())
}

// helper is a marked stand-in process plus a channel closed once it has been
// reaped, so tests can tell "still running" from "dead" without tripping over
// zombie children (a killed-but-unreaped child still answers kill -0).
type helper struct {
	cmd    *exec.Cmd
	pid    int
	marker string
	done   chan struct{}
}

// reaped reports whether the helper has fully exited and been reaped.
func (h *helper) reaped() bool {
	select {
	case <-h.done:
		return true
	default:
		return false
	}
}

// startHelper launches a marked helper process. When ownGroup is true it gets
// its own process group (an out-of-scope process, like the Firstmate watcher);
// otherwise it inherits the test's group (an in-scope run descendant). A single
// background Wait reaps it; t.Cleanup only signals it to exit.
func startHelper(t *testing.T, marker string, ownGroup bool) *helper {
	t.Helper()
	cmd := exec.Command(os.Args[0], marker)
	cmd.Env = append(os.Environ(), "NM_PROCGUARD_HELPER=1")
	if ownGroup {
		cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper %q: %v", marker, err)
	}
	h := &helper{cmd: cmd, pid: cmd.Process.Pid, marker: marker, done: make(chan struct{})}
	go func() {
		_, _ = cmd.Process.Wait()
		close(h.done)
	}()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	if !waitFor(t, func() bool { return !h.reaped() && markerVisible(marker) }) {
		t.Fatalf("helper %q did not become visible in ps", marker)
	}
	return h
}

func markerVisible(marker string) bool {
	procs, err := Snapshot()
	if err != nil {
		return false
	}
	for _, p := range procs {
		if strings.Contains(p.Args, marker) {
			return true
		}
	}
	return false
}

func waitFor(t *testing.T, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return cond()
}

// TestRun_BroadKillRefusedSentinelSurvives is the end-to-end regression for the
// incident. An out-of-scope watcher (fm-watch.sh-shaped) is alive; the guard
// must refuse `pkill -f <marker>` and leave it running, while the SAME broad
// intent executed WITHOUT the guard (a raw signal to the matched pid) does kill
// it - reproducing the observed cross-run damage deterministically.
func TestRun_BroadKillRefusedSentinelSurvives(t *testing.T) {
	sentinelMarker := fmt.Sprintf("NMPG_SENTINEL_fm-watch.sh_%d", os.Getpid())
	sentinel := startHelper(t, sentinelMarker, true /* own group => out of scope */)

	scopePGID, err := syscall.Getpgid(0)
	if err != nil {
		t.Fatalf("getpgid: %v", err)
	}
	sentPGID, err := syscall.Getpgid(sentinel.pid)
	if err != nil {
		t.Fatalf("getpgid sentinel: %v", err)
	}
	if sentPGID == scopePGID {
		t.Fatalf("sentinel pgid %d must differ from scope pgid %d", sentPGID, scopePGID)
	}

	// Precondition: this process CAN signal the out-of-scope sentinel (same uid).
	// That is exactly the ambient authority the guard has to contain.
	if sentinel.reaped() {
		t.Fatalf("sentinel should be alive before the test")
	}

	// The guard must refuse the broad kill and signal nothing.
	code := Run("pkill", []string{"-f", sentinelMarker})
	if code != exitOutOfScope {
		t.Fatalf("Run(pkill -f %s) = %d, want exitOutOfScope(%d)", sentinelMarker, code, exitOutOfScope)
	}
	time.Sleep(200 * time.Millisecond)
	if sentinel.reaped() {
		t.Fatalf("sentinel was killed despite being out of scope - guard failed")
	}

	// Reproduce the observed damage: the same target, signalled WITHOUT the
	// guard, dies. This is the behavior the guard prevents above.
	if err := syscall.Kill(sentinel.pid, syscall.SIGTERM); err != nil {
		t.Fatalf("raw kill of out-of-scope sentinel failed: %v", err)
	}
	if !waitFor(t, sentinel.reaped) {
		t.Fatalf("sentinel survived an unguarded SIGTERM; vulnerability not reproduced")
	}
}

// TestRun_ScopedCleanupStillWorks proves the guard still lets a run reap its own
// descendants: an in-scope worker sharing the run's process group is signalled
// and dies.
func TestRun_ScopedCleanupStillWorks(t *testing.T) {
	childMarker := fmt.Sprintf("NMPG_CHILD_worker_%d", os.Getpid())
	child := startHelper(t, childMarker, false /* inherit test group => in scope */)

	scopePGID, _ := syscall.Getpgid(0)
	childPGID, err := syscall.Getpgid(child.pid)
	if err != nil {
		t.Fatalf("getpgid child: %v", err)
	}
	if childPGID != scopePGID {
		t.Skipf("child pgid %d != scope pgid %d in this harness; scope precondition unmet", childPGID, scopePGID)
	}

	code := Run("pkill", []string{"-f", childMarker})
	if code != exitSignalled {
		t.Fatalf("Run(pkill -f %s) = %d, want exitSignalled(%d)", childMarker, code, exitSignalled)
	}
	if !waitFor(t, child.reaped) {
		t.Fatalf("in-scope worker was not reaped; scoped cleanup broke")
	}
}

func TestParsePS(t *testing.T) {
	// Mixed BSD/Linux-style output: leading spaces, multi-token args, and a
	// truncation-free args column.
	out := "" +
		"    1     1 /sbin/launchd\n" +
		" 5001  5000 node ./test-worker.js --run\n" +
		" 9000  9000 /bin/sh /Users/control/firstmate/bin/fm-watch.sh --daemon\n" +
		"garbage line without numbers\n"
	procs := parsePS(out)
	if len(procs) != 3 {
		t.Fatalf("parsed %d procs, want 3: %+v", len(procs), procs)
	}
	if procs[1].PID != 5001 || procs[1].PGID != 5000 || procs[1].Comm != "node" {
		t.Fatalf("proc[1] = %+v, want pid 5001 pgid 5000 comm node", procs[1])
	}
	if procs[1].Args != "node ./test-worker.js --run" {
		t.Fatalf("proc[1].Args = %q", procs[1].Args)
	}
	if procs[2].PID != 9000 || procs[2].Comm != "/bin/sh" {
		t.Fatalf("proc[2] = %+v", procs[2])
	}
}

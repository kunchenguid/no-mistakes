//go:build e2e

package e2e

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/e2edaemon"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// The daemon's exclusive OS lock on NM_HOME is released by the kernel only
// when the owning process actually dies, so "the daemon stopped" has to mean
// "the daemon process is gone" for the next start to be able to take the
// root. When it did not, `daemon restart` raced its own predecessor: the
// replacement child failed acquireSingletonLock and exited before readiness
// with status 1, surfacing as `nm daemon restart while running: exit status 1`
// in the user journey.
//
// Both tests observe the daemon immediately on command return, with no grace
// period. A grace period is exactly the flake tolerance that would stop these
// from discriminating: a stop that only becomes true a few milliseconds later
// is the defect, not a variant of correct behavior.

func TestDaemonStopLeavesNoDaemonProcessOwningTheRoot(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("nm init: %v\n%s", err, out)
	}
	stopped := daemonPIDForRoot(t, h)

	out, err := h.Run("daemon", "stop")
	if err != nil {
		t.Fatalf("nm daemon stop: %v\n%s", err, out)
	}
	if !strings.Contains(out, "daemon stopped") {
		t.Fatalf("daemon stop output should report stopped, got:\n%s", out)
	}

	if alive, err := e2edaemon.ProcessAlive(stopped); err != nil {
		t.Fatalf("probe stopped daemon pid %d: %v", stopped, err)
	} else if alive {
		t.Fatalf("daemon stop reported success while daemon pid %d was still running and still holding the %s lock", stopped, h.NMHome)
	}
	assertNoDaemonProcessesForRoot(t, h, "after daemon stop")
}

func TestDaemonRestartReplacesTheDaemonWithExactlyOneOwner(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("nm init: %v\n%s", err, out)
	}
	previous := daemonPIDForRoot(t, h)

	out, err := h.Run("daemon", "restart")
	if err != nil {
		t.Fatalf("nm daemon restart: %v\n%s", err, out)
	}
	if !strings.Contains(out, "daemon restarted") {
		t.Fatalf("daemon restart output should report restarted, got:\n%s", out)
	}

	current := daemonPIDForRoot(t, h)
	if current == previous {
		t.Fatalf("daemon restart left the original process %d running, want a replacement daemon", previous)
	}
	if alive, err := e2edaemon.ProcessAlive(previous); err != nil {
		t.Fatalf("probe replaced daemon pid %d: %v", previous, err)
	} else if alive {
		t.Fatalf("daemon restart reported success while replaced daemon pid %d was still running", previous)
	}

	owners := daemonProcessesForRoot(t, h, "after daemon restart")
	if len(owners) != 1 || owners[0] != current {
		t.Fatalf("daemon processes owning %s after restart = %v, want exactly the live daemon [%d]", h.NMHome, owners, current)
	}

	status, err := h.Run("daemon", "status")
	if err != nil {
		t.Fatalf("nm daemon status after restart: %v\n%s", err, status)
	}
	if !strings.Contains(status, "daemon running") {
		t.Fatalf("daemon status after restart should show running, got:\n%s", status)
	}
}

func daemonPIDForRoot(t *testing.T, h *Harness) int {
	t.Helper()
	pid, err := daemon.ReadPID(paths.WithRoot(h.NMHome))
	if err != nil {
		t.Fatalf("read daemon pid file: %v", err)
	}
	if pid <= 0 {
		t.Fatalf("daemon pid = %d, want a positive daemon pid", pid)
	}
	return pid
}

func daemonProcessesForRoot(t *testing.T, h *Harness, when string) []int {
	t.Helper()
	pids, err := e2edaemon.FindDaemonsForRoot(h.NMHome)
	if err != nil {
		t.Fatalf("enumerate daemon processes %s: %v", when, err)
	}
	return pids
}

func assertNoDaemonProcessesForRoot(t *testing.T, h *Harness, when string) {
	t.Helper()
	if pids := daemonProcessesForRoot(t, h, when); len(pids) != 0 {
		t.Fatalf("daemon processes still owning %s %s: %v, want none", h.NMHome, when, pids)
	}
}

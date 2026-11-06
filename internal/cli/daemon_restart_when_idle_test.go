package cli

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// Issue #1691: `daemon restart --when-idle` with runs active schedules the
// restart on the running daemon instead of refusing or stopping it.
func TestDaemonRestartWhenIdleSchedulesInsteadOfStopping(t *testing.T) {
	nmHome := makeSocketSafeTempDir(t)
	t.Setenv("NM_HOME", nmHome)
	createLifecycleGuardRuns(t, paths.WithRoot(nmHome))

	stopCalled, scheduled := false, false
	prevStop, prevRunning, prevSchedule := daemonStopFn, daemonIsRunningFn, daemonScheduleRestartFn
	daemonStopFn = func(*paths.Paths) error { stopCalled = true; return nil }
	daemonIsRunningFn = func(*paths.Paths) (bool, error) { return true, nil }
	daemonScheduleRestartFn = func(*paths.Paths) (*ipc.RestartStatusResult, error) {
		scheduled = true
		return &ipc.RestartStatusResult{Pending: true, WaitingRuns: 2}, nil
	}
	t.Cleanup(func() {
		daemonStopFn, daemonIsRunningFn, daemonScheduleRestartFn = prevStop, prevRunning, prevSchedule
	})

	out, err := executeCmd("daemon", "restart", "--when-idle")
	if err != nil {
		t.Fatalf("daemon restart --when-idle should schedule, got %v\noutput: %s", err, out)
	}
	if !scheduled || stopCalled {
		t.Fatalf("scheduled=%t stop=%t, want the restart scheduled and the daemon left running", scheduled, stopCalled)
	}
	if !strings.Contains(out, "the daemon restarts after 2 active runs finish") {
		t.Fatalf("output should say what the restart waits for, got %q", out)
	}
}

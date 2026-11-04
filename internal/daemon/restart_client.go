package daemon

import (
	"errors"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// ErrRestartWhenIdleUnsupported reports a running daemon from before
// schedule_restart existed: it can only be restarted immediately.
var ErrRestartWhenIdleUnsupported = errors.New("the running daemon is too old to wait for active runs before restarting")

// ErrDaemonNotReachable reports that nothing accepted a connection on the
// daemon socket, so there is no running daemon to protect.
var ErrDaemonNotReachable = errors.New("daemon is not reachable")

// ScheduleRestart asks the running daemon to restart through its executable
// file once no run is executing (see RunManager.ScheduleRestartWhenIdle).
func ScheduleRestart(p *paths.Paths) (*ipc.RestartStatusResult, error) {
	return callRestartMethod(p, ipc.MethodScheduleRestart)
}

// RestartStatus reads whether the running daemon has a restart scheduled.
func RestartStatus(p *paths.Paths) (*ipc.RestartStatusResult, error) {
	return callRestartMethod(p, ipc.MethodRestartStatus)
}

func callRestartMethod(p *paths.Paths, method string) (*ipc.RestartStatusResult, error) {
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		return nil, fmt.Errorf("connect to daemon: %w: %w", ErrDaemonNotReachable, err)
	}
	defer client.Close()
	var result ipc.RestartStatusResult
	if err := client.Call(method, &ipc.ScheduleRestartParams{}, &result); err != nil {
		var rpcErr *ipc.RPCError
		if errors.As(err, &rpcErr) && rpcErr.Code == ipc.ErrMethodNotFound {
			return nil, ErrRestartWhenIdleUnsupported
		}
		return nil, err
	}
	return &result, nil
}

// DescribeScheduledRestart words what a scheduled restart waits for.
func DescribeScheduledRestart(status *ipc.RestartStatusResult) string {
	if status == nil || status.WaitingRuns == 0 {
		return "the daemon is restarting now: no run is executing"
	}
	runs := "runs finish"
	if status.WaitingRuns == 1 {
		runs = "run finishes"
	}
	return fmt.Sprintf("the daemon restarts after %d active %s; new pushes wait and then run on the restarted daemon", status.WaitingRuns, runs)
}

// DescribePendingRestart words a pending restart for `daemon status` and
// `doctor`, so a daemon holding work never looks idle.
func DescribePendingRestart(status *ipc.RestartStatusResult) string {
	return fmt.Sprintf("restart pending for an update: waiting for %d active %s; %d %s held", status.WaitingRuns, plural(status.WaitingRuns, "run", "runs"), status.HeldPushes, plural(status.HeldPushes, "push", "pushes"))
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

package update

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/lifecycle"
)

func (u *updater) confirmActiveRunsBeforeUpdate() error {
	runs, err := lifecycle.ActiveRuns(u.paths)
	if err != nil {
		return fmt.Errorf("check active pipeline runs: %w", err)
	}

	u.runsWereActive = len(runs) > 0

	// --when-idle schedules the restart whenever the running daemon can wait,
	// even with no run active now: a run started during the download must
	// not be stopped by an immediate restart either.
	if u.whenIdle && !u.force {
		waitErr := u.daemonCanRestartWhenIdle()
		if waitErr == nil {
			u.restartWhenIdle = true
			if len(runs) > 0 {
				fmt.Fprintf(u.stderrWriter(), "%d active pipeline %s will finish on the current version; the daemon restarts on the new version once no run is executing\n", len(runs), runNoun(len(runs)))
				fmt.Fprint(u.stderrWriter(), lifecycle.RunList(runs))
			}
			return nil
		}
		if len(runs) > 0 {
			u.writeActiveRunWarning(runs)
			return fmt.Errorf("refusing update because %d active pipeline runs are in progress and %v; pass --force to stop/restart the daemon anyway", len(runs), waitErr)
		}
		// No run is active. Only a daemon that is absent or too old falls
		// back to the immediate restart; any other probe failure leaves a
		// daemon that may still be running, so refuse before replacing it.
		var probeErr *probeFailedError
		if errors.As(waitErr, &probeErr) {
			return fmt.Errorf("refusing update because %v; retry, or pass --force to stop/restart the daemon anyway", waitErr)
		}
	}
	if len(runs) == 0 {
		return nil
	}

	u.writeActiveRunWarning(runs)
	if u.force {
		fmt.Fprintln(u.stderrWriter(), "FORCE: continuing update and daemon restart despite active pipeline runs")
		return nil
	}

	return fmt.Errorf("refusing update because %d active pipeline runs are in progress; pass --force to stop/restart the daemon anyway", len(runs))
}

// daemonCanRestartWhenIdle reports why the running daemon cannot wait for
// active runs before restarting, or nil when it can.
func (u *updater) daemonCanRestartWhenIdle() error {
	if u.restartStatus == nil || u.scheduleRestart == nil {
		return errors.New("the running daemon cannot wait for them")
	}
	if _, err := u.restartStatus(); err != nil {
		if errors.Is(err, daemon.ErrRestartWhenIdleUnsupported) {
			return errors.New("the running daemon is too old to wait for them")
		}
		// A refused connection proves nothing on its own: a running daemon
		// that is slow to accept must not be restarted at once.
		if errors.Is(err, daemon.ErrDaemonNotReachable) {
			if alive, aliveErr := daemonIsRunning(u.paths); aliveErr == nil && !alive {
				return errors.New("no daemon is running")
			}
		}
		return &probeFailedError{err}
	}
	return nil
}

// probeFailedError marks a restart_status failure that is neither a missing
// nor an unsupported daemon.
type probeFailedError struct{ err error }

func (e *probeFailedError) Error() string {
	return fmt.Sprintf("the running daemon could not be asked to wait for runs: %v", e.err)
}

func (e *probeFailedError) Unwrap() error { return e.err }

func runNoun(n int) string {
	if n == 1 {
		return "run"
	}
	return "runs"
}

func (u *updater) writeActiveRunWarning(runs []*db.Run) {
	runWord := "runs"
	if len(runs) == 1 {
		runWord = "run"
	}
	fmt.Fprintf(u.stderrWriter(), "warning: update will restart the daemon while %d active pipeline %s are in progress\n", len(runs), runWord)
	fmt.Fprint(u.stderrWriter(), lifecycle.RunList(runs))
	fmt.Fprintln(u.stderrWriter(), "continuing can cause these pipelines to fail")
}

func readYes(input io.Reader) bool {
	if input == nil {
		input = os.Stdin
	}
	response, err := bufio.NewReader(input).ReadString('\n')
	if err != nil && response == "" {
		return false
	}
	answer := strings.ToLower(strings.TrimSpace(response))
	return answer == "y" || answer == "yes"
}

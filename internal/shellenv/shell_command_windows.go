//go:build windows

package shellenv

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"
)

// CREATE_NEW_PROCESS_GROUP is the Windows creation flag that makes the child
// the root of a new process group, mirroring the unix Setpgid behavior. It is
// the foundation for whole-tree cancellation: taskkill /T walks the tree rooted
// at this process.
const createNewProcessGroup = 0x00000200

// taskkillExitNoSuchProcess is the nonzero exit code taskkill returns when no
// process matches the given PID (the child had already exited before we could
// kill it). All other nonzero codes — 1 for access denied, malformed arguments,
// or a transient process-table error — are genuine kill failures that must not
// be collapsed into os.ErrProcessDone.
const taskkillExitNoSuchProcess = 128

// defaultWaitDelay is the pipe backstop installed on Windows. taskkill /T /F
// usually reaches every descendant, but when it cannot (access denied on a
// child, or a handle inherited and still held open) a nonzero WaitDelay lets
// the exec package close inherited stdout/stderr pipes and return from Wait
// instead of blocking forever on a Read held open by a surviving grandchild.
// It is a worst-case ceiling only: in the common (successful) case the pipes
// close immediately and Wait returns without waiting.
const defaultWaitDelay = 5 * time.Second

// ConfigureShellCommand is the Windows counterpart to the unix helper. There
// are no Unix-style process groups or signals here, so cancellation runs
// `taskkill /T /F /PID`, which terminates the process and everything it
// spawned (test runners, agent-spawned git/build tools). Without this,
// exec.CommandContext only TerminateProcess-es the direct child and leaks the
// grandchildren, keeping the worktree locked.
//
// The cancel path distinguishes an already-gone PID (os.ErrProcessDone) from a
// real taskkill failure. On a real failure — or if taskkill itself is missing —
// it falls back to killing at least the direct child and surfaces the original
// error rather than swallowing it. A nonzero WaitDelay is installed as a pipe
// backstop. Together these keep Wait/pipe reads from hanging when a descendant
// survives a failed tree kill, which the previous "every nonzero exit is
// ErrProcessDone" mapping could cause.
func ConfigureShellCommand(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= createNewProcessGroup

	// Install a WaitDelay backstop unless the caller has chosen one
	// explicitly (the short login-shell probe, for example, uses a tighter
	// bound of its own).
	if cmd.WaitDelay == 0 {
		cmd.WaitDelay = defaultWaitDelay
	}

	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		pid := strconv.Itoa(cmd.Process.Pid)
		kill := exec.Command("taskkill", "/T", "/F", "/PID", pid)
		err := kill.Run()
		switch {
		case err == nil:
			// Whole tree terminated.
			return nil
		case errors.Is(err, exec.ErrNotFound):
			// taskkill itself is unavailable (PATH stripped, a stripped
			// Windows image, etc.). Cannot tree-kill; fall through to the
			// direct-child backstop below so the direct child is at least
			// terminated and Wait can make progress.
		case isTaskkillAlreadyGone(err):
			// The child had already exited; the benign, locale-independent
			// signal (taskkill exit code 128).
			return os.ErrProcessDone
		default:
			// Real taskkill failure (access denied, malformed invocation,
			// transient process-table error). Previously this branch was
			// unreachable: every nonzero exit was mapped to
			// os.ErrProcessDone, which Go reads as "already exited" and so
			// the exec package skipped even the default direct-child
			// TerminateProcess. With no WaitDelay backstop that could
			// leave Wait/pipe reads hung if a descendant still held a
			// handle. Fall through to the direct-child backstop, then
			// surface the real taskkill error instead of swallowing it.
		}
		// Backstop: at least terminate the direct child so the exec
		// package's Wait can make progress. If even that reports the
		// process already done, honor it; otherwise surface the original
		// taskkill failure so a genuine kill error is not silently lost.
		if killErr := cmd.Process.Kill(); killErr != nil {
			if errors.Is(killErr, os.ErrProcessDone) {
				return os.ErrProcessDone
			}
			return fmt.Errorf("taskkill /PID %s: %w; process kill: %v", pid, err, killErr)
		}
		if err != nil {
			return fmt.Errorf("taskkill /PID %s: %w", pid, err)
		}
		return nil
	}
}

// TerminateShellCommandGroup is the Windows counterpart of the unix helper: it
// best-effort tree-kills the process and everything it spawned via
// `taskkill /T /F /PID`, reaping grandchildren (a test runner's worker pool, a
// build watcher) that outlived a normally-exited leader. cmd.Cancel already
// covers cancellation; callers defer this so the success/failure paths are
// covered too. Errors (including an already-gone PID) are intentionally
// ignored - this is a post-Wait cleanup, not a control-flow operation. A nil or
// never-started command is a no-op.
func TerminateShellCommandGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pid := strconv.Itoa(cmd.Process.Pid)
	_ = exec.Command("taskkill", "/T", "/F", "/PID", pid).Run()
}

// isTaskkillAlreadyGone reports whether a taskkill error means the target PID
// no longer exists (the child had already exited). taskkill emits exit code
// taskkillExitNoSuchProcess for that case; matching on the numeric exit code
// keeps the detection locale-independent, since the accompanying stderr text
// ("...not found.") is locale-translated. All other nonzero codes are treated
// as genuine failures by the caller, which then falls back to a direct kill.
func isTaskkillAlreadyGone(err error) bool {
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return false
	}
	return exitErr.ExitCode() == taskkillExitNoSuchProcess
}

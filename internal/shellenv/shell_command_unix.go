//go:build unix

package shellenv

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// defaultWaitDelay bounds the time Wait spends after the process exits before
// force-closing stdout/stderr pipes that a surviving grandchild inherited and
// still holds open. Without it, a single leaked descendant (a test runner's
// worker, a build watcher) keeps a pipe Read blocked forever and wedges the
// step. It is a worst-case ceiling only: in the common case the pipes close
// immediately and Wait returns without waiting. Mirrors the Windows backstop.
const defaultWaitDelay = 5 * time.Second

// ConfigureShellCommand isolates cmd in its own process group (Setpgid) and
// installs a cmd.Cancel that SIGKILLs the whole group when cmd's context is
// cancelled. exec.CommandContext otherwise only kills the direct child PID,
// leaving grandchildren (a test runner's worker processes, an agent-spawned
// git/build/editor) running and holding the worktree locked.
//
// Cancellation is only half the story: cmd.Cancel never fires when the child
// exits on its own, so callers must also defer TerminateShellCommandGroup after
// Start to reap any grandchild that outlived a normally-exited leader.
//
// Apply this to every long-lived subprocess no-mistakes spawns on behalf of a
// cancellable step/agent invocation.
func ConfigureShellCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if cmd.WaitDelay == 0 {
		cmd.WaitDelay = defaultWaitDelay
	}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
}

// TerminateShellCommandGroup SIGKILLs every process still alive in cmd's
// process group. ConfigureShellCommand put cmd in its own group via Setpgid, so
// the group id equals the leader pid and the group persists as long as any
// member is alive - including grandchildren that outlived the leader and
// reparented to init (a vitest worker pool, a build watcher, a dev server).
//
// cmd.Cancel only reaps the group on context cancellation; this is the
// success-path counterpart. Call it via defer after Start so a cleanly-exited
// agent cannot leak orphaned grandchildren. It is a harmless no-op (ESRCH) when
// the group is already empty, e.g. after a cancellation kill already swept it.
func TerminateShellCommandGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

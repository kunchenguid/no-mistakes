//go:build unix

package shellenv

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// defaultWaitDelay is the pipe backstop installed on cmd.WaitDelay, mirroring
// the Windows helper. After a cancelled command's leader is signalled, a
// grandchild that inherited and still holds open the leader's stdout/stderr
// pipe would otherwise wedge cmd.Wait (and any in-flight pipe Read) forever.
// A nonzero WaitDelay lets the exec package close those inherited pipes and
// return instead of blocking. It is a worst-case ceiling only: on a clean exit
// the pipes close immediately and Wait returns without waiting.
const defaultWaitDelay = 5 * time.Second

// ConfigureShellCommand isolates cmd in its own process group (Setpgid) and
// installs a cmd.Cancel that SIGKILLs the whole group when cmd's context is
// cancelled. exec.CommandContext otherwise only kills the direct child PID,
// leaving grandchildren (a test runner's worker processes, an agent-spawned
// git/build/editor) running and holding the worktree locked.
//
// Cancellation is only half the lifecycle: cmd.Cancel never fires when the
// command exits on its own (success or failure). Use RunShellCommand,
// OutputShellCommand, or CombinedOutputShellCommand for one-shot commands, or
// use StartShellCommand and defer TerminateShellCommandGroup immediately after
// a successful start when the caller needs manual pipe handling. If a parser
// reads stdout/stderr until EOF, the goroutine that owns Wait should terminate
// the group when the leader exits so inherited pipe holders cannot wedge the
// parser.
//
// Apply this to every long-lived subprocess no-mistakes spawns on behalf of a
// cancellable step/agent invocation.
func ConfigureShellCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Install the WaitDelay backstop unless the caller picked one explicitly
	// (the short login-shell probe uses a tighter bound of its own).
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

// StartShellCommand starts cmd after ConfigureShellCommand has prepared its
// process-group lifecycle. Unix needs no extra setup beyond cmd.Start, but the
// wrapper keeps call sites aligned with Windows job-object setup.
func StartShellCommand(cmd *exec.Cmd) error {
	return cmd.Start()
}

// TerminateShellCommandGroup SIGKILLs the whole process group led by a command
// configured with ConfigureShellCommand. It is the success/failure-path
// counterpart to cmd.Cancel: callers defer it right after a successful Start so
// the group is reaped however Run returns - clean exit, parse error, or
// wait error - not only on context cancellation.
//
// Why this matters: Setpgid puts each agent/command in its own group, but a
// test runner's worker pool, a build watcher, or a dev server the agent spawned
// can outlive the leader. On a normal exit nothing signals the group, so those
// grandchildren reparent to init and keep running (and keep their memory). They
// accumulate across runs until the host is out of memory, at which point the OS
// OOM-killer reaps processes - including the daemon - with an uncatchable
// SIGKILL, surfacing as "daemon crashed during execution". Reaping the group on
// every exit path closes that leak so the test step can never take the daemon
// down.
//
// It is safe to call unconditionally after Wait: the group persists only while
// a member is alive, so when the leader exited cleanly with no survivors the
// kill is a harmless no-op (ESRCH). A nil or never-started command is a no-op.
func TerminateShellCommandGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	// Negative PID targets the whole group (Setpgid made the leader's PID the
	// group ID). errors.Is(ESRCH) is the expected, benign "no survivors" case.
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

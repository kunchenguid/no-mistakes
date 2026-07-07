//go:build windows

package shellenv

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/kunchenguid/no-mistakes/internal/winproc"
	"golang.org/x/sys/windows"
)

// createNewProcessGroup keeps cancellation fallback paths isolated from the
// parent console process group.
const createNewProcessGroup = 0x00000200

// taskkillExitNoSuchProcess is the nonzero exit code taskkill returns when no
// process matches the given PID (the child had already exited before we could
// kill it). All other nonzero codes are genuine kill failures that must not
// be collapsed into os.ErrProcessDone.
const taskkillExitNoSuchProcess = 128

// defaultWaitDelay bounds Wait when a failed cleanup leaves inherited handles
// open.
const defaultWaitDelay = 5 * time.Second

type shellCommandJobState struct {
	handle   windows.Handle
	assigned atomic.Bool
}

var shellCommandJobs sync.Map
var shellCommandJobSetupErrors sync.Map

var newShellCommandJobFunc = newShellCommandJob
var assignShellCommandJobFunc = assignShellCommandJob
var resumeProcessThreadsFunc = resumeProcessThreads

// ConfigureShellCommand prepares a Windows command for whole-tree cleanup on
// cancellation and normal exit. StartShellCommand assigns a kill-on-close job
// and fails if that guarantee is unavailable.
//
// Use RunShellCommand, OutputShellCommand, or CombinedOutputShellCommand for
// one-shot commands, or use StartShellCommand and defer
// TerminateShellCommandGroup immediately after a successful start when the
// caller needs manual pipe handling. If a parser reads stdout/stderr until EOF,
// the goroutine that owns Wait should terminate the group when the leader exits
// so inherited pipe holders cannot wedge the parser.
func ConfigureShellCommand(cmd *exec.Cmd) {
	// Suppress the visible console window Windows would otherwise allocate for
	// this console child (agents, cmd.exe shell steps) when spawned from the
	// console-less daemon. See issue #287. Harden allocates SysProcAttr if
	// needed; the process-group flag below is then OR-ed in alongside it.
	winproc.Harden(cmd)
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= createNewProcessGroup
	if job, err := newShellCommandJobFunc(); err == nil {
		shellCommandJobs.Store(cmd, &shellCommandJobState{handle: job})
		cmd.SysProcAttr.CreationFlags |= windows.CREATE_SUSPENDED
	} else {
		shellCommandJobSetupErrors.Store(cmd, err)
	}

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
		if terminateShellCommandJob(cmd, false) {
			return nil
		}
		pid := strconv.Itoa(cmd.Process.Pid)
		kill := exec.Command("taskkill", "/T", "/F", "/PID", pid)
		winproc.Harden(kill)
		err := kill.Run()
		switch {
		case err == nil:
			return nil
		case errors.Is(err, exec.ErrNotFound):
		case isTaskkillAlreadyGone(err):
			return os.ErrProcessDone
		default:
		}
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

// StartShellCommand starts cmd and assigns it to the job object created by
// ConfigureShellCommand. If the job cannot be created or assigned, the command
// fails instead of running without clean-exit descendant cleanup.
func StartShellCommand(cmd *exec.Cmd) error {
	if err, ok := takeShellCommandJobSetupError(cmd); ok {
		return fmt.Errorf("windows job object setup: %w", err)
	}
	if err := cmd.Start(); err != nil {
		closeShellCommandJob(cmd)
		return err
	}
	job, ok := shellCommandJob(cmd)
	if !ok {
		return nil
	}
	if err := assignShellCommandJobFunc(job.handle, uint32(cmd.Process.Pid)); err != nil {
		return failStartedShellCommand(cmd, fmt.Errorf("assign process to job object: %w", err))
	}
	job.assigned.Store(true)
	if err := resumeProcessThreadsFunc(uint32(cmd.Process.Pid)); err != nil {
		return failStartedShellCommand(cmd, err)
	}
	return nil
}

// TerminateShellCommandGroup terminates the Windows job object for cmd. Callers
// defer it after a successful StartShellCommand so clean exits and ordinary
// errors get the same process-tree cleanup as context cancellation. A nil or
// never-started command is a no-op.
func TerminateShellCommandGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	if terminateShellCommandJob(cmd, true) {
		return
	}
	pid := strconv.Itoa(cmd.Process.Pid)
	kill := exec.Command("taskkill", "/T", "/F", "/PID", pid)
	winproc.Harden(kill)
	_ = kill.Run()
}

func newShellCommandJob() (windows.Handle, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, err
	}
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	ret, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	)
	if ret == 0 {
		_ = windows.CloseHandle(job)
		return 0, err
	}
	return job, nil
}

func shellCommandJob(cmd *exec.Cmd) (*shellCommandJobState, bool) {
	if cmd == nil {
		return nil, false
	}
	value, ok := shellCommandJobs.Load(cmd)
	if !ok {
		return nil, false
	}
	job, ok := value.(*shellCommandJobState)
	return job, ok
}

func takeShellCommandJobSetupError(cmd *exec.Cmd) (error, bool) {
	if cmd == nil {
		return nil, false
	}
	value, ok := shellCommandJobSetupErrors.LoadAndDelete(cmd)
	if !ok {
		return nil, false
	}
	err, ok := value.(error)
	return err, ok
}

func closeShellCommandJob(cmd *exec.Cmd) bool {
	if cmd == nil {
		return false
	}
	value, ok := shellCommandJobs.LoadAndDelete(cmd)
	if !ok {
		return false
	}
	job, ok := value.(*shellCommandJobState)
	if !ok {
		return false
	}
	_ = windows.CloseHandle(job.handle)
	return true
}

func terminateShellCommandJob(cmd *exec.Cmd, closeJob bool) bool {
	job, ok := shellCommandJob(cmd)
	if !ok {
		return false
	}
	assigned := job.assigned.Load()
	if assigned {
		_ = windows.TerminateJobObject(job.handle, 1)
	}
	if closeJob {
		closeShellCommandJob(cmd)
	}
	return assigned
}

func assignShellCommandJob(job windows.Handle, pid uint32) error {
	process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, pid)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(process)
	return windows.AssignProcessToJobObject(job, process)
}

func resumeProcessThreads(pid uint32) error {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return err
	}
	defer windows.CloseHandle(snapshot)

	var entry windows.ThreadEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	if err := windows.Thread32First(snapshot, &entry); err != nil {
		if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
			return nil
		}
		return err
	}

	resumed := false
	for {
		if entry.OwnerProcessID == pid {
			thread, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
			if err == nil {
				_, err = windows.ResumeThread(thread)
				_ = windows.CloseHandle(thread)
				if err != nil {
					return err
				}
				resumed = true
			} else if !errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
				return err
			}
		}
		if err := windows.Thread32Next(snapshot, &entry); err != nil {
			if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
				break
			}
			return err
		}
	}
	if !resumed {
		return fmt.Errorf("no suspended threads found for pid %d", pid)
	}
	return nil
}

func failStartedShellCommand(cmd *exec.Cmd, cause error) error {
	terminateShellCommandJob(cmd, true)
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
	return cause
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

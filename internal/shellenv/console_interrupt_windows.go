//go:build windows

package shellenv

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const windowsCooperativeCommandArg = "--internal-windows-cooperative-command="

// allocConsoleModeNoWindow is ALLOC_CONSOLE_MODE_NO_WINDOW.
const allocConsoleModeNoWindow = 2

var (
	modKernel32                 = windows.NewLazySystemDLL("kernel32.dll")
	procGetConsoleProcessList   = modKernel32.NewProc("GetConsoleProcessList")
	procAllocConsoleWithOptions = modKernel32.NewProc("AllocConsoleWithOptions")
)

// allocConsoleOptions is ALLOC_CONSOLE_OPTIONS.
type allocConsoleOptions struct {
	mode          uint32
	useShowWindow int32
	showWindow    uint16
}

// RunWindowsCooperativeCommandHelper handles the private console process that
// launches a configured command in a targetable process group.
func RunWindowsCooperativeCommandHelper(args []string) (bool, int, error) {
	if len(args) == 0 || !strings.HasPrefix(args[0], windowsCooperativeCommandArg) {
		return false, 0, nil
	}
	if len(args) < 3 {
		return true, 1, errors.New("missing Windows cooperative command arguments")
	}

	eventName := strings.TrimPrefix(args[0], windowsCooperativeCommandArg)
	if eventName == "" {
		return true, 1, errors.New("empty Windows cooperative cancel event name")
	}
	exitCode, err := runWindowsCooperativeCommand(eventName, args[1], args[2:])
	return true, exitCode, err
}

func runWindowsCooperativeCommand(eventName, targetPath string, targetArgs []string) (int, error) {
	eventNamePtr, err := windows.UTF16PtrFromString(eventName)
	if err != nil {
		return 1, fmt.Errorf("encode Windows cooperative cancel event name: %w", err)
	}
	cancelEvent, err := windows.OpenEvent(windows.SYNCHRONIZE, false, eventNamePtr)
	if err != nil {
		return 1, fmt.Errorf("open Windows cooperative cancel event: %w", err)
	}
	defer windows.CloseHandle(cancelEvent)

	if err := ensureWindowsCooperativeConsole(); err != nil {
		return 1, err
	}

	// This is the narrow exception to the usual winproc.Harden rule. The target
	// must inherit this helper's private console for targeted CTRL+BREAK delivery;
	// Harden would add CREATE_NO_WINDOW and detach it from that console.
	// HideWindow suppresses a visible window without breaking console membership.
	target := &exec.Cmd{
		Path:   targetPath,
		Args:   append([]string(nil), targetArgs...),
		Stdin:  os.Stdin,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
		SysProcAttr: &syscall.SysProcAttr{
			CreationFlags: windows.CREATE_NEW_PROCESS_GROUP,
			HideWindow:    true,
		},
	}
	if err := target.Start(); err != nil {
		return 1, fmt.Errorf("start Windows cooperative command: %w", err)
	}

	targetHandle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(target.Process.Pid))
	if err != nil {
		return windowsCommandExit(target.Wait())
	}
	defer windows.CloseHandle(targetHandle)

	event, err := windows.WaitForMultipleObjects([]windows.Handle{cancelEvent, targetHandle}, false, windows.INFINITE)
	if err != nil {
		return 1, fmt.Errorf("wait for Windows cooperative command: %w", err)
	}
	if event == windows.WAIT_OBJECT_0 {
		if err := windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(target.Process.Pid)); err != nil {
			return 1, fmt.Errorf("send CTRL_BREAK to Windows command group: %w", err)
		}
		if _, err := windows.WaitForSingleObject(targetHandle, windows.INFINITE); err != nil {
			return 1, fmt.Errorf("wait for interrupted Windows cooperative command: %w", err)
		}
	}
	return windowsCommandExit(target.Wait())
}

// ensureWindowsCooperativeConsole gives the helper the private console its
// target must share. The shipped binary's consoleAllocationPolicy=detached
// also overrides the CREATE_NEW_CONSOLE this helper is started with, so on the
// builds that honor it (26100 and later) the helper starts without a console
// and allocates one without a window. AllocConsoleWithOptions exists on exactly
// those builds; older ones ignore the manifest and already gave it a console.
func ensureWindowsCooperativeConsole() error {
	var pids [1]uint32
	if n, _, _ := procGetConsoleProcessList.Call(uintptr(unsafe.Pointer(&pids[0])), uintptr(len(pids))); n != 0 {
		return nil
	}
	if procAllocConsoleWithOptions.Find() != nil {
		return nil
	}
	options := allocConsoleOptions{mode: allocConsoleModeNoWindow}
	var result uint32
	hr, _, _ := procAllocConsoleWithOptions.Call(uintptr(unsafe.Pointer(&options)), uintptr(unsafe.Pointer(&result)))
	if int32(hr) < 0 {
		return fmt.Errorf("allocate Windows cooperative console: HRESULT %#x", uint32(hr))
	}
	return nil
}

func windowsCommandExit(err error) (int, error) {
	if err == nil {
		return 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode(), nil
	}
	return 1, err
}

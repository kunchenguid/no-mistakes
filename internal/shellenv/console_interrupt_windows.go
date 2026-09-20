//go:build windows

package shellenv

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/winproc"
	"golang.org/x/sys/windows"
)

const windowsConsoleInterruptArg = "--internal-windows-console-interrupt="

var (
	kernel32          = windows.NewLazySystemDLL("kernel32.dll")
	procAttachConsole = kernel32.NewProc("AttachConsole")
	procFreeConsole   = kernel32.NewProc("FreeConsole")
)

// RunWindowsConsoleInterruptHelper handles the private subprocess mode used to
// deliver CTRL+BREAK to a command's console. The helper is necessary because a
// process can attach to only one console at a time; attaching the daemon itself
// would race every other command it starts. It must be called before ordinary
// CLI initialization.
func RunWindowsConsoleInterruptHelper(args []string) (bool, error) {
	if len(args) != 1 || !strings.HasPrefix(args[0], windowsConsoleInterruptArg) {
		return false, nil
	}
	pidText := strings.TrimPrefix(args[0], windowsConsoleInterruptArg)
	pid, err := strconv.ParseUint(pidText, 10, 32)
	if err != nil || pid == 0 {
		return true, fmt.Errorf("invalid Windows console process ID %q", pidText)
	}
	return true, generateWindowsConsoleInterrupt(uint32(pid))
}

func sendWindowsConsoleInterrupt(pid uint32) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, exe, windowsConsoleInterruptArg+strconv.FormatUint(uint64(pid), 10))
	winproc.Harden(cmd)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("send Windows console interrupt: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func generateWindowsConsoleInterrupt(pid uint32) error {
	// The helper is normally launched with CREATE_NO_WINDOW, but detaching first
	// also makes this safe when a test runner or unusual host supplied a console.
	procFreeConsole.Call()
	if r, _, err := procAttachConsole.Call(uintptr(pid)); r == 0 {
		return fmt.Errorf("AttachConsole(%d): %w", pid, err)
	}
	defer procFreeConsole.Call()

	// CTRL+BREAK is the Windows control event that can be limited to the process
	// group ConfigureShellCommand created. The sender is attached to the same
	// private console but is not part of that group, so it does not receive it.
	if err := windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, pid); err != nil {
		return fmt.Errorf("GenerateConsoleCtrlEvent: %w", err)
	}
	// Delivery is asynchronous. Keep the sender attached briefly so teardown of
	// its console attachment cannot race the target handlers starting.
	time.Sleep(100 * time.Millisecond)
	return nil
}

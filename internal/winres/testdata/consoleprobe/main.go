//go:build windows

// Command consoleprobe is a test helper for internal/winres. The test builds
// it with -ldflags=-H=windowsgui, so it is a GUI-subsystem process without a
// console, like Task Scheduler or Explorer starting no-mistakes.exe. It starts
// the command named by its arguments without console creation flags
// (CREATE_NEW_CONSOLE, CREATE_NO_WINDOW, DETACHED_PROCESS), waits for the
// child's "ready" line, and observes from outside whether Windows gave the
// child a console: AttachConsole succeeds only for a process that has one.
// It prints "console" or "no-console" and exits after the child does.
//
// Usage:
//
//	consoleprobe [-hide] <exe> [args...]
//
// The child must print "ready\n" on stdout and exit when its stdin closes.
// -hide asks Windows to start any console window hidden (STARTF_USESHOWWINDOW
// with SW_HIDE), which keeps a control run from flashing a window.
package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

var (
	kernel32      = windows.NewLazySystemDLL("kernel32.dll")
	attachConsole = kernel32.NewProc("AttachConsole")
	freeConsole   = kernel32.NewProc("FreeConsole")
)

func main() {
	result, err := probe()
	if err != nil {
		fmt.Fprintf(os.Stdout, "error: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stdout, result)
}

func probe() (string, error) {
	hide := flag.Bool("hide", false, "start a console window hidden")
	flag.Parse()
	if flag.NArg() == 0 {
		return "", errors.New("usage: consoleprobe [-hide] <exe> [args...]")
	}
	cmd := exec.Command(flag.Arg(0), flag.Args()[1:]...)
	if *hide {
		cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return "", err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", err
	}
	if err := cmd.Start(); err != nil {
		return "", err
	}
	defer func() {
		_ = stdin.Close()
		_ = cmd.Wait()
	}()
	line, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil || line != "ready\n" {
		return "", fmt.Errorf("child readiness: line %q, err %v", line, err)
	}

	// Windows allocates a console before the child's entry point runs, so
	// the answer is settled once the child reported ready.
	r, _, callErr := attachConsole.Call(uintptr(cmd.Process.Pid))
	if r != 0 {
		_, _, _ = freeConsole.Call()
		return "console", nil
	}
	if errors.Is(callErr, windows.ERROR_INVALID_HANDLE) {
		return "no-console", nil
	}
	return "", fmt.Errorf("AttachConsole(%d): %w", cmd.Process.Pid, callErr)
}

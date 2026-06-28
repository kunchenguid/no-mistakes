//go:build windows

package shellenv

import (
	"context"
	"errors"
	"os/exec"
	"strconv"
	"testing"

	"golang.org/x/sys/windows"
)

// TestIsTaskkillAlreadyGone pins down the locale-independent contract the
// Windows cancel path relies on: taskkill exit code 128 (no matching PID) is
// the only nonzero exit treated as "the child already exited", so every other
// nonzero code falls through to the direct-child-kill backstop instead of
// being swallowed as os.ErrProcessDone. It runs only on Windows; on Linux the
// windows build tag excludes it from `go test ./...`, while `GOOS=windows go
// vet` keeps it compile-checked.
func TestIsTaskkillAlreadyGone(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "exit 128 is already gone", err: exitCodeErr(t, 128), want: true},
		{name: "exit 1 access denied is a real failure", err: exitCodeErr(t, 1), want: false},
		{name: "exec.ErrNotFound is not already-gone", err: exec.ErrNotFound, want: false},
		{name: "wrapped exit 128 still detected", err: wrapErr(exitCodeErr(t, 128)), want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isTaskkillAlreadyGone(tt.err); got != tt.want {
				t.Fatalf("isTaskkillAlreadyGone(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestStartShellCommandFailsWhenJobSetupFails(t *testing.T) {
	setupErr := errors.New("job setup denied")
	oldNewJob := newShellCommandJobFunc
	newShellCommandJobFunc = func() (windows.Handle, error) {
		return 0, setupErr
	}
	t.Cleanup(func() {
		newShellCommandJobFunc = oldNewJob
	})

	cmd := exec.CommandContext(context.Background(), "cmd", "/c", "exit", "0")
	ConfigureShellCommand(cmd)
	if _, ok := shellCommandJob(cmd); ok {
		t.Fatal("expected no job state when job setup fails")
	}
	err := StartShellCommand(cmd)
	if !errors.Is(err, setupErr) {
		t.Fatalf("StartShellCommand() error = %v, want setup error", err)
	}
	if cmd.Process != nil {
		t.Fatal("expected command not to start after job setup failure")
	}
}

func TestStartShellCommandFailsWhenJobAssignmentFails(t *testing.T) {
	assignmentErr := errors.New("assignment denied")
	oldAssign := assignShellCommandJobFunc
	assignShellCommandJobFunc = func(windows.Handle, uint32) error {
		return assignmentErr
	}
	t.Cleanup(func() {
		assignShellCommandJobFunc = oldAssign
	})

	cmd := exec.CommandContext(context.Background(), "cmd", "/c", "exit", "0")
	ConfigureShellCommand(cmd)
	if _, ok := shellCommandJob(cmd); !ok {
		t.Skip("job object setup unavailable")
	}
	err := StartShellCommand(cmd)
	if !errors.Is(err, assignmentErr) {
		t.Fatalf("StartShellCommand() error = %v, want assignment error", err)
	}
	if _, ok := shellCommandJob(cmd); ok {
		t.Fatal("expected failed job state to be closed")
	}
	if cmd.Process == nil {
		t.Fatal("expected command to have started before assignment failure")
	}
	if cmd.ProcessState == nil || !cmd.ProcessState.Exited() {
		t.Fatal("expected failed start to kill and wait for the suspended process")
	}
}

// exitCodeErr runs `cmd /c exit N` and returns the resulting *exec.ExitError so
// the helper is exercised against a real ProcessState with the chosen code.
func exitCodeErr(t *testing.T, code int) error {
	t.Helper()
	err := exec.Command("cmd", "/c", "exit", strconv.Itoa(code)).Run()
	if err == nil {
		t.Fatalf("expected exit %d to yield a nonzero-run error", code)
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("expected *exec.ExitError, got %T: %v", err, err)
	}
	if exitErr.ExitCode() != code {
		t.Fatalf("exit code = %d, want %d", exitErr.ExitCode(), code)
	}
	return err
}

type wrappedErr struct{ e error }

func (w wrappedErr) Error() string { return "wrapped: " + w.e.Error() }
func (w wrappedErr) Unwrap() error { return w.e }

func wrapErr(e error) error { return wrappedErr{e: e} }

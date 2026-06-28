package shellenv

import (
	"bytes"
	"errors"
	"os/exec"
)

// RunShellCommand starts cmd with StartShellCommand, waits for it, and then
// terminates any surviving command-group descendants.
//
// Use this instead of cmd.Run after ConfigureShellCommand so clean exits and
// ordinary errors get the same process-tree cleanup as context cancellation.
func RunShellCommand(cmd *exec.Cmd) error {
	if err := StartShellCommand(cmd); err != nil {
		return err
	}
	defer TerminateShellCommandGroup(cmd)
	return cmd.Wait()
}

// OutputShellCommand is the process-tree-cleaning counterpart to cmd.Output.
// It requires cmd.Stdout to be unset, captures stdout, and delegates lifecycle
// cleanup to RunShellCommand.
func OutputShellCommand(cmd *exec.Cmd) ([]byte, error) {
	if cmd.Stdout != nil {
		return nil, errors.New("exec: Stdout already set")
	}
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	err := RunShellCommand(cmd)
	return stdout.Bytes(), err
}

// CombinedOutputShellCommand is the process-tree-cleaning counterpart to
// cmd.CombinedOutput. It requires cmd.Stdout and cmd.Stderr to be unset,
// captures both streams, and delegates lifecycle cleanup to RunShellCommand.
func CombinedOutputShellCommand(cmd *exec.Cmd) ([]byte, error) {
	if cmd.Stdout != nil {
		return nil, errors.New("exec: Stdout already set")
	}
	if cmd.Stderr != nil {
		return nil, errors.New("exec: Stderr already set")
	}
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	err := RunShellCommand(cmd)
	return output.Bytes(), err
}

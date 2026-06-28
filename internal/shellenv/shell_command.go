package shellenv

import (
	"bytes"
	"errors"
	"os/exec"
)

func RunShellCommand(cmd *exec.Cmd) error {
	if err := StartShellCommand(cmd); err != nil {
		return err
	}
	defer TerminateShellCommandGroup(cmd)
	return cmd.Wait()
}

func OutputShellCommand(cmd *exec.Cmd) ([]byte, error) {
	if cmd.Stdout != nil {
		return nil, errors.New("exec: Stdout already set")
	}
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	err := RunShellCommand(cmd)
	return stdout.Bytes(), err
}

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

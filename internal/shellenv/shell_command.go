package shellenv

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"reflect"
	"time"
)

type shellOutputPipe struct {
	reader *os.File
	writer *os.File
	dst    io.Writer
}

// RunShellCommand starts cmd with StartShellCommand, waits for it, and
// terminates any surviving command-group descendants.
//
// Use this instead of cmd.Run after ConfigureShellCommand so clean exits and
// ordinary errors get the same process-tree cleanup as context cancellation.
func RunShellCommand(cmd *exec.Cmd) error {
	pipes, err := prepareShellOutputPipes(cmd)
	if err != nil {
		return err
	}
	if len(pipes) > 0 {
		return runShellCommandWithOutputPipes(cmd, pipes)
	}
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

func prepareShellOutputPipes(cmd *exec.Cmd) ([]shellOutputPipe, error) {
	var pipes []shellOutputPipe
	stdout := cmd.Stdout
	stderr := cmd.Stderr

	if stdout != nil && !isShellOutputFile(stdout) {
		pipe, err := newShellOutputPipe(stdout)
		if err != nil {
			return nil, err
		}
		cmd.Stdout = pipe.writer
		pipes = append(pipes, pipe)
		if sameShellOutputWriter(stdout, stderr) {
			cmd.Stderr = pipe.writer
			return pipes, nil
		}
	}

	if stderr != nil && !isShellOutputFile(stderr) {
		pipe, err := newShellOutputPipe(stderr)
		if err != nil {
			closeShellOutputPipes(pipes)
			return nil, err
		}
		cmd.Stderr = pipe.writer
		pipes = append(pipes, pipe)
	}

	return pipes, nil
}

func newShellOutputPipe(dst io.Writer) (shellOutputPipe, error) {
	reader, writer, err := os.Pipe()
	if err != nil {
		return shellOutputPipe{}, err
	}
	return shellOutputPipe{reader: reader, writer: writer, dst: dst}, nil
}

func runShellCommandWithOutputPipes(cmd *exec.Cmd, pipes []shellOutputPipe) error {
	if err := StartShellCommand(cmd); err != nil {
		closeShellOutputPipes(pipes)
		return err
	}
	for _, pipe := range pipes {
		_ = pipe.writer.Close()
	}

	copyCh := make(chan error, len(pipes))
	for _, pipe := range pipes {
		go func(pipe shellOutputPipe) {
			_, err := io.Copy(pipe.dst, pipe.reader)
			_ = pipe.reader.Close()
			copyCh <- err
		}(pipe)
	}

	waitCh := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		TerminateShellCommandGroup(cmd)
		waitCh <- err
	}()

	var waitErr error
	waitDone := false
	remainingCopies := len(pipes)
	var copyErr error
	var waitDelay <-chan time.Time
	var waitDelayTimer *time.Timer
	waitDelayExpired := false
	for !waitDone || remainingCopies > 0 {
		select {
		case err := <-waitCh:
			waitErr = err
			waitDone = true
			if remainingCopies > 0 && cmd.WaitDelay > 0 {
				waitDelayTimer = time.NewTimer(cmd.WaitDelay)
				waitDelay = waitDelayTimer.C
			}
		case err := <-copyCh:
			remainingCopies--
			if err != nil && copyErr == nil {
				copyErr = err
				TerminateShellCommandGroup(cmd)
			}
		case <-waitDelay:
			waitDelayExpired = true
			waitDelay = nil
			closeShellOutputPipeReaders(pipes)
		}
	}
	if waitDelayTimer != nil {
		waitDelayTimer.Stop()
	}
	if waitErr != nil {
		return waitErr
	}
	if waitDelayExpired {
		return exec.ErrWaitDelay
	}
	return copyErr
}

func closeShellOutputPipes(pipes []shellOutputPipe) {
	for _, pipe := range pipes {
		_ = pipe.reader.Close()
		_ = pipe.writer.Close()
	}
}

func closeShellOutputPipeReaders(pipes []shellOutputPipe) {
	for _, pipe := range pipes {
		_ = pipe.reader.Close()
	}
}

func isShellOutputFile(w io.Writer) bool {
	_, ok := w.(*os.File)
	return ok
}

func sameShellOutputWriter(a, b io.Writer) bool {
	if a == nil || b == nil {
		return false
	}
	av := reflect.ValueOf(a)
	bv := reflect.ValueOf(b)
	if av.Type() != bv.Type() || !av.Type().Comparable() {
		return false
	}
	return av.Equal(bv)
}

package agent

import (
	"fmt"
	"os"
	"os/exec"
	"sync"

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

type nativeAgentCommand struct {
	cmd           *exec.Cmd
	stdout        *os.File
	stderr        *os.File
	waitCh        chan error
	terminateOnce sync.Once
}

func startNativeAgentCommand(cmd *exec.Cmd) (*nativeAgentCommand, error) {
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		_ = stdoutR.Close()
		_ = stdoutW.Close()
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}

	cmd.Stdout = stdoutW
	cmd.Stderr = stderrW
	if err := shellenv.StartShellCommand(cmd); err != nil {
		_ = stdoutR.Close()
		_ = stdoutW.Close()
		_ = stderrR.Close()
		_ = stderrW.Close()
		return nil, err
	}
	_ = stdoutW.Close()
	_ = stderrW.Close()

	started := &nativeAgentCommand{
		cmd:    cmd,
		stdout: stdoutR,
		stderr: stderrR,
		waitCh: make(chan error, 1),
	}
	go func() {
		err := cmd.Wait()
		started.terminate()
		started.waitCh <- err
	}()
	return started, nil
}

func (c *nativeAgentCommand) terminate() {
	c.terminateOnce.Do(func() {
		shellenv.TerminateShellCommandGroup(c.cmd)
	})
}

func (c *nativeAgentCommand) wait() error {
	return <-c.waitCh
}

func (c *nativeAgentCommand) closePipes() {
	_ = c.stdout.Close()
	_ = c.stderr.Close()
}

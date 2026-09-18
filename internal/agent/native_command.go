package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

type nativeAgentCommand struct {
	cmd            *exec.Cmd
	stdout         *nativeAgentPipe
	stderr         *nativeAgentPipe
	waitCh         chan error
	exited         chan struct{}
	terminateOnce  sync.Once
	closePipesOnce sync.Once
	pipeMu         sync.Mutex
	remainingPipes int
	pipesDone      chan struct{}
}

func writeNativeAgentStdin(stdin io.WriteCloser, prompt string) <-chan error {
	errCh := make(chan error, 1)
	go func() {
		_, writeErr := io.WriteString(stdin, prompt)
		closeErr := stdin.Close()
		errCh <- errors.Join(writeErr, closeErr)
	}()
	return errCh
}

type nativeAgentPipe struct {
	file     *os.File
	done     func()
	doneOnce sync.Once
	// activity, when set, is called for every non-empty read. It is the only
	// evidence no-mistakes has that a native agent is still alive during the
	// long tool-using stretches that produce no assistant prose.
	activity func()
}

func (p *nativeAgentPipe) Read(b []byte) (int, error) {
	n, err := p.file.Read(b)
	if n > 0 && p.activity != nil {
		p.activity()
	}
	if err != nil {
		p.markDone()
	}
	return n, err
}

func (p *nativeAgentPipe) Close() error {
	err := p.file.Close()
	p.markDone()
	return err
}

func (p *nativeAgentPipe) markDone() {
	p.doneOnce.Do(p.done)
}

// startNativeAgentCommand starts cmd with dedicated stdout/stderr pipes.
// activity, when non-nil, is invoked on every non-empty read from either pipe;
// see LifecyclePhaseActivity for why subprocess byte liveness - not assistant
// prose - is the signal that distinguishes a working agent from a wedged one.
//
// ctx cancellation must unblock a parser sitting in stdout/stderr Read even
// when the subprocess ignores SIGTERM. CommandContext kills the process group,
// but a wedged omp that keeps its stdout fd open leaves bufio.Scanner blocked
// in Read, so the stall watcher that cancelled ctx never returns. Closing the
// local pipe ends that Read with EOF/error and lets the invocation fail.
func startNativeAgentCommand(ctx context.Context, cmd *exec.Cmd, activity func()) (*nativeAgentCommand, error) {
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
		cmd:            cmd,
		waitCh:         make(chan error, 1),
		exited:         make(chan struct{}),
		remainingPipes: 2,
		pipesDone:      make(chan struct{}),
	}
	started.stdout = &nativeAgentPipe{file: stdoutR, done: started.markPipeDone, activity: activity}
	started.stderr = &nativeAgentPipe{file: stderrR, done: started.markPipeDone, activity: activity}
	go func() {
		err := cmd.Wait()
		started.terminate()
		started.waitCh <- started.waitForPipes(err)
		close(started.exited)
	}()
	if ctx != nil {
		go func() {
			select {
			case <-ctx.Done():
				started.terminate()
				started.closePipes()
			case <-started.exited:
			}
		}()
	}
	return started, nil
}

func (c *nativeAgentCommand) markPipeDone() {
	c.pipeMu.Lock()
	defer c.pipeMu.Unlock()
	c.remainingPipes--
	if c.remainingPipes == 0 {
		close(c.pipesDone)
	}
}

func (c *nativeAgentCommand) pid() int {
	if c == nil || c.cmd == nil || c.cmd.Process == nil {
		return 0
	}
	return c.cmd.Process.Pid
}

func (c *nativeAgentCommand) waitForPipes(waitErr error) error {
	if c.cmd.WaitDelay <= 0 {
		<-c.pipesDone
		return waitErr
	}
	timer := time.NewTimer(c.cmd.WaitDelay)
	defer timer.Stop()
	select {
	case <-c.pipesDone:
		return waitErr
	case <-timer.C:
		c.closePipes()
		if waitErr == nil {
			return exec.ErrWaitDelay
		}
		return waitErr
	}
}

func (c *nativeAgentCommand) terminate() {
	c.terminateOnce.Do(func() {
		shellenv.TerminateShellCommandGroup(c.cmd)
	})
}

func (c *nativeAgentCommand) waitAfterParseError(parseErr error) error {
	c.terminate()
	c.closePipes()
	waitErr := c.wait()
	if errors.Is(waitErr, exec.ErrWaitDelay) {
		return waitErr
	}
	return parseErr
}

func (c *nativeAgentCommand) wait() error {
	return <-c.waitCh
}

func (c *nativeAgentCommand) closePipes() {
	c.closePipesOnce.Do(func() {
		_ = c.stdout.Close()
		_ = c.stderr.Close()
	})
}

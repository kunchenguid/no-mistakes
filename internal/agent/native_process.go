package agent

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/shellenv"
)

const nativeProcessWaitDelay = 5 * time.Second

type nativeProcess struct {
	cmd            *exec.Cmd
	stdout         *nativeProcessPipe
	stderr         *nativeProcessPipe
	waitCh         chan error
	terminateOnce  sync.Once
	closePipesOnce sync.Once
	pipeMu         sync.Mutex
	remainingPipes int
	pipesDone      chan struct{}
}

func (p *nativeProcess) pid() int {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

type nativeProcessPipe struct {
	file     *os.File
	done     func()
	doneOnce sync.Once
}

func (p *nativeProcessPipe) Read(b []byte) (int, error) {
	n, err := p.file.Read(b)
	if err != nil {
		p.markDone()
	}
	return n, err
}

func (p *nativeProcessPipe) Close() error {
	err := p.file.Close()
	p.markDone()
	return err
}

func (p *nativeProcessPipe) markDone() {
	p.doneOnce.Do(p.done)
}

func startNativeProcess(cmd *exec.Cmd) (*nativeProcess, error) {
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
	shellenv.ConfigureShellCommand(cmd)
	if cmd.WaitDelay == 0 {
		cmd.WaitDelay = nativeProcessWaitDelay
	}
	if err := cmd.Start(); err != nil {
		_ = stdoutR.Close()
		_ = stdoutW.Close()
		_ = stderrR.Close()
		_ = stderrW.Close()
		return nil, err
	}
	_ = stdoutW.Close()
	_ = stderrW.Close()

	started := &nativeProcess{
		cmd:            cmd,
		waitCh:         make(chan error, 1),
		remainingPipes: 2,
		pipesDone:      make(chan struct{}),
	}
	started.stdout = &nativeProcessPipe{file: stdoutR, done: started.markPipeDone}
	started.stderr = &nativeProcessPipe{file: stderrR, done: started.markPipeDone}
	go func() {
		err := cmd.Wait()
		started.terminate()
		started.waitCh <- started.waitForPipes(err)
	}()
	return started, nil
}

func (p *nativeProcess) markPipeDone() {
	p.pipeMu.Lock()
	defer p.pipeMu.Unlock()
	p.remainingPipes--
	if p.remainingPipes == 0 {
		close(p.pipesDone)
	}
}

func (p *nativeProcess) waitForPipes(waitErr error) error {
	timer := time.NewTimer(p.cmd.WaitDelay)
	defer timer.Stop()
	select {
	case <-p.pipesDone:
		return waitErr
	case <-timer.C:
		p.closePipes()
		if waitErr == nil {
			return exec.ErrWaitDelay
		}
		return waitErr
	}
}

func (p *nativeProcess) terminate() {
	p.terminateOnce.Do(func() {
		if p.cmd.Cancel != nil {
			_ = p.cmd.Cancel()
		}
	})
}

func (p *nativeProcess) waitAfterParseError(parseErr error) error {
	p.terminate()
	p.closePipes()
	waitErr := p.wait()
	if errors.Is(waitErr, exec.ErrWaitDelay) {
		return waitErr
	}
	return parseErr
}

func (p *nativeProcess) wait() error {
	return <-p.waitCh
}

func (p *nativeProcess) closePipes() {
	p.closePipesOnce.Do(func() {
		_ = p.stdout.Close()
		_ = p.stderr.Close()
	})
}

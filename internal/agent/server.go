package agent

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"time"
)

// managedServerOutput holds the writer used for managed-server stdout and
// stderr. Defaults to os.Stderr so debug output is visible in normal CLI
// sessions; callers rendering to a raw terminal (e.g. the setup wizard
// alt-screen) should override it so log lines don't corrupt the display.
var (
	managedServerOutputMu sync.Mutex
	managedServerOutput   io.Writer = os.Stderr
)

// SetManagedServerOutput routes future managed-server stdout/stderr to w.
// Passing nil resets to the default (os.Stderr). Only affects servers
// started after this call; already-running servers keep their original fds.
func SetManagedServerOutput(w io.Writer) {
	managedServerOutputMu.Lock()
	defer managedServerOutputMu.Unlock()
	if w == nil {
		w = os.Stderr
	}
	managedServerOutput = w
}

func currentManagedServerOutput() io.Writer {
	managedServerOutputMu.Lock()
	defer managedServerOutputMu.Unlock()
	return managedServerOutput
}

// managedServer manages a persistent HTTP server process (used by rovodev and opencode agents).
type managedServer struct {
	cmd     *exec.Cmd
	port    int
	pidFile string        // path to the on-disk PID record; empty if tracking disabled
	exited  chan struct{} // closed exactly once when cmd.Wait returns
	waitErr error         // result of cmd.Wait; only read after exited is closed
}

// getAvailablePort finds an ephemeral port by binding to :0 and releasing.
func getAvailablePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, fmt.Errorf("allocate port: %w", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port, nil
}

// startServerWithPort spawns the server process on a given port and waits for health.
// The process is not tied to ctx - it outlives individual Run calls and is stopped via shutdown().
// ctx is only used for the health check timeout.
// agentName tags the PID tracking file so crash-recovery can identify orphans.
func startServerWithPort(ctx context.Context, agentName, bin string, args []string, cwd string, healthPath string, port int) (*managedServer, error) {
	cmd := exec.Command(bin, args...)
	cmd.Dir = cwd
	cmd.Stdin = nil
	out := currentManagedServerOutput()
	cmd.Stdout = out // server stdout goes to the configured sink for debugging
	cmd.Stderr = out
	configureManagedServerCmd(cmd)

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start server %s: %w", bin, err)
	}

	pidFile := writeServerPIDFile(currentServerPIDsDir(), ServerPIDInfo{
		PID:       cmd.Process.Pid,
		Agent:     agentName,
		Bin:       bin,
		Port:      port,
		StartedAt: time.Now().UTC(),
	})

	srv := &managedServer{cmd: cmd, port: port, pidFile: pidFile, exited: make(chan struct{})}
	go func() {
		srv.waitErr = cmd.Wait()
		close(srv.exited)
	}()

	// Wait for health check to pass
	if err := srv.waitForHealth(ctx, healthPath); err != nil {
		srv.shutdown()
		return nil, err
	}

	return srv, nil
}

// baseURL returns the server's base URL.
func (s *managedServer) baseURL() string {
	return fmt.Sprintf("http://127.0.0.1:%d", s.port)
}

// waitForHealth polls the health endpoint until it returns 200 or timeout.
// If the server process exits before becoming healthy, it returns immediately
// with an exit error instead of waiting out the 30s deadline.
func (s *managedServer) waitForHealth(ctx context.Context, path string) error {
	url := s.baseURL() + path
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.After(30 * time.Second)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.exited:
			return fmt.Errorf("server exited before becoming healthy: %w", s.waitErr)
		case <-deadline:
			return fmt.Errorf("server health check timed out after 30s")
		default:
		}

		resp, err := client.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}

		select {
		case <-s.exited:
			return fmt.Errorf("server exited before becoming healthy: %w", s.waitErr)
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// shutdown gracefully stops the server process. The long-running goroutine
// spawned in startServerWithPort owns cmd.Wait(); shutdown signals the
// process and waits on s.exited to observe termination.
// The PID tracking file is removed only after the process is confirmed
// exited - if SIGKILL fails to reap it, the file is left on disk so a
// future daemon can finish the job.
func (s *managedServer) shutdown() {
	if s.cmd == nil || s.cmd.Process == nil {
		removeServerPIDFile(s.pidFile)
		return
	}

	// Already exited (e.g. early-exit path)?
	select {
	case <-s.exited:
		removeServerPIDFile(s.pidFile)
		return
	default:
	}

	_ = signalManagedProcess(s.cmd, false)

	select {
	case <-s.exited:
		removeServerPIDFile(s.pidFile)
		return
	case <-time.After(3 * time.Second):
	}

	slog.Warn("server did not exit gracefully, sending SIGKILL", "pid", s.cmd.Process.Pid)
	_ = signalManagedProcess(s.cmd, true)

	select {
	case <-s.exited:
		removeServerPIDFile(s.pidFile)
	case <-time.After(5 * time.Second):
		slog.Warn("server process did not exit after SIGKILL", "pid", s.cmd.Process.Pid)
	}
}

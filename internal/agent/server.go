package agent

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"time"
)

// managedServer manages a persistent HTTP server process (used by rovodev and opencode agents).
type managedServer struct {
	cmd     *exec.Cmd
	port    int
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
func startServerWithPort(ctx context.Context, bin string, args []string, cwd string, healthPath string, port int) (*managedServer, error) {
	cmd := exec.Command(bin, args...)
	cmd.Dir = cwd
	cmd.Stdin = nil
	cmd.Stdout = os.Stderr // server stdout goes to our stderr for debugging
	cmd.Stderr = os.Stderr
	configureManagedServerCmd(cmd)

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start server %s: %w", bin, err)
	}

	srv := &managedServer{cmd: cmd, port: port, exited: make(chan struct{})}
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
func (s *managedServer) shutdown() {
	if s.cmd == nil || s.cmd.Process == nil {
		return
	}

	// Already exited (e.g. early-exit path)?
	select {
	case <-s.exited:
		return
	default:
	}

	_ = signalManagedProcess(s.cmd, false)

	select {
	case <-s.exited:
		return
	case <-time.After(3 * time.Second):
	}

	slog.Warn("server did not exit gracefully, sending SIGKILL", "pid", s.cmd.Process.Pid)
	_ = signalManagedProcess(s.cmd, true)

	select {
	case <-s.exited:
	case <-time.After(5 * time.Second):
		slog.Warn("server process did not exit after SIGKILL", "pid", s.cmd.Process.Pid)
	}
}

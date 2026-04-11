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
	cmd  *exec.Cmd
	port int
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

	srv := &managedServer{cmd: cmd, port: port}

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
func (s *managedServer) waitForHealth(ctx context.Context, path string) error {
	url := s.baseURL() + path
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.After(30 * time.Second)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
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

		time.Sleep(250 * time.Millisecond)
	}
}

// shutdown gracefully stops the server process.
func (s *managedServer) shutdown() {
	if s.cmd == nil || s.cmd.Process == nil {
		return
	}

	_ = signalManagedProcess(s.cmd, false)

	// Wait up to 3 seconds for graceful exit
	done := make(chan struct{})
	go func() {
		_ = s.cmd.Wait()
		close(done)
	}()

	select {
	case <-done:
		return
	case <-time.After(3 * time.Second):
	}

	// Force kill
	slog.Warn("server did not exit gracefully, sending SIGKILL", "pid", s.cmd.Process.Pid)
	_ = signalManagedProcess(s.cmd, true)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		slog.Warn("server process did not exit after SIGKILL", "pid", s.cmd.Process.Pid)
	}
}

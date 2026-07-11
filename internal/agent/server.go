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

type synchronizedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (w *synchronizedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.w.Write(p)
}

// defaultHealthTimeout bounds how long startServerWithPort waits for a freshly
// spawned server to answer its health endpoint before giving up. It is generous
// enough to absorb cold starts under host load: opencode has been observed
// taking 15s+ just to emit its first log line when the machine is busy.
const defaultHealthTimeout = 60 * time.Second

// managedServer manages a persistent HTTP server process (used by rovodev and opencode agents).
type managedServer struct {
	process       *nativeProcess
	port          int
	pidFile       string        // path to the on-disk PID record; empty if tracking disabled
	exited        chan struct{} // closed exactly once when process.wait returns
	waitErr       error         // result of process.wait; only read after exited is closed
	healthTimeout time.Duration // health-check deadline; defaults to defaultHealthTimeout when zero
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
	cmd := exec.CommandContext(context.Background(), bin, args...)
	cmd.Dir = cwd
	cmd.Stdin = nil
	cmd.Env = gitSafeEnv(cwd)

	process, err := startNativeProcess(cmd)
	if err != nil {
		return nil, fmt.Errorf("start server %s: %w", bin, err)
	}
	out := &synchronizedWriter{w: currentManagedServerOutput()}
	go func() {
		_, _ = io.Copy(out, process.stdout)
	}()
	go func() {
		_, _ = io.Copy(out, process.stderr)
	}()

	pidFile := writeServerPIDFile(currentServerPIDsDir(), ServerPIDInfo{
		PID:            process.pid(),
		Owner:          currentServerPIDOwner(),
		OwnerPID:       os.Getpid(),
		OwnerStartedAt: CurrentProcessStartedAt(),
		Agent:          agentName,
		Bin:            bin,
		Port:           port,
		StartedAt:      time.Now().UTC(),
	})

	srv := &managedServer{
		process:       process,
		port:          port,
		pidFile:       pidFile,
		exited:        make(chan struct{}),
		healthTimeout: defaultHealthTimeout,
	}
	go func() {
		srv.waitErr = process.wait()
		process.closePipes()
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

// formatHealthTimeout renders a health-check deadline for error messages,
// preferring a plain seconds form (e.g. "60s") over Duration's "1m0s".
func formatHealthTimeout(d time.Duration) string {
	if d%time.Second == 0 {
		return fmt.Sprintf("%ds", int(d/time.Second))
	}
	return d.String()
}

// waitForHealth polls the health endpoint until it returns 200 or timeout.
// If the server process exits before becoming healthy, it returns immediately
// with an exit error instead of waiting out the health-check deadline.
func (s *managedServer) waitForHealth(ctx context.Context, path string) error {
	url := s.baseURL() + path
	client := &http.Client{Timeout: 2 * time.Second}
	timeout := s.healthTimeout
	if timeout <= 0 {
		timeout = defaultHealthTimeout
	}
	deadline := time.After(timeout)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.exited:
			return fmt.Errorf("server exited before becoming healthy: %w", s.waitErr)
		case <-deadline:
			return fmt.Errorf("server health check timed out after %s", formatHealthTimeout(timeout))
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

// shutdown terminates the server through the shared native process lifecycle
// and waits for its single wait owner to observe exit. The PID tracking file is
// removed only after confirmed exit; otherwise recovery retains the durable
// record and can finish reaping the process tree.
func (s *managedServer) shutdown() {
	if s.process == nil {
		removeServerPIDFile(s.pidFile)
		return
	}

	select {
	case <-s.exited:
		removeServerPIDFile(s.pidFile)
		return
	default:
	}

	s.process.terminate()
	timer := time.NewTimer(nativeProcessWaitDelay + time.Second)
	defer timer.Stop()
	select {
	case <-s.exited:
		removeServerPIDFile(s.pidFile)
	case <-timer.C:
		slog.Warn("server process did not exit after termination", "pid", s.process.pid())
	}
}

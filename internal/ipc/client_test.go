package ipc

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

type timeoutDialError struct{}

func (timeoutDialError) Error() string   { return "dial timeout" }
func (timeoutDialError) Timeout() bool   { return true }
func (timeoutDialError) Temporary() bool { return true }

func TestDialConnectTimeoutFailsFastAndNamesSocket(t *testing.T) {
	const timeout = 25 * time.Millisecond
	t.Setenv("NM_DAEMON_CONNECT_TIMEOUT", timeout.String())

	originalDial := dialNetworkWithTimeout
	dialNetworkWithTimeout = func(network, address string, gotTimeout time.Duration) (net.Conn, error) {
		if gotTimeout != timeout {
			t.Fatalf("dial timeout = %v, want %v", gotTimeout, timeout)
		}
		time.Sleep(gotTimeout + 10*time.Millisecond)
		return nil, timeoutDialError{}
	}
	t.Cleanup(func() {
		dialNetworkWithTimeout = originalDial
	})

	socketPath := filepath.Join(t.TempDir(), "no-mistakes-dead.sock")
	if runtime.GOOS == "windows" {
		endpoint := fmt.Sprintf("127.0.0.1:1\ntoken\n%d", os.Getpid())
		if err := os.WriteFile(socketPath, []byte(endpoint), 0o600); err != nil {
			t.Fatalf("write endpoint file: %v", err)
		}
	}
	started := time.Now()
	client, err := Dial(socketPath)
	elapsed := time.Since(started)
	if client != nil {
		t.Fatal("Dial returned a client for a timed-out socket")
	}
	if err == nil {
		t.Fatal("Dial returned nil error for a timed-out socket")
	}
	if elapsed > 250*time.Millisecond {
		t.Fatalf("Dial took %v, want fast failure", elapsed)
	}
	if !IsConnectTimeout(err) {
		t.Fatalf("Dial error %T %v, want connect timeout", err, err)
	}
	if !strings.Contains(err.Error(), socketPath) {
		t.Fatalf("Dial error = %q, want socket path %q", err.Error(), socketPath)
	}
}

// TestDialUsesTheConnectTimeoutOfTheRootItDials pins the timeout to the root
// that owns the socket. A receive hook resolves its root from the gate it was
// handed and runs with no NM_HOME, so reading the ambient root's config would
// dial one daemon under another root's timeout.
func TestDialUsesTheConnectTimeoutOfTheRootItDials(t *testing.T) {
	writeRoot := func(root, timeout string) string {
		t.Helper()
		if err := os.MkdirAll(root, 0o755); err != nil {
			t.Fatal(err)
		}
		cfg := filepath.Join(root, "config.yaml")
		if err := os.WriteFile(cfg, []byte("daemon_connect_timeout: \""+timeout+"\"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return filepath.Join(root, "socket")
	}

	base := t.TempDir()
	ambient := filepath.Join(base, "ambient")
	writeRoot(ambient, "1s")
	t.Setenv("NM_HOME", ambient)

	const want = 7 * time.Second
	socketPath := writeRoot(filepath.Join(base, "owning"), want.String())
	if runtime.GOOS == "windows" {
		endpoint := fmt.Sprintf("127.0.0.1:1\ntoken\n%d", os.Getpid())
		if err := os.WriteFile(socketPath, []byte(endpoint), 0o600); err != nil {
			t.Fatalf("write endpoint file: %v", err)
		}
	}

	var got time.Duration
	originalDial := dialNetworkWithTimeout
	dialNetworkWithTimeout = func(network, address string, timeout time.Duration) (net.Conn, error) {
		got = timeout
		return nil, timeoutDialError{}
	}
	t.Cleanup(func() { dialNetworkWithTimeout = originalDial })

	if _, err := Dial(socketPath); err == nil {
		t.Fatal("expected the dial to the dead socket to fail")
	}
	if got != want {
		t.Fatalf("dial timeout = %v, want %v (the owning root's, not the ambient root's)", got, want)
	}
}

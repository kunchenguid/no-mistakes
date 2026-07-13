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

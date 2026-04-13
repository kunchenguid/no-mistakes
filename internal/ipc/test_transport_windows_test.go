//go:build integration && windows

package ipc_test

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
)

func rawDial(t *testing.T, endpoint string) net.Conn {
	t.Helper()
	data, err := os.ReadFile(endpoint)
	if err != nil {
		t.Fatalf("read endpoint: %v", err)
	}
	lines := strings.SplitN(string(data), "\n", 3)
	if len(lines) < 3 {
		t.Fatalf("invalid endpoint file")
	}
	addr := strings.TrimSpace(lines[0])
	token := strings.TrimSpace(lines[1])
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("raw dial: %v", err)
	}
	if _, err := fmt.Fprintf(conn, "%s\n", token); err != nil {
		conn.Close()
		t.Fatalf("send token: %v", err)
	}
	return conn
}

func rawListen(t *testing.T, endpoint string) net.Listener {
	t.Helper()
	_ = os.Remove(endpoint)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	content := fmt.Sprintf("%s\ntest-token\n%d", ln.Addr().String(), os.Getpid())
	if err := os.WriteFile(endpoint, []byte(content), 0o600); err != nil {
		ln.Close()
		t.Fatal(err)
	}
	return &tokenStripListener{Listener: ln}
}

// tokenStripListener wraps a net.Listener to auto-consume the auth token
// line sent by dial(), so test server goroutines see requests directly.
type tokenStripListener struct {
	net.Listener
}

func (l *tokenStripListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	r := bufio.NewReader(conn)
	if _, err := r.ReadString('\n'); err != nil {
		conn.Close()
		return nil, fmt.Errorf("read auth token: %w", err)
	}
	return &testBufferedConn{Conn: conn, r: r}, nil
}

type testBufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *testBufferedConn) Read(p []byte) (int, error) {
	return c.r.Read(p)
}

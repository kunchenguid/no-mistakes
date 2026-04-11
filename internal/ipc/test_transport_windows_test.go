//go:build windows

package ipc_test

import (
	"net"
	"os"
	"strings"
	"testing"
)

func rawDial(t *testing.T, endpoint string) net.Conn {
	t.Helper()
	addr, err := os.ReadFile(endpoint)
	if err != nil {
		t.Fatalf("read endpoint: %v", err)
	}
	conn, err := net.Dial("tcp", strings.TrimSpace(string(addr)))
	if err != nil {
		t.Fatalf("raw dial: %v", err)
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
	if err := os.WriteFile(endpoint, []byte(ln.Addr().String()), 0o600); err != nil {
		ln.Close()
		t.Fatal(err)
	}
	return ln
}

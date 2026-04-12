//go:build !windows

package ipc_test

import (
	"net"
	"os"
	"testing"
)

func rawDial(t *testing.T, endpoint string) net.Conn {
	t.Helper()
	conn, err := net.Dial("unix", endpoint)
	if err != nil {
		t.Fatalf("raw dial: %v", err)
	}
	return conn
}

func rawListen(t *testing.T, endpoint string) net.Listener {
	t.Helper()
	_ = os.Remove(endpoint)
	ln, err := net.Listen("unix", endpoint)
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

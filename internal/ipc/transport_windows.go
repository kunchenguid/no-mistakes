//go:build windows

package ipc

import (
	"fmt"
	"net"
	"os"
	"strings"
)

func listen(endpoint string) (net.Listener, error) {
	_ = os.Remove(endpoint)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(endpoint, []byte(ln.Addr().String()), 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

func dial(endpoint string) (net.Conn, error) {
	addr, err := os.ReadFile(endpoint)
	if err != nil {
		return nil, err
	}
	target := strings.TrimSpace(string(addr))
	if target == "" {
		return nil, fmt.Errorf("empty ipc endpoint")
	}
	return net.Dial("tcp", target)
}

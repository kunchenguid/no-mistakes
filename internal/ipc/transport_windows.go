//go:build windows

package ipc

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"syscall"
)

func listen(endpoint string) (net.Listener, error) {
	_ = os.Remove(endpoint)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	token, err := generateToken()
	if err != nil {
		ln.Close()
		return nil, err
	}
	content := fmt.Sprintf("%s\n%s\n%d", ln.Addr().String(), token, os.Getpid())
	if err := os.WriteFile(endpoint, []byte(content), 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return &tokenListener{Listener: ln, token: token}, nil
}

func dial(endpoint string) (net.Conn, error) {
	data, err := os.ReadFile(endpoint)
	if err != nil {
		return nil, err
	}
	lines := strings.SplitN(string(data), "\n", 3)
	if len(lines) < 3 {
		return nil, fmt.Errorf("invalid ipc endpoint file")
	}
	addr := strings.TrimSpace(lines[0])
	token := strings.TrimSpace(lines[1])
	pidStr := strings.TrimSpace(lines[2])
	if addr == "" || token == "" {
		return nil, fmt.Errorf("invalid ipc endpoint file")
	}
	if pid, err := strconv.Atoi(pidStr); err == nil && !processAlive(pid) {
		return nil, fmt.Errorf("daemon process %d is no longer running", pid)
	}
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}
	if _, err := fmt.Fprintf(conn, "%s\n", token); err != nil {
		conn.Close()
		return nil, fmt.Errorf("send auth token: %w", err)
	}
	return conn, nil
}

func generateToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// tokenListener wraps a net.Listener to verify auth tokens on accepted connections.
// Connections that fail to present the correct token are silently closed.
type tokenListener struct {
	net.Listener
	token string
}

func (tl *tokenListener) Accept() (net.Conn, error) {
	for {
		conn, err := tl.Listener.Accept()
		if err != nil {
			return nil, err
		}
		r := bufio.NewReader(conn)
		line, err := r.ReadString('\n')
		if err != nil {
			conn.Close()
			continue
		}
		if strings.TrimSpace(line) != tl.token {
			conn.Close()
			continue
		}
		return &bufferedConn{Conn: conn, r: r}, nil
	}
}

// bufferedConn wraps a net.Conn so that bytes already buffered by a
// bufio.Reader are available for subsequent reads.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (bc *bufferedConn) Read(p []byte) (int, error) {
	return bc.r.Read(p)
}

func processAlive(pid int) bool {
	const processQueryLimitedInformation = 0x1000
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return false
	}
	syscall.CloseHandle(h)
	return true
}

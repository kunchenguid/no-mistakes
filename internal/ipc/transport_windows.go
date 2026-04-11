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
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/windows"
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
	tmpFile := endpoint + ".tmp"
	if err := os.WriteFile(tmpFile, []byte(content), 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	if err := restrictFileACL(tmpFile); err != nil {
		os.Remove(tmpFile)
		ln.Close()
		return nil, fmt.Errorf("restrict endpoint file ACL: %w", err)
	}
	if err := os.Rename(tmpFile, endpoint); err != nil {
		os.Remove(tmpFile)
		ln.Close()
		return nil, fmt.Errorf("rename endpoint file: %w", err)
	}
	return &tokenListener{
		Listener: ln,
		token:    token,
		connCh:   make(chan net.Conn),
		done:     make(chan struct{}),
	}, nil
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
// Auth is performed in per-connection goroutines so that slow or invalid
// connections do not block acceptance of other clients.
type tokenListener struct {
	net.Listener
	token     string
	connCh    chan net.Conn
	done      chan struct{}
	startOnce sync.Once
	closeOnce sync.Once
}

func (tl *tokenListener) start() {
	go func() {
		for {
			raw, err := tl.Listener.Accept()
			if err != nil {
				tl.closeOnce.Do(func() { close(tl.done) })
				return
			}
			go tl.authenticate(raw)
		}
	}()
}

func (tl *tokenListener) authenticate(conn net.Conn) {
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	r := bufio.NewReader(conn)
	line, err := r.ReadString('\n')
	if err != nil {
		conn.Close()
		return
	}
	conn.SetReadDeadline(time.Time{})
	if strings.TrimSpace(line) != tl.token {
		conn.Close()
		return
	}
	select {
	case tl.connCh <- &bufferedConn{Conn: conn, r: r}:
	case <-tl.done:
		conn.Close()
	}
}

func (tl *tokenListener) Accept() (net.Conn, error) {
	tl.startOnce.Do(tl.start)
	select {
	case conn := <-tl.connCh:
		return conn, nil
	case <-tl.done:
		return nil, net.ErrClosed
	}
}

func (tl *tokenListener) Close() error {
	tl.closeOnce.Do(func() { close(tl.done) })
	return tl.Listener.Close()
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

func restrictFileACL(path string) error {
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return err
	}
	defer token.Close()

	user, err := token.GetTokenUser()
	if err != nil {
		return err
	}

	access := []windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.SET_ACCESS,
		Inheritance:       windows.NO_INHERITANCE,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_USER,
			TrusteeValue: windows.TrusteeValueFromSID(user.User.Sid),
		},
	}}

	acl, err := windows.ACLFromEntries(access, nil)
	if err != nil {
		return err
	}

	return windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, acl, nil,
	)
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

//go:build !windows

package ipc

import (
	"fmt"
	"net"
	"os"
	"syscall"
	"time"
)

// listen binds a Unix domain socket at endpoint. If endpoint already exists
// and something is actively listening on it, that means a live daemon
// (possibly one whose singleton lock we failed to acquire, or a caller that
// bypassed the daemon package entirely) already owns this path: listen fails
// instead of unlinking the path out from under it, which previously let a
// second daemon silently steal the socket and leave the first alive but
// unreachable. Only a proven-stale path (nothing answers - a leftover from an
// unclean exit) is removed before binding.
func listen(endpoint string) (net.Listener, error) {
	if err := CheckEndpointPath(endpoint); err != nil {
		return nil, err
	}
	if conn, err := net.DialTimeout("unix", endpoint, 200*time.Millisecond); err == nil {
		conn.Close()
		return nil, fmt.Errorf("ipc socket %s is already in use by a live listener", endpoint)
	}
	_ = os.Remove(endpoint)
	oldMask := syscall.Umask(0o077)
	ln, err := net.Listen("unix", endpoint)
	syscall.Umask(oldMask)
	return ln, err
}

func dial(endpoint string, timeout time.Duration) (net.Conn, error) {
	if err := CheckEndpointPath(endpoint); err != nil {
		return nil, err
	}
	return dialNetworkWithTimeout("unix", endpoint, timeout)
}

// maxEndpointPathLen is the longest socket path the kernel accepts: sun_path
// holds the path plus its NUL terminator.
var maxEndpointPathLen = len(syscall.RawSockaddrUnix{}.Path) - 1

// CheckEndpointPath reports a *SocketPathTooLongError when endpoint cannot be
// bound or connected as a Unix socket on this platform.
func CheckEndpointPath(endpoint string) error {
	if len(endpoint) > maxEndpointPathLen {
		return &SocketPathTooLongError{Path: endpoint, Limit: maxEndpointPathLen}
	}
	return nil
}

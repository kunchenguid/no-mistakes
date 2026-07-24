//go:build darwin

package ipc

import (
	"net"
	"syscall"

	"golang.org/x/sys/unix"
)

func authenticatedPeerPID(conn net.Conn) int {
	sc, ok := conn.(syscall.Conn)
	if !ok {
		return 0
	}
	raw, err := sc.SyscallConn()
	if err != nil {
		return 0
	}
	pid := 0
	if err := raw.Control(func(fd uintptr) {
		value, getErr := unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID)
		if getErr == nil && value > 0 {
			pid = value
		}
	}); err != nil {
		return 0
	}
	return pid
}

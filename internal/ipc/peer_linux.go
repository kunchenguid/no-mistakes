//go:build linux

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
		cred, getErr := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		if getErr == nil && cred != nil && cred.Pid > 0 {
			pid = int(cred.Pid)
		}
	}); err != nil {
		return 0
	}
	return pid
}

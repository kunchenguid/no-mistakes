//go:build !windows

package ipc

import (
	"net"
	"os"
	"syscall"
)

func listen(endpoint string) (net.Listener, error) {
	_ = os.Remove(endpoint)
	oldMask := syscall.Umask(0o077)
	ln, err := net.Listen("unix", endpoint)
	syscall.Umask(oldMask)
	return ln, err
}

func dial(endpoint string) (net.Conn, error) {
	return net.Dial("unix", endpoint)
}

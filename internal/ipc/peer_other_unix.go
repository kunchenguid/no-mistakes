//go:build !windows && !darwin && !linux

package ipc

import "net"

func authenticatedPeerPID(net.Conn) int { return 0 }

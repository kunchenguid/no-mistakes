//go:build !windows

package branchsync

import (
	"fmt"
	"net"
	"strings"
	"time"
)

func listenInternalMutationAuthority(_ string) (net.Listener, string, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, "", err
	}
	return listener, "tcp://" + listener.Addr().String(), nil
}

func dialInternalMutationAuthority(endpoint string) (net.Conn, error) {
	conn, err := net.DialTimeout("tcp", strings.TrimPrefix(endpoint, "tcp://"), 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("connect live authority: %w", err)
	}
	return conn, nil
}

func closeInternalMutationAuthority(_ string) {
}

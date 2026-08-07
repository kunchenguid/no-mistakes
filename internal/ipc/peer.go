package ipc

import "context"

type peerPIDContextKey struct{}

// PeerPID returns the OS-authenticated process ID of the local IPC client.
// Zero means the transport cannot authenticate a peer process on this platform.
func PeerPID(ctx context.Context) int {
	pid, _ := ctx.Value(peerPIDContextKey{}).(int)
	return pid
}

func withPeerPID(ctx context.Context, pid int) context.Context {
	if pid <= 0 {
		return ctx
	}
	return context.WithValue(ctx, peerPIDContextKey{}, pid)
}

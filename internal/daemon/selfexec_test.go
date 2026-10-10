//go:build !windows

package daemon

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

func TestEnsureDaemonRefusesOverLongSocketPathBeforeStarting(t *testing.T) {
	root := filepath.Join(t.TempDir(), strings.Repeat("r", 120))
	p := paths.WithRoot(root)

	oldHealth, oldStart := daemonHealthCheck, daemonStart
	t.Cleanup(func() { daemonHealthCheck, daemonStart = oldHealth, oldStart })
	daemonHealthCheck = func(*paths.Paths) (bool, error) {
		t.Fatal("EnsureDaemon probed the daemon past the socket path check")
		return false, nil
	}
	daemonStart = func(*paths.Paths) error {
		t.Fatal("EnsureDaemon started the daemon past the socket path check")
		return nil
	}

	err := EnsureDaemon(p)
	var tooLong *ipc.SocketPathTooLongError
	if !errors.As(err, &tooLong) {
		t.Fatalf("EnsureDaemon error = %v, want *ipc.SocketPathTooLongError", err)
	}
}

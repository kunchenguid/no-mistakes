//go:build !windows

package ipc_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
)

var socketPathLimit = len(syscall.RawSockaddrUnix{}.Path) - 1

func socketPathOfLength(t *testing.T, n int) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ipc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	pad := n - len(dir) - 1
	if pad < 1 {
		t.Fatalf("temp dir %q leaves no room for a %d-byte socket path", dir, n)
	}
	path := filepath.Join(dir, strings.Repeat("s", pad))
	if len(path) != n {
		t.Fatalf("built a %d-byte path, want %d", len(path), n)
	}
	return path
}

func requireTooLong(t *testing.T, err error, path string) {
	t.Helper()
	var tooLong *ipc.SocketPathTooLongError
	if !errors.As(err, &tooLong) {
		t.Fatalf("error = %v, want *SocketPathTooLongError", err)
	}
	for _, want := range []string{"NM_HOME", path, "shorter"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if tooLong.Limit != socketPathLimit {
		t.Errorf("Limit = %d, want %d", tooLong.Limit, socketPathLimit)
	}
}

func TestListen_RejectsSocketPathOverLimit(t *testing.T) {
	path := socketPathOfLength(t, socketPathLimit+1)
	err := ipc.NewServer().Listen(path)
	if err == nil {
		t.Fatal("expected Listen to refuse an over-limit socket path")
	}
	requireTooLong(t, err, path)
}

func TestDial_RejectsSocketPathOverLimit(t *testing.T) {
	path := socketPathOfLength(t, socketPathLimit+1)
	c, err := ipc.Dial(path)
	if err == nil {
		c.Close()
		t.Fatal("expected Dial to refuse an over-limit socket path")
	}
	requireTooLong(t, err, path)
}

func TestSocketPathAtLimitStillWorks(t *testing.T) {
	path := socketPathOfLength(t, socketPathLimit)
	startServer(t, path)
	c, err := ipc.Dial(path)
	if err != nil {
		t.Fatalf("Dial at the limit: %v", err)
	}
	c.Close()
}

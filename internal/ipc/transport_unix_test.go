//go:build !windows

package ipc_test

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
)

// TestServe_SecondListenerForLiveSocketDoesNotStealIt is the regression test
// for the socket-steal bug: listen() used to unconditionally os.Remove the
// endpoint before binding, so a second Serve for the same path would unlink
// a live daemon's socket, bind its own, and leave the first daemon alive but
// unreachable by path. Serve must now refuse when something is already
// listening, and the first server must remain reachable afterward.
func TestServe_SecondListenerForLiveSocketDoesNotStealIt(t *testing.T) {
	sock := socketPath(t)
	srv1 := startServer(t, sock)
	_ = srv1

	if c, err := ipc.Dial(sock); err != nil {
		t.Fatalf("first server not reachable before second Serve attempt: %v", err)
	} else {
		c.Close()
	}

	srv2 := ipc.NewServer()
	err := srv2.Serve(sock)
	if err == nil {
		t.Fatal("expected second Serve on a live socket path to fail")
	}

	if c, err := ipc.Dial(sock); err != nil {
		t.Fatalf("first server became unreachable after second Serve attempt: %v", err)
	} else {
		c.Close()
	}
}

package ipc_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
)

func TestServerClose(t *testing.T) {
	sock := socketPath(t)
	srv := ipc.NewServer()
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(sock) }()

	// wait for server to be ready
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c, err := ipc.Dial(sock)
		if err == nil {
			c.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	srv.Close()
	err := <-errCh
	if err != nil {
		t.Errorf("Serve returned unexpected error: %v", err)
	}

	// dial should fail after close (on Windows named pipes may need a moment)
	dialDeadline := time.Now().Add(2 * time.Second)
	for {
		_, dialErr := ipc.Dial(sock)
		if dialErr != nil {
			break
		}
		if time.Now().After(dialDeadline) {
			t.Error("expected dial to fail after server close")
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestDialNonexistentSocket(t *testing.T) {
	_, err := ipc.Dial(filepath.Join(t.TempDir(), "nonexistent.sock"))
	if err == nil {
		t.Error("expected error dialing nonexistent socket")
	}
}

func TestServerInvalidJSON(t *testing.T) {
	sock := socketPath(t)
	startServer(t, sock)

	// Send invalid JSON and verify parse error response.
	conn := rawDial(t, sock)
	defer conn.Close()

	// Write invalid JSON.
	fmt.Fprintln(conn, "this is not json")

	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		t.Fatal("no response for invalid JSON")
	}
	var resp ipc.Response
	if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if resp.Error == nil {
		t.Fatal("expected error response for invalid JSON")
	}
	if resp.Error.Code != ipc.ErrParseError {
		t.Errorf("code = %d, want %d", resp.Error.Code, ipc.ErrParseError)
	}
}

func TestServerExitsWhenListenerClosed(t *testing.T) {
	sock := socketPath(t)
	srv := ipc.NewServer()
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(sock) }()

	// Wait for server to be ready.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c, err := ipc.Dial(sock)
		if err == nil {
			c.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Simulate the listener being closed externally (e.g. tokenListener
	// self-close on Windows after too many accept errors) by removing the
	// socket file and dialing to confirm the server is up, then closing
	// the server's listener via Close. But we want to test that the server
	// exits even without s.Close() being called. Instead, we close the
	// underlying listener by calling srv.CloseListener().
	srv.CloseListener()

	select {
	case err := <-errCh:
		if err != nil {
			t.Errorf("Serve returned unexpected error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not exit after listener was closed")
	}
}

func TestServerDoubleClose(t *testing.T) {
	sock := socketPath(t)
	srv := ipc.NewServer()
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(sock) }()

	// Wait for server to be ready.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c, err := ipc.Dial(sock)
		if err == nil {
			c.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	// Close twice — should not panic.
	srv.Close()
	srv.Close()

	if err := <-errCh; err != nil {
		t.Errorf("Serve returned unexpected error: %v", err)
	}
}

func TestCallServerDisconnect(t *testing.T) {
	sock := socketPath(t)
	srv := startServer(t, sock)

	c, err := ipc.Dial(sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	// Close the server, causing the connection to drop.
	srv.Close()
	time.Sleep(50 * time.Millisecond) // let server close propagate

	// Call should return an error (connection closed or send failure).
	var result json.RawMessage
	err = c.Call("health", nil, &result)
	if err == nil {
		t.Fatal("expected error when server is closed")
	}
}

func TestServerEmptyLine(t *testing.T) {
	sock := socketPath(t)
	srv := startServer(t, sock)

	srv.Handle("echo", func(_ context.Context, params json.RawMessage) (interface{}, error) {
		return map[string]string{"echo": "ok"}, nil
	})

	// Connect raw and send an empty line, then a valid request.
	conn := rawDial(t, sock)
	defer conn.Close()

	// Send empty line first.
	conn.Write([]byte("\n"))

	// Send valid request.
	req, _ := ipc.NewRequest("echo", nil)
	enc := json.NewEncoder(conn)
	enc.Encode(req)

	// Read response — should get a valid response (empty line was skipped).
	scanner := bufio.NewScanner(conn)
	if !scanner.Scan() {
		t.Fatal("expected response")
	}
	var resp ipc.Response
	if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("unexpected error: %s", resp.Error.Message)
	}
}

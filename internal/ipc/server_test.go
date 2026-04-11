package ipc_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
)

// socketPath returns a short socket path to stay within macOS 104-byte limit.
func socketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ipc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "s.sock")
}

func startServer(t *testing.T, sock string) *ipc.Server {
	t.Helper()
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
	t.Cleanup(func() {
		srv.Close()
		if err := <-errCh; err != nil {
			// server returns nil on clean close
		}
	})
	return srv
}

func TestServerClientRoundTrip(t *testing.T) {
	sock := socketPath(t)
	srv := startServer(t, sock)

	type echoParams struct {
		Message string `json:"message"`
	}
	type echoResult struct {
		Echo string `json:"echo"`
	}

	srv.Handle("echo", func(_ context.Context, raw json.RawMessage) (interface{}, error) {
		var p echoParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		return echoResult{Echo: p.Message}, nil
	})

	c, err := ipc.Dial(sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	var result echoResult
	if err := c.Call("echo", echoParams{Message: "hello"}, &result); err != nil {
		t.Fatalf("call: %v", err)
	}
	if result.Echo != "hello" {
		t.Errorf("got echo=%q, want %q", result.Echo, "hello")
	}
}

func TestMethodNotFound(t *testing.T) {
	sock := socketPath(t)
	startServer(t, sock)

	c, err := ipc.Dial(sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	var result json.RawMessage
	err = c.Call("nonexistent", nil, &result)
	if err == nil {
		t.Fatal("expected error for unknown method")
	}
	rpcErr, ok := err.(*ipc.RPCError)
	if !ok {
		t.Fatalf("expected RPCError, got %T: %v", err, err)
	}
	if rpcErr.Code != ipc.ErrMethodNotFound {
		t.Errorf("got code %d, want %d", rpcErr.Code, ipc.ErrMethodNotFound)
	}
}

func TestHandlerError(t *testing.T) {
	sock := socketPath(t)
	srv := startServer(t, sock)

	srv.Handle("fail", func(_ context.Context, _ json.RawMessage) (interface{}, error) {
		return nil, fmt.Errorf("something broke")
	})

	c, err := ipc.Dial(sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	var result json.RawMessage
	err = c.Call("fail", nil, &result)
	if err == nil {
		t.Fatal("expected error")
	}
	rpcErr, ok := err.(*ipc.RPCError)
	if !ok {
		t.Fatalf("expected RPCError, got %T: %v", err, err)
	}
	if rpcErr.Code != ipc.ErrInternal {
		t.Errorf("got code %d, want %d", rpcErr.Code, ipc.ErrInternal)
	}
	if rpcErr.Message != "something broke" {
		t.Errorf("got message=%q, want %q", rpcErr.Message, "something broke")
	}
}

func TestMultipleClients(t *testing.T) {
	sock := socketPath(t)
	srv := startServer(t, sock)

	type addParams struct {
		A int `json:"a"`
		B int `json:"b"`
	}
	type addResult struct {
		Sum int `json:"sum"`
	}

	srv.Handle("add", func(_ context.Context, raw json.RawMessage) (interface{}, error) {
		var p addParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		return addResult{Sum: p.A + p.B}, nil
	})

	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			c, err := ipc.Dial(sock)
			if err != nil {
				t.Errorf("client %d dial: %v", n, err)
				return
			}
			defer c.Close()

			var result addResult
			if err := c.Call("add", addParams{A: n, B: 10}, &result); err != nil {
				t.Errorf("client %d call: %v", n, err)
				return
			}
			if result.Sum != n+10 {
				t.Errorf("client %d: got sum=%d, want %d", n, result.Sum, n+10)
			}
		}(i)
	}
	wg.Wait()
}

func TestMultipleCallsOnSameConnection(t *testing.T) {
	sock := socketPath(t)
	srv := startServer(t, sock)

	type echoParams struct {
		Message string `json:"message"`
	}
	type echoResult struct {
		Echo string `json:"echo"`
	}

	srv.Handle("echo", func(_ context.Context, raw json.RawMessage) (interface{}, error) {
		var p echoParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		return echoResult{Echo: p.Message}, nil
	})

	c, err := ipc.Dial(sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	for i := 0; i < 5; i++ {
		msg := fmt.Sprintf("msg-%d", i)
		var result echoResult
		if err := c.Call("echo", echoParams{Message: msg}, &result); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if result.Echo != msg {
			t.Errorf("call %d: got echo=%q, want %q", i, result.Echo, msg)
		}
	}
}

func TestNilParams(t *testing.T) {
	sock := socketPath(t)
	srv := startServer(t, sock)

	srv.Handle("health", func(_ context.Context, _ json.RawMessage) (interface{}, error) {
		return ipc.HealthResult{Status: "ok"}, nil
	})

	c, err := ipc.Dial(sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	var result ipc.HealthResult
	if err := c.Call("health", nil, &result); err != nil {
		t.Fatalf("call: %v", err)
	}
	if result.Status != "ok" {
		t.Errorf("got status=%q, want %q", result.Status, "ok")
	}
}

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

	// dial should fail after close
	_, err = ipc.Dial(sock)
	if err == nil {
		t.Error("expected dial to fail after server close")
	}
}

func TestDialNonexistentSocket(t *testing.T) {
	_, err := ipc.Dial(filepath.Join(t.TempDir(), "nonexistent.sock"))
	if err == nil {
		t.Error("expected error dialing nonexistent socket")
	}
}

func TestStreamHandler(t *testing.T) {
	sock := socketPath(t)
	srv := startServer(t, sock)

	type streamParams struct {
		Count int `json:"count"`
	}
	type streamEvent struct {
		Index int `json:"index"`
	}

	srv.HandleStream("stream_test", func(_ context.Context, raw json.RawMessage, send func(interface{}) error) error {
		var p streamParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return err
		}
		for i := 0; i < p.Count; i++ {
			if err := send(streamEvent{Index: i}); err != nil {
				return err
			}
		}
		return nil
	})

	// Dial and send the subscribe-like request manually.
	conn, err := ipc.Dial(sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// First, verify we get the initial OK response via Call.
	// Actually, Call reads exactly one line as response, so let's use
	// the Subscribe pattern manually with a raw connection.
	conn.Close()

	// Use raw connection to test streaming.
	rawConn := rawDial(t, sock)
	defer rawConn.Close()

	encoder := json.NewEncoder(rawConn)
	scanner := bufio.NewScanner(rawConn)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)

	req, _ := ipc.NewRequest("stream_test", streamParams{Count: 3})
	if err := encoder.Encode(req); err != nil {
		t.Fatalf("send request: %v", err)
	}

	// Read initial response.
	if !scanner.Scan() {
		t.Fatal("no initial response")
	}
	var resp ipc.Response
	if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
		t.Fatalf("parse response: %v", err)
	}
	if resp.Error != nil {
		t.Fatalf("initial response error: %v", resp.Error)
	}

	// Read 3 streamed events.
	for i := 0; i < 3; i++ {
		if !scanner.Scan() {
			t.Fatalf("event %d: no data", i)
		}
		var event streamEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			t.Fatalf("event %d parse: %v", i, err)
		}
		if event.Index != i {
			t.Errorf("event %d: index=%d, want %d", i, event.Index, i)
		}
	}
}

func TestStreamHandlerAndRegularCoexist(t *testing.T) {
	sock := socketPath(t)
	srv := startServer(t, sock)

	// Register both a regular and a stream handler.
	srv.Handle("echo", func(_ context.Context, raw json.RawMessage) (interface{}, error) {
		return map[string]string{"echo": "hello"}, nil
	})
	srv.HandleStream("stream_noop", func(_ context.Context, _ json.RawMessage, send func(interface{}) error) error {
		return nil // stream handler that completes immediately
	})

	// Regular handler should still work.
	c, err := ipc.Dial(sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	var result map[string]string
	if err := c.Call("echo", nil, &result); err != nil {
		t.Fatalf("call: %v", err)
	}
	if result["echo"] != "hello" {
		t.Errorf("got echo=%q, want %q", result["echo"], "hello")
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

func TestCallWithNilResult(t *testing.T) {
	sock := socketPath(t)
	srv := startServer(t, sock)

	srv.Handle("noop", func(_ context.Context, _ json.RawMessage) (interface{}, error) {
		return map[string]bool{"ok": true}, nil
	})

	c, err := ipc.Dial(sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()

	// Call with nil result pointer — should succeed without unmarshaling result.
	if err := c.Call("noop", nil, nil); err != nil {
		t.Fatalf("call with nil result: %v", err)
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

func TestSubscribeServerError(t *testing.T) {
	sock := socketPath(t)
	srv := startServer(t, sock)

	// Register subscribe as a regular handler that returns an error.
	// The client's Subscribe reads this error response and should return it.
	srv.Handle(ipc.MethodSubscribe, func(_ context.Context, _ json.RawMessage) (interface{}, error) {
		return nil, fmt.Errorf("run not found")
	})

	_, _, err := ipc.Subscribe(sock, &ipc.SubscribeParams{RunID: "nonexistent"})
	if err == nil {
		t.Fatal("expected error from Subscribe")
	}
	if !strings.Contains(err.Error(), "run not found") {
		t.Errorf("error = %q, want to contain 'run not found'", err)
	}
}

func TestSubscribeMalformedEvent(t *testing.T) {
	sock := socketPath(t)
	srv := startServer(t, sock)

	// Stream handler sends one valid event, one malformed JSON, one valid event.
	srv.HandleStream(ipc.MethodSubscribe, func(_ context.Context, _ json.RawMessage, send func(interface{}) error) error {
		s1 := "first"
		if err := send(ipc.Event{Type: ipc.EventRunUpdated, RunID: "r1", Status: &s1}); err != nil {
			return err
		}
		// Send raw malformed JSON by encoding a special string that the encoder wraps.
		// We need to write raw bytes — use send with a type that produces invalid Event JSON.
		// Actually, the send function calls encoder.Encode which always produces valid JSON.
		// So we need to write directly to the connection. Instead, we'll test this via a raw socket.
		return nil
	})

	// This approach won't work for malformed events since send always produces valid JSON.
	// Use a raw socket instead on a separate path to avoid a race where the old
	// listener's Close unlinks the socket after the new listener creates it.
	srv.Close()

	// Start a fresh minimal server with raw socket control.
	sock = socketPath(t)
	ln := rawListen(t, sock)
	defer ln.Close()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		scanner := bufio.NewScanner(conn)
		if !scanner.Scan() {
			return
		}
		// Read the subscribe request, send OK response.
		var req ipc.Request
		json.Unmarshal(scanner.Bytes(), &req)

		enc := json.NewEncoder(conn)
		okResp := ipc.Response{JSONRPC: "2.0", ID: req.ID}
		okResult, _ := json.Marshal(map[string]bool{"ok": true})
		okResp.Result = okResult
		enc.Encode(okResp)

		// Send valid event.
		s1 := "first"
		enc.Encode(ipc.Event{Type: ipc.EventRunUpdated, RunID: "r1", Status: &s1})

		// Send malformed JSON.
		conn.Write([]byte("{bad json}\n"))

		// Send another valid event.
		s2 := "second"
		enc.Encode(ipc.Event{Type: ipc.EventRunUpdated, RunID: "r1", Status: &s2})
	}()

	ch, cancel, err := ipc.Subscribe(sock, &ipc.SubscribeParams{RunID: "r1"})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer cancel()

	var events []ipc.Event
	for event := range ch {
		events = append(events, event)
	}

	// Malformed event should be skipped — we should get 2 events, not 3.
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2 (malformed should be skipped)", len(events))
	}
	if events[0].Status == nil || *events[0].Status != "first" {
		t.Errorf("event 0 status = %v, want 'first'", events[0].Status)
	}
	if events[1].Status == nil || *events[1].Status != "second" {
		t.Errorf("event 1 status = %v, want 'second'", events[1].Status)
	}
}

func TestSubscribeConnectionClosedBeforeResponse(t *testing.T) {
	sock := socketPath(t)
	os.Remove(sock)

	// Start a raw server that closes connection immediately after accept.
	ln := rawListen(t, sock)
	defer ln.Close()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		// Read the request then close without responding.
		scanner := bufio.NewScanner(conn)
		scanner.Scan() // consume the request
		conn.Close()
	}()

	time.Sleep(50 * time.Millisecond)

	_, _, err := ipc.Subscribe(sock, &ipc.SubscribeParams{RunID: "r1"})
	if err == nil {
		t.Fatal("expected error when connection closed before response")
	}
	if !strings.Contains(err.Error(), "connection closed") {
		t.Errorf("error = %q, want to contain 'connection closed'", err)
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

func TestSubscribeClient(t *testing.T) {
	sock := socketPath(t)
	srv := startServer(t, sock)

	// Set up a stream handler that sends 3 events.
	srv.HandleStream(ipc.MethodSubscribe, func(_ context.Context, raw json.RawMessage, send func(interface{}) error) error {
		var p ipc.SubscribeParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return err
		}
		for i := 0; i < 3; i++ {
			status := fmt.Sprintf("event-%d", i)
			event := ipc.Event{
				Type:   ipc.EventRunUpdated,
				RunID:  p.RunID,
				Status: &status,
			}
			if err := send(event); err != nil {
				return err
			}
		}
		return nil // handler returns → connection closes → channel closes
	})

	ch, cancel, err := ipc.Subscribe(sock, &ipc.SubscribeParams{RunID: "run123"})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	defer cancel()

	var events []ipc.Event
	for event := range ch {
		events = append(events, event)
	}

	if len(events) != 3 {
		t.Fatalf("got %d events, want 3", len(events))
	}
	for i, event := range events {
		wantStatus := fmt.Sprintf("event-%d", i)
		if event.RunID != "run123" {
			t.Errorf("event %d: runID=%q, want %q", i, event.RunID, "run123")
		}
		if event.Status == nil || *event.Status != wantStatus {
			t.Errorf("event %d: status=%v, want %q", i, event.Status, wantStatus)
		}
	}
}

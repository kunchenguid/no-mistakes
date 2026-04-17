package ipc_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
)

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

func TestSubscribeClient_EnableWindowsCI(t *testing.T) {
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

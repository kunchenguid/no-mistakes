package ipc_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
)

func TestStreamHandler_EnableWindowsCI(t *testing.T) {
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

func TestStreamRequestsLogAtInfo(t *testing.T) {
	sock := socketPath(t)
	srv := startServer(t, sock)

	type streamEvent struct {
		Index int `json:"index"`
	}

	srv.HandleStream("stream_test", func(_ context.Context, _ json.RawMessage, send func(interface{}) error) error {
		return send(streamEvent{Index: 0})
	})

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	prev := slog.Default()
	slog.SetDefault(logger)
	defer slog.SetDefault(prev)

	rawConn := rawDial(t, sock)
	defer rawConn.Close()

	encoder := json.NewEncoder(rawConn)
	scanner := bufio.NewScanner(rawConn)
	scanner.Buffer(make([]byte, 0, 1024*1024), 1024*1024)

	req, _ := ipc.NewRequest("stream_test", nil)
	if err := encoder.Encode(req); err != nil {
		t.Fatalf("send request: %v", err)
	}

	if !scanner.Scan() {
		t.Fatal("no initial response")
	}

	if !scanner.Scan() {
		t.Fatal("no stream event")
	}

	logOutput := logs.String()
	if !strings.Contains(logOutput, "msg=\"ipc stream request\" method=stream_test") {
		t.Fatalf("stream request log missing: %s", logOutput)
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

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestStreamSSEReturnsHTTPStatusError(t *testing.T) {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "subscription failed", http.StatusBadGateway)
	}))
	defer server.Close()

	ready := make(chan struct{})
	err := streamSSE(context.Background(), server.URL, io.Discard, ready)
	if err == nil {
		t.Fatal("expected streamSSE to fail on non-200 response")
	}
	if !strings.Contains(err.Error(), "502") {
		t.Fatalf("error = %q, want HTTP status", err)
	}
	select {
	case <-ready:
		t.Fatal("ready channel closed for failed SSE subscription")
	default:
	}
}

func TestCaptureOpencodeFlavourRequiresSessionIdle(t *testing.T) {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/session":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"sess-123"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/global/event":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprint(w, "event: message\ndata: {\"type\":\"session.updated\"}\n\n")
		case r.Method == http.MethodPost && r.URL.Path == "/session/sess-123/message":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode message body: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"msg-123"}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/session/sess-123":
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := captureOpencodeFlavour(ctx, server.URL, dir, "hi", "")
	if err == nil {
		t.Fatal("expected error when session.idle is missing")
	}
	if !strings.Contains(err.Error(), "session.idle") {
		t.Fatalf("error = %q, want mention of session.idle", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "sse.txt")); !os.IsNotExist(statErr) {
		t.Fatalf("sse.txt should not be written on truncated capture, stat err = %v", statErr)
	}
}

func TestCaptureOpencodeFlavourWaitsForSSESubscription(t *testing.T) {
	var (
		mu         sync.Mutex
		subscribed bool
		messages   chan string
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/session":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"sess-123"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/global/event":
			time.Sleep(350 * time.Millisecond)
			mu.Lock()
			subscribed = true
			messages = make(chan string, 1)
			ch := messages
			mu.Unlock()
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			select {
			case msg := <-ch:
				_, _ = io.WriteString(w, msg)
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
			case <-r.Context().Done():
			}
		case r.Method == http.MethodPost && r.URL.Path == "/session/sess-123/message":
			mu.Lock()
			ch := messages
			ready := subscribed
			mu.Unlock()
			if ready {
				ch <- "event: message\ndata: {\"type\":\"session.idle\"}\n\n"
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"msg-123"}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/session/sess-123":
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	if err := captureOpencodeFlavour(ctx, server.URL, dir, "hi", ""); err != nil {
		t.Fatalf("captureOpencodeFlavour: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "sse.txt"))
	if err != nil {
		t.Fatalf("read sse.txt: %v", err)
	}
	if !strings.Contains(string(data), "session.idle") {
		t.Fatalf("sse.txt = %q, want session.idle", data)
	}
}

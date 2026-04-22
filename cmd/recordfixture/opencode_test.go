package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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

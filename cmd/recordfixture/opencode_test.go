package main

import (
	"context"
	"encoding/json"
	"errors"
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
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "subscription failed", http.StatusBadGateway)
	}))
	defer server.Close()

	ready := make(chan struct{})
	err := streamSSE(context.Background(), server.URL, nil, io.Discard, ready)
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

// v2FakeServer is the minimum of opencode 2.x the recorder drives: the
// authenticated /api tree with a data-enveloped session, an event stream and
// a prompt that plays out on that stream.
func v2FakeServer(t *testing.T, auth map[string]string, onPrompt func(sessionID string, publish func(string))) *httptest.Server {
	t.Helper()
	var (
		mu       sync.Mutex
		streams  []chan string
		streamed bool
	)
	publish := func(frame string) {
		mu.Lock()
		subs := append([]chan string(nil), streams...)
		mu.Unlock()
		for _, ch := range subs {
			ch <- frame
		}
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if want := auth["Authorization"]; want != "" && r.Header.Get("Authorization") != want {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/session":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode session body: %v", err)
			}
			if _, ok := body["location"]; !ok {
				t.Errorf("session body missing location: %v", body)
			}
			if _, ok := body["permissions"]; !ok {
				t.Errorf("session body missing permissions: %v", body)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{"id":"ses_123","projectID":"p"}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/event":
			ch := make(chan string, 8)
			mu.Lock()
			streams = append(streams, ch)
			streamed = true
			mu.Unlock()
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			for {
				select {
				case msg := <-ch:
					_, _ = io.WriteString(w, msg)
					if f, ok := w.(http.Flusher); ok {
						f.Flush()
					}
				case <-r.Context().Done():
					return
				}
			}
		case r.Method == http.MethodPost && r.URL.Path == "/api/session/ses_123/prompt":
			var body struct {
				Text string `json:"text"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Text == "" {
				t.Errorf("prompt body = %+v err = %v, want a text payload", body, err)
			}
			mu.Lock()
			ready := streamed
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":{"id":"msg_u","sessionID":"ses_123","type":"user"}}`))
			if ready && onPrompt != nil {
				onPrompt("ses_123", publish)
			}
		case r.Method == http.MethodGet && r.URL.Path == "/api/session/ses_123/message":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"m1","type":"assistant","content":[{"type":"text","text":"OK"}],"time":{"created":2}},{"id":"msg_u","type":"user","text":"hi","time":{"created":1}}],"cursor":{}}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/api/session/ses_123":
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func TestCaptureOpencodeFlavourRequiresExecutionEnd(t *testing.T) {
	server := v2FakeServer(t, nil, func(sessionID string, publish func(string)) {
		publish("data: {\"id\":\"e\",\"type\":\"session.text.ended\",\"data\":{\"sessionID\":\"ses_123\",\"assistantMessageID\":\"m1\",\"ordinal\":0,\"text\":\"OK\"}}\n\n")
	})
	defer server.Close()

	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	err := captureOpencodeFlavour(ctx, server.URL, nil, dir, "hi", "")
	if err == nil {
		t.Fatal("expected error when the execution end event is missing")
	}
	if !strings.Contains(err.Error(), "session.execution") {
		t.Fatalf("error = %q, want mention of the execution end", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "sse.txt")); !os.IsNotExist(statErr) {
		t.Fatalf("sse.txt should not be written on truncated capture, stat err = %v", statErr)
	}
}

func TestCaptureOpencodeFlavourRecordsTheTurn(t *testing.T) {
	auth := opencodeAuth("pw")
	server := v2FakeServer(t, auth, func(sessionID string, publish func(string)) {
		// Another session's event must not be kept; this session's are.
		publish("data: {\"id\":\"e0\",\"type\":\"session.text.ended\",\"data\":{\"sessionID\":\"ses_other\",\"assistantMessageID\":\"x\",\"ordinal\":0,\"text\":\"not ours\"}}\n\n")
		publish("data: {\"id\":\"e1\",\"type\":\"session.text.ended\",\"data\":{\"sessionID\":\"ses_123\",\"assistantMessageID\":\"m1\",\"ordinal\":0,\"text\":\"OK\"}}\n\n")
		publish("data: {\"id\":\"e2\",\"type\":\"session.execution.succeeded\",\"data\":{\"sessionID\":\"ses_123\"}}\n\n")
	})
	defer server.Close()

	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := captureOpencodeFlavour(ctx, server.URL, auth, dir, "hi", `{"type":"object"}`); err != nil {
		t.Fatalf("captureOpencodeFlavour: %v", err)
	}

	sse, err := os.ReadFile(filepath.Join(dir, "sse.txt"))
	if err != nil {
		t.Fatalf("read sse.txt: %v", err)
	}
	if !strings.Contains(string(sse), "session.execution.succeeded") || !strings.Contains(string(sse), `"text":"OK"`) {
		t.Fatalf("sse.txt = %q, want the session's events through the execution end", sse)
	}
	if strings.Contains(string(sse), "ses_other") {
		t.Fatalf("sse.txt = %q, want other sessions' events dropped", sse)
	}
	session, err := os.ReadFile(filepath.Join(dir, "session.json"))
	if err != nil || !strings.Contains(string(session), `"data":{"id":"ses_123"`) {
		t.Fatalf("session.json = %q err = %v", session, err)
	}
	messages, err := os.ReadFile(filepath.Join(dir, "messages.json"))
	if err != nil || !strings.Contains(string(messages), `"type":"assistant"`) {
		t.Fatalf("messages.json = %q err = %v", messages, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "message.json")); !os.IsNotExist(err) {
		t.Fatal("the v1 message.json must not be written")
	}
}

func TestOpencodeSchemaPrompt(t *testing.T) {
	if got := opencodeSchemaPrompt("hi", ""); got != "hi" {
		t.Fatalf("plain prompt = %q", got)
	}
	got := opencodeSchemaPrompt("hi", `{"type":"object"}`)
	if !strings.HasPrefix(got, "hi\n") || !strings.Contains(got, `The JSON must match this schema exactly: {"type":"object"}`) {
		t.Fatalf("schema prompt = %q", got)
	}
}

func TestWaitHealthReturnsContextCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := waitHealth(ctx, server.URL, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want %v", err, context.Canceled)
	}
}

func TestWaitHealthFailsFastWithoutTheV2API(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/info" {
			t.Errorf("probe path = %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") == "" {
			t.Error("probe must authenticate")
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	start := time.Now()
	err := waitHealth(context.Background(), server.URL, opencodeAuth("pw"))
	if err == nil || !strings.Contains(err.Error(), "opencode 2.x") {
		t.Fatalf("error = %v, want the version requirement", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("waitHealth took %v on a 404, want fast failure", time.Since(start))
	}
}

func TestSSEEventIdentity(t *testing.T) {
	eventType, sessionID := sseEventIdentity([]byte("data: {\"id\":\"e\",\"type\":\"session.step.ended\",\"data\":{\"sessionID\":\"ses_1\"}}"))
	if eventType != "session.step.ended" || sessionID != "ses_1" {
		t.Fatalf("identity = %q/%q", eventType, sessionID)
	}
	eventType, sessionID = sseEventIdentity([]byte("data: {\"id\":\"e\",\"type\":\"server.connected\",\"data\":{}}"))
	if eventType != "server.connected" || sessionID != "" {
		t.Fatalf("identity = %q/%q", eventType, sessionID)
	}
	if got, _ := sseEventIdentity([]byte(": heartbeat")); got != "" {
		t.Fatalf("comment frame identity = %q", got)
	}
}

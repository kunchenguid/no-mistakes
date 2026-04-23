package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFakeOpencodeServerUnsubscribeLeavesCopiedSubscriberSafe(t *testing.T) {
	srv := newFakeOpencodeServer(defaultScenario())
	ch := make(chan []byte, 1)
	srv.subscribe(ch)

	srv.mu.Lock()
	subs := append([]chan []byte(nil), srv.subscribers...)
	srv.mu.Unlock()

	srv.unsubscribe(ch)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("send to copied subscriber panicked after unsubscribe: %v", r)
		}
	}()

	subs[0] <- []byte("data: {}\n\n")
}

func TestFakeOpencodeServerConfiguredFixtureLoadFailureIsNotSilent(t *testing.T) {
	t.Setenv("FAKEAGENT_FIXTURE", t.TempDir())
	fixtureDir := filepath.Join(os.Getenv("FAKEAGENT_FIXTURE"), "opencode", "structured")
	if err := os.MkdirAll(fixtureDir, 0o755); err != nil {
		t.Fatalf("mkdir fixture dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(fixtureDir, "session.json"), []byte(`{"id":"sess-123"}`), 0o644); err != nil {
		t.Fatalf("write session fixture: %v", err)
	}

	srv := newFakeOpencodeServer(defaultScenario())
	req := httptest.NewRequest(http.MethodGet, "/global/health", nil)
	rec := httptest.NewRecorder()

	srv.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
}

func TestPatchOpencodeMessageRequiresRecordedInfo(t *testing.T) {
	t.Helper()

	_, err := patchOpencodeMessage([]byte(`{"id":"msg-123"}`), Action{
		Structured: map[string]any{"summary": "ok"},
	})
	if err == nil {
		t.Fatal("expected malformed recorded message to fail")
	}
	if !containsAll(err.Error(), []string{"message", "info"}) {
		t.Fatalf("error = %q, want mention of missing info", err)
	}
}

func containsAll(s string, want []string) bool {
	for _, part := range want {
		if !strings.Contains(s, part) {
			return false
		}
	}
	return true
}

func TestPatchOpencodeMessagePreservesRecordedInfo(t *testing.T) {
	t.Helper()

	raw := []byte(`{"info":{"id":"msg-123","role":"assistant"}}`)
	patched, err := patchOpencodeMessage(raw, Action{Structured: map[string]any{"summary": "ok"}})
	if err != nil {
		t.Fatalf("patchOpencodeMessage: %v", err)
	}
	var resp struct {
		Info struct {
			ID         string          `json:"id"`
			Role       string          `json:"role"`
			Structured json.RawMessage `json:"structured"`
		} `json:"info"`
	}
	if err := json.Unmarshal(patched, &resp); err != nil {
		t.Fatalf("unmarshal patched response: %v", err)
	}
	if resp.Info.ID != "msg-123" || resp.Info.Role != "assistant" {
		t.Fatalf("patched info = %+v, want recorded id and role", resp.Info)
	}
	if string(resp.Info.Structured) != `{"summary":"ok"}` {
		t.Fatalf("structured = %s, want patched payload", resp.Info.Structured)
	}
}

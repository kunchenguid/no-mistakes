package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

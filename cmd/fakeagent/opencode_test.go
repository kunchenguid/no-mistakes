package main

import (
	"bytes"
	"encoding/json"
	"fmt"
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

func TestOpencodeFixtureRewritesSessionIDsPerRequest(t *testing.T) {
	t.Helper()

	fixture := &opencodeFixture{
		sessionID: "ses_recorded",
		session:   []byte(`{"id":"ses_recorded","slug":"recorded"}`),
		sse:       []byte("data: {\"payload\":{\"type\":\"message.updated\",\"properties\":{\"sessionID\":\"ses_recorded\",\"info\":{\"sessionID\":\"ses_recorded\"}}}}\n\ndata: {\"payload\":{\"type\":\"session.idle\",\"properties\":{\"sessionID\":\"ses_recorded\"}}}\n\n"),
		message:   []byte(`{"info":{"id":"msg-123","role":"assistant","sessionID":"ses_recorded"},"parts":[{"type":"text","sessionID":"ses_recorded","messageID":"msg-123"}]}`),
	}

	firstSession, err := rewriteOpencodeFixtureSession(fixture, "ses_first")
	if err != nil {
		t.Fatalf("rewrite session: %v", err)
	}
	secondSession, err := rewriteOpencodeFixtureSession(fixture, "ses_second")
	if err != nil {
		t.Fatalf("rewrite session again: %v", err)
	}
	if bytes.Equal(firstSession, secondSession) {
		t.Fatal("rewritten sessions should differ per request")
	}
	if bytes.Contains(firstSession, []byte("ses_recorded")) || bytes.Contains(secondSession, []byte("ses_recorded")) {
		t.Fatal("rewritten session payload should not keep recorded session ID")
	}

	rewrittenSSE, err := rewriteOpencodeFixtureSSE(fixture, Action{Structured: map[string]any{"summary": "ok"}}, "ses_first")
	if err != nil {
		t.Fatalf("rewrite sse: %v", err)
	}
	if !bytes.Contains(rewrittenSSE, []byte("ses_first")) {
		t.Fatalf("rewritten sse = %s, want new session ID", rewrittenSSE)
	}
	if bytes.Contains(rewrittenSSE, []byte("ses_recorded")) {
		t.Fatalf("rewritten sse = %s, want recorded session ID removed", rewrittenSSE)
	}

	rewrittenMessage, err := rewriteOpencodeFixtureMessage(fixture, Action{Structured: map[string]any{"summary": "ok"}}, "ses_first")
	if err != nil {
		t.Fatalf("rewrite message: %v", err)
	}
	if !bytes.Contains(rewrittenMessage, []byte("ses_first")) {
		t.Fatalf("rewritten message = %s, want new session ID", rewrittenMessage)
	}
	if bytes.Contains(rewrittenMessage, []byte("ses_recorded")) {
		t.Fatalf("rewritten message = %s, want recorded session ID removed", rewrittenMessage)
	}
}

func TestFakeOpencodeFixturePlainRunRewritesRecordedText(t *testing.T) {
	t.Helper()

	srv := newFakeOpencodeServer(&Scenario{Actions: []Action{{
		Match: "plain",
		Text:  "scenario text",
	}}})
	srv.fixture = &opencodeFixture{
		sessionID: "ses_recorded",
		session:   []byte(`{"id":"ses_recorded"}`),
		sse: []byte(strings.Join([]string{
			`data: {"payload":{"type":"message.part.updated","properties":{"sessionID":"ses_recorded","part":{"id":"p1","messageID":"msg-123","sessionID":"ses_recorded","type":"text","text":"recorded text","metadata":{"openai":{"phase":"final_answer"}}}}}}`,
			"",
			`data: {"payload":{"type":"message.part.delta","properties":{"sessionID":"ses_recorded","partID":"p1","field":"text","delta":"recorded text"}}}`,
			"",
			`data: {"payload":{"type":"session.idle","properties":{"sessionID":"ses_recorded"}}}`,
			"",
		}, "\n")),
		message: []byte(`{"info":{"id":"msg-123","role":"assistant","sessionID":"ses_recorded"},"parts":[{"type":"text","text":"recorded text","metadata":{"openai":{"phase":"final_answer"}}}]}`),
	}

	createReq := httptest.NewRequest(http.MethodPost, "/session", strings.NewReader(fmt.Sprintf(`{"directory":%q}`, t.TempDir())))
	createReq.Header.Set("Content-Type", "application/json")
	createRec := httptest.NewRecorder()
	srv.routes().ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusOK {
		t.Fatalf("create session status = %d, want %d", createRec.Code, http.StatusOK)
	}

	var session struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(createRec.Body.Bytes(), &session); err != nil {
		t.Fatalf("unmarshal session: %v", err)
	}

	ch := make(chan []byte, 1)
	srv.subscribe(ch)
	defer srv.unsubscribe(ch)

	msgReq := httptest.NewRequest(http.MethodPost, "/session/"+session.ID+"/message", strings.NewReader(`{"parts":[{"type":"text","text":"plain prompt"}]}`))
	msgReq.Header.Set("Content-Type", "application/json")
	msgRec := httptest.NewRecorder()
	srv.routes().ServeHTTP(msgRec, msgReq)
	if msgRec.Code != http.StatusOK {
		t.Fatalf("message status = %d, want %d", msgRec.Code, http.StatusOK)
	}

	broadcast := <-ch
	if !bytes.Contains(broadcast, []byte("scenario text")) {
		t.Fatalf("broadcast = %s, want scenario text", broadcast)
	}
	if bytes.Contains(broadcast, []byte("recorded text")) {
		t.Fatalf("broadcast = %s, want recorded text removed", broadcast)
	}
	if !bytes.Contains(msgRec.Body.Bytes(), []byte("scenario text")) {
		t.Fatalf("message = %s, want scenario text", msgRec.Body.Bytes())
	}
	if bytes.Contains(msgRec.Body.Bytes(), []byte("recorded text")) {
		t.Fatalf("message = %s, want recorded text removed", msgRec.Body.Bytes())
	}
}

func TestFakeOpencodeServerAppliesEditsInSessionDirectory(t *testing.T) {
	t.Helper()

	wd := t.TempDir()
	dir := filepath.Join(wd, "session-dir")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir session dir: %v", err)
	}
	t.Chdir(wd)

	srv := newFakeOpencodeServer(&Scenario{Actions: []Action{{
		Match: "fix",
		Edits: []Edit{{Path: filepath.Join("nested", "note.txt"), New: "hello\n"}},
	}}})

	createReq := httptest.NewRequest(http.MethodPost, "/session", strings.NewReader(fmt.Sprintf(`{"directory":%q}`, dir)))
	createReq.Header.Set("Content-Type", "application/json")
	createRec := httptest.NewRecorder()
	srv.routes().ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusOK {
		t.Fatalf("create session status = %d, want %d", createRec.Code, http.StatusOK)
	}

	var session struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(createRec.Body.Bytes(), &session); err != nil {
		t.Fatalf("unmarshal session: %v", err)
	}
	if session.ID == "" {
		t.Fatal("expected session id")
	}

	msgReq := httptest.NewRequest(http.MethodPost, "/session/"+session.ID+"/message", strings.NewReader(`{"parts":[{"type":"text","text":"please fix this"}]}`))
	msgReq.Header.Set("Content-Type", "application/json")
	msgRec := httptest.NewRecorder()
	srv.routes().ServeHTTP(msgRec, msgReq)
	if msgRec.Code != http.StatusOK {
		t.Fatalf("message status = %d, want %d", msgRec.Code, http.StatusOK)
	}

	if _, err := os.Stat(filepath.Join(dir, "nested", "note.txt")); err != nil {
		t.Fatalf("expected edit in session directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wd, "nested", "note.txt")); !os.IsNotExist(err) {
		t.Fatalf("working directory edit err = %v, want not exist", err)
	}
}

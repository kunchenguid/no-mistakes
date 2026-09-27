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
	if err := os.WriteFile(filepath.Join(fixtureDir, "session.json"), []byte(`{"data":{"id":"sess-123"}}`), 0o644); err != nil {
		t.Fatalf("write session fixture: %v", err)
	}

	srv := newFakeOpencodeServer(defaultScenario())
	req := httptest.NewRequest(http.MethodGet, "/api/info", nil)
	rec := httptest.NewRecorder()

	srv.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusInternalServerError)
	}
}

// TestFakeOpencodeServerRequiresTheServePassword pins the contract the
// daemon relies on: the real server adopts OPENCODE_SERVER_PASSWORD and
// refuses every /api request that does not carry it as basic auth.
func TestFakeOpencodeServerRequiresTheServePassword(t *testing.T) {
	t.Setenv("OPENCODE_SERVER_PASSWORD", "secret")
	srv := newFakeOpencodeServer(defaultScenario())

	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/info", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", rec.Code)
	}

	wrongUser := httptest.NewRequest(http.MethodGet, "/api/info", nil)
	wrongUser.SetBasicAuth("admin", "secret")
	rec = httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, wrongUser)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong-user status = %d, want 401", rec.Code)
	}

	ok := httptest.NewRequest(http.MethodGet, "/api/info", nil)
	ok.SetBasicAuth("opencode", "secret")
	rec = httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, ok)
	if rec.Code != http.StatusOK {
		t.Fatalf("authenticated status = %d, want 200", rec.Code)
	}
}

func TestPatchOpencodeMessagesRequiresRecordedData(t *testing.T) {
	_, err := patchOpencodeMessages([]byte(`{"id":"msg-123"}`), "ok")
	if err == nil {
		t.Fatal("expected malformed recorded message list to fail")
	}
	if !containsAll(err.Error(), []string{"messages", "data"}) {
		t.Fatalf("error = %q, want mention of missing data", err)
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

func TestPatchOpencodeMessagesRewritesOnlyTheLastAssistantText(t *testing.T) {
	raw := []byte(`{"data":[` +
		`{"id":"idle","type":"idle","outcome":"succeeded","time":{"created":40}},` +
		`{"id":"m2","type":"assistant","content":[{"type":"text","text":"recorded final"}],"tokens":{"input":1,"output":2},"time":{"created":30}},` +
		`{"id":"m1","type":"assistant","content":[{"type":"text","text":"recorded narration"},{"type":"tool","name":"shell"}],"time":{"created":20}},` +
		`{"id":"u","type":"user","text":"prompt","time":{"created":10}}` +
		`],"cursor":{}}`)
	patched, err := patchOpencodeMessages(raw, `{"summary":"ok"}`)
	if err != nil {
		t.Fatalf("patchOpencodeMessages: %v", err)
	}
	var resp struct {
		Data []struct {
			ID      string `json:"id"`
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			Tokens map[string]int `json:"tokens"`
		} `json:"data"`
	}
	if err := json.Unmarshal(patched, &resp); err != nil {
		t.Fatalf("unmarshal patched response: %v", err)
	}
	if len(resp.Data) != 4 || resp.Data[0].Type != "idle" || resp.Data[3].Type != "user" {
		t.Fatalf("patched list lost its shape: %s", patched)
	}
	if resp.Data[1].Content[0].Text != `{"summary":"ok"}` || resp.Data[1].Tokens["input"] != 1 {
		t.Fatalf("last assistant text = %+v, want the scenario JSON with tokens kept", resp.Data[1])
	}
	if resp.Data[2].Content[0].Text != "" || resp.Data[2].Content[1].Type != "tool" {
		t.Fatalf("earlier assistant content = %+v, want narration blanked and the tool kept", resp.Data[2].Content)
	}
}

func createSession(t *testing.T, srv *fakeOpencodeServer, dir string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/session", strings.NewReader(fmt.Sprintf(`{"location":{"directory":%q},"permissions":[{"action":"*","resource":"*","effect":"allow"}]}`, dir)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("create session status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var session struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &session); err != nil {
		t.Fatalf("unmarshal session: %v", err)
	}
	if session.Data.ID == "" {
		t.Fatalf("session response carried no data.id: %s", rec.Body.String())
	}
	return session.Data.ID
}

func sendPrompt(t *testing.T, srv *fakeOpencodeServer, sessionID, text string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/session/"+sessionID+"/prompt", strings.NewReader(fmt.Sprintf(`{"text":%q}`, text)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("prompt status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	return rec
}

func listMessages(t *testing.T, srv *fakeOpencodeServer, sessionID string) []byte {
	t.Helper()
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/session/"+sessionID+"/message", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("messages status = %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	return rec.Body.Bytes()
}

// TestFakeOpencodeServerStructuredScenarioIsTheAssistantText pins the v2
// shape of structured output: there is no structured slot, the JSON is what
// the assistant writes, on the stream and in the message list alike.
func TestFakeOpencodeServerStructuredScenarioIsTheAssistantText(t *testing.T) {
	srv := newFakeOpencodeServer(&Scenario{Actions: []Action{{
		Match:         "raw",
		Text:          "scenario text",
		StructuredRaw: `"not an object"`,
	}}})
	sessionID := createSession(t, srv, t.TempDir())

	ch := make(chan []byte, 8)
	srv.subscribe(ch)
	defer srv.unsubscribe(ch)

	rec := sendPrompt(t, srv, sessionID, "raw prompt")
	var admitted struct {
		Data struct {
			Type    string `json:"type"`
			Payload struct {
				Text string `json:"text"`
			} `json:"payload"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &admitted); err != nil {
		t.Fatalf("unmarshal prompt response: %v", err)
	}
	if admitted.Data.Type != "user" || admitted.Data.Payload.Text != "raw prompt" {
		t.Fatalf("prompt response = %s, want the admitted user entry", rec.Body.String())
	}

	var streamed bytes.Buffer
	for len(ch) > 0 {
		streamed.Write(<-ch)
	}
	if !bytes.Contains(streamed.Bytes(), []byte(`\"not an object\"`)) {
		t.Fatalf("stream = %s, want the structured payload as assistant text", streamed.Bytes())
	}
	if !bytes.Contains(streamed.Bytes(), []byte("session.execution.succeeded")) {
		t.Fatalf("stream = %s, want the turn ended", streamed.Bytes())
	}

	msgs := listMessages(t, srv, sessionID)
	if !bytes.Contains(msgs, []byte(`\"not an object\"`)) || bytes.Contains(msgs, []byte("scenario text")) {
		t.Fatalf("messages = %s, want the structured payload and not the human text", msgs)
	}
	if !bytes.Contains(msgs, []byte(`"type":"idle"`)) {
		t.Fatalf("messages = %s, want the idle marker", msgs)
	}
}

func TestOpencodeFixtureRewritesSessionIDsPerRequest(t *testing.T) {
	fixture := &opencodeFixture{
		sessionID: "ses_recorded",
		session:   []byte(`{"data":{"id":"ses_recorded","projectID":"p"}}`),
		sse: []byte("data: {\"id\":\"e1\",\"type\":\"session.text.ended\",\"data\":{\"sessionID\":\"ses_recorded\",\"assistantMessageID\":\"m1\",\"ordinal\":0,\"text\":\"recorded\"}}\n\n" +
			"data: {\"id\":\"e2\",\"type\":\"session.execution.succeeded\",\"data\":{\"sessionID\":\"ses_recorded\"}}\n\n"),
		messages: []byte(`{"data":[{"id":"m1","type":"assistant","content":[{"type":"text","text":"recorded"}],"time":{"created":2}},{"id":"u","type":"user","text":"p","time":{"created":1}}],"cursor":{}}`),
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

	rewrittenSSE, err := rewriteOpencodeFixtureSSE(fixture, `{"summary":"ok"}`, "ses_first")
	if err != nil {
		t.Fatalf("rewrite sse: %v", err)
	}
	if !bytes.Contains(rewrittenSSE, []byte("ses_first")) || bytes.Contains(rewrittenSSE, []byte("ses_recorded")) {
		t.Fatalf("rewritten sse = %s, want the new session ID only", rewrittenSSE)
	}
	if !bytes.Contains(rewrittenSSE, []byte(`\"summary\":\"ok\"`)) || bytes.Contains(rewrittenSSE, []byte("recorded\"")) {
		t.Fatalf("rewritten sse = %s, want the scenario text in place of the recorded text", rewrittenSSE)
	}

	rewrittenMessages, err := rewriteOpencodeFixtureMessages(fixture, `{"summary":"ok"}`, "ses_first")
	if err != nil {
		t.Fatalf("rewrite messages: %v", err)
	}
	if bytes.Contains(rewrittenMessages, []byte("ses_recorded")) {
		t.Fatalf("rewritten messages = %s, want recorded session ID removed", rewrittenMessages)
	}
	if !bytes.Contains(rewrittenMessages, []byte(`\"summary\":\"ok\"`)) {
		t.Fatalf("rewritten messages = %s, want the scenario text", rewrittenMessages)
	}
}

// TestFakeOpencodeFixturePlainRunRewritesRecordedText replays a recorded
// multi-step turn: the narration step is blanked, the final step carries the
// scenario text, and delta streaming collapses to one delta so the stream and
// the ended snapshot agree.
func TestFakeOpencodeFixturePlainRunRewritesRecordedText(t *testing.T) {
	srv := newFakeOpencodeServer(&Scenario{Actions: []Action{{
		Match: "plain",
		Text:  "scenario text",
	}}})
	srv.fixture = &opencodeFixture{
		sessionID: "ses_recorded",
		session:   []byte(`{"data":{"id":"ses_recorded"}}`),
		sse: []byte(strings.Join([]string{
			`data: {"id":"e1","type":"session.text.ended","data":{"sessionID":"ses_recorded","assistantMessageID":"m1","ordinal":0,"text":"recorded narration"}}`,
			"",
			`data: {"id":"e2","type":"session.tool.called","data":{"sessionID":"ses_recorded","assistantMessageID":"m1","id":"t1","name":"shell","input":{},"executed":false}}`,
			"",
			`data: {"id":"e3","type":"session.text.delta","data":{"sessionID":"ses_recorded","assistantMessageID":"m2","ordinal":0,"delta":"recorded "}}`,
			"",
			`data: {"id":"e4","type":"session.text.delta","data":{"sessionID":"ses_recorded","assistantMessageID":"m2","ordinal":0,"delta":"final"}}`,
			"",
			`data: {"id":"e5","type":"session.text.ended","data":{"sessionID":"ses_recorded","assistantMessageID":"m2","ordinal":0,"text":"recorded final"}}`,
			"",
			`data: {"id":"e6","type":"session.execution.succeeded","data":{"sessionID":"ses_recorded"}}`,
			"",
		}, "\n")),
		messages: []byte(`{"data":[` +
			`{"id":"idle","type":"idle","outcome":"succeeded","time":{"created":4}},` +
			`{"id":"m2","type":"assistant","content":[{"type":"text","text":"recorded final"}],"time":{"created":3}},` +
			`{"id":"m1","type":"assistant","content":[{"type":"text","text":"recorded narration"},{"type":"tool","name":"shell"}],"time":{"created":2}},` +
			`{"id":"u","type":"user","text":"p","time":{"created":1}}],"cursor":{}}`),
	}

	sessionID := createSession(t, srv, t.TempDir())

	ch := make(chan []byte, 1)
	srv.subscribe(ch)
	defer srv.unsubscribe(ch)

	sendPrompt(t, srv, sessionID, "plain prompt")

	broadcast := <-ch
	if !bytes.Contains(broadcast, []byte("scenario text")) || bytes.Contains(broadcast, []byte("recorded")) {
		t.Fatalf("broadcast = %s, want scenario text and no recorded text", broadcast)
	}
	if !bytes.Contains(broadcast, []byte(sessionID)) || bytes.Contains(broadcast, []byte("ses_recorded")) {
		t.Fatalf("broadcast = %s, want the new session id", broadcast)
	}
	// The narration block is blank, the final block's first delta carries
	// the whole text and its second delta is empty, and the snapshot agrees.
	wantDeltas := []string{`"delta":"scenario text"`, `"delta":""`}
	for _, want := range wantDeltas {
		if !bytes.Contains(broadcast, []byte(want)) {
			t.Errorf("broadcast missing %s:\n%s", want, broadcast)
		}
	}
	if !bytes.Contains(broadcast, []byte(`"text":""`)) || !bytes.Contains(broadcast, []byte(`"text":"scenario text"`)) {
		t.Errorf("broadcast should blank the narration and carry the scenario text as the final snapshot:\n%s", broadcast)
	}

	msgs := listMessages(t, srv, sessionID)
	if !bytes.Contains(msgs, []byte("scenario text")) || bytes.Contains(msgs, []byte("recorded")) {
		t.Fatalf("messages = %s, want scenario text and no recorded text", msgs)
	}
	if !bytes.Contains(msgs, []byte(`"type":"tool"`)) {
		t.Fatalf("messages = %s, want the recorded tool content kept", msgs)
	}
}

func TestFakeOpencodeServerAppliesEditsInSessionDirectory(t *testing.T) {
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

	sessionID := createSession(t, srv, dir)
	sendPrompt(t, srv, sessionID, "please fix this")

	if _, err := os.Stat(filepath.Join(dir, "nested", "note.txt")); err != nil {
		t.Fatalf("expected edit in session directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wd, "nested", "note.txt")); !os.IsNotExist(err) {
		t.Fatalf("working directory edit err = %v, want not exist", err)
	}
}

func TestFakeOpencodeServerLifecycleRoutes(t *testing.T) {
	srv := newFakeOpencodeServer(defaultScenario())
	sessionID := createSession(t, srv, t.TempDir())

	for _, tc := range []struct {
		method, path string
		want         int
	}{
		{http.MethodPost, "/api/experimental/session/" + sessionID + "/wait", http.StatusNoContent},
		{http.MethodPost, "/api/session/" + sessionID + "/interrupt", http.StatusOK},
		{http.MethodDelete, "/api/session/" + sessionID, http.StatusNoContent},
		{http.MethodPost, "/session", http.StatusNotFound},
	} {
		rec := httptest.NewRecorder()
		srv.routes().ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
		if rec.Code != tc.want {
			t.Errorf("%s %s = %d, want %d", tc.method, tc.path, rec.Code, tc.want)
		}
	}
}

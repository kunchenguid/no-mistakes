package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/runenv"
)

func TestOpencodeAgent_CloseWithoutServer(t *testing.T) {
	a := &opencodeAgent{bin: "opencode"}
	if err := a.Close(); err != nil {
		t.Errorf("Close without server should not error: %v", err)
	}
}

// TestOpencodeAgent_FullFlow drives the whole v2 session lifecycle: an
// authenticated session create with the run's cwd and blanket permissions,
// the event stream subscribed before the prompt is admitted, the prompt as a
// `text` payload carrying the schema instructions, the turn followed to its
// execution end, and the structured output read back from the message list.
func TestOpencodeAgent_FullFlow(t *testing.T) {
	const answer = `{"summary":"all good"}`
	f := newFakeOpencode(t, fakeOpencodeTurn{
		events:   ocTextTurn("m1", answer, 100, 50),
		messages: ocMessages().user("p").assistantText("m1", answer, 100, 50).idle("succeeded").newestFirst(),
	})

	var chunks []string
	cwd := t.TempDir()
	result, err := f.agent().Run(context.Background(), RunOpts{
		Prompt:     "review this code",
		CWD:        cwd,
		JSONSchema: json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string"}},"required":["summary"]}`),
		OnChunk:    func(text string) { chunks = append(chunks, text) },
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var output map[string]any
	if err := json.Unmarshal(result.Output, &output); err != nil {
		t.Fatalf("failed to parse output %q: %v", result.Output, err)
	}
	if output["summary"] != "all good" {
		t.Errorf("output = %v", output)
	}
	if result.Usage.InputTokens != 100 || result.Usage.OutputTokens != 50 || !result.Usage.Reported {
		t.Errorf("usage = %+v, want 100/50 reported", result.Usage)
	}
	if strings.Join(chunks, "") != answer {
		t.Errorf("chunks = %q, want the streamed answer", chunks)
	}

	body := f.lastSessionBody()
	if loc, _ := body["location"].(map[string]any); loc["directory"] != cwd {
		t.Errorf("session location = %#v, want the run's cwd", body["location"])
	}
	perms, _ := body["permissions"].([]any)
	if len(perms) != 1 {
		t.Fatalf("permissions = %#v, want one blanket rule", body["permissions"])
	}
	if rule, _ := perms[0].(map[string]any); rule["action"] != "*" || rule["resource"] != "*" || rule["effect"] != "allow" {
		t.Errorf("permission rule = %#v", perms[0])
	}
	if _, pinned := body["model"]; pinned {
		t.Errorf("session pinned a model with no profile: %#v", body["model"])
	}

	prompt := f.lastPrompt()
	if !strings.HasPrefix(prompt, "review this code") || !strings.Contains(prompt, "must match this schema exactly") {
		t.Errorf("prompt = %q, want the schema instructions appended", prompt)
	}

	for _, want := range []string{
		"POST /api/session",
		"GET /api/event",
		"POST /api/session/ses_1/prompt",
		"GET /api/session/ses_1/message",
		"DELETE /api/session/ses_1",
	} {
		if f.called(want) == 0 {
			t.Errorf("expected %s", want)
		}
	}
	if f.calledMatching("POST", "/api/session/ses_1/interrupt") != 0 {
		t.Error("a turn that finished must not be interrupted")
	}
}

func TestOpencodeAgent_NoSchemaReturnsTheTurnText(t *testing.T) {
	f := newFakeOpencode(t, fakeOpencodeTurn{
		events:   ocTextTurn("m1", "plain answer", 3, 4),
		messages: ocMessages().user("p").assistantText("m1", "plain answer", 3, 4).idle("succeeded").newestFirst(),
	})
	result, err := f.agent().Run(context.Background(), RunOpts{Prompt: "say something", CWD: t.TempDir()})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Text != "plain answer" || result.Output != nil {
		t.Errorf("result text = %q output = %s, want the assistant text and no structured output", result.Text, result.Output)
	}
	if f.lastPrompt() != "say something" {
		t.Errorf("prompt = %q, want it unchanged without a schema", f.lastPrompt())
	}
}

// TestOpencodeAgent_ToolTurnJoinsStepMessages covers v2's per-step assistant
// messages: a tool step and the final text are separate messages, and the
// output is the turn's text with the step texts separated, exactly what the
// stream showed.
func TestOpencodeAgent_ToolTurnJoinsStepMessages(t *testing.T) {
	f := newFakeOpencode(t, fakeOpencodeTurn{
		events: []string{
			ocTextEnded("m1", 0, "Looking at the file."),
			ocToolCalled("m1"),
			ocStepEnded("m1", 2, 9),
			ocTextEnded("m2", 0, `{"summary":"done"}`),
			ocStepEnded("m2", 2, 4),
			ocSucceeded(),
		},
		messages: ocMessages().user("p").
			assistantText("m1", "Looking at the file.", 2, 9).
			assistantTool("m1b").
			assistantText("m2", `{"summary":"done"}`, 2, 4).
			idle("succeeded").newestFirst(),
	})
	var chunks []string
	result, err := f.agent().Run(context.Background(), RunOpts{
		Prompt:     "review",
		CWD:        t.TempDir(),
		JSONSchema: json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string"}},"required":["summary"]}`),
		OnChunk:    func(text string) { chunks = append(chunks, text) },
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(result.Output) != `{"summary":"done"}` {
		t.Errorf("output = %s, want the JSON extracted from the joined text", result.Output)
	}
	if strings.Join(chunks, "|") != "Looking at the file.|\n\n|{\"summary\":\"done\"}" {
		t.Errorf("chunks = %q, want the steps separated", chunks)
	}
	// 2+9 from the first step, 2+9 from the tool message, 2+4 from the last.
	if result.Usage.InputTokens != 6 || result.Usage.OutputTokens != 22 {
		t.Errorf("usage = %d/%d, want every assistant message summed", result.Usage.InputTokens, result.Usage.OutputTokens)
	}
}

func TestOpencodeAgent_StructuredOutputViolationIsRejected(t *testing.T) {
	f := newFakeOpencode(t, fakeOpencodeTurn{
		events:   ocTextTurn("m1", `{"other":1}`, 1, 1),
		messages: ocMessages().user("p").assistantText("m1", `{"other":1}`, 1, 1).idle("succeeded").newestFirst(),
	})
	_, err := runFakeOpencode(t, f)
	if err == nil {
		t.Fatal("expected the schema violation to fail")
	}
	if !IsStructuredOutputRejected(err) {
		t.Errorf("a schema violation must be reported as a structured-output rejection, got %v", err)
	}
	if f.sessionCount() != 1 {
		t.Errorf("sessions = %d, want no retry of a rejected output", f.sessionCount())
	}
}

// TestOpencodeAgent_MessageListFailureFallsBackToStreamedText keeps a
// finished turn usable when its record cannot be read: the stream saw every
// event, so what it streamed is the output and its step usage is the usage.
func TestOpencodeAgent_MessageListFailureFallsBackToStreamedText(t *testing.T) {
	f := newFakeOpencode(t, fakeOpencodeTurn{
		events:         ocTextTurn("m1", `{"summary":"streamed"}`, 7, 8),
		messagesStatus: http.StatusInternalServerError,
	})
	result, err := runFakeOpencode(t, f)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(result.Output) != `{"summary":"streamed"}` {
		t.Errorf("output = %s", result.Output)
	}
	if result.Usage.InputTokens != 7 || result.Usage.OutputTokens != 8 {
		t.Errorf("usage = %+v, want the stream's step usage", result.Usage)
	}
}

func TestOpencodeAgent_InterruptedTurnFailsWithoutRetry(t *testing.T) {
	defer withFastBackoff(t)()
	f := newFakeOpencode(t, fakeOpencodeTurn{
		events:   []string{ocTextEnded("m1", 0, "partial"), ocInterrupted("shutdown")},
		messages: ocMessages().user("p").assistantText("m1", "partial", 1, 1).idle("interrupted").newestFirst(),
	})
	_, err := runFakeOpencode(t, f)
	if err == nil {
		t.Fatal("expected an interrupted turn to fail")
	}
	if !strings.Contains(err.Error(), "interrupted (shutdown)") {
		t.Errorf("error = %q, want the interrupt reason", err)
	}
	if f.sessionCount() != 1 {
		t.Errorf("sessions = %d, want no retry", f.sessionCount())
	}
}

// TestOpencodeAgent_SucceededTurnWithOnlyAnErrorSurfacesIt covers a turn
// that ended "succeeded" but wrote nothing except an error on its last
// assistant message: the error is the actionable fact, not "no text output".
func TestOpencodeAgent_SucceededTurnWithOnlyAnErrorSurfacesIt(t *testing.T) {
	f := newFakeOpencode(t, fakeOpencodeTurn{
		events: []string{ocSucceeded()},
		messages: ocMessages().user("p").
			assistantError("m1", `{"type":"provider.invalid-request","message":"model rejected the request","status":400}`).
			idle("succeeded").newestFirst(),
	})
	_, err := runFakeOpencode(t, f)
	if err == nil {
		t.Fatal("expected the recorded error to surface")
	}
	for _, want := range []string{"provider.invalid-request", "400", "model rejected the request"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should carry %q", err, want)
		}
	}
}

func TestOpencodeAgent_WrongPasswordIsRefusedAtSessionCreate(t *testing.T) {
	f := newFakeOpencode(t)
	a := f.agent()
	a.password = "not-the-password"
	_, err := a.Run(context.Background(), RunOpts{Prompt: "x", CWD: t.TempDir()})
	if err == nil {
		t.Fatal("expected the unauthenticated session create to fail")
	}
	if !strings.Contains(err.Error(), "create session") || !strings.Contains(err.Error(), "401") {
		t.Errorf("error = %q, want the 401 on session create", err)
	}
}

func TestOpencodeAgent_EventStreamCarriesAuth(t *testing.T) {
	// The fake refuses every unauthenticated /api request, so a full flow
	// passing proves the stream, the prompt, the list and the delete all
	// carried the header. This pins the one that is easy to forget: the
	// stream is a hand-rolled request, not a doJSON call.
	f := newFakeOpencode(t, fakeOpencodeTurn{
		events:   ocTextTurn("m1", "ok", 1, 1),
		messages: ocMessages().user("p").assistantText("m1", "ok", 1, 1).idle("succeeded").newestFirst(),
	})
	if _, err := f.agent().Run(context.Background(), RunOpts{Prompt: "x", CWD: t.TempDir()}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if f.called("GET /api/event") != 1 {
		t.Errorf("event stream calls = %d, want 1", f.called("GET /api/event"))
	}
}

func TestOpencodeAuthHeaders(t *testing.T) {
	headers := opencodeAuthHeaders("s3cret")
	// base64("opencode:s3cret")
	if headers["Authorization"] != "Basic b3BlbmNvZGU6czNjcmV0" {
		t.Errorf("Authorization = %q", headers["Authorization"])
	}
}

// TestOpencodeHealthProbe pins the fast failures: a server that is up but
// speaks no v2 API, or refuses the generated password, must not spend the
// whole health deadline being polled.
func TestOpencodeHealthProbe(t *testing.T) {
	probe := opencodeHealthProbe("pw")
	if probe.path != "/api/info" {
		t.Errorf("path = %q, want /api/info", probe.path)
	}
	if probe.headers["Authorization"] == "" {
		t.Error("probe must authenticate")
	}
	if err := probe.reject(http.StatusNotFound); err == nil || !strings.Contains(err.Error(), "opencode 2.x") {
		t.Errorf("404 should name the version requirement, got %v", err)
	}
	if err := probe.reject(http.StatusUnauthorized); err == nil || !strings.Contains(err.Error(), opencodeServerPasswordEnv) {
		t.Errorf("401 should name the password variable, got %v", err)
	}
	if err := probe.reject(http.StatusServiceUnavailable); err != nil {
		t.Errorf("a 503 during boot must keep polling, got %v", err)
	}
}

func TestWaitForHealth_RejectedStatusFailsFast(t *testing.T) {
	var gotAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	srv := &managedServer{port: mustParsePort(server.URL), exited: make(chan struct{}), healthTimeout: 5 * time.Second}
	start := time.Now()
	err := srv.waitForHealth(context.Background(), opencodeHealthProbe("pw"))
	if err == nil || !strings.Contains(err.Error(), "no v2 API") {
		t.Fatalf("expected the 404 to fail the probe, got %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Errorf("rejected status should fail fast, took %v", time.Since(start))
	}
	if gotAuth != opencodeAuthHeaders("pw")["Authorization"] {
		t.Errorf("health probe Authorization = %q, want the generated password", gotAuth)
	}
}

func TestWaitForHealth_KeepsPollingUnrejectedStatuses(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	srv := &managedServer{port: mustParsePort(server.URL), exited: make(chan struct{}), healthTimeout: 5 * time.Second}
	if err := srv.waitForHealth(context.Background(), opencodeHealthProbe("pw")); err != nil {
		t.Fatalf("expected the probe to succeed once the server answered 200, got %v", err)
	}
	if calls < 3 {
		t.Errorf("calls = %d, want the 503s polled through", calls)
	}
}

// TestOpencodeAgent_EnsureServerHandsThePasswordToTheServe process pins the
// contract with `opencode serve`: the daemon generates the password and sets
// BOTH variables opencode consults, so an operator's OPENCODE_PASSWORD for
// some other server can never win over the one the daemon will authenticate
// with, and the serve argv stays the managed flags.
func TestOpencodeAgent_EnsureServerHandsThePasswordToTheServeProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script server stub")
	}
	dir := t.TempDir()
	capture := filepath.Join(dir, "env.txt")
	bin := filepath.Join(dir, "opencode")
	script := "#!/bin/sh\nprintf 'args:%s\\nserver:%s\\nclient:%s\\n' \"$*\" \"$OPENCODE_SERVER_PASSWORD\" \"$OPENCODE_PASSWORD\" > \"$CAPTURE_FILE\"\nexit 1\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OPENCODE_PASSWORD", "operators-other-server")

	a := &opencodeAgent{bin: bin, extraArgs: []string{"--cors", "http://x"}, subprocessContext: newSubprocessContext(runenv.Overlay{})}
	_, err := a.ensureServer(context.Background(), dir, []string{"CAPTURE_FILE=" + capture})
	if err == nil {
		t.Fatal("expected the stub server's early exit to fail startup")
	}

	data, err := os.ReadFile(capture)
	if err != nil {
		t.Fatalf("server stub did not run: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 {
		t.Fatalf("capture = %q", data)
	}
	args := strings.TrimPrefix(lines[0], "args:")
	if !strings.HasPrefix(args, "serve --cors http://x --hostname 127.0.0.1 --port ") || !strings.HasSuffix(args, " --print-logs") {
		t.Errorf("serve argv = %q", args)
	}
	server := strings.TrimPrefix(lines[1], "server:")
	client := strings.TrimPrefix(lines[2], "client:")
	if server == "" || len(server) < 32 {
		t.Errorf("OPENCODE_SERVER_PASSWORD = %q, want a generated secret", server)
	}
	if client != server {
		t.Errorf("OPENCODE_PASSWORD = %q, want it overridden to the generated %q", client, server)
	}
	if a.server != nil || a.password != "" {
		t.Error("a failed start must leave no server or password behind")
	}
}

func TestOpencodeAgent_ContextCancellationStopsTheTurn(t *testing.T) {
	// A turn whose stream never ends must stop with the caller's context.
	f := newFakeOpencode(t, fakeOpencodeTurn{events: []string{ocTextEnded("m1", 0, "thinking")}})
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	defer withFastEvidenceWait(t)()
	_, err := f.agent().Run(ctx, RunOpts{Prompt: "x", CWD: t.TempDir()})
	if err == nil {
		t.Fatal("expected the cancelled turn to fail")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !strings.Contains(err.Error(), "context deadline exceeded") {
		t.Errorf("error = %v, want the context deadline", err)
	}
	if f.calledMatching("POST", "/api/session/ses_1/interrupt") == 0 {
		t.Error("an abandoned turn must be interrupted")
	}
}

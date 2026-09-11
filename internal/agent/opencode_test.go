package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
)

func TestOpencodeAgent_CloseWithoutServer(t *testing.T) {
	a := &opencodeAgent{bin: "opencode"}
	if err := a.Close(); err != nil {
		t.Errorf("Close without server should not error: %v", err)
	}
}

// TestOpencodeAgent_FullFlow tests the full session lifecycle using a mock HTTP server.
func TestOpencodeAgent_FullFlow(t *testing.T) {
	calledPaths := make(map[string]bool)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calledPaths[r.Method+" "+r.URL.Path] = true
		switch {
		case r.URL.Path == "/session" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"id":"test-session-456"}`)

		case r.URL.Path == "/global/event" && r.Method == http.MethodGet:
			if r.Header.Get("Accept") != "text/event-stream" {
				t.Error("expected Accept: text/event-stream")
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			// Send text delta events then usage and idle
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"message.part.updated\",\"properties\":{\"sessionID\":\"test-session-456\",\"part\":{\"id\":\"p1\",\"messageID\":\"msg1\",\"type\":\"text\",\"text\":\"{\\\"success\\\":true,\\\"summary\\\":\\\"all good\\\"}\"}}}}\n\n")
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"message.updated\",\"properties\":{\"sessionID\":\"test-session-456\",\"info\":{\"id\":\"msg1\",\"role\":\"assistant\",\"tokens\":{\"input\":100,\"output\":50}}}}}\n\n")
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"session.idle\"}}\n\n")

		case r.URL.Path == "/session/test-session-456/message" && r.Method == http.MethodPost:
			// Return message response with structured output
			fmt.Fprint(w, `{"info":{"id":"msg1","role":"assistant","structured":{"success":true,"summary":"all good"},"tokens":{"input":100,"output":50}},"parts":[{"type":"text","text":"{\"success\":true,\"summary\":\"all good\"}"}]}`)

		case r.URL.Path == "/session/test-session-456" && r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusOK)

		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	a := &opencodeAgent{
		bin:    "opencode",
		server: &managedServer{port: mustParsePort(server.URL)},
	}

	var chunks []string
	result, err := a.Run(context.Background(), RunOpts{
		Prompt:     "review this code",
		CWD:        t.TempDir(),
		JSONSchema: json.RawMessage(`{"type":"object"}`),
		OnChunk:    func(text string) { chunks = append(chunks, text) },
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected result")
	}

	// Verify structured output from response
	var output map[string]any
	if err := json.Unmarshal(result.Output, &output); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}
	if output["success"] != true {
		t.Errorf("expected success=true, got %v", output["success"])
	}

	// Verify usage
	if result.Usage.InputTokens != 100 {
		t.Errorf("expected input tokens 100, got %d", result.Usage.InputTokens)
	}
	if result.Usage.OutputTokens != 50 {
		t.Errorf("expected output tokens 50, got %d", result.Usage.OutputTokens)
	}

	// Verify chunks received
	if len(chunks) < 1 {
		t.Error("expected at least 1 chunk")
	}

	// Verify key API calls were made
	if !calledPaths["POST /session"] {
		t.Error("expected POST /session call")
	}
	if !calledPaths["GET /global/event"] {
		t.Error("expected GET /global/event call")
	}
	if !calledPaths["POST /session/test-session-456/message"] {
		t.Error("expected POST /session/{id}/message call")
	}
}

func TestOpencodeAgent_BackfillsAssistantTextWhenStreamCannotClassifyOrphans(t *testing.T) {
	calledPaths := make(map[string]bool)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calledPaths[r.Method+" "+r.URL.Path] = true
		switch {
		case r.URL.Path == "/session" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"id":"test-session-789"}`)

		case r.URL.Path == "/global/event" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"message.part.delta\",\"properties\":{\"sessionID\":\"test-session-789\",\"field\":\"text\",\"partID\":\"p1\",\"delta\":\"hello \"}}}\n\n")
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"message.part.delta\",\"properties\":{\"sessionID\":\"test-session-789\",\"field\":\"text\",\"partID\":\"p2\",\"delta\":\"world\"}}}\n\n")
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"message.updated\",\"properties\":{\"sessionID\":\"test-session-789\",\"info\":{\"id\":\"msg1\",\"role\":\"assistant\",\"tokens\":{\"input\":100,\"output\":50}}}}}\n\n")
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"session.idle\"}}\n\n")

		case r.URL.Path == "/session/test-session-789/message" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"info":{"id":"msg1","role":"assistant","structured":{"summary":"hello world"},"tokens":{"input":100,"output":50}},"parts":[{"type":"text","text":"hello world"}]}`)

		case r.URL.Path == "/session/test-session-789" && r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusOK)

		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	a := &opencodeAgent{
		bin:    "opencode",
		server: &managedServer{port: mustParsePort(server.URL)},
	}

	var chunks []string
	result, err := a.Run(context.Background(), RunOpts{
		Prompt:     "review this code",
		CWD:        t.TempDir(),
		JSONSchema: json.RawMessage(`{"type":"object"}`),
		OnChunk:    func(text string) { chunks = append(chunks, text) },
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected result")
	}
	if len(chunks) != 1 || chunks[0] != "hello world" {
		t.Fatalf("expected one backfilled chunk, got %v", chunks)
	}
	if result.Text != "hello world" {
		t.Fatalf("expected result text 'hello world', got %q", result.Text)
	}
	if string(result.Output) != `{"summary":"hello world"}` {
		t.Fatalf("expected structured summary 'hello world', got %s", string(result.Output))
	}
	if !calledPaths[http.MethodGet+" /global/event"] {
		t.Fatal("expected event stream to be called")
	}
}

func TestOpencodeAgent_BackfillsAllAssistantResponseParts(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/session" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"id":"s1"}`)

		case r.URL.Path == "/global/event" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"session.idle\"}}\n\n")

		case r.URL.Path == "/session/s1/message" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"info":{"id":"msg1","role":"assistant"},"parts":[{"type":"text","text":"hello "},{"type":"text","text":"world"}]}`)

		case r.URL.Path == "/session/s1" && r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusOK)

		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	a := &opencodeAgent{
		bin:    "opencode",
		server: &managedServer{port: mustParsePort(server.URL)},
	}

	var chunks []string
	result, err := a.Run(context.Background(), RunOpts{
		Prompt:  "hello",
		CWD:     t.TempDir(),
		OnChunk: func(text string) { chunks = append(chunks, text) },
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Text != "hello world" {
		t.Fatalf("expected combined response text, got %q", result.Text)
	}
	if len(chunks) != 1 || chunks[0] != "hello world" {
		t.Fatalf("expected one combined backfill chunk, got %v", chunks)
	}
}

func TestOpencodeAgent_BackfillsMissingResponseSuffixAfterStreaming(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/session" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"id":"s1"}`)

		case r.URL.Path == "/global/event" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"message.part.updated\",\"properties\":{\"sessionID\":\"s1\",\"part\":{\"id\":\"p1\",\"messageID\":\"msg1\",\"type\":\"text\",\"text\":\"hello\"}}}}\n\n")
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"message.updated\",\"properties\":{\"sessionID\":\"s1\",\"info\":{\"id\":\"msg1\",\"role\":\"assistant\"}}}}\n\n")
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"session.idle\"}}\n\n")

		case r.URL.Path == "/session/s1/message" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"info":{"id":"msg1","role":"assistant"},"parts":[{"type":"text","text":"hello world"}]}`)

		case r.URL.Path == "/session/s1" && r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusOK)

		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	a := &opencodeAgent{
		bin:    "opencode",
		server: &managedServer{port: mustParsePort(server.URL)},
	}

	var chunks []string
	result, err := a.Run(context.Background(), RunOpts{
		Prompt:  "hello",
		CWD:     t.TempDir(),
		OnChunk: func(text string) { chunks = append(chunks, text) },
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Text != "hello world" {
		t.Fatalf("expected completed response text, got %q", result.Text)
	}
	if got := strings.Join(chunks, ""); got != "hello world" {
		t.Fatalf("expected streamed and backfilled text to form full response, got %q from %v", got, chunks)
	}
	if len(chunks) != 2 || chunks[1] != " world" {
		t.Fatalf("expected missing suffix backfill, got %v", chunks)
	}
}

func TestOpencodeAgent_BackfillsMissingResponseSuffixAfterToolStep(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/session" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"id":"s1"}`)

		case r.URL.Path == "/global/event" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"message.part.updated\",\"properties\":{\"sessionID\":\"s1\",\"part\":{\"id\":\"p1\",\"messageID\":\"msg1\",\"type\":\"text\",\"text\":\"hello\"}}}}\n\n")
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"message.updated\",\"properties\":{\"sessionID\":\"s1\",\"info\":{\"id\":\"msg1\",\"role\":\"assistant\"}}}}\n\n")
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"message.part.updated\",\"properties\":{\"sessionID\":\"s1\",\"part\":{\"id\":\"step1\",\"messageID\":\"msg1\",\"type\":\"step-finish\",\"tokens\":{\"input\":10,\"output\":5}}}}}\n\n")
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"session.idle\"}}\n\n")

		case r.URL.Path == "/session/s1/message" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"info":{"id":"msg1","role":"assistant"},"parts":[{"type":"text","text":"hello world"}]}`)

		case r.URL.Path == "/session/s1" && r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusOK)

		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	a := &opencodeAgent{
		bin:    "opencode",
		server: &managedServer{port: mustParsePort(server.URL)},
	}

	var chunks []string
	result, err := a.Run(context.Background(), RunOpts{
		Prompt:  "hello",
		CWD:     t.TempDir(),
		OnChunk: func(text string) { chunks = append(chunks, text) },
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Text != "hello world" {
		t.Fatalf("expected completed response text, got %q", result.Text)
	}
	if got := strings.Join(chunks, ""); got != "hello\n\n world" {
		t.Fatalf("expected streamed and backfilled text with separator, got %q from %v", got, chunks)
	}
	if len(chunks) != 3 || chunks[1] != "\n\n" || chunks[2] != " world" {
		t.Fatalf("expected separator before missing suffix backfill, got %v", chunks)
	}
}

func TestOpencodeAgent_DoesNotSeparateBackfillWhenToolStepPrecedesFirstText(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/session" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"id":"s1"}`)

		case r.URL.Path == "/global/event" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"message.part.updated\",\"properties\":{\"sessionID\":\"s1\",\"part\":{\"id\":\"step1\",\"messageID\":\"msg1\",\"type\":\"step-finish\",\"tokens\":{\"input\":10,\"output\":5}}}}}\n\n")
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"message.part.updated\",\"properties\":{\"sessionID\":\"s1\",\"part\":{\"id\":\"p1\",\"messageID\":\"msg1\",\"type\":\"text\",\"text\":\"hello\"}}}}\n\n")
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"message.updated\",\"properties\":{\"sessionID\":\"s1\",\"info\":{\"id\":\"msg1\",\"role\":\"assistant\"}}}}\n\n")
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"session.idle\"}}\n\n")

		case r.URL.Path == "/session/s1/message" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"info":{"id":"msg1","role":"assistant"},"parts":[{"type":"text","text":"hello world"}]}`)

		case r.URL.Path == "/session/s1" && r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusOK)

		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	a := &opencodeAgent{
		bin:    "opencode",
		server: &managedServer{port: mustParsePort(server.URL)},
	}

	var chunks []string
	result, err := a.Run(context.Background(), RunOpts{
		Prompt:  "hello",
		CWD:     t.TempDir(),
		OnChunk: func(text string) { chunks = append(chunks, text) },
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Text != "hello world" {
		t.Fatalf("expected completed response text, got %q", result.Text)
	}
	if got := strings.Join(chunks, ""); got != "hello world" {
		t.Fatalf("expected streamed and backfilled text without separator, got %q from %v", got, chunks)
	}
	if len(chunks) != 2 || chunks[1] != " world" {
		t.Fatalf("expected suffix backfill without separator, got %v", chunks)
	}
}

// TestOpencodeAgent_NoSchema tests the flow without a JSON schema.
func TestOpencodeAgent_NoSchema(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/session" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"id":"s1"}`)

		case r.URL.Path == "/global/event" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"message.part.delta\",\"properties\":{\"sessionID\":\"s1\",\"field\":\"text\",\"partID\":\"p1\",\"delta\":\"done\"}}}\n\n")
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"session.idle\"}}\n\n")

		case r.URL.Path == "/session/s1/message" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"info":{"id":"msg1","role":"assistant"},"parts":[{"type":"text","text":"done"}]}`)

		case r.URL.Path == "/session/s1" && r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusOK)

		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	a := &opencodeAgent{
		bin:    "opencode",
		server: &managedServer{port: mustParsePort(server.URL)},
	}

	result, err := a.Run(context.Background(), RunOpts{
		Prompt: "hello",
		CWD:    t.TempDir(),
		// No JSONSchema
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected result")
	}
	if result.Text != "done" {
		t.Fatalf("expected plain text result, got %q", result.Text)
	}
	if result.Output != nil {
		t.Fatalf("expected nil structured output, got %s", string(result.Output))
	}
}

// TestOpencodeAgent_FinalAnswerPreferred tests that final_answer phase text is preferred.
func TestOpencodeAgent_FinalAnswerPreferred(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/session" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"id":"s1"}`)

		case r.URL.Path == "/global/event" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "text/event-stream")
			// First text part (regular), then final_answer part
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"message.part.updated\",\"properties\":{\"sessionID\":\"s1\",\"part\":{\"id\":\"p1\",\"type\":\"text\",\"text\":\"thinking...\"}}}}\n\n")
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"message.part.updated\",\"properties\":{\"sessionID\":\"s1\",\"part\":{\"id\":\"p2\",\"type\":\"text\",\"text\":\"{\\\"answer\\\":42}\",\"metadata\":{\"openai\":{\"phase\":\"final_answer\"}}}}}}\n\n")
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"session.idle\"}}\n\n")

		case r.URL.Path == "/session/s1/message" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"info":{"id":"msg1","role":"assistant"},"parts":[{"type":"text","text":"thinking..."},{"type":"text","text":"{\"answer\":42}","metadata":{"openai":{"phase":"final_answer"}}}]}`)

		case r.URL.Path == "/session/s1" && r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusOK)

		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	a := &opencodeAgent{
		bin:    "opencode",
		server: &managedServer{port: mustParsePort(server.URL)},
	}

	result, err := a.Run(context.Background(), RunOpts{
		Prompt: "what is 6*7",
		CWD:    t.TempDir(),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Text != `{"answer":42}` {
		t.Fatalf("expected final_answer text, got %q", result.Text)
	}
	if result.Output != nil {
		t.Fatalf("expected nil structured output, got %s", string(result.Output))
	}
}

// TestOpencodeAgent_StructuredOutputError asserts that when opencode returns
// an info.error with name=StructuredOutputError, the agent surfaces a
// precise error and does NOT fall through to text-parsing the streamed
// reasoning prose. Regression: run 01KWDTFPNXTC94YEYCN23XFFG1 surfaced
// "invalid character 'N' looking for beginning of value" instead of the
// real cause.
func TestOpencodeAgent_StructuredOutputError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/session" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"id":"s1"}`)

		case r.URL.Path == "/global/event" && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			// Stream reasoning prose (no JSON) - this is exactly the
			// shape real opencode emits when the model never calls the
			// StructuredOutput tool.
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"message.part.updated\",\"properties\":{\"sessionID\":\"s1\",\"part\":{\"id\":\"p1\",\"messageID\":\"msg1\",\"type\":\"text\",\"text\":\"Now I need to find the failing test. The only failing test is foo.\"}}}}\n\n")
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"message.updated\",\"properties\":{\"sessionID\":\"s1\",\"info\":{\"id\":\"msg1\",\"role\":\"assistant\"}}}}\n\n")
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"session.idle\"}}\n\n")

		case r.URL.Path == "/session/s1/message" && r.Method == http.MethodPost:
			// opencode signals structured-output failure via
			// info.error.name = "StructuredOutputError". The body
			// intentionally omits info.structured.
			fmt.Fprint(w, `{"info":{"id":"msg1","role":"assistant","error":{"name":"StructuredOutputError","message":"Model did not produce structured output","retries":2}},"parts":[{"type":"text","text":"Now I need to find the failing test. The only failing test is foo."}]}`)

		case r.URL.Path == "/session/s1" && r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusOK)

		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	a := &opencodeAgent{
		bin:    "opencode",
		server: &managedServer{port: mustParsePort(server.URL)},
	}

	result, err := a.Run(context.Background(), RunOpts{
		Prompt:     "fix the failing tests",
		CWD:        t.TempDir(),
		JSONSchema: json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string"}},"required":["summary"]}`),
	})
	if err == nil {
		t.Fatalf("expected error, got result %+v", result)
	}
	if result != nil {
		t.Fatalf("expected nil result on error, got %+v", result)
	}
	msg := err.Error()
	if !strings.Contains(msg, "structured output failed") {
		t.Errorf("expected error to mention structured output failure, got %q", msg)
	}
	if !strings.Contains(msg, "2 internal retries") {
		t.Errorf("expected error to surface retry count, got %q", msg)
	}
	if strings.Contains(msg, "invalid character") {
		t.Errorf("error must not be the JSON-parse-on-prose symptom, got %q", msg)
	}
	if strings.Contains(msg, "Now I need to find the failing test") {
		t.Errorf("error must not embed the reasoning prose snippet, got %q", msg)
	}
}

func TestOpencodeAgent_ThinkingToolChoiceConflictFallsBackToValidatedText(t *testing.T) {
	var sessions atomic.Int32
	var eventStreams atomic.Int32
	var nativeNestedFormatSeen atomic.Bool
	var nativeRootFormatSeen atomic.Bool
	var nativeProfileAndRetriesSeen atomic.Bool
	var fallbackFormatSeen atomic.Bool

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/session" && r.Method == http.MethodPost:
			id := sessions.Add(1)
			fmt.Fprintf(w, `{"id":"s%d"}`, id)

		case r.URL.Path == "/global/event" && r.Method == http.MethodGet:
			if eventStreams.Add(1) == 1 {
				fmt.Fprint(w, "data: {\"payload\":{\"type\":\"message.part.updated\",\"properties\":{\"sessionID\":\"s1\",\"part\":{\"id\":\"p1\",\"messageID\":\"msg1\",\"type\":\"text\",\"text\":\"thinking before conflict\"}}}}\n\n")
				fmt.Fprint(w, "data: {\"payload\":{\"type\":\"message.updated\",\"properties\":{\"sessionID\":\"s1\",\"info\":{\"id\":\"msg1\",\"role\":\"assistant\"}}}}\n\n")
			}
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"session.idle\"}}\n\n")

		case r.URL.Path == "/session/s1/message" && r.Method == http.MethodPost:
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode native request: %v", err)
			}
			_, hasRootFormat := body["format"]
			nativeRootFormatSeen.Store(hasRootFormat)
			info, _ := body["info"].(map[string]any)
			format, hasNestedFormat := info["format"].(map[string]any)
			nativeNestedFormatSeen.Store(hasNestedFormat)
			model, _ := body["model"].(map[string]any)
			nativeProfileAndRetriesSeen.Store(
				model["providerID"] == "openai" &&
					model["modelID"] == "gpt-5" &&
					body["variant"] == "high" &&
					format["retryCount"] == float64(2),
			)
			fmt.Fprint(w, `{"info":{"id":"msg1","role":"assistant","error":{"name":"APIError","data":{"message":"Provider returned error","responseBody":"Thinking may not be enabled when tool_choice forces tool use."}}}}`)

		case r.URL.Path == "/session/s2/message" && r.Method == http.MethodPost:
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode fallback request: %v", err)
			}
			_, hasFormat := body["format"]
			fallbackFormatSeen.Store(hasFormat)
			fmt.Fprint(w, `{"info":{"id":"msg2","role":"assistant"},"parts":[{"type":"text","text":"{\"summary\":\"all good\"}"}]}`)

		case (r.URL.Path == "/session/s1" || r.URL.Path == "/session/s2") && r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusOK)

		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	a := &opencodeAgent{
		bin:    "opencode",
		server: &managedServer{port: mustParsePort(server.URL)},
		profile: agentcfg.Profile{
			Model:  "openai/gpt-5",
			Effort: agentcfg.EffortHigh,
		},
	}
	var chunks []string
	var fallbackEvents int
	result, err := a.Run(context.Background(), RunOpts{
		Prompt:     "review the changes",
		CWD:        t.TempDir(),
		JSONSchema: json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string"}},"required":["summary"],"additionalProperties":false}`),
		OnChunk:    func(text string) { chunks = append(chunks, text) },
		OnLifecycle: func(event LifecycleEvent) {
			if event.Phase == LifecyclePhaseFallback {
				fallbackEvents++
			}
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := string(result.Output); got != `{"summary":"all good"}` {
		t.Fatalf("output = %s", got)
	}
	if !nativeNestedFormatSeen.Load() {
		t.Error("first request did not nest native json_schema format in info")
	}
	if nativeRootFormatSeen.Load() {
		t.Error("first request unexpectedly used root json_schema format")
	}
	if !nativeProfileAndRetriesSeen.Load() {
		t.Error("first request did not preserve model, variant, and retryCount")
	}
	if fallbackFormatSeen.Load() {
		t.Error("fallback request unexpectedly used native json_schema format")
	}
	if got := sessions.Load(); got != 2 {
		t.Fatalf("sessions = %d, want 2", got)
	}
	if !strings.Contains(strings.Join(chunks, ""), "thinking before conflict") {
		t.Fatalf("chunks = %q, want output from the failed native-format attempt", chunks)
	}
	if fallbackEvents != 1 {
		t.Fatalf("fallback lifecycle events = %d, want one fresh prompt-only attempt boundary", fallbackEvents)
	}
	t.Logf("native nested format=%v; native root format=%v; model/variant/retryCount preserved=%v; fallback format=%v; validated output=%s", nativeNestedFormatSeen.Load(), nativeRootFormatSeen.Load(), nativeProfileAndRetriesSeen.Load(), fallbackFormatSeen.Load(), result.Output)
}

func TestOpencodeAgent_ThinkingToolChoiceFallbackRejectsSchemaViolation(t *testing.T) {
	var sessions atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/session" && r.Method == http.MethodPost:
			id := sessions.Add(1)
			fmt.Fprintf(w, `{"id":"s%d"}`, id)
		case r.URL.Path == "/global/event" && r.Method == http.MethodGet:
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"session.idle\"}}\n\n")
		case r.URL.Path == "/session/s1/message" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"info":{"id":"msg1","role":"assistant","error":{"name":"APIError","data":{"responseBody":"Thinking may not be enabled when tool_choice forces tool use."}}}}`)
		case r.URL.Path == "/session/s2/message" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"info":{"id":"msg2","role":"assistant"},"parts":[{"type":"text","text":"{\"summary\":42}"}]}`)
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	a := &opencodeAgent{
		bin:    "opencode",
		server: &managedServer{port: mustParsePort(server.URL)},
	}
	result, err := a.Run(context.Background(), RunOpts{
		Prompt:     "review the changes",
		CWD:        t.TempDir(),
		JSONSchema: json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string"}},"required":["summary"],"additionalProperties":false}`),
	})
	if err == nil {
		t.Fatalf("Run unexpectedly succeeded with result %+v", result)
	}
	if result != nil {
		t.Fatalf("result = %+v, want nil", result)
	}
	if !strings.Contains(err.Error(), "summary must be string") {
		t.Fatalf("error = %q, want original schema's string constraint", err)
	}
	if got := sessions.Load(); got != 2 {
		t.Fatalf("sessions = %d, want 2", got)
	}
	t.Logf("two attempts completed; invalid fallback rejected: %v", err)
}

func TestOpencodeAgent_ThinkingToolChoiceConflictFromSSEFallsBackOnce(t *testing.T) {
	var sessions atomic.Int32
	var eventStreams atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/session" && r.Method == http.MethodPost:
			id := sessions.Add(1)
			fmt.Fprintf(w, `{"id":"s%d"}`, id)
		case r.URL.Path == "/global/event" && r.Method == http.MethodGet:
			if eventStreams.Add(1) == 1 {
				fmt.Fprint(w, `data: {"payload":{"type":"session.error","properties":{"sessionID":"s1","error":{"name":"APIError","data":{"message":"tool_choice 'required' is incompatible with thinking enabled"}}}}}`+"\n\n")
				return
			}
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"session.idle\"}}\n\n")
		case r.URL.Path == "/session/s1/message" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"info":{"id":"msg1","role":"assistant"}}`)
		case r.URL.Path == "/session/s2/message" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"info":{"id":"msg2","role":"assistant"},"parts":[{"type":"text","text":"{\"summary\":\"sse fallback passed\"}"}]}`)
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	a := &opencodeAgent{bin: "opencode", server: &managedServer{port: mustParsePort(server.URL)}}
	result, err := a.Run(context.Background(), RunOpts{
		Prompt:     "review the changes",
		CWD:        t.TempDir(),
		JSONSchema: json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string"}},"required":["summary"],"additionalProperties":false}`),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := string(result.Output); got != `{"summary":"sse fallback passed"}` {
		t.Fatalf("output = %s", got)
	}
	if got := sessions.Load(); got != 2 {
		t.Fatalf("sessions = %d, want exactly one fallback retry", got)
	}
	t.Logf("SSE provider conflict triggered one fallback; validated output=%s", result.Output)
}

func TestOpencodeAgent_UnrelatedThinkingLimitationDoesNotFallback(t *testing.T) {
	var sessions atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/session" && r.Method == http.MethodPost:
			id := sessions.Add(1)
			fmt.Fprintf(w, `{"id":"s%d"}`, id)
		case r.URL.Path == "/global/event" && r.Method == http.MethodGet:
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"session.idle\"}}\n\n")
		case r.URL.Path == "/session/s1/message" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"info":{"id":"msg1","role":"assistant","error":{"name":"APIError","data":{"message":"tool_choice is required. Thinking is not supported when streaming."}}}}`)
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	a := &opencodeAgent{
		bin:    "opencode",
		server: &managedServer{port: mustParsePort(server.URL)},
	}
	_, err := a.Run(context.Background(), RunOpts{
		Prompt:     "review the changes",
		CWD:        t.TempDir(),
		JSONSchema: json.RawMessage(`{"type":"object"}`),
	})
	if err == nil {
		t.Fatal("Run unexpectedly succeeded")
	}
	if got := sessions.Load(); got != 1 {
		t.Fatalf("sessions = %d, want 1", got)
	}
}

func TestThinkingToolChoiceConflictClassification(t *testing.T) {
	tests := []struct {
		name string
		text string
		want bool
	}{
		{name: "deepseek", text: `tool_choice 'required' is incompatible with thinking enabled`, want: true},
		{name: "canonical provider wording", text: `Thinking may not be enabled when tool_choice forces tool use.`, want: true},
		{name: "issue wording", text: `thinking mode can't be combined with a forced tool_choice`, want: true},
		{name: "reasoning variant", text: `Required tool choice cannot be combined with reasoning`, want: true},
		// Review findings F1 and F2 on the #965 change: an only-auto verdict
		// that names thinking is this conditional conflict, not a blanket
		// gateway restriction, even with no relational verb to match on.
		{name: "must be auto when thinking is enabled", text: `tool_choice must be auto when thinking is enabled`, want: true},
		{name: "only auto supported when thinking is active", text: `only auto is supported for tool_choice when thinking is active`, want: true},
		{name: "unsupported when extended thinking is enabled", text: `tool_choice parameter is unsupported when extended thinking is enabled`, want: true},
		{name: "reasoning variant of the same overlap", text: `tool_choice must be auto when reasoning is enabled`, want: true},
		// The blanket rejection must not be claimed as a thinking conflict.
		{name: "free gateway payload is not a thinking conflict", text: `only "auto" is supported for "tool_choice". "none", "required", and named function choices are not currently supported`, want: false},
		{name: "compatible requirement", text: `tool_choice is required and cannot be disabled when thinking is enabled`, want: false},
		{name: "unrelated multi-clause limitation", text: `tool_choice is required. Thinking is not supported when streaming.`, want: false},
		{name: "ordinary structured failure", text: `Model did not produce structured output`, want: false},
		{name: "unrelated provider failure", text: `provider does not support this model`, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isThinkingToolChoiceConflictText(tt.text); got != tt.want {
				t.Fatalf("isThinkingToolChoiceConflictText(%q) = %v, want %v", tt.text, got, tt.want)
			}
			// The two detectors stay disjoint: a thinking conflict never
			// reports itself as a blanket only-auto rejection.
			if tt.want && isForcedToolChoiceUnsupportedText(tt.text) {
				t.Fatalf("isForcedToolChoiceUnsupportedText(%q) = true, want false", tt.text)
			}
		})
	}
}

func TestForcedToolChoiceUnsupportedClassification(t *testing.T) {
	tests := []struct {
		name string
		text string
		want bool
	}{
		{name: "free gateway exact payload", text: `only "auto" is supported for "tool_choice". "none", "required", and named function choices are not currently supported`, want: true},
		{name: "unquoted only auto", text: `only auto is supported for tool_choice`, want: true},
		{name: "must be auto", text: `tool_choice must be "auto"`, want: true},
		{name: "unsupported tool_choice", text: `unsupported tool_choice`, want: true},
		{name: "reversed tool_choice only auto", text: `tool_choice: only "auto" is supported`, want: true},
		{name: "thinking conflict is not a blanket rejection", text: `tool_choice 'required' is incompatible with thinking enabled`, want: false},
		{name: "canonical thinking wording is not a blanket rejection", text: `Thinking may not be enabled when tool_choice forces tool use.`, want: false},
		{name: "thinking without auto is not a blanket rejection", text: `Thinking mode does not support this tool_choice`, want: false},
		{name: "compatible requirement", text: `tool_choice is required and cannot be disabled when thinking is enabled`, want: false},
		{name: "unrelated multi-clause limitation", text: `tool_choice is required. Thinking is not supported when streaming.`, want: false},
		{name: "ordinary structured failure", text: `Model did not produce structured output`, want: false},
		{name: "unrelated provider failure", text: `provider does not support this model`, want: false},
		{name: "rate limit is not a rejection", text: `rate limit exceeded for model, retry after 60s`, want: false},
		{name: "429 rate limit is not a rejection", text: `429 Too Many Requests: rate-limit exceeded, retry later`, want: false},
		{name: "generic 500 is not a rejection", text: `internal server error (status 500)`, want: false},
		{name: "mere mention without a verdict", text: `tool_choice is required for structured output`, want: false},
		// Review findings F1 and F2 on the #965 change: these name a thinking
		// mode, so they are thinking conflicts even though the thinking
		// patterns miss them for want of a relational verb. The blanket
		// detector must decline them rather than misreport the cause.
		{name: "must be auto when thinking is enabled", text: `tool_choice must be auto when thinking is enabled`, want: false},
		{name: "only auto supported when thinking is active", text: `only auto is supported for tool_choice when thinking is active`, want: false},
		{name: "unsupported when extended thinking is enabled", text: `tool_choice parameter is unsupported when extended thinking is enabled`, want: false},
		{name: "reasoning variant of the same overlap", text: `tool_choice must be auto when reasoning is enabled`, want: false},
		// A blanket rejection in one sentence still matches when an adjacent
		// sentence happens to mention thinking.
		{name: "blanket rejection beside an unrelated thinking sentence", text: `only "auto" is supported for "tool_choice". Thinking is configured per request.`, want: true},
		// Gate finding review-1: the dead [^.] guard let the token and the
		// verdict sit in different clauses of one sentence. Both of these
		// matched before that guard was replaced.
		{name: "only auto scaling in a later clause", text: `tool_choice is restricted, and only auto scaling is enabled`, want: false},
		{name: "only auto placement in a later clause", text: `tool_choice rejected, only auto placement is supported here`, want: false},
		{name: "colon as clause separator with only auto scaling", text: `tool_choice logged: only auto scaling remains in this region`, want: false},
		{name: "colon as clause separator with only auto placement", text: `tool_choice quota note: only auto placement left on the cluster`, want: false},
		// A colon straight after the token is still one clause.
		{name: "colon then only auto", text: `tool_choice: only auto`, want: true},
		{name: "unsupported model beside tool_choice", text: `tool_choice set, but the requested model is unsupported`, want: false},
		{name: "tool_choice value unsupported", text: `tool_choice value is unsupported`, want: true},
		{name: "invalid parameter without a verdict", text: `invalid tool_choice parameter`, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isForcedToolChoiceUnsupportedText(tt.text); got != tt.want {
				t.Fatalf("isForcedToolChoiceUnsupportedText(%q) = %v, want %v", tt.text, got, tt.want)
			}
			// The two detectors stay disjoint: a blanket rejection never
			// reports itself as a thinking conflict.
			if tt.want && isThinkingToolChoiceConflictText(tt.text) {
				t.Fatalf("isThinkingToolChoiceConflictText(%q) = true, want false", tt.text)
			}
		})
	}
}

// TestOpencodeFallbackTrigger_SentinelsStayDistinct pins the sentinel
// contract the fallback relies on: each builder carries its own sentinel,
// never the other's, and both open the same prompt-only fallback while an
// unrelated error opens none.
func TestOpencodeFallbackTrigger_SentinelsStayDistinct(t *testing.T) {
	conflict := thinkingConflict(opencodeToolsNone, nil)
	if !errors.Is(conflict, errOpencodeThinkingToolChoiceConflict) {
		t.Errorf("thinkingConflict must carry the thinking sentinel, got %v", conflict)
	}
	if errors.Is(conflict, errOpencodeForcedToolChoiceUnsupported) {
		t.Errorf("thinkingConflict must not carry the only-auto sentinel, got %v", conflict)
	}
	if !opencodeFallbackTrigger(conflict) {
		t.Errorf("thinkingConflict must trigger the prompt-only fallback, got %v", conflict)
	}

	rejection := forcedToolChoiceConflict(opencodeToolsNone, nil)
	if !errors.Is(rejection, errOpencodeForcedToolChoiceUnsupported) {
		t.Errorf("forcedToolChoiceConflict must carry the only-auto sentinel, got %v", rejection)
	}
	if errors.Is(rejection, errOpencodeThinkingToolChoiceConflict) {
		t.Errorf("forcedToolChoiceConflict must not carry the thinking sentinel, got %v", rejection)
	}
	if !opencodeFallbackTrigger(rejection) {
		t.Errorf("forcedToolChoiceConflict must trigger the prompt-only fallback, got %v", rejection)
	}

	if opencodeFallbackTrigger(errors.New("provider does not support this model")) {
		t.Error("an unrelated provider error must not trigger the prompt-only fallback")
	}

	// The pre-existing thinking wording keeps its own detector and sentinel.
	for _, text := range []string{
		`tool_choice 'required' is incompatible with thinking enabled`,
		`Thinking may not be enabled when tool_choice forces tool use.`,
	} {
		if !isThinkingToolChoiceConflictText(text) {
			t.Errorf("isThinkingToolChoiceConflictText(%q) = false, want true", text)
		}
		if isForcedToolChoiceUnsupportedText(text) {
			t.Errorf("isForcedToolChoiceUnsupportedText(%q) = true, want false", text)
		}
	}
}

func TestOpencodeAgent_ForcedToolChoiceFallsBackToValidatedText(t *testing.T) {
	var sessions atomic.Int32
	var fallbackFormatSeen atomic.Bool

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/session" && r.Method == http.MethodPost:
			id := sessions.Add(1)
			fmt.Fprintf(w, `{"id":"s%d"}`, id)
		case r.URL.Path == "/global/event" && r.Method == http.MethodGet:
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"session.idle\"}}\n\n")
		case r.URL.Path == "/session/s1/message" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"info":{"id":"msg1","role":"assistant","error":{"name":"APIError","data":{"message":"only \"auto\" is supported for \"tool_choice\". \"none\", \"required\", and named function choices are not currently supported","statusCode":400,"isRetryable":false}}},"parts":[]}`)
		case r.URL.Path == "/session/s2/message" && r.Method == http.MethodPost:
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode fallback request: %v", err)
			}
			_, hasFormat := body["format"]
			fallbackFormatSeen.Store(hasFormat)
			fmt.Fprint(w, `{"info":{"id":"msg2","role":"assistant"},"parts":[{"type":"text","text":"{\"summary\":\"gateway fallback passed\"}"}]}`)
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	a := &opencodeAgent{
		bin:    "opencode",
		server: &managedServer{port: mustParsePort(server.URL)},
	}
	result, err := a.Run(context.Background(), RunOpts{
		Prompt:     "review the changes",
		CWD:        t.TempDir(),
		JSONSchema: json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string"}},"required":["summary"],"additionalProperties":false}`),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := string(result.Output); got != `{"summary":"gateway fallback passed"}` {
		t.Fatalf("output = %s", got)
	}
	if got := sessions.Load(); got != 2 {
		t.Fatalf("sessions = %d, want exactly one fallback retry", got)
	}
	if fallbackFormatSeen.Load() {
		t.Error("fallback request unexpectedly used native json_schema format")
	}
	t.Logf("gateway only-auto rejection triggered one fallback; validated output=%s", result.Output)
}

func TestOpencodeAgent_ForcedToolChoiceFromSSEFallsBackOnce(t *testing.T) {
	var sessions atomic.Int32
	var eventStreams atomic.Int32

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/session" && r.Method == http.MethodPost:
			id := sessions.Add(1)
			fmt.Fprintf(w, `{"id":"s%d"}`, id)
		case r.URL.Path == "/global/event" && r.Method == http.MethodGet:
			if eventStreams.Add(1) == 1 {
				fmt.Fprint(w, `data: {"payload":{"type":"session.error","properties":{"sessionID":"s1","error":{"name":"APIError","data":{"message":"only \"auto\" is supported for \"tool_choice\". \"none\", \"required\", and named function choices are not currently supported"}}}}}`+"\n\n")
				return
			}
			fmt.Fprint(w, "data: {\"payload\":{\"type\":\"session.idle\"}}\n\n")
		case r.URL.Path == "/session/s1/message" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"info":{"id":"msg1","role":"assistant"}}`)
		case r.URL.Path == "/session/s2/message" && r.Method == http.MethodPost:
			fmt.Fprint(w, `{"info":{"id":"msg2","role":"assistant"},"parts":[{"type":"text","text":"{\"summary\":\"sse gateway fallback passed\"}"}]}`)
		case r.Method == http.MethodDelete:
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	a := &opencodeAgent{bin: "opencode", server: &managedServer{port: mustParsePort(server.URL)}}
	result, err := a.Run(context.Background(), RunOpts{
		Prompt:     "review the changes",
		CWD:        t.TempDir(),
		JSONSchema: json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string"}},"required":["summary"],"additionalProperties":false}`),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := string(result.Output); got != `{"summary":"sse gateway fallback passed"}` {
		t.Fatalf("output = %s", got)
	}
	if got := sessions.Load(); got != 2 {
		t.Fatalf("sessions = %d, want exactly one fallback retry", got)
	}
	t.Logf("SSE gateway only-auto rejection triggered one fallback; validated output=%s", result.Output)
}

// TestToolChoiceDetectorsPartitionRejections pins the invariant the two
// sentinels exist for: every tool_choice rejection reaches the prompt-only
// fallback, exactly one detector claims it, and an unrelated provider error
// triggers no fallback at all.
func TestToolChoiceDetectorsPartitionRejections(t *testing.T) {
	tests := []struct {
		name     string
		text     string
		thinking bool
		forced   bool
	}{
		{name: "free gateway blanket rejection", text: `only "auto" is supported for "tool_choice". "none", "required", and named function choices are not currently supported`, forced: true},
		{name: "must be auto", text: `tool_choice must be auto`, forced: true},
		{name: "relational thinking conflict", text: `tool_choice 'required' is incompatible with thinking enabled`, thinking: true},
		{name: "verdict naming thinking", text: `tool_choice must be auto when thinking is enabled`, thinking: true},
		{name: "verdict naming reasoning", text: `tool_choice parameter is unsupported when extended reasoning is enabled`, thinking: true},
		{name: "rate limit", text: `rate limit exceeded for model, retry after 60s`},
		{name: "unsupported model", text: `tool_choice set, but the requested model is unsupported`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			thinking := isThinkingToolChoiceConflictText(tc.text)
			forced := isForcedToolChoiceUnsupportedText(tc.text)
			if thinking != tc.thinking || forced != tc.forced {
				t.Fatalf("thinking=%v forced=%v, want thinking=%v forced=%v", thinking, forced, tc.thinking, tc.forced)
			}
			if thinking && forced {
				t.Fatal("both detectors claimed the same rejection; the sentinels must stay disjoint")
			}
		})
	}
}

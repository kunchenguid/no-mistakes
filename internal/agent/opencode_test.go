package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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

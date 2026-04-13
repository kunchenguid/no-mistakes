//go:build unit

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

func TestParseRovodevSSE_TextEvent(t *testing.T) {
	input := `event: text
data: {"content":"hello world"}

`
	var usage TokenUsage
	var latestText string
	var chunks []string

	err := parseRovodevSSE(strings.NewReader(input), func(text string) {
		chunks = append(chunks, text)
	}, &usage, &latestText)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if latestText != "hello world" {
		t.Errorf("expected latest text 'hello world', got %q", latestText)
	}
	if len(chunks) != 1 || chunks[0] != "hello world" {
		t.Errorf("expected 1 chunk 'hello world', got %v", chunks)
	}
}

func TestParseRovodevSSE_UsageEvent(t *testing.T) {
	input := `event: request-usage
data: {"usage":{"input_tokens":100,"output_tokens":50,"cache_read_tokens":30,"cache_write_tokens":10}}

`
	var usage TokenUsage
	var latestText string

	err := parseRovodevSSE(strings.NewReader(input), nil, &usage, &latestText)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if usage.InputTokens != 100 {
		t.Errorf("expected input tokens 100, got %d", usage.InputTokens)
	}
	if usage.OutputTokens != 50 {
		t.Errorf("expected output tokens 50, got %d", usage.OutputTokens)
	}
	if usage.CacheReadTokens != 30 {
		t.Errorf("expected cache read tokens 30, got %d", usage.CacheReadTokens)
	}
	if usage.CacheCreationTokens != 10 {
		t.Errorf("expected cache creation tokens 10, got %d", usage.CacheCreationTokens)
	}
}

func TestParseRovodevSSE_SeparatesAfterToolReturn(t *testing.T) {
	input := `event: text
data: {"content":"before tool"}

event: tool-return
data: {"content":"ignored"}

event: text
data: {"content":"after tool"}

`
	var usage TokenUsage
	var latestText string
	var chunks []string

	err := parseRovodevSSE(strings.NewReader(input), func(text string) {
		chunks = append(chunks, text)
	}, &usage, &latestText)

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) != 3 {
		t.Fatalf("expected 3 chunks (text, separator, text), got %d: %v", len(chunks), chunks)
	}
	if chunks[0] != "before tool" {
		t.Errorf("expected 'before tool', got %q", chunks[0])
	}
	if chunks[1] != "\n\n" {
		t.Errorf("expected separator '\\n\\n', got %q", chunks[1])
	}
	if chunks[2] != "after tool" {
		t.Errorf("expected 'after tool', got %q", chunks[2])
	}
	if latestText != "after tool" {
		t.Errorf("expected latest text 'after tool' after tool-return reset, got %q", latestText)
	}
}

func TestParseRovodevSSE_EventKindFallback(t *testing.T) {
	// When event: field is missing, fall back to event_kind in JSON
	input := `data: {"event_kind":"text","content":"fallback text"}

`
	var usage TokenUsage
	var latestText string

	err := parseRovodevSSE(strings.NewReader(input), nil, &usage, &latestText)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if latestText != "fallback text" {
		t.Errorf("expected latest text 'fallback text', got %q", latestText)
	}
}

func TestParseRovodevSSE_MultipleUsageAccumulates(t *testing.T) {
	input := `event: request-usage
data: {"usage":{"input_tokens":50,"output_tokens":20}}

event: request-usage
data: {"usage":{"input_tokens":100,"output_tokens":30}}

`
	var usage TokenUsage
	var latestText string

	err := parseRovodevSSE(strings.NewReader(input), nil, &usage, &latestText)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if usage.InputTokens != 150 {
		t.Errorf("expected accumulated input tokens 150, got %d", usage.InputTokens)
	}
	if usage.OutputTokens != 50 {
		t.Errorf("expected accumulated output tokens 50, got %d", usage.OutputTokens)
	}
}

func TestParseRovodevSSE_EmptyData(t *testing.T) {
	input := "event: text\ndata: \n\n"
	var usage TokenUsage
	var latestText string

	err := parseRovodevSSE(strings.NewReader(input), nil, &usage, &latestText)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if latestText != "" {
		t.Errorf("expected empty latest text, got %q", latestText)
	}
}

func TestDoJSON_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Error("expected Content-Type application/json")
		}
		if r.Header.Get("x-custom") != "value" {
			t.Error("expected x-custom header")
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"result":"ok"}`)
	}))
	defer server.Close()

	headers := map[string]string{"x-custom": "value"}
	body := map[string]string{"key": "val"}
	resp, err := doJSON(context.Background(), http.MethodPost, server.URL+"/test", headers, body)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(resp) != `{"result":"ok"}` {
		t.Errorf("unexpected response: %s", string(resp))
	}
}

func TestDoJSON_ErrorStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, "bad request")
	}))
	defer server.Close()

	_, err := doJSON(context.Background(), http.MethodGet, server.URL+"/test", nil, nil)
	if err == nil {
		t.Fatal("expected error for 400 status")
	}
	if !strings.Contains(err.Error(), "400") {
		t.Errorf("error should contain status code: %v", err)
	}
}

func TestDoJSON_NilBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") == "application/json" {
			t.Error("should not set Content-Type for nil body")
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{}`)
	}))
	defer server.Close()

	resp, err := doJSON(context.Background(), http.MethodGet, server.URL+"/test", nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(resp) != `{}` {
		t.Errorf("unexpected response: %s", string(resp))
	}
}

func TestGetAvailablePort(t *testing.T) {
	port, err := getAvailablePort()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if port <= 0 || port > 65535 {
		t.Errorf("expected valid port, got %d", port)
	}

	// Should return different ports on successive calls
	port2, err := getAvailablePort()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Ports may be the same in rare cases, but should be valid
	if port2 <= 0 || port2 > 65535 {
		t.Errorf("expected valid port, got %d", port2)
	}
}

func TestRovodevAgent_CloseWithoutServer(t *testing.T) {
	a := &rovodevAgent{bin: "acli"}
	if err := a.Close(); err != nil {
		t.Errorf("Close without server should not error: %v", err)
	}
}

// TestRovodevAgent_FullFlow tests the full session lifecycle using a mock HTTP server.
func TestRovodevAgent_FullFlow(t *testing.T) {
	step := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v3/sessions/create" && r.Method == http.MethodPost:
			step++
			fmt.Fprint(w, `{"session_id":"test-session-123"}`)

		case r.URL.Path == "/v3/inline-system-prompt" && r.Method == http.MethodPut:
			step++
			if r.Header.Get("x-session-id") != "test-session-123" {
				t.Error("expected x-session-id header")
			}
			w.WriteHeader(http.StatusOK)

		case r.URL.Path == "/v3/set_chat_message" && r.Method == http.MethodPost:
			step++
			if r.Header.Get("x-session-id") != "test-session-123" {
				t.Error("expected x-session-id header")
			}
			w.WriteHeader(http.StatusOK)

		case r.URL.Path == "/v3/stream_chat" && r.Method == http.MethodGet:
			step++
			if r.Header.Get("x-session-id") != "test-session-123" {
				t.Error("expected x-session-id header")
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			// Send usage event then text event with JSON output
			fmt.Fprint(w, "event: request-usage\ndata: {\"usage\":{\"input_tokens\":100,\"output_tokens\":50}}\n\n")
			fmt.Fprint(w, "event: text\ndata: {\"content\":\"{\\\"success\\\":true,\\\"summary\\\":\\\"all good\\\"}\"}\n\n")

		case r.URL.Path == "/v3/sessions/test-session-123" && r.Method == http.MethodDelete:
			step++
			w.WriteHeader(http.StatusOK)

		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	// Create agent with a mock server already running
	a := &rovodevAgent{
		bin:    "acli",
		server: &managedServer{port: 0}, // will be overridden
	}
	// Parse the test server's port from URL
	a.server = &managedServer{
		port: mustParsePort(server.URL),
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
	if result.Usage.InputTokens != 100 {
		t.Errorf("expected input tokens 100, got %d", result.Usage.InputTokens)
	}
	if result.Usage.OutputTokens != 50 {
		t.Errorf("expected output tokens 50, got %d", result.Usage.OutputTokens)
	}
	if len(chunks) != 1 {
		t.Errorf("expected 1 chunk, got %d", len(chunks))
	}

	// Verify structured output parsed
	var output map[string]any
	if err := json.Unmarshal(result.Output, &output); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}
	if output["success"] != true {
		t.Errorf("expected success=true, got %v", output["success"])
	}

	// Verify steps: create session, set prompt, set message, stream, delete
	if step < 4 {
		t.Errorf("expected at least 4 API calls, got %d", step)
	}
}

// TestRovodevAgent_NoSchema tests that system prompt is skipped when no schema.
func TestRovodevAgent_NoSchema(t *testing.T) {
	calledPaths := make(map[string]bool)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calledPaths[r.URL.Path] = true
		switch {
		case r.URL.Path == "/v3/sessions/create":
			fmt.Fprint(w, `{"session_id":"s1"}`)
		case r.URL.Path == "/v3/set_chat_message":
			w.WriteHeader(http.StatusOK)
		case r.URL.Path == "/v3/stream_chat":
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "event: text\ndata: {\"content\":\"done\"}\n\n")
		case r.URL.Path == "/v3/sessions/s1":
			w.WriteHeader(http.StatusOK)
		default:
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	a := &rovodevAgent{
		bin:    "acli",
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
	if result.Text != "done" {
		t.Fatalf("expected plain text result, got %q", result.Text)
	}
	if result.Output != nil {
		t.Fatalf("expected nil structured output, got %s", string(result.Output))
	}

	// System prompt endpoint should NOT have been called
	if calledPaths["/v3/inline-system-prompt"] {
		t.Error("should not call inline-system-prompt when no schema")
	}
}

func mustParsePort(url string) int {
	// url format: http://127.0.0.1:PORT
	var port int
	fmt.Sscanf(url, "http://127.0.0.1:%d", &port)
	return port
}

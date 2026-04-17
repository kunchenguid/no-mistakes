package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestClaudeAgent_BuildArgs(t *testing.T) {
	ca := &claudeAgent{bin: "/usr/bin/claude"}
	schema := json.RawMessage(`{"type":"object"}`)
	args := ca.buildArgs("do something", schema)

	expected := []string{
		"-p", "do something",
		"--verbose",
		"--output-format", "stream-json",
		"--json-schema", `{"type":"object"}`,
		"--dangerously-skip-permissions",
	}

	if len(args) != len(expected) {
		t.Fatalf("expected %d args, got %d: %v", len(expected), len(args), args)
	}
	for i, want := range expected {
		if args[i] != want {
			t.Errorf("arg[%d]: expected %q, got %q", i, want, args[i])
		}
	}
}

func TestClaudeAgent_BuildArgs_NoSchema(t *testing.T) {
	ca := &claudeAgent{bin: "claude"}
	args := ca.buildArgs("prompt", nil)

	// Without schema, should not include --json-schema flag
	for _, arg := range args {
		if arg == "--json-schema" {
			t.Error("should not include --json-schema when schema is nil")
		}
	}
	// Should still have core args
	if args[0] != "-p" || args[1] != "prompt" {
		t.Error("missing -p flag")
	}
}

func TestParseClaudeEvents_AssistantMessage(t *testing.T) {
	events := `{"type":"assistant","message":{"usage":{"input_tokens":100,"output_tokens":50},"content":[{"type":"text","text":"hello world"}]}}
`
	var chunks []string
	var usage TokenUsage

	err := parseClaudeEvents(
		context.Background(),
		strings.NewReader(events),
		func(text string) { chunks = append(chunks, text) },
		&usage,
		nil,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) != 1 || chunks[0] != "hello world" {
		t.Errorf("expected chunk 'hello world', got %v", chunks)
	}
	if usage.InputTokens != 100 {
		t.Errorf("expected input tokens 100, got %d", usage.InputTokens)
	}
	if usage.OutputTokens != 50 {
		t.Errorf("expected output tokens 50, got %d", usage.OutputTokens)
	}
}

func TestParseClaudeEvents_ResultEvent(t *testing.T) {
	output := map[string]any{"success": true, "summary": "done"}
	outputJSON, _ := json.Marshal(output)
	event := map[string]any{
		"type":              "result",
		"subtype":           "success",
		"structured_output": json.RawMessage(outputJSON),
		"usage": map[string]any{
			"input_tokens":  200,
			"output_tokens": 100,
		},
	}
	line, _ := json.Marshal(event)

	var usage TokenUsage
	var result *claudeResult

	err := parseClaudeEvents(
		context.Background(),
		bytes.NewReader(append(line, '\n')),
		nil,
		&usage,
		&result,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected result event")
	}
	if result.Subtype != "success" {
		t.Errorf("expected subtype 'success', got %q", result.Subtype)
	}
	if result.StructuredOutput == nil {
		t.Fatal("expected structured_output")
	}
}

func TestParseClaudeEvents_LargeAssistantEvent(t *testing.T) {
	largeText := strings.Repeat("x", 128*1024)
	line, err := json.Marshal(map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"usage": map[string]any{
				"input_tokens":  10,
				"output_tokens": 5,
			},
			"content": []map[string]any{{
				"type": "text",
				"text": largeText,
			}},
		},
	})
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}

	var chunks []string
	var usage TokenUsage

	err = parseClaudeEvents(
		context.Background(),
		bytes.NewReader(append(line, '\n')),
		func(text string) { chunks = append(chunks, text) },
		&usage,
		nil,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) != 1 || chunks[0] != largeText {
		t.Fatalf("unexpected chunks: got %d chunks", len(chunks))
	}
	if usage.InputTokens != 10 || usage.OutputTokens != 5 {
		t.Fatalf("unexpected usage: %+v", usage)
	}
}

func TestParseClaudeEvents_MultipleEvents(t *testing.T) {
	events := strings.Join([]string{
		`{"type":"assistant","message":{"usage":{"input_tokens":50,"output_tokens":10},"content":[{"type":"text","text":"thinking..."}]}}`,
		`{"type":"assistant","message":{"usage":{"input_tokens":50,"output_tokens":40},"content":[{"type":"text","text":"done"}]}}`,
		`{"type":"result","subtype":"success","structured_output":{"success":true},"usage":{"input_tokens":100,"output_tokens":50}}`,
		"",
	}, "\n")

	var chunks []string
	var usage TokenUsage
	var result *claudeResult

	err := parseClaudeEvents(
		context.Background(),
		strings.NewReader(events),
		func(text string) { chunks = append(chunks, text) },
		&usage,
		&result,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) != 2 {
		t.Fatalf("expected 2 chunks, got %d: %v", len(chunks), chunks)
	}
	if chunks[0] != "thinking..." {
		t.Errorf("expected first chunk 'thinking...', got %q", chunks[0])
	}
	if chunks[1] != "done" {
		t.Errorf("expected second chunk 'done', got %q", chunks[1])
	}
	// Usage accumulates across assistant events
	if usage.InputTokens != 100 {
		t.Errorf("expected accumulated input tokens 100, got %d", usage.InputTokens)
	}
	if usage.OutputTokens != 50 {
		t.Errorf("expected accumulated output tokens 50, got %d", usage.OutputTokens)
	}
	if result == nil {
		t.Fatal("expected result event")
	}
}

func TestParseClaudeEvents_NoSeparatorForFirstMessage(t *testing.T) {
	events := `{"type":"assistant","message":{"usage":{"input_tokens":10,"output_tokens":5},"content":[{"type":"text","text":"only message"}]}}
`
	var chunks []string
	var usage TokenUsage

	err := parseClaudeEvents(
		context.Background(),
		strings.NewReader(events),
		func(text string) { chunks = append(chunks, text) },
		&usage,
		nil,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) != 1 || chunks[0] != "only message" {
		t.Errorf("expected 1 chunk 'only message', got %v", chunks)
	}
}

func TestParseClaudeEvents_NoSeparatorAfterToolOnlyEvent(t *testing.T) {
	// First assistant event has only tool_use (no text), second has text.
	// No separator because no text was emitted before.
	events := strings.Join([]string{
		`{"type":"assistant","message":{"usage":{"input_tokens":10,"output_tokens":5},"content":[{"type":"tool_use","text":""}]}}`,
		`{"type":"assistant","message":{"usage":{"input_tokens":10,"output_tokens":5},"content":[{"type":"text","text":"after tools"}]}}`,
		"",
	}, "\n")

	var chunks []string
	var usage TokenUsage

	err := parseClaudeEvents(
		context.Background(),
		strings.NewReader(events),
		func(text string) { chunks = append(chunks, text) },
		&usage,
		nil,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) != 1 || chunks[0] != "after tools" {
		t.Errorf("expected 1 chunk 'after tools', got %v", chunks)
	}
}

func TestParseClaudeEvents_DoesNotSeparateSplitAssistantReply(t *testing.T) {
	events := strings.Join([]string{
		`{"type":"assistant","message":{"usage":{"input_tokens":10,"output_tokens":5},"content":[{"type":"text","text":"hello "}]}}`,
		`{"type":"assistant","message":{"usage":{"input_tokens":10,"output_tokens":5},"content":[{"type":"text","text":"world"}]}}`,
		"",
	}, "\n")

	var chunks []string
	var usage TokenUsage

	err := parseClaudeEvents(
		context.Background(),
		strings.NewReader(events),
		func(text string) { chunks = append(chunks, text) },
		&usage,
		nil,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) != 2 {
		t.Fatalf("expected 2 chunks, got %d: %v", len(chunks), chunks)
	}
	if chunks[0] != "hello " || chunks[1] != "world" {
		t.Fatalf("expected streamed reply chunks, got %v", chunks)
	}
}

func TestParseClaudeEvents_SkipsMalformedLines(t *testing.T) {
	events := "not json\n{\"type\":\"assistant\",\"message\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":5},\"content\":[{\"type\":\"text\",\"text\":\"ok\"}]}}\n"

	var chunks []string
	var usage TokenUsage

	err := parseClaudeEvents(
		context.Background(),
		strings.NewReader(events),
		func(text string) { chunks = append(chunks, text) },
		&usage,
		nil,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(chunks) != 1 || chunks[0] != "ok" {
		t.Errorf("expected 1 chunk 'ok', got %v", chunks)
	}
}

func TestParseClaudeEvents_CacheTokens(t *testing.T) {
	events := `{"type":"assistant","message":{"usage":{"input_tokens":100,"output_tokens":50,"cache_read_input_tokens":30,"cache_creation_input_tokens":10},"content":[]}}
`
	var usage TokenUsage
	err := parseClaudeEvents(context.Background(), strings.NewReader(events), nil, &usage, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if usage.CacheReadTokens != 30 {
		t.Errorf("expected cache read tokens 30, got %d", usage.CacheReadTokens)
	}
	if usage.CacheCreationTokens != 10 {
		t.Errorf("expected cache creation tokens 10, got %d", usage.CacheCreationTokens)
	}
}

func TestParseClaudeEvents_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	// Create a reader that would block — but context cancellation should stop parsing
	events := `{"type":"assistant","message":{"usage":{"input_tokens":10,"output_tokens":5},"content":[{"type":"text","text":"ok"}]}}
`
	var usage TokenUsage
	err := parseClaudeEvents(ctx, strings.NewReader(events), nil, &usage, nil)
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
}

func TestParseClaudeEvents_ErrorResult(t *testing.T) {
	events := `{"type":"result","subtype":"error","is_error":true,"structured_output":null,"usage":{"input_tokens":0,"output_tokens":0}}
`
	var usage TokenUsage
	var result *claudeResult

	err := parseClaudeEvents(context.Background(), strings.NewReader(events), nil, &usage, &result)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected result")
	}
	if !result.IsError {
		t.Error("expected IsError to be true")
	}
}

func TestClaudeAgent_FinalizeResult_NoSchemaAllowsTextOnly(t *testing.T) {
	result, err := finalizeClaudeResult(&claudeResult{
		Subtype: "success",
		text:    "All tests pass. Here's what I fixed:",
	}, nil, TokenUsage{InputTokens: 10, OutputTokens: 5})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Text != "All tests pass. Here's what I fixed:" {
		t.Errorf("unexpected text: %q", result.Text)
	}
	if result.Output != nil {
		t.Fatalf("expected nil structured output, got %s", string(result.Output))
	}
	if result.Usage.InputTokens != 10 || result.Usage.OutputTokens != 5 {
		t.Errorf("unexpected usage: %+v", result.Usage)
	}
}

func TestClaudeAgent_FinalizeResult_WithSchemaRequiresStructuredOutput(t *testing.T) {
	_, err := finalizeClaudeResult(&claudeResult{Subtype: "success", text: "plain text"}, json.RawMessage(`{"type":"object"}`), TokenUsage{})
	if err == nil {
		t.Fatal("expected error when structured output is missing")
	}
	if !errors.Is(err, errNoStructuredOutput) {
		t.Fatalf("expected errNoStructuredOutput, got: %v", err)
	}
}

func TestClaudeAgent_FinalizeResult_ErrorSubtypeNotRetryable(t *testing.T) {
	_, err := finalizeClaudeResult(&claudeResult{Subtype: "error", IsError: true}, json.RawMessage(`{"type":"object"}`), TokenUsage{})
	if err == nil {
		t.Fatal("expected error for error subtype")
	}
	if errors.Is(err, errNoStructuredOutput) {
		t.Fatal("error subtype should not be retryable")
	}
}

func TestParseClaudeEvents_ResultCapturesRawEvent(t *testing.T) {
	events := `{"type":"result","subtype":"success","is_error":false,"structured_output":null}` + "\n"

	var usage TokenUsage
	var result *claudeResult

	err := parseClaudeEvents(context.Background(), strings.NewReader(events), nil, &usage, &result)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result == nil {
		t.Fatal("expected result event")
	}
	if result.rawEvent == nil {
		t.Fatal("expected rawEvent to be captured")
	}
	if !strings.Contains(string(result.rawEvent), `"subtype":"success"`) {
		t.Errorf("rawEvent should contain original JSON, got: %s", string(result.rawEvent))
	}
}

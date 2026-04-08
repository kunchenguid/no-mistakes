package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestNew_Claude(t *testing.T) {
	a, err := New(types.AgentClaude, "claude")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a.Name() != "claude" {
		t.Errorf("expected name %q, got %q", "claude", a.Name())
	}
}

func TestNew_Codex(t *testing.T) {
	a, err := New(types.AgentCodex, "codex")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a.Name() != "codex" {
		t.Errorf("expected name %q, got %q", "codex", a.Name())
	}
}

func TestNew_RovoDev(t *testing.T) {
	a, err := New(types.AgentRovoDev, "acli")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a.Name() != "rovodev" {
		t.Errorf("expected name %q, got %q", "rovodev", a.Name())
	}
}

func TestNew_OpenCode(t *testing.T) {
	a, err := New(types.AgentOpenCode, "opencode")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a.Name() != "opencode" {
		t.Errorf("expected name %q, got %q", "opencode", a.Name())
	}
}

func TestNew_Unknown(t *testing.T) {
	_, err := New("nonexistent", "foo")
	if err == nil {
		t.Fatal("expected error for unknown agent")
	}
	if !strings.Contains(err.Error(), "unknown agent") {
		t.Errorf("expected 'unknown agent' in error, got: %v", err)
	}
}

func TestTokenUsage_Total(t *testing.T) {
	u := TokenUsage{
		InputTokens:         100,
		OutputTokens:        50,
		CacheReadTokens:     20,
		CacheCreationTokens: 10,
	}
	if u.Total() != 150 {
		t.Errorf("expected total 150, got %d", u.Total())
	}
}

func TestTokenUsage_Add(t *testing.T) {
	a := TokenUsage{InputTokens: 100, OutputTokens: 50}
	b := TokenUsage{InputTokens: 200, OutputTokens: 75, CacheReadTokens: 30}
	a.Add(b)
	if a.InputTokens != 300 {
		t.Errorf("expected InputTokens 300, got %d", a.InputTokens)
	}
	if a.OutputTokens != 125 {
		t.Errorf("expected OutputTokens 125, got %d", a.OutputTokens)
	}
	if a.CacheReadTokens != 30 {
		t.Errorf("expected CacheReadTokens 30, got %d", a.CacheReadTokens)
	}
}

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
		t.Errorf("expected 2 chunks, got %d", len(chunks))
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
	if !strings.Contains(err.Error(), "no structured output") {
		t.Fatalf("expected no structured output error, got: %v", err)
	}
}

func TestFinalizeTextResult_NoSchemaAllowsTextOnly(t *testing.T) {
	result, err := finalizeTextResult("codex", "fixed it", nil, TokenUsage{InputTokens: 1, OutputTokens: 2})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Text != "fixed it" {
		t.Errorf("unexpected text: %q", result.Text)
	}
	if result.Output != nil {
		t.Fatalf("expected nil structured output, got %s", string(result.Output))
	}
	if result.Usage.InputTokens != 1 || result.Usage.OutputTokens != 2 {
		t.Errorf("unexpected usage: %+v", result.Usage)
	}
}

func TestFinalizeTextResult_WithSchemaParsesJSON(t *testing.T) {
	result, err := finalizeTextResult("codex", `{"done":true}`, json.RawMessage(`{"type":"object"}`), TokenUsage{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var output map[string]any
	if err := json.Unmarshal(result.Output, &output); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}
	if output["done"] != true {
		t.Errorf("expected done=true, got %v", output["done"])
	}
}

func TestCodexAgent_BuildArgs(t *testing.T) {
	ca := &codexAgent{bin: "codex"}
	args := ca.buildArgs("fix the bug")

	expected := []string{
		"exec", "fix the bug",
		"--json",
		"--dangerously-bypass-approvals-and-sandbox",
		"--color", "never",
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

func TestParseCodexEvents_AgentMessage(t *testing.T) {
	events := strings.Join([]string{
		`{"type":"item.completed","item":{"type":"agent_message","text":"{\"success\":true,\"summary\":\"done\"}"}}`,
		`{"type":"turn.completed","usage":{"input_tokens":200,"cached_input_tokens":50,"output_tokens":100}}`,
		"",
	}, "\n")

	var usage TokenUsage
	var lastMessage string

	err := parseCodexEvents(
		context.Background(),
		strings.NewReader(events),
		nil,
		&usage,
		&lastMessage,
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if lastMessage != `{"success":true,"summary":"done"}` {
		t.Errorf("unexpected last message: %s", lastMessage)
	}
	if usage.InputTokens != 200 {
		t.Errorf("expected input tokens 200, got %d", usage.InputTokens)
	}
	if usage.OutputTokens != 100 {
		t.Errorf("expected output tokens 100, got %d", usage.OutputTokens)
	}
	if usage.CacheReadTokens != 50 {
		t.Errorf("expected cache read tokens 50, got %d", usage.CacheReadTokens)
	}
}

func TestParseCodexEvents_SkipsMalformedLines(t *testing.T) {
	events := "garbage\n{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":10,\"output_tokens\":5}}\n"

	var usage TokenUsage
	var lastMessage string
	err := parseCodexEvents(context.Background(), strings.NewReader(events), nil, &usage, &lastMessage)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if usage.InputTokens != 10 {
		t.Errorf("expected 10 input tokens, got %d", usage.InputTokens)
	}
}

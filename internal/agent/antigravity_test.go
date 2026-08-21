package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestAntigravityAgent_BuildArgs(t *testing.T) {
	a := &antigravityAgent{bin: "agy"}
	args := a.buildArgs("test prompt", "", "")

	expected := []string{"--dangerously-skip-permissions", "--print", "test prompt", "--output-format", "stream-json"}
	if len(args) != len(expected) {
		t.Fatalf("expected %d args, got %d: %v", len(expected), len(args), args)
	}
	for i, want := range expected {
		if args[i] != want {
			t.Errorf("arg[%d]: expected %q, got %q", i, want, args[i])
		}
	}
}

func TestAntigravityAgent_BuildArgs_WithSchema(t *testing.T) {
	a := &antigravityAgent{bin: "agy"}
	args := a.buildArgs("test prompt", "/tmp/schema.json", "")

	expected := []string{"--dangerously-skip-permissions", "--print", "test prompt", "--json-schema", "/tmp/schema.json", "--output-format", "stream-json"}
	if len(args) != len(expected) {
		t.Fatalf("expected %d args, got %d: %v", len(expected), len(args), args)
	}
	for i, want := range expected {
		if args[i] != want {
			t.Errorf("arg[%d]: expected %q, got %q", i, want, args[i])
		}
	}
}

func TestAntigravityAgent_BuildArgs_WithExtraArgs(t *testing.T) {
	a := &antigravityAgent{bin: "agy", extraArgs: []string{"--debug"}}
	args := a.buildArgs("test prompt", "", "")

	expected := []string{"--debug", "--dangerously-skip-permissions", "--print", "test prompt", "--output-format", "stream-json"}
	if len(args) != len(expected) {
		t.Fatalf("expected %d args, got %d: %v", len(expected), len(args), args)
	}
	for i, want := range expected {
		if args[i] != want {
			t.Errorf("arg[%d]: expected %q, got %q", i, want, args[i])
		}
	}
}

func TestAntigravityAgent_BuildArgs_ResumesConversation(t *testing.T) {
	a := &antigravityAgent{bin: "agy"}
	args := a.buildArgs("test prompt", "", "conv-123")

	joined := strings.Join(args, " ")
	if !strings.Contains(joined, "--conversation conv-123") {
		t.Errorf("args = %v, want --conversation conv-123 for a resumable session", args)
	}
}

func TestAntigravityAgent_SupportsSessionResume(t *testing.T) {
	a := &antigravityAgent{bin: "agy"}
	if !SupportsSessionResume(a) {
		t.Fatal("SupportsSessionResume(antigravity) = false, want true")
	}
	for _, provider := range []string{"antigravity", "agy"} {
		if !SupportsSessionProvider(a, provider) {
			t.Errorf("SupportsSessionProvider(%q) = false, want true", provider)
		}
	}
	if SupportsSessionProvider(a, "claude") {
		t.Error("SupportsSessionProvider(claude) = true, want false")
	}
}

func TestAntigravityParser(t *testing.T) {
	stream := `
{"event": "step_update", "step_update": {"text_delta": "hello"}}
{"event": "step_update", "step_update": {"tool_call_delta": " world"}}
{"event": "step_update", "step_update": {"tool_info": {"parameters": {"tool": "info"}}}}
{"event": "step_update", "step_update": {"subagent_info": "doing subagent things"}}
{"event": "step_update", "step_update": {"usage": {"input_tokens": 10, "output_tokens": 5, "cache_read_tokens": 2}}}
{"event": "result", "result": {"status": "SUCCESS"}}
`
	buf := bytes.NewBufferString(stream)
	p := &antigravityParser{}
	err := p.parse(context.Background(), buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	text := p.finalText()
	expectedChunks := []string{
		"hello world",
		`{"tool":"info"}`,
		"doing subagent things",
	}

	for _, chunk := range expectedChunks {
		if !strings.Contains(text, chunk) {
			t.Errorf("expected text to contain %q, got %q", chunk, text)
		}
	}

	if p.usage.InputTokens != 10 {
		t.Errorf("expected 10 input tokens, got %d", p.usage.InputTokens)
	}
	if p.usage.OutputTokens != 5 {
		t.Errorf("expected 5 output tokens, got %d", p.usage.OutputTokens)
	}
	if p.usage.CacheReadTokens != 2 {
		t.Errorf("expected 2 cache read tokens, got %d", p.usage.CacheReadTokens)
	}
	if p.errorMessage != "" {
		t.Errorf("unexpected error message: %s", p.errorMessage)
	}
}

func TestAntigravityParser_StructuredOutputOverride(t *testing.T) {
	stream := `
{"event": "step_update", "step_update": {"text_delta": "hello"}}
{"event": "result", "result": {"status": "SUCCESS", "structured_output": {"success": true}}}
`
	buf := bytes.NewBufferString(stream)
	p := &antigravityParser{}
	err := p.parse(context.Background(), buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	text := p.finalText()
	expected := `{"success":true}`
	if text != expected {
		t.Errorf("expected %q, got %q", expected, text)
	}
}

func TestAntigravityParser_ErrorStatus(t *testing.T) {
	stream := `
{"event": "step_update", "step_update": {"text_delta": "failing"}}
{"event": "result", "result": {"status": "ERROR", "error": "something went wrong"}}
`
	buf := bytes.NewBufferString(stream)
	p := &antigravityParser{}
	err := p.parse(context.Background(), buf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if p.errorMessage != "something went wrong" {
		t.Errorf("expected error message 'something went wrong', got %q", p.errorMessage)
	}
}

func TestAntigravityParser_CapturesConversationID(t *testing.T) {
	stream := `
{"event": "init", "conversation_id": "conv-init"}
{"event": "step_update", "step_update": {"conversation_id": "conv-init", "text_delta": "working"}}
{"event": "result", "result": {"conversation_id": "conv-result", "status": "SUCCESS"}}
`
	buf := bytes.NewBufferString(stream)
	p := &antigravityParser{}
	if err := p.parse(context.Background(), buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The terminal result event names the conversation that actually served
	// the invocation, so it wins.
	if p.sessionID != "conv-result" {
		t.Errorf("sessionID = %q, want conv-result from the terminal result event", p.sessionID)
	}
}

func TestAntigravityParser_MapsThinkingTokensToReasoning(t *testing.T) {
	stream := `
{"event": "step_update", "step_update": {"usage": {"input_tokens": 10, "output_tokens": 5, "thinking_tokens": 42}}}
{"event": "result", "result": {"status": "SUCCESS", "usage": {"input_tokens": 12, "output_tokens": 7, "thinking_tokens": 50}}}
`
	buf := bytes.NewBufferString(stream)
	p := &antigravityParser{}
	if err := p.parse(context.Background(), buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The terminal result event's usage is authoritative for the invocation.
	if p.usage.ReasoningTokens != 50 {
		t.Errorf("ReasoningTokens = %d, want 50", p.usage.ReasoningTokens)
	}
	if !p.usage.ReasoningReported {
		t.Error("ReasoningReported = false, want true when thinking_tokens is present")
	}
	if p.usage.OutputTokens != 7 {
		t.Errorf("OutputTokens = %d, want 7 from result usage", p.usage.OutputTokens)
	}
}

func TestAntigravityParser_ThinkingTokensAbsentLeavesReasoningUnreported(t *testing.T) {
	stream := `
{"event": "step_update", "step_update": {"usage": {"input_tokens": 10, "output_tokens": 5}}}
{"event": "result", "result": {"status": "SUCCESS"}}
`
	buf := bytes.NewBufferString(stream)
	p := &antigravityParser{}
	if err := p.parse(context.Background(), buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if p.usage.ReasoningTokens != 0 {
		t.Errorf("ReasoningTokens = %d, want 0 when unreported", p.usage.ReasoningTokens)
	}
	if p.usage.ReasoningReported {
		t.Error("ReasoningReported = true, want false so a genuine zero stays distinguishable")
	}
}

func TestAntigravityParser_ResponseWinsOverStreamDeltas(t *testing.T) {
	stream := `
{"event": "step_update", "step_update": {"text_delta": "partial thought "}}
{"event": "step_update", "step_update": {"text_delta": "streamed aloud"}}
{"event": "result", "result": {"status": "SUCCESS", "response": "the final answer"}}
`
	buf := bytes.NewBufferString(stream)
	p := &antigravityParser{}
	if err := p.parse(context.Background(), buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	text := p.finalText()
	if text != "the final answer" {
		t.Errorf("finalText() = %q, want the authoritative result.response", text)
	}
}

func TestAntigravityParser_StructuredOutputWinsOverResponse(t *testing.T) {
	stream := `
{"event": "result", "result": {"status": "SUCCESS", "response": "{\"from\":\"response\"}", "structured_output": {"success": true}}}
`
	buf := bytes.NewBufferString(stream)
	p := &antigravityParser{}
	if err := p.parse(context.Background(), buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	text := p.finalText()
	expected := `{"success":true}`
	if text != expected {
		t.Errorf("finalText() = %q, want structured_output %q", text, expected)
	}
}

// writeFakeAgy writes a fake agy binary that emits the given JSONL
// lines on stdout (one echo per line) and exits with exitCode. It returns the
// path to the fake binary.
func writeFakeAgy(t *testing.T, dir string, jsonlLines []string, exitCode int) string {
	t.Helper()

	name := "agy"
	if runtime.GOOS == "windows" {
		name = "agy.cmd"
	}
	bin := filepath.Join(dir, name)

	var script string
	if runtime.GOOS == "windows" {
		lines := []string{"@echo off"}
		for _, l := range jsonlLines {
			lines = append(lines, "echo "+winEchoEscape(l))
		}
		lines = append(lines, "exit /b "+itoa(exitCode))
		script = strings.Join(lines, "\r\n")
	} else {
		lines := []string{"#!/bin/sh"}
		for _, l := range jsonlLines {
			lines = append(lines, "printf '%s\\n' "+shellSingleQuote(l))
		}
		lines = append(lines, "exit "+itoa(exitCode))
		script = strings.Join(lines, "\n") + "\n"
	}
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake agy: %v", err)
	}
	return bin
}

func TestAntigravityAgent_RunParsesJSONOutput(t *testing.T) {
	dir := t.TempDir()
	bin := writeFakeAgy(t, dir, []string{
		`{"event": "step_update", "step_update": {"text_delta": "{\"ok\":true}"}}`,
		`{"event": "result", "result": {"status": "SUCCESS"}}`,
	}, 0)

	var chunks []string
	ca := &antigravityAgent{bin: bin}
	result, err := ca.Run(context.Background(), RunOpts{
		Prompt:     "do work",
		CWD:        t.TempDir(),
		JSONSchema: json.RawMessage(`{"type":"object"}`),
		OnChunk:    func(text string) { chunks = append(chunks, text) },
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	var output map[string]bool
	if err := json.Unmarshal(result.Output, &output); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if !output["ok"] {
		t.Fatalf("output = %s, want ok true", string(result.Output))
	}
	if len(chunks) != 1 || chunks[0] != `{"ok":true}` {
		t.Errorf("chunks = %q", chunks)
	}
}

func TestAntigravityAgent_RunReportsErrorOnNonZeroExit(t *testing.T) {
	dir := t.TempDir()
	bin := writeFakeAgy(t, dir, []string{
		`{"event": "result", "result": {"status": "ERROR", "error": "not authenticated"}}`,
	}, 0) // exit with 0 so waitErr is nil, falling through to errorMessage check

	ca := &antigravityAgent{bin: bin}
	_, err := ca.Run(context.Background(), RunOpts{
		Prompt: "do work",
		CWD:    t.TempDir(),
	})
	if err == nil {
		t.Fatal("expected error on non-zero exit")
	}
	if !strings.Contains(err.Error(), "not authenticated") {
		t.Fatalf("error = %v, want antigravity error detail", err)
	}
}

func TestAntigravityAgent_RunReportsSchemaMiss(t *testing.T) {
	dir := t.TempDir()
	// The fake agent returns a SUCCESS result with plain prose that will
	// never satisfy a strict JSON schema requiring {"summary": string}.
	bin := writeFakeAgy(t, dir, []string{
		`{"event": "step_update", "step_update": {"text_delta": "Here is my analysis of the changes."}}`,
		`{"event": "result", "result": {"status": "SUCCESS"}}`,
	}, 0)

	schema := json.RawMessage(`{
		"type": "object",
		"properties": {"summary": {"type": "string"}},
		"required": ["summary"],
		"additionalProperties": false
	}`)

	ca := &antigravityAgent{bin: bin}
	_, err := ca.Run(context.Background(), RunOpts{
		Prompt:     "summarize",
		CWD:        t.TempDir(),
		JSONSchema: schema,
	})
	if err == nil {
		t.Fatal("expected schema-miss error when agent returns plain prose")
	}
	if !strings.Contains(err.Error(), "output parse") {
		t.Fatalf("error = %v, want schema/parse failure detail", err)
	}
}

// writeFakeAgyRecordingArgs writes a fake agy binary that appends its
// space-joined argv to $AGY_TEST_ARGS_FILE and then emits the given JSONL
// lines, so tests can assert on the exact flags no-mistakes passed.
func writeFakeAgyRecordingArgs(t *testing.T, dir string, jsonlLines []string) string {
	t.Helper()

	name := "agy"
	if runtime.GOOS == "windows" {
		name = "agy.cmd"
	}
	bin := filepath.Join(dir, name)

	var script string
	if runtime.GOOS == "windows" {
		lines := []string{"@echo off"}
		lines = append(lines, `echo %* >> "%AGY_TEST_ARGS_FILE%"`)
		for _, l := range jsonlLines {
			lines = append(lines, "echo "+winEchoEscape(l))
		}
		lines = append(lines, "exit /b 0")
		script = strings.Join(lines, "\r\n")
	} else {
		lines := []string{"#!/bin/sh"}
		lines = append(lines, `printf '%s\n' "$*" >> "$AGY_TEST_ARGS_FILE"`)
		for _, l := range jsonlLines {
			lines = append(lines, "printf '%s\\n' "+shellSingleQuote(l))
		}
		lines = append(lines, "exit 0")
		script = strings.Join(lines, "\n") + "\n"
	}
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake agy: %v", err)
	}
	return bin
}

func TestAntigravityAgent_RunReportsSessionIdentity(t *testing.T) {
	dir := t.TempDir()
	bin := writeFakeAgy(t, dir, []string{
		`{"event": "init", "conversation_id": "conv-fresh-1"}`,
		`{"event": "result", "result": {"conversation_id": "conv-fresh-1", "status": "SUCCESS", "response": "done"}}`,
	}, 0)

	ca := &antigravityAgent{bin: bin}
	result, err := ca.Run(context.Background(), RunOpts{Prompt: "do work", CWD: t.TempDir()})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.SessionID != "conv-fresh-1" {
		t.Errorf("SessionID = %q, want conv-fresh-1", result.SessionID)
	}
	if result.Provider != "antigravity" {
		t.Errorf("Provider = %q, want antigravity", result.Provider)
	}
	if result.Resumed {
		t.Error("Resumed = true, want false for a fresh session")
	}
}

func TestAntigravityAgent_RunResumesRecordedConversation(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "argv.jsonl")
	t.Setenv("AGY_TEST_ARGS_FILE", argsFile)
	bin := writeFakeAgyRecordingArgs(t, dir, []string{
		`{"event": "result", "result": {"conversation_id": "conv-123", "status": "SUCCESS", "response": "resumed answer"}}`,
	})

	ca := &antigravityAgent{bin: bin}
	result, err := ca.Run(context.Background(), RunOpts{
		Prompt: "continue",
		CWD:    t.TempDir(),
		Session: &SessionRef{
			ID:    "conv-123",
			Agent: "antigravity",
		},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	argsData, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("read args log: %v", err)
	}
	argv := strings.TrimSpace(string(argsData))
	if !strings.Contains(argv, "--conversation conv-123") {
		t.Errorf("argv = %q, want --conversation conv-123", argv)
	}
	if result.SessionID != "conv-123" {
		t.Errorf("SessionID = %q, want conv-123", result.SessionID)
	}
	if !result.Resumed {
		t.Error("Resumed = false, want true when the requested conversation served the turn")
	}
}

func TestAntigravityAgent_RunStaleConversationStartsFreshWithoutClaimingResume(t *testing.T) {
	dir := t.TempDir()
	bin := writeFakeAgy(t, dir, []string{
		`{"event": "init", "conversation_id": "conv-new-9"}`,
		`{"event": "result", "result": {"conversation_id": "conv-new-9", "status": "SUCCESS", "response": "fresh start"}}`,
	}, 0)

	ca := &antigravityAgent{bin: bin}
	result, err := ca.Run(context.Background(), RunOpts{
		Prompt: "continue",
		CWD:    t.TempDir(),
		Session: &SessionRef{
			ID:    "conv-pruned",
			Agent: "antigravity",
		},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Resumed {
		t.Error("Resumed = true, want false when agy silently started a fresh conversation")
	}
	if result.SessionID != "conv-new-9" {
		t.Errorf("SessionID = %q, want conv-new-9 so the new identity is persisted", result.SessionID)
	}
}

func TestAntigravityAgent_RunCarriesUsageAndResponsePrecedence(t *testing.T) {
	dir := t.TempDir()
	bin := writeFakeAgy(t, dir, []string{
		`{"event": "step_update", "step_update": {"text_delta": "streamed prose"}}`,
		`{"event": "step_update", "step_update": {"usage": {"input_tokens": 100, "output_tokens": 20, "thinking_tokens": 8}}}`,
		`{"event": "result", "result": {"status": "SUCCESS", "response": "final verdict", "usage": {"input_tokens": 110, "output_tokens": 25, "thinking_tokens": 9}}}`,
	}, 0)

	var chunks []string
	ca := &antigravityAgent{bin: bin}
	result, err := ca.Run(context.Background(), RunOpts{
		Prompt:  "do work",
		CWD:     t.TempDir(),
		OnChunk: func(text string) { chunks = append(chunks, text) },
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.Text != "final verdict" {
		t.Errorf("Text = %q, want the authoritative result.response", result.Text)
	}
	if !result.UsageReported {
		t.Error("UsageReported = false, want true")
	}
	if result.Usage.ReasoningTokens != 9 || !result.Usage.ReasoningReported {
		t.Errorf("Usage.ReasoningTokens/ReasoningReported = %d/%v, want 9/true", result.Usage.ReasoningTokens, result.Usage.ReasoningReported)
	}
	if len(chunks) == 0 || chunks[0] != "streamed prose" {
		t.Errorf("chunks = %q, want streamed deltas still delivered", chunks)
	}
}

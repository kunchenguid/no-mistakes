package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestAntigravityAgent_BuildArgs(t *testing.T) {
	ag := &antigravityAgent{bin: "agy"}
	opts := RunOpts{}
	args := ag.buildArgs("fix the bug", opts)

	expected := []string{
		"-p", "fix the bug",
		"--output-format", "stream-json",
		"--dangerously-skip-permissions",
		"--print-timeout", "30m",
		"--new-project",
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

func TestAntigravityAgent_BuildArgs_ExtraArgsAndOptions(t *testing.T) {
	ag := &antigravityAgent{
		bin:                    "agy",
		extraArgs:              []string{"--model", "gemini-2.5-pro", "--effort", "high"},
		disableProjectSettings: true,
	}
	opts := RunOpts{
		JSONSchema: json.RawMessage(`{"type":"object"}`),
		Session:    &SessionRef{ID: "session-12345", Agent: "antigravity"},
	}
	args := ag.buildArgs("review code", opts)

	expected := []string{
		"--disable-slash-commands",
		"--model", "gemini-2.5-pro",
		"--effort", "high",
		"-p", "review code",
		"--output-format", "stream-json",
		"--dangerously-skip-permissions",
		"--print-timeout", "30m",
		"--json-schema", `{"type":"object"}`,
		"--conversation", "session-12345",
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

func TestAntigravityAgent_SupportsCapabilities(t *testing.T) {
	ag := &antigravityAgent{bin: "agy", disableProjectSettings: true}

	if !ag.ReportsAgentAttempts() {
		t.Error("expected ReportsAgentAttempts() = true")
	}
	if !ag.SupportsSessionResume() {
		t.Error("expected SupportsSessionResume() = true")
	}
	if !ag.SupportsSessionProvider("antigravity") || !ag.SupportsSessionProvider("agy") {
		t.Error("expected SupportsSessionProvider to support antigravity and agy")
	}
	if ag.SupportsSessionProvider("claude") {
		t.Error("expected SupportsSessionProvider(claude) = false")
	}
	if !ag.NeutralizesGateInstructions() {
		t.Error("expected NeutralizesGateInstructions() = true when disableProjectSettings is true")
	}

	agDisabled := &antigravityAgent{bin: "agy", disableProjectSettings: false}
	if agDisabled.NeutralizesGateInstructions() {
		t.Error("expected NeutralizesGateInstructions() = false when disableProjectSettings is false")
	}

	created, err := NewWithOptions(types.AgentAntigravity, "agy", nil, Options{DisableProjectSettings: true})
	if err != nil {
		t.Fatalf("NewWithOptions failed: %v", err)
	}
	if !NeutralizesGateInstructions(created) {
		t.Error("expected NewWithOptions with DisableProjectSettings: true to neutralize gate instructions")
	}
}

func TestAntigravityAgent_ParseEvents_Success(t *testing.T) {
	stream := strings.Join([]string{
		`{"event":"init","conversation_id":"conv-abc","init":{"cwd":"/repo","tools":["run_command"]}}`,
		`{"event":"step_update","step_update":{"conversation_id":"conv-abc","step_index":0,"state":"ACTIVE","step_type":"agent_response","text_delta":"Hello "}}`,
		`{"event":"step_update","step_update":{"conversation_id":"conv-abc","step_index":0,"state":"DONE","step_type":"agent_response","text_delta":"world!"}}`,
		`{"event":"result","result":{"conversation_id":"conv-abc","status":"SUCCESS","response":"Hello world!","num_turns":1,"usage":{"input_tokens":120,"output_tokens":45,"thinking_tokens":30,"cache_read_tokens":10,"total_tokens":165}}}`,
	}, "\n")

	var (
		usage            TokenUsage
		sessionID        string
		status           = "SUCCESS"
		resultResponse   string
		resultStructured json.RawMessage
		resultError      string
		chunks           []string
	)

	onChunk := func(c string) {
		chunks = append(chunks, c)
	}

	err := parseAntigravityEvents(
		context.Background(),
		strings.NewReader(stream),
		onChunk,
		&usage,
		&sessionID,
		&status,
		&resultResponse,
		&resultStructured,
		&resultError,
	)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	if sessionID != "conv-abc" {
		t.Errorf("sessionID = %q, want conv-abc", sessionID)
	}
	if status != "SUCCESS" {
		t.Errorf("status = %q, want SUCCESS", status)
	}
	if resultResponse != "Hello world!" {
		t.Errorf("resultResponse = %q, want Hello world!", resultResponse)
	}
	if strings.Join(chunks, "") != "Hello world!" {
		t.Errorf("chunks = %q, want Hello world!", strings.Join(chunks, ""))
	}
	if !usage.Reported {
		t.Error("expected usage.Reported = true")
	}
	if usage.InputTokens != 120 {
		t.Errorf("usage.InputTokens = %d, want 120", usage.InputTokens)
	}
	if usage.OutputTokens != 45 {
		t.Errorf("usage.OutputTokens = %d, want 45", usage.OutputTokens)
	}
	if usage.ReasoningTokens != 30 {
		t.Errorf("usage.ReasoningTokens = %d, want 30", usage.ReasoningTokens)
	}
	if usage.CacheReadTokens != 10 {
		t.Errorf("usage.CacheReadTokens = %d, want 10", usage.CacheReadTokens)
	}
}

func TestAntigravityAgent_ParseEvents_ErrorStatus(t *testing.T) {
	stream := strings.Join([]string{
		`{"event":"init","conversation_id":"conv-err"}`,
		`{"event":"result","result":{"conversation_id":"conv-err","status":"ERROR","error":"rate limit exceeded"}}`,
	}, "\n")

	var (
		usage            TokenUsage
		sessionID        string
		status           = "SUCCESS"
		resultResponse   string
		resultStructured json.RawMessage
		resultError      string
	)

	err := parseAntigravityEvents(
		context.Background(),
		strings.NewReader(stream),
		nil,
		&usage,
		&sessionID,
		&status,
		&resultResponse,
		&resultStructured,
		&resultError,
	)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	if status != "ERROR" {
		t.Errorf("status = %q, want ERROR", status)
	}
	if resultError != "rate limit exceeded" {
		t.Errorf("resultError = %q, want 'rate limit exceeded'", resultError)
	}
}

func TestAntigravityAgent_RunOnce_FakeBinary(t *testing.T) {
	tempDir := t.TempDir()
	binPath := filepath.Join(tempDir, "fake-agy")

	streamOutput := `{"event":"init","conversation_id":"fake-session-999"}
{"event":"step_update","step_update":{"conversation_id":"fake-session-999","step_index":0,"state":"DONE","text_delta":"{\"verdict\":\"pass\"}\n"}}
{"event":"result","result":{"conversation_id":"fake-session-999","status":"SUCCESS","response":"{\"verdict\":\"pass\"}","usage":{"input_tokens":50,"output_tokens":10,"thinking_tokens":5,"total_tokens":60}}}
`

	if runtime.GOOS == "windows" {
		binPath += ".cmd"
		script := "@echo off\r\necho " + strings.ReplaceAll(streamOutput, "\n", "\r\necho ")
		if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	} else {
		script := "#!/bin/sh\ncat << 'EOF'\n" + streamOutput + "EOF\n"
		if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	ag := &antigravityAgent{bin: binPath}
	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"verdict": {"type": "string", "enum": ["pass", "fail"]}
		},
		"required": ["verdict"]
	}`)

	res, err := ag.Run(context.Background(), RunOpts{
		Prompt:     "check something",
		JSONSchema: schema,
		CWD:        tempDir,
		Session:    &SessionRef{ID: "fake-session-999", Agent: "antigravity"},
	})
	if err != nil {
		t.Fatalf("Run failed: %v", err)
	}

	if res.SessionID != "fake-session-999" {
		t.Errorf("res.SessionID = %q, want fake-session-999", res.SessionID)
	}
	if !res.Resumed {
		t.Error("expected res.Resumed = true")
	}
	if res.Provider != "antigravity" {
		t.Errorf("res.Provider = %q, want antigravity", res.Provider)
	}
	if res.Usage.InputTokens != 50 || res.Usage.OutputTokens != 10 || res.Usage.ReasoningTokens != 5 {
		t.Errorf("unexpected usage: %+v", res.Usage)
	}

	var outputMap map[string]any
	if err := json.Unmarshal(res.Output, &outputMap); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if outputMap["verdict"] != "pass" {
		t.Errorf("verdict = %v, want pass", outputMap["verdict"])
	}
}

func TestBuildAntigravityPrompt(t *testing.T) {
	prompt := "do work"
	res := buildAntigravityPrompt(prompt, nil)
	if res != prompt {
		t.Errorf("without schema: got %q, want %q", res, prompt)
	}

	schema := json.RawMessage(`{"type":"object"}`)
	resWithSchema := buildAntigravityPrompt(prompt, schema)
	if !strings.Contains(resWithSchema, "final assistant response must be only valid JSON") {
		t.Errorf("missing contract in prompt: %s", resWithSchema)
	}
}

func TestExtractLastJSONObject(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
		found    bool
	}{
		{
			name:     "clean json",
			input:    `{"verdict":"pass"}`,
			expected: `{"verdict":"pass"}`,
			found:    true,
		},
		{
			name:     "prose before json",
			input:    `Here is the review result:\n{"verdict":"pass","findings":[]}`,
			expected: `{"verdict":"pass","findings":[]}`,
			found:    true,
		},
		{
			name:     "multiple json objects",
			input:    `{"intermediate":1}{"verdict":"fail"}`,
			expected: `{"verdict":"fail"}`,
			found:    true,
		},
		{
			name:     "no valid json",
			input:    `This is just plain text with { unclosed brace`,
			expected: ``,
			found:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, found := extractLastJSONObject(tt.input)
			if found != tt.found {
				t.Fatalf("expected found=%v, got %v", tt.found, found)
			}
			if found && string(got) != tt.expected {
				t.Errorf("got %s, want %s", string(got), tt.expected)
			}
		})
	}
}

func TestAntigravityAgent_ParseEvents_WithToolAndSubagentDeltas(t *testing.T) {
	stream := strings.Join([]string{
		`{"event":"step_update","step_update":{"text_delta":"Starting tool: "}}`,
		`{"event":"step_update","step_update":{"tool_call_delta":"run_command "}}`,
		`{"event":"step_update","step_update":{"arguments_delta":"git status"}}`,
		`{"event":"step_update","step_update":{"tool_info":{"command":"git status"}}}`,
		`{"event":"step_update","step_update":{"subagent_info":"researching codebase"}}`,
		`{"event":"result","result":{"status":"SUCCESS","response":"Done"}}`,
	}, "\n")

	var (
		usage            TokenUsage
		sessionID        string
		status           = "SUCCESS"
		resultResponse   string
		resultStructured json.RawMessage
		resultError      string
		chunks           []string
	)

	onChunk := func(c string) {
		chunks = append(chunks, c)
	}

	err := parseAntigravityEvents(
		context.Background(),
		strings.NewReader(stream),
		onChunk,
		&usage,
		&sessionID,
		&status,
		&resultResponse,
		&resultStructured,
		&resultError,
	)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}

	combined := strings.Join(chunks, "")
	if !strings.Contains(combined, "Starting tool: run_command git status") {
		t.Errorf("missing tool deltas in stream: %q", combined)
	}
	if !strings.Contains(combined, "git status") {
		t.Errorf("missing tool info in stream: %q", combined)
	}
	if !strings.Contains(combined, "researching codebase") {
		t.Errorf("missing subagent info in stream: %q", combined)
	}
}

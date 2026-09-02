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

func TestNew_KnownAgents(t *testing.T) {
	tests := []struct {
		name     string
		agent    types.AgentName
		bin      string
		wantName string
	}{
		{name: "claude", agent: types.AgentClaude, bin: "claude", wantName: "claude"},
		{name: "codex", agent: types.AgentCodex, bin: "codex", wantName: "codex"},
		{name: "grok", agent: types.AgentGrok, bin: "grok", wantName: "grok"},
		{name: "rovodev", agent: types.AgentRovoDev, bin: "acli", wantName: "rovodev"},
		{name: "opencode", agent: types.AgentOpenCode, bin: "opencode", wantName: "opencode"},
		{name: "pi", agent: types.AgentPi, bin: "pi", wantName: "pi"},
		{name: "copilot", agent: types.AgentCopilot, bin: "copilot", wantName: "copilot"},
		{name: "cursor alias", agent: types.AgentCursor, bin: "acpx", wantName: "acp:cursor"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, err := New(tt.agent, tt.bin, nil)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if a.Name() != tt.wantName {
				t.Errorf("expected name %q, got %q", tt.wantName, a.Name())
			}
		})
	}
}

func TestNew_ACPAgent(t *testing.T) {
	a, err := New("acp:gemini", "acpx", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a.Name() != "acp:gemini" {
		t.Errorf("name = %q, want acp:gemini", a.Name())
	}
}

func TestNewWithOptions_ACPRegistryOverride(t *testing.T) {
	a, err := NewWithOptions("acp:local-gemini", "acpx", nil, Options{
		ACPRegistryOverrides: map[string]string{"local-gemini": "node /tmp/mock-acp.mjs"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	acpx, ok := a.(*acpxAgent)
	if !ok {
		t.Fatalf("agent type = %T, want *acpxAgent", a)
	}
	args := acpx.buildArgs(RunOpts{Prompt: "do work", CWD: "/repo"})
	joined := strings.Join(args, "\x00")
	if !strings.Contains(joined, "--agent\x00node /tmp/mock-acp.mjs") {
		t.Fatalf("args = %q, want raw --agent override", args)
	}
	if strings.Contains(joined, "\x00local-gemini\x00") {
		t.Fatalf("args = %q, should not include target subcommand when override is used", args)
	}
}

func TestACPAliasUsesDefaultCommand(t *testing.T) {
	a, err := New(types.AgentCursor, "acpx", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	acpx, ok := a.(*acpxAgent)
	if !ok {
		t.Fatalf("agent type = %T, want *acpxAgent", a)
	}
	if acpx.target != "cursor" {
		t.Errorf("target = %q, want cursor", acpx.target)
	}
	if acpx.rawCommand != "cursor-agent acp" {
		t.Errorf("rawCommand = %q, want cursor-agent acp", acpx.rawCommand)
	}
	args := acpx.buildArgs(RunOpts{Prompt: "do work"})
	joined := strings.Join(args, "\x00")
	if !strings.Contains(joined, "--agent\x00cursor-agent acp") {
		t.Fatalf("args = %q, want alias default command", args)
	}
}

func TestACPTargetUsesAliasDefaultCommand(t *testing.T) {
	a, err := New("acp:cursor", "acpx", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	acpx, ok := a.(*acpxAgent)
	if !ok {
		t.Fatalf("agent type = %T, want *acpxAgent", a)
	}
	if acpx.target != "cursor" {
		t.Errorf("target = %q, want cursor", acpx.target)
	}
	if acpx.rawCommand != "cursor-agent acp" {
		t.Errorf("rawCommand = %q, want cursor-agent acp", acpx.rawCommand)
	}
	args := acpx.buildArgs(RunOpts{Prompt: "do work"})
	joined := strings.Join(args, "\x00")
	if !strings.Contains(joined, "--agent\x00cursor-agent acp") {
		t.Fatalf("args = %q, want target default command", args)
	}
}

func TestACPAliasRegistryOverrideRespected(t *testing.T) {
	a, err := NewWithOptions(types.AgentCursor, "acpx", nil, Options{
		ACPRegistryOverrides: map[string]string{"cursor": "/opt/cursor/cursor-agent acp --profile work"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	acpx, ok := a.(*acpxAgent)
	if !ok {
		t.Fatalf("agent type = %T, want *acpxAgent", a)
	}
	if acpx.rawCommand != "/opt/cursor/cursor-agent acp --profile work" {
		t.Errorf("rawCommand = %q, want override value", acpx.rawCommand)
	}
}

func TestACPAliasBlankRegistryOverrideUsesDefaultCommand(t *testing.T) {
	a, err := NewWithOptions(types.AgentCursor, "acpx", nil, Options{
		ACPRegistryOverrides: map[string]string{"cursor": " \t"},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	acpx, ok := a.(*acpxAgent)
	if !ok {
		t.Fatalf("agent type = %T, want *acpxAgent", a)
	}
	if acpx.rawCommand != "cursor-agent acp" {
		t.Errorf("rawCommand = %q, want cursor-agent acp", acpx.rawCommand)
	}
}

func TestACPAgentBuildArgsUsesExecMode(t *testing.T) {
	a := &acpxAgent{target: "gemini"}
	args := a.buildArgs(RunOpts{Prompt: "do work"})

	if got, want := args[len(args)-4:], []string{"gemini", "exec", "--file", "-"}; strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("trailing args = %q, want %q", got, want)
	}
}

func TestACPAgentRunReportsJSONRPCErrorMessage(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-only")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "acpx")
	contents := `#!/bin/sh
printf '%s\n' '{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"not authenticated"}}'
exit 1
`
	if err := os.WriteFile(script, []byte(contents), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	a, err := New("acp:gemini", script, nil)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	_, err = a.Run(context.Background(), RunOpts{Prompt: "do work", CWD: dir})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "not authenticated") {
		t.Fatalf("error = %v, want JSON-RPC error message", err)
	}
}

func TestParseAcpxJSONEventsParsesUsageFields(t *testing.T) {
	events := strings.Join([]string{
		`{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"usage_update","input_tokens":100,"output_tokens":50,"cache_read_input_tokens":30,"cache_creation_input_tokens":10}}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"usage_update","_meta":{"usage":{"inputTokens":120,"outputTokens":60,"cacheReadInputTokens":40,"cacheCreationInputTokens":15}}}}}`,
		`{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"done"}}}}`,
	}, "\n") + "\n"
	var usage TokenUsage

	text, stdoutErr, err := parseAcpxJSONEvents(context.Background(), strings.NewReader(events), nil, &usage)
	if err != nil {
		t.Fatalf("parseAcpxJSONEvents() error = %v", err)
	}
	if stdoutErr != "" {
		t.Fatalf("stdout error = %q, want empty", stdoutErr)
	}
	if text != "done" {
		t.Fatalf("text = %q, want done", text)
	}
	want := TokenUsage{InputTokens: 120, OutputTokens: 60, CacheReadTokens: 40, CacheCreationTokens: 15, Reported: true, CacheCreationReported: true}
	if usage != want {
		t.Fatalf("usage = %+v, want %+v", usage, want)
	}
}

func TestParseAcpxJSONEventsParsesCacheWriteUsageFields(t *testing.T) {
	events := `{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"usage_update","input_tokens":5,"output_tokens":3,"cache_write_tokens":7}}}` + "\n"
	var usage TokenUsage

	_, _, err := parseAcpxJSONEvents(context.Background(), strings.NewReader(events), nil, &usage)
	if err != nil {
		t.Fatalf("parseAcpxJSONEvents() error = %v", err)
	}
	if usage.CacheCreationTokens != 7 {
		t.Fatalf("cache creation tokens = %d, want 7", usage.CacheCreationTokens)
	}
}

func TestParseAcpxJSONEventsParsesNormalizedCachedUsageFields(t *testing.T) {
	events := `{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"usage_update","inputTokens":5,"outputTokens":3,"cachedReadTokens":11,"cachedWriteTokens":13}}}` + "\n"
	var usage TokenUsage

	_, _, err := parseAcpxJSONEvents(context.Background(), strings.NewReader(events), nil, &usage)
	if err != nil {
		t.Fatalf("parseAcpxJSONEvents() error = %v", err)
	}
	want := TokenUsage{InputTokens: 5, OutputTokens: 3, CacheReadTokens: 11, CacheCreationTokens: 13, Reported: true, CacheCreationReported: true}
	if usage != want {
		t.Fatalf("usage = %+v, want %+v", usage, want)
	}
}

func TestParseAcpxJSONEventsParsesResultUsage(t *testing.T) {
	events := `{"jsonrpc":"2.0","id":1,"result":{"usage":{"input_tokens":21,"output_tokens":8,"cachedReadTokens":5,"cachedWriteTokens":2}}}` + "\n"
	var usage TokenUsage

	_, _, err := parseAcpxJSONEvents(context.Background(), strings.NewReader(events), nil, &usage)
	if err != nil {
		t.Fatalf("parseAcpxJSONEvents() error = %v", err)
	}
	want := TokenUsage{InputTokens: 21, OutputTokens: 8, CacheReadTokens: 5, CacheCreationTokens: 2, Reported: true, CacheCreationReported: true}
	if usage != want {
		t.Fatalf("usage = %+v, want %+v", usage, want)
	}
}

func TestACPAgentRunParsesAcpxJSONOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-only")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "acpx")
	argLog := filepath.Join(dir, "args.txt")
	stdinLog := filepath.Join(dir, "stdin.txt")
	t.Setenv("ARG_LOG", argLog)
	t.Setenv("STDIN_LOG", stdinLog)
	contents := `#!/bin/sh
printf '%s\n' "$@" > "$ARG_LOG"
cat > "$STDIN_LOG"
printf '%s\n' '{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"usage_update","used":123,"size":1000}}}'
printf '%s\n' '{"jsonrpc":"2.0","method":"session/update","params":{"update":{"sessionUpdate":"agent_message_chunk","content":{"type":"text","text":"{\"done\":true}"}}}}'
`
	if err := os.WriteFile(script, []byte(contents), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	a, err := New("acp:gemini", script, nil)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	var chunks []string
	schema := json.RawMessage(`{"type":"object"}`)
	result, err := a.Run(context.Background(), RunOpts{
		Prompt:     "do work",
		CWD:        dir,
		JSONSchema: schema,
		OnChunk:    func(text string) { chunks = append(chunks, text) },
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	var output map[string]bool
	if err := json.Unmarshal(result.Output, &output); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if !output["done"] {
		t.Fatalf("output = %s, want done true", string(result.Output))
	}
	if result.Usage.InputTokens != 123 {
		t.Errorf("input tokens = %d, want 123", result.Usage.InputTokens)
	}
	if len(chunks) != 1 || chunks[0] != `{"done":true}` {
		t.Errorf("chunks = %q", chunks)
	}
	argsData, err := os.ReadFile(argLog)
	if err != nil {
		t.Fatalf("read args: %v", err)
	}
	argsText := string(argsData)
	for _, want := range []string{"--cwd\n" + dir, "--format\njson", "--json-strict", "gemini", "exec\n--file\n-"} {
		if !strings.Contains(argsText, want) {
			t.Errorf("args missing %q in:\n%s", want, argsText)
		}
	}
	if strings.Contains(argsText, "do work") {
		t.Errorf("args contain prompt:\n%s", argsText)
	}
	stdinData, err := os.ReadFile(stdinLog)
	if err != nil {
		t.Fatalf("read stdin: %v", err)
	}
	if got, want := string(stdinData), buildACPStructuredPrompt("do work", schema); got != want {
		t.Errorf("stdin = %q, want %q", got, want)
	}
}

func TestNew_Unknown(t *testing.T) {
	_, err := New("nonexistent", "foo", nil)
	if err == nil {
		t.Fatal("expected error for unknown agent")
	}
	if !strings.Contains(err.Error(), "unknown agent") {
		t.Errorf("expected 'unknown agent' in error, got: %v", err)
	}
	if !strings.Contains(err.Error(), string(types.AgentAuto)) {
		t.Errorf("expected auto agent option in error, got: %v", err)
	}
	if !strings.Contains(err.Error(), "config.yaml") {
		t.Errorf("expected config guidance in error, got: %v", err)
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

func TestFinalizeTextResult_WithSchemaPreservesTypeErrorForValidJSON(t *testing.T) {
	for _, text := range []string{"[]", "null"} {
		t.Run(text, func(t *testing.T) {
			_, err := finalizeTextResult("codex", text, json.RawMessage(`{"type":"object"}`), TokenUsage{})
			if err == nil {
				t.Fatal("expected schema validation error")
			}
			if strings.Contains(err.Error(), "ended its turn with prose") {
				t.Fatalf("schema error was masked as prose turn ending: %v", err)
			}
			if !strings.Contains(err.Error(), "JSON output must be object") {
				t.Fatalf("expected object type error, got: %v", err)
			}
		})
	}
}

func TestFinalizeTextResult_WithSchemaParsesFencedJSON(t *testing.T) {
	text := "review complete\n\n```json\n{\"done\":true}\n```"
	result, err := finalizeTextResult("codex", text, json.RawMessage(`{"type":"object"}`), TokenUsage{})
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
	if result.Text != text {
		t.Errorf("expected original text to be preserved, got %q", result.Text)
	}
}

func TestFinalizeTextResult_WithSchemaParsesInlineOpenFence(t *testing.T) {
	// Codex/GPT-5 sometimes glues the opening ```json fence to the end of
	// the prior reasoning line, with no newline between text and backticks.
	text := "thinking about edge cases now.```json\n{\"done\":true}\n```"
	result, err := finalizeTextResult("codex", text, json.RawMessage(`{"type":"object"}`), TokenUsage{})
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

func TestFinalizeTextResult_WithSchemaParsesInlineCloseFence(t *testing.T) {
	// Symmetric case: closing fence immediately follows the JSON with no
	// newline before the backticks.
	text := "prelude\n```json\n{\"done\":true}```"
	result, err := finalizeTextResult("codex", text, json.RawMessage(`{"type":"object"}`), TokenUsage{})
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

func TestFinalizeTextResult_WithSchemaParsesBareJSONAfterText(t *testing.T) {
	// No fence at all: reasoning prose followed by a raw JSON object.
	text := "Here's the review:\n{\"done\":true}"
	result, err := finalizeTextResult("codex", text, json.RawMessage(`{"type":"object"}`), TokenUsage{})
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

func TestFinalizeTextResult_WithSchemaIgnoresNonMatchingBareJSON(t *testing.T) {
	// If reasoning text embeds a non-matching JSON object and the final
	// answer is a schema-valid object, the schema-valid one should be returned.
	text := `I considered {"foo":"bar"} as one option. Final: {"done":true}`
	schema := json.RawMessage(`{
		"type":"object",
		"properties":{
			"done":{"type":"boolean"}
		},
		"required":["done"]
	}`)
	result, err := finalizeTextResult("codex", text, schema, TokenUsage{})
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

func TestFinalizeTextResult_WithSchemaRejectsAmbiguousBareJSON(t *testing.T) {
	// If multiple bare JSON objects in prose validate against the schema,
	// reject rather than silently picking the last one and discarding prior verdicts/findings.
	text := `First verdict: {"findings":["issue 1"],"summary":"first"}. Another candidate: {"findings":[],"summary":"second"}`
	schema := json.RawMessage(`{
		"type":"object",
		"properties":{
			"findings":{"type":"array"},
			"summary":{"type":"string"}
		},
		"required":["findings","summary"]
	}`)
	_, err := finalizeTextResult("codex", text, schema, TokenUsage{})
	if err == nil {
		t.Fatal("expected ambiguous bare JSON to fail")
	}
	if !strings.Contains(err.Error(), "multiple bare JSON objects") {
		t.Fatalf("expected multiple bare JSON objects error, got %v", err)
	}
}

func TestFinalizeTextResult_ACPAgentTakesTerminalBareJSON(t *testing.T) {
	text := `Draft: {"findings":["a"],"summary":"draft"}. Final: {"findings":["a"],"summary":"final"}`
	schema := json.RawMessage(`{
		"type":"object",
		"properties":{
			"findings":{"type":"array"},
			"summary":{"type":"string"}
		},
		"required":["findings","summary"]
	}`)
	result, err := finalizeTextResult("acp:omp", text, schema, TokenUsage{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var output map[string]any
	if err := json.Unmarshal(result.Output, &output); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}
	if output["summary"] != "final" {
		t.Errorf("expected summary=final, got %v", output["summary"])
	}
}

func TestFinalizeTextResult_WithSchemaRejectsBareJSONMissingRequiredKeys(t *testing.T) {
	text := `I inspected the diff and found no issues. {"foo":"bar"}`
	schema := json.RawMessage(`{
		"type":"object",
		"properties":{
			"findings":{"type":"array"},
			"summary":{"type":"string"}
		},
		"required":["findings","summary"]
	}`)

	_, err := finalizeTextResult("codex", text, schema, TokenUsage{})
	if err == nil {
		t.Fatal("expected bare JSON missing required keys to fail")
	}
}

func TestFinalizeTextResult_WithSchemaRejectsNestedEnumViolations(t *testing.T) {
	text := `review complete {"findings":[{"severity":"fatal","description":"x","action":"fix-it"}],"summary":"1 issue"}`
	schema := json.RawMessage(`{
		"type":"object",
		"properties":{
			"findings":{
				"type":"array",
				"items":{
					"type":"object",
					"properties":{
						"severity":{"type":"string","enum":["error","warning","info"]},
						"description":{"type":"string"},
						"action":{"type":"string","enum":["auto-fix","ask-user","no-op"]}
					},
					"required":["severity","description","action"]
				}
			},
			"summary":{"type":"string"}
		},
		"required":["findings","summary"]
	}`)

	_, err := finalizeTextResult("codex", text, schema, TokenUsage{})
	if err == nil {
		t.Fatal("expected nested enum violation to fail")
	}
}

func TestFinalizeTextResult_WithSchemaAllowsNullOptionalFieldsInTextFallback(t *testing.T) {
	text := `{"findings":[{"severity":"warning","file":null,"line":null,"description":"x","action":"auto-fix"}],"summary":"1 issue"}`
	schema := json.RawMessage(`{
		"type":"object",
		"properties":{
			"findings":{
				"type":"array",
				"items":{
					"type":"object",
					"properties":{
						"severity":{"type":"string","enum":["error","warning","info"]},
						"file":{"type":"string"},
						"line":{"type":"integer"},
						"description":{"type":"string"},
						"action":{"type":"string","enum":["no-op","auto-fix","ask-user"]}
					},
					"required":["severity","description","action"]
				}
			},
			"summary":{"type":"string"}
		},
		"required":["findings","summary"]
	}`)

	result, err := finalizeTextResult("opencode", text, schema, TokenUsage{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(result.Output) != text {
		t.Fatalf("unexpected output: %s", string(result.Output))
	}
}

func TestFinalizeTextResult_WithSchemaParsesCodexRealWorldOutput(t *testing.T) {
	// Regression: real codex output from pipeline 01KPYD4SD644SR9JCNX6Y.
	// Reasoning sentences were concatenated with no newlines, and the
	// opening ```json fence was glued to the end of the last sentence.
	text := "Reviewing the diff between `ba90e3c` and `6fdb361` first.I'm reading the patch now.I'm down to edge cases: timer semantics after multiple `result` events.```json\n" +
		"{\n" +
		"  \"findings\": [],\n" +
		"  \"risk_assessment\": {\n" +
		"    \"risk_level\": \"low\",\n" +
		"    \"risk_rationale\": \"clean\"\n" +
		"  }\n" +
		"}\n" +
		"```"
	result, err := finalizeTextResult("codex", text, json.RawMessage(`{"type":"object"}`), TokenUsage{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var output struct {
		Findings       []any `json:"findings"`
		RiskAssessment struct {
			RiskLevel string `json:"risk_level"`
		} `json:"risk_assessment"`
	}
	if err := json.Unmarshal(result.Output, &output); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}
	if output.RiskAssessment.RiskLevel != "low" {
		t.Errorf("expected risk_level=low, got %q", output.RiskAssessment.RiskLevel)
	}
}

func TestFinalizeTextResult_WithSchemaRejectsAmbiguousFencedJSON(t *testing.T) {
	text := strings.Join([]string{
		"```json",
		`{"first":true}`,
		"```",
		"```json",
		`{"second":true}`,
		"```",
	}, "\n")
	_, err := finalizeTextResult("codex", text, json.RawMessage(`{"type":"object"}`), TokenUsage{})
	if err == nil {
		t.Fatal("expected ambiguous fenced JSON to fail")
	}
	if !strings.Contains(err.Error(), "multiple JSON code fences") {
		t.Fatalf("expected multiple JSON code fences error, got %v", err)
	}
}

func TestFencedJSONCandidates_IgnoreBackticksInsideJSONString(t *testing.T) {
	text := "review complete\n```json\n{\"summary\":\"quoted ```snippet``` in markdown\",\"findings\":[]}\n```\npostlude"

	closed, open := fencedJSONCandidates(text)
	if len(open) != 0 {
		t.Fatalf("expected no open candidates, got %d", len(open))
	}
	if len(closed) != 1 {
		t.Fatalf("expected 1 closed candidate, got %d", len(closed))
	}
	want := "{\"summary\":\"quoted ```snippet``` in markdown\",\"findings\":[]}\n"
	if closed[0] != want {
		t.Fatalf("candidate = %q, want %q", closed[0], want)
	}
}

func TestFencedJSONCandidates_AllowIndentedClosingFence(t *testing.T) {
	text := "review complete\n```json\n{\"summary\":\"ok\",\"findings\":[]}\n   ```\nnext paragraph"

	closed, open := fencedJSONCandidates(text)
	if len(open) != 0 {
		t.Fatalf("expected no open candidates, got %d", len(open))
	}
	if len(closed) != 1 {
		t.Fatalf("expected 1 closed candidate, got %d", len(closed))
	}
	want := "{\"summary\":\"ok\",\"findings\":[]}\n"
	if closed[0] != want {
		t.Fatalf("candidate = %q, want %q", closed[0], want)
	}
}

func TestFinalizeTextResult_WithSchemaIgnoresJSONInsideNonJSONFence(t *testing.T) {
	text := strings.Join([]string{
		"Reasoning follows.",
		"```markdown",
		"Example output:",
		"```json",
		`{"done":true}`,
		"```",
		"```",
		"Final answer: not valid JSON",
	}, "\n")

	closed, open := fencedJSONCandidates(text)
	if len(closed) != 0 || len(open) != 0 {
		t.Fatalf("expected no fenced JSON candidates, got closed=%q open=%q", closed, open)
	}

	_, err := finalizeTextResult("codex", text, json.RawMessage(`{"type":"object"}`), TokenUsage{})
	if err == nil {
		t.Fatal("expected parse failure")
	}
}

func TestFinalizeTextResult_ParseErrorIncludesOutputSnippet(t *testing.T) {
	text := "Now I've applied all four fixes and verified the build passes."
	_, err := finalizeTextResult("copilot", text, json.RawMessage(`{"type":"object"}`), TokenUsage{})
	if err == nil {
		t.Fatal("expected parse failure on prose output")
	}
	if !strings.Contains(err.Error(), "output snippet:") {
		t.Errorf("error should include an output snippet, got %v", err)
	}
	if !strings.Contains(err.Error(), "Now I've applied") {
		t.Errorf("error should embed the offending text, got %v", err)
	}
}

func TestOutputSnippet_TruncatesLongText(t *testing.T) {
	long := strings.Repeat("x", 500)
	got := outputSnippet(long)
	if !strings.HasSuffix(got, "…") {
		t.Errorf("expected ellipsis suffix on truncated snippet, got %q", got)
	}
	if runes := []rune(got); len(runes) != 201 {
		t.Errorf("expected 200 runes plus ellipsis, got %d runes", len(runes))
	}
}

func TestFinalizeTextResult_WithSchemaParsesPiSameLineFence(t *testing.T) {
	// Regression: pi JSONL output concatenates content blocks without
	// separators, producing ```json glued to the JSON body on the same line
	// (no newline after the fence info token). The fence parser must still
	// recognize the ```json fence and extract the body.
	text := "The change is docs-only: 3 lines appended to README.md.```json{  \"findings\": [],  \"summary\": \"docs-only change\"}"
	result, err := finalizeTextResult("pi", text, json.RawMessage(`{"type":"object"}`), TokenUsage{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var output struct {
		Findings []any  `json:"findings"`
		Summary  string `json:"summary"`
	}
	if err := json.Unmarshal(result.Output, &output); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}
	if output.Summary != "docs-only change" {
		t.Errorf("expected summary=docs-only change, got %q", output.Summary)
	}
}

func TestFenceContentStart_InfoTokenWithSameLineBody(t *testing.T) {
	// fence info token followed immediately by content on the same line.
	start, info := fenceContentStart("```json{\"findings\":[]}", 0)
	if info != "json" {
		t.Errorf("expected info=json, got %q", info)
	}
	want := `{"findings":[]}`
	if got := "```json{\"findings\":[]}"[start:]; got != want {
		t.Errorf("content start = %d (got %q), want %q", start, got, want)
	}
}

func TestFinalizeTextResult_WithSchemaParsesProseQuotingFenceExampleThenClosedBlock(t *testing.T) {
	// Regression (upstream PR review failure): pi review output quotes
	// ```json{...} as an inline example inside prose (an unclosed fence),
	// then ends with a real closed ```json block. The quoted example must
	// not shadow the trailing block, and must not fail the whole parse.
	text := strings.Join([]string{
		"The fix: ` ```json{\"findings\":[]}` glued to the body on the same line.",
		"I traced the old fence parser in internal/agent/agent.go.",
		"```json",
		`{"findings":[{"id":"F1","severity":"warning"}],"summary":"one issue"}`,
		"```",
	}, "\n")
	result, err := finalizeTextResult("pi", text, json.RawMessage(`{"type":"object"}`), TokenUsage{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var output struct {
		Findings []struct {
			ID       string `json:"id"`
			Severity string `json:"severity"`
		} `json:"findings"`
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal(result.Output, &output); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}
	if len(output.Findings) != 1 || output.Findings[0].ID != "F1" {
		t.Errorf("expected trailing block findings F1, got %+v", output.Findings)
	}
	if output.Summary != "one issue" {
		t.Errorf("expected summary=one issue, got %q", output.Summary)
	}
}

func TestFinalizeTextResult_WithSchemaParsesProseWrappedJSONObject(t *testing.T) {
	// Models routinely end a turn with a narration sentence before the JSON
	// object without markdown code fences.
	text := "I have completed testing and found no issues.\n{\"findings\":[],\"summary\":\"clean test run\"}"
	result, err := finalizeTextResult("antigravity", text, json.RawMessage(`{"type":"object"}`), TokenUsage{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var output struct {
		Findings []any  `json:"findings"`
		Summary  string `json:"summary"`
	}
	if err := json.Unmarshal(result.Output, &output); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}
	if output.Summary != "clean test run" {
		t.Errorf("expected summary=%q, got %q", "clean test run", output.Summary)
	}
}

func TestFinalizeTextResult_WithSchemaRejectsBareJSONBeforeTrailingProse(t *testing.T) {
	text := `Example: {"findings":[],"summary":"clean"}. I found a bug in the diff.`
	_, err := finalizeTextResult("antigravity", text, json.RawMessage(`{"type":"object"}`), TokenUsage{})
	if err == nil {
		t.Fatal("expected trailing prose after bare JSON to fail")
	}
}

func TestFinalizeTextResult_WithSchemaParsesProseWrappedJSONObjectWithBracesInStrings(t *testing.T) {
	text := "Done with verification.\n{\"findings\":[],\"summary\":\"fixed {unmatched brace} in pattern\"}"
	result, err := finalizeTextResult("antigravity", text, json.RawMessage(`{"type":"object"}`), TokenUsage{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var output struct {
		Findings []any  `json:"findings"`
		Summary  string `json:"summary"`
	}
	if err := json.Unmarshal(result.Output, &output); err != nil {
		t.Fatalf("failed to parse output: %v", err)
	}
	if output.Summary != "fixed {unmatched brace} in pattern" {
		t.Errorf("expected summary=%q, got %q", "fixed {unmatched brace} in pattern", output.Summary)
	}
}

func TestFinalizeTextResult_WithSchemaPreservesSchemaErrorOnProseWrappedJSON(t *testing.T) {
	text := "I checked the diff and here is my review:\n{\"unexpected_field\":\"value\"}\nThanks."
	schema := json.RawMessage(`{
		"type":"object",
		"properties":{
			"findings":{"type":"array"},
			"summary":{"type":"string"}
		},
		"required":["findings","summary"]
	}`)
	_, err := finalizeTextResult("antigravity", text, schema, TokenUsage{})
	if err == nil {
		t.Fatal("expected schema validation error")
	}
	if strings.Contains(err.Error(), "ended its turn with prose") {
		t.Fatalf("schema error was masked as prose turn ending: %v", err)
	}
	if !strings.Contains(err.Error(), "violates schema") && !strings.Contains(err.Error(), "missing required") {
		t.Fatalf("expected schema violation error, got: %v", err)
	}
}

func TestFinalizeTextResult_ProseWithoutJSONReturnsEndedWithProseError(t *testing.T) {
	text := "I have completed testing and found no issues. Everything passes."
	_, err := finalizeTextResult("antigravity", text, json.RawMessage(`{"type":"object"}`), TokenUsage{})
	if err == nil {
		t.Fatal("expected error for prose turn ending")
	}
	if !strings.Contains(err.Error(), "ended its turn with prose instead of the required JSON object") {
		t.Fatalf("expected ended with prose error, got: %v", err)
	}
}

func TestLastBareJSONObject_IgnoresIncidentalExamplePrecedingProse(t *testing.T) {
	// Greptile review regression: an incidental schema-valid JSON example quoted
	// mid-prose before substantive findings must not be mistaken for the final verdict.
	text := strings.Join([]string{
		"Here is an example of what a clean output looks like:",
		"{\"findings\":[],\"summary\":\"clean\"}",
		"",
		"However, upon actual inspection of the diff, I found several critical bugs:",
		"Line 42 has a nil dereference.",
	}, "\n")
	schema := json.RawMessage(`{
		"type":"object",
		"properties":{
			"findings":{"type":"array"},
			"summary":{"type":"string"}
		},
		"required":["findings","summary"]
	}`)
	_, err := finalizeTextResult("antigravity", text, schema, TokenUsage{})
	if err == nil {
		t.Fatal("expected failure because bare JSON was an incidental example before substantive prose")
	}
}

func TestFinalizeTextResult_ConflictingFencedAndBareJSONReturnsError(t *testing.T) {
	// Greptile review regression: when an agent response contains a schema-valid fenced
	// verdict and a different schema-valid bare verdict in prose, the conflicting verdict
	// must be rejected instead of silently picking one format over the other.
	text := strings.Join([]string{
		"I completed the review and found an issue:",
		"```json",
		`{"findings":[{"id":"BUG1","severity":"error"}],"summary":"bug found"}`,
		"```",
		`{"findings":[],"summary":"all clean"}`,
	}, "\n")
	schema := json.RawMessage(`{
		"type":"object",
		"properties":{
			"findings":{"type":"array"},
			"summary":{"type":"string"}
		},
		"required":["findings","summary"]
	}`)
	_, err := finalizeTextResult("antigravity", text, schema, TokenUsage{})
	if err == nil {
		t.Fatal("expected error on conflicting fenced and bare JSON verdicts")
	}
	if !strings.Contains(err.Error(), "conflicting JSON candidates") && !strings.Contains(err.Error(), "multiple JSON") {
		t.Fatalf("expected conflicting/multiple JSON candidates error, got: %v", err)
	}
}

func TestFinalizeTextResult_IdenticalFencedAndBareJSONSucceeds(t *testing.T) {
	text := strings.Join([]string{
		"```json",
		`{"findings":[],"summary":"all clean"}`,
		"```",
		`{"findings":[],"summary":"all clean"}`,
	}, "\n")
	schema := json.RawMessage(`{
		"type":"object",
		"properties":{
			"findings":{"type":"array"},
			"summary":{"type":"string"}
		},
		"required":["findings","summary"]
	}`)
	result, err := finalizeTextResult("antigravity", text, schema, TokenUsage{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var output struct {
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal(result.Output, &output); err != nil {
		t.Fatalf("failed to unmarshal output: %v", err)
	}
	if output.Summary != "all clean" {
		t.Errorf("expected summary='all clean', got %q", output.Summary)
	}
}

func TestFinalizeTextResult_ConflictingLargeIntegerVerdictsReturnError(t *testing.T) {
	text := strings.Join([]string{
		"```json",
		`{"value":9007199254740992}`,
		"```",
		`{"value":9007199254740993}`,
	}, "\n")
	schema := json.RawMessage(`{
		"type":"object",
		"properties":{"value":{"type":"integer"}},
		"required":["value"]
	}`)
	_, err := finalizeTextResult("antigravity", text, schema, TokenUsage{})
	if err == nil {
		t.Fatal("expected distinct large integer verdicts to conflict")
	}
	if !strings.Contains(err.Error(), "conflicting JSON candidates") {
		t.Fatalf("expected conflicting JSON candidates error, got %v", err)
	}
}

func TestFinalizeTextResult_IdenticalNumericSpellingSucceeds(t *testing.T) {
	text := strings.Join([]string{
		"```json",
		`{"confidence":1}`,
		"```",
		`{"confidence":1.0}`,
	}, "\n")
	schema := json.RawMessage(`{
		"type":"object",
		"properties":{"confidence":{"type":"number"}},
		"required":["confidence"]
	}`)
	result, err := finalizeTextResult("antigravity", text, schema, TokenUsage{})
	if err != nil {
		t.Fatalf("expected equivalent numeric verdicts to succeed, got %v", err)
	}
	var output map[string]any
	if err := json.Unmarshal(result.Output, &output); err != nil {
		t.Fatalf("failed to unmarshal output: %v", err)
	}
	if output["confidence"] != float64(1) {
		t.Fatalf("confidence = %v, want 1", output["confidence"])
	}
}

func TestFinalizeTextResult_EquivalentLargeExponentVerdictsSucceed(t *testing.T) {
	text := strings.Join([]string{
		"```json",
		`{"confidence":1e1000001}`,
		"```",
		`{"confidence":10e1000000}`,
	}, "\n")
	schema := json.RawMessage(`{
		"type":"object",
		"properties":{"confidence":{"type":"number"}},
		"required":["confidence"]
	}`)
	result, err := finalizeTextResult("antigravity", text, schema, TokenUsage{})
	if err != nil {
		t.Fatalf("expected equivalent large-exponent verdicts to succeed, got %v", err)
	}
	if string(result.Output) != `{"confidence":1e1000001}` {
		t.Fatalf("unexpected output: %s", string(result.Output))
	}
}

func TestFinalizeTextResult_DistinctLargeExponentVerdictsConflict(t *testing.T) {
	text := strings.Join([]string{
		"```json",
		`{"confidence":1e1000001}`,
		"```",
		`{"confidence":2e1000001}`,
	}, "\n")
	schema := json.RawMessage(`{
		"type":"object",
		"properties":{"confidence":{"type":"number"}},
		"required":["confidence"]
	}`)
	_, err := finalizeTextResult("antigravity", text, schema, TokenUsage{})
	if err == nil {
		t.Fatal("expected distinct large-exponent verdicts to conflict")
	}
	if !strings.Contains(err.Error(), "conflicting JSON candidates") {
		t.Fatalf("expected conflicting JSON candidates error, got %v", err)
	}
}

func TestFinalizeTextResult_ProseWithNonJSONFenceReturnsEndedWithProseError(t *testing.T) {
	text := "I ran tests and they passed:\n```bash\ngo test ./...\n```\nAll done."
	_, err := finalizeTextResult("antigravity", text, json.RawMessage(`{"type":"object"}`), TokenUsage{})
	if err == nil {
		t.Fatal("expected error for prose turn ending")
	}
	if !strings.Contains(err.Error(), "ended its turn with prose instead of the required JSON object") {
		t.Fatalf("expected ended with prose error, got: %v", err)
	}
}

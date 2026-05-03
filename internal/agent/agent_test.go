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
		{name: "rovodev", agent: types.AgentRovoDev, bin: "acli", wantName: "rovodev"},
		{name: "opencode", agent: types.AgentOpenCode, bin: "opencode", wantName: "opencode"},
		{name: "pi", agent: types.AgentPi, bin: "pi", wantName: "pi"},
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

func TestACPAgentBuildArgsUsesExecMode(t *testing.T) {
	a := &acpxAgent{target: "gemini"}
	args := a.buildArgs(RunOpts{Prompt: "do work"})

	if got, want := args[len(args)-3:], []string{"gemini", "exec", "do work"}; strings.Join(got, "\x00") != strings.Join(want, "\x00") {
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
	want := TokenUsage{InputTokens: 120, OutputTokens: 60, CacheReadTokens: 40, CacheCreationTokens: 15}
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

func TestACPAgentRunParsesAcpxJSONOutput(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is Unix-only")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "acpx")
	argLog := filepath.Join(dir, "args.txt")
	t.Setenv("ARG_LOG", argLog)
	contents := `#!/bin/sh
printf '%s\n' "$@" > "$ARG_LOG"
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
	result, err := a.Run(context.Background(), RunOpts{
		Prompt:     "do work",
		CWD:        dir,
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
	for _, want := range []string{"--cwd\n" + dir, "--format\njson", "--json-strict", "gemini", "do work"} {
		if !strings.Contains(argsText, want) {
			t.Errorf("args missing %q in:\n%s", want, argsText)
		}
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

func TestFinalizeTextResult_WithSchemaPrefersLastBareJSON(t *testing.T) {
	// If reasoning text embeds a decorative JSON object and the final
	// answer is a separate object at the end, the final one should win.
	text := `I considered {"foo":"bar"} as one option. Final: {"done":true}`
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

	got := fencedJSONCandidates(text)
	if len(got) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(got))
	}
	want := "{\"summary\":\"quoted ```snippet``` in markdown\",\"findings\":[]}\n"
	if got[0] != want {
		t.Fatalf("candidate = %q, want %q", got[0], want)
	}
}

func TestFencedJSONCandidates_AllowIndentedClosingFence(t *testing.T) {
	text := "review complete\n```json\n{\"summary\":\"ok\",\"findings\":[]}\n   ```\nnext paragraph"

	got := fencedJSONCandidates(text)
	if len(got) != 1 {
		t.Fatalf("expected 1 candidate, got %d", len(got))
	}
	want := "{\"summary\":\"ok\",\"findings\":[]}\n"
	if got[0] != want {
		t.Fatalf("candidate = %q, want %q", got[0], want)
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

	if got := fencedJSONCandidates(text); len(got) != 0 {
		t.Fatalf("expected no fenced JSON candidates, got %q", got)
	}

	_, err := finalizeTextResult("codex", text, json.RawMessage(`{"type":"object"}`), TokenUsage{})
	if err == nil {
		t.Fatal("expected parse failure")
	}
}

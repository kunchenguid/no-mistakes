package agent

import (
	"encoding/json"
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

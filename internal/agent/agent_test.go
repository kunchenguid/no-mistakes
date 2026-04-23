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

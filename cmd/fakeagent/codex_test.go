package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestPatchCodexFixtureStructuredRunRewritesAgentMessageText(t *testing.T) {
	t.Helper()

	raw := []byte("{\"type\":\"thread.started\"}\n{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"recorded text\"}}\n{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":2}}\n")
	patched, err := patchCodexFixture(raw, Action{
		Text:       "scenario text",
		Structured: map[string]any{"summary": "patched summary"},
	})
	if err != nil {
		t.Fatalf("patchCodexFixture: %v", err)
	}

	lines := bytes.Split(bytes.TrimSpace(patched), []byte("\n"))
	if len(lines) != 3 {
		t.Fatalf("got %d jsonl lines, want 3", len(lines))
	}

	if string(lines[0]) != `{"type":"thread.started"}` {
		t.Fatalf("first line = %s, want thread.started passthrough", lines[0])
	}

	var item struct {
		Type string `json:"type"`
		Item struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"item"`
	}
	if err := json.Unmarshal(lines[1], &item); err != nil {
		t.Fatalf("unmarshal item.completed: %v", err)
	}
	if item.Type != "item.completed" || item.Item.Type != "agent_message" {
		t.Fatalf("item event = %+v, want completed agent_message", item)
	}
	if item.Item.Text != `{"summary":"patched summary"}` {
		t.Fatalf("agent_message text = %q, want patched structured JSON", item.Item.Text)
	}

	if string(lines[2]) != `{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":2}}` {
		t.Fatalf("turn.completed line = %s, want passthrough", lines[2])
	}
}

func TestPatchCodexFixturePlainRunRewritesAgentMessageText(t *testing.T) {
	t.Helper()

	raw := []byte("{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"recorded text\"}}\n")
	patched, err := patchCodexFixture(raw, Action{Text: "scenario text"})
	if err != nil {
		t.Fatalf("patchCodexFixture: %v", err)
	}

	var item struct {
		Item struct {
			Text string `json:"text"`
		} `json:"item"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(patched), &item); err != nil {
		t.Fatalf("unmarshal patched item: %v", err)
	}
	if item.Item.Text != "scenario text" {
		t.Fatalf("agent_message text = %q, want scenario text", item.Item.Text)
	}
}

func TestFilterStructuredToSchemaKeepsOnlyDeclaredProperties(t *testing.T) {
	schemaPath := filepath.Join(t.TempDir(), "schema.json")
	schema := []byte(`{
		"type": "object",
		"properties": {
			"findings": {"type": "array"},
			"risk_level": {"type": "string"},
			"risk_rationale": {"type": "string"}
		},
		"required": ["findings", "risk_level", "risk_rationale"]
	}`)
	if err := os.WriteFile(schemaPath, schema, 0o644); err != nil {
		t.Fatalf("write schema: %v", err)
	}

	structured := map[string]any{
		"findings":        []any{},
		"risk_level":      "low",
		"risk_rationale":  "no risks",
		"summary":         "no issues found",
		"tested":          []any{"x"},
		"testing_summary": "simulated tests passed",
		"title":           "feat: extra",
		"body":            "extra body",
	}

	got, err := filterStructuredToSchema(structured, schemaPath)
	if err != nil {
		t.Fatalf("filter: %v", err)
	}
	want := map[string]any{
		"findings":       []any{},
		"risk_level":     "low",
		"risk_rationale": "no risks",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("filtered = %#v\nwant = %#v", got, want)
	}
}

func TestFilterStructuredToSchemaNilWhenNoSchema(t *testing.T) {
	structured := map[string]any{"summary": "ok"}
	got, err := filterStructuredToSchema(structured, "")
	if err != nil {
		t.Fatalf("filter: %v", err)
	}
	if !reflect.DeepEqual(got, structured) {
		t.Fatalf("got %#v, want passthrough %#v", got, structured)
	}
}

func TestExtractCodexOutputSchemaPath(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
	}{
		{
			name: "separate flag",
			args: []string{"exec", "--output-schema", "/tmp/s.json", "prompt", "--json"},
			want: "/tmp/s.json",
		},
		{
			name: "equals form",
			args: []string{"exec", "--output-schema=/tmp/s.json", "prompt", "--json"},
			want: "/tmp/s.json",
		},
		{
			name: "absent",
			args: []string{"exec", "prompt", "--json"},
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractCodexOutputSchema(tc.args); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestExtractCodexPromptSkipsOutputSchemaValue(t *testing.T) {
	t.Helper()

	args := []string{
		"exec",
		"--output-schema", "/tmp/schema.json",
		"--model", "gpt-5.4",
		"review this diff",
		"--json",
	}

	if got := extractCodexPrompt(args); got != "review this diff" {
		t.Fatalf("prompt = %q, want %q", got, "review this diff")
	}
}

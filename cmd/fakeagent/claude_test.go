package main

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestPatchClaudeFixtureStructuredRunRewritesAssistantText(t *testing.T) {
	t.Helper()

	raw := []byte("{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"recorded assistant text\"}]}}\n{\"type\":\"result\",\"result\":\"recorded result\",\"structured_output\":{\"summary\":\"recorded summary\"}}\n")
	patched, err := patchClaudeFixture(raw, Action{
		Text:       "scenario text",
		Structured: map[string]any{"summary": "patched summary"},
	})
	if err != nil {
		t.Fatalf("patchClaudeFixture: %v", err)
	}

	lines := bytes.Split(bytes.TrimSpace(patched), []byte("\n"))
	if len(lines) != 2 {
		t.Fatalf("got %d jsonl lines, want 2", len(lines))
	}

	var assistant struct {
		Type    string `json:"type"`
		Message struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(lines[0], &assistant); err != nil {
		t.Fatalf("unmarshal assistant event: %v", err)
	}
	if assistant.Type != "assistant" {
		t.Fatalf("assistant type = %q, want assistant", assistant.Type)
	}
	if len(assistant.Message.Content) != 1 || assistant.Message.Content[0].Text != "scenario text" {
		t.Fatalf("assistant content = %+v, want scenario text", assistant.Message.Content)
	}

	var result struct {
		Type             string          `json:"type"`
		Result           string          `json:"result"`
		StructuredOutput json.RawMessage `json:"structured_output"`
	}
	if err := json.Unmarshal(lines[1], &result); err != nil {
		t.Fatalf("unmarshal result event: %v", err)
	}
	if result.Result != "scenario text" {
		t.Fatalf("result text = %q, want scenario text", result.Result)
	}
	if string(result.StructuredOutput) != `{"summary":"patched summary"}` {
		t.Fatalf("structured_output = %s, want patched payload", result.StructuredOutput)
	}
}

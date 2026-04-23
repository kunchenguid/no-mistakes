package main

import (
	"bytes"
	"encoding/json"
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

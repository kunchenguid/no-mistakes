package agent

import (
	"context"
	"strings"
	"testing"
)

func TestParseGeminiEvents_IgnoresSystemEvents(t *testing.T) {
	stream := `{"type": "init", "status": "started"}
{"type": "message", "role": "assistant", "content": "hello"}
{"type": "tool_call", "name": "foo"}
{"type": "message", "role": "assistant", "content": " world"}
{"type": "result", "status": "success", "stats": {"input_tokens": 10, "output_tokens": 5}}`

	var usage TokenUsage
	var result *geminiResult
	
	err := parseGeminiEvents(context.Background(), strings.NewReader(stream), nil, &usage, &result)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	
	if result == nil {
		t.Fatalf("expected result, got nil")
	}
	
	if result.Text != "hello world" {
		t.Errorf("expected text 'hello world', got %q", result.Text)
	}
}

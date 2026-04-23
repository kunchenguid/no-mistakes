package main

import (
	"bytes"
	"testing"
)

func TestScrubClaudeHookEvents_RemovesInitEvent(t *testing.T) {
	input := []byte("{\"type\":\"assistant\",\"message\":\"keep\"}\n{\"subtype\":\"init\",\"session_id\":\"abc\",\"tools\":[\"terminal-axi\"]}\n{\"type\":\"result\",\"status\":\"ok\"}\n")

	scrubbed := scrubClaudeHookEvents(input)

	if bytes.Contains(scrubbed, []byte(`"subtype":"init"`)) {
		t.Fatalf("expected init event removed, got %q", scrubbed)
	}
	want := []byte("{\"type\":\"assistant\",\"message\":\"keep\"}\n{\"type\":\"result\",\"status\":\"ok\"}\n")
	if !bytes.Equal(scrubbed, want) {
		t.Fatalf("scrubbed = %q, want %q", scrubbed, want)
	}
}

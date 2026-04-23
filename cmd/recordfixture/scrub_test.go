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

func TestReplacePathForms_ReplacesEscapedWindowsPaths(t *testing.T) {
	input := []byte(`{"cwd":"C:\\Users\\kun\\project","tmp":"C:\\Users\\kun\\AppData\\Local\\Temp\\recordfixture-123"}`)

	scrubbed := replacePathForms(input, `C:\Users\kun\AppData\Local\Temp`, "/tmp")
	scrubbed = replacePathForms(scrubbed, `C:\Users\kun`, "/private/tmp/fixture-cwd")

	if bytes.Contains(scrubbed, []byte(`C:\\Users\\kun`)) {
		t.Fatalf("expected escaped home path removed, got %q", scrubbed)
	}
	if !bytes.Contains(scrubbed, []byte(`/tmp\\recordfixture-123`)) {
		t.Fatalf("expected escaped temp path preserved under placeholder, got %q", scrubbed)
	}
}

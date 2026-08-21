package main

import (
	"bytes"
	"strings"
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

func TestScrubAgyConversationIDsReplacesUUIDs(t *testing.T) {
	line := `{"event":"init","conversation_id":"5f63304d-2c5a-48a3-aa40-6f077ce51686","init":{"cwd":"/tmp/x"}}` + "\n" +
		`{"event":"result","result":{"conversation_id":"0198a7b6-c5d4-4e3f-9a2b-123456789abc","status":"SUCCESS"}}` + "\n"

	scrubbed := string(scrubAgyConversationIDs([]byte(line)))

	if strings.Contains(scrubbed, "5f63304d") || strings.Contains(scrubbed, "0198a7b6") {
		t.Fatalf("conversation UUIDs survived scrubbing:\n%s", scrubbed)
	}
	want := `{"event":"init","conversation_id":"00000000-0000-4000-8000-000000000000","init":{"cwd":"/tmp/x"}}` + "\n" +
		`{"event":"result","result":{"conversation_id":"00000000-0000-4000-8000-000000000000","status":"SUCCESS"}}` + "\n"
	if scrubbed != want {
		t.Fatalf("scrubbed =\n%s\nwant\n%s", scrubbed, want)
	}
}

func TestScrubBytesAppliesAgyConversationIDs(t *testing.T) {
	data := []byte(`{"result":{"conversation_id":"11111111-2222-4333-8444-555555555555"}}`)
	got := string(scrubBytes(data))
	if strings.Contains(got, "11111111") {
		t.Fatalf("scrubBytes did not apply conversation-id scrubbing: %s", got)
	}
}

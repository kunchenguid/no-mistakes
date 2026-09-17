package agentcfg

import (
	"reflect"
	"strings"
	"testing"
)

func TestPiProfileValidationAndMatching(t *testing.T) {
	full := &PiProfile{Model: "openai-codex/gpt-5.4", Effort: EffortHigh}
	for _, request := range []*PiProfile{nil, full, {Model: full.Model}, {Effort: full.Effort}} {
		if err := request.ValidateRequest(); err != nil {
			t.Fatal(err)
		}
		if !full.Matches(request) {
			t.Fatalf("did not match %+v", request)
		}
	}
	for _, request := range []*PiProfile{{}, {Model: "gpt-5"}, {Model: "openai/*"}, {Model: "openai/gpt:high"}, {Model: "https://secret@host/path"}, {Model: "openai/model\nsecret"}, {Effort: "HIGH"}, {Effort: " high "}} {
		if err := request.ValidateRequest(); err == nil || strings.Contains(err.Error(), "secret") {
			t.Fatalf("unsafe acceptance/error: %v", err)
		}
	}
	if (&PiProfile{Model: full.Model}).Validate() == nil {
		t.Fatal("partial persisted pin accepted")
	}
	if full.Matches(&PiProfile{Model: "anthropic/gpt-5.4"}) || (*PiProfile)(nil).Matches(full) {
		t.Fatal("conflicting pin matched")
	}
}

func TestPiSelectionArgs(t *testing.T) {
	raw := []string{"--model=x", "--provider", "p", "--thinking=low", "--models", "*", "--api-key", "private", "--no-context-files"}
	before := append([]string(nil), raw...)
	got, found := PiSelectionArgs(raw)
	if !found || !reflect.DeepEqual(got, []string{"--api-key", "private", "--no-context-files"}) || !reflect.DeepEqual(raw, before) {
		t.Fatalf("strip = %v %v", got, found)
	}
}

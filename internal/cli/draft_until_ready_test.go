package cli

import (
	"strings"
	"testing"
)

// The primary trigger is a git push through the gate, so the per-run flag has
// to survive the line-oriented push-option transport exactly like --intent.
func TestDraftUntilReadyPushOptionRoundTrip(t *testing.T) {
	opt := formatDraftUntilReadyPushOption(true)
	if opt == "" {
		t.Fatal("formatDraftUntilReadyPushOption(true) returned empty")
	}
	got, err := parseDraftUntilReadyPushOptions([]string{"no-mistakes.skip=test", opt})
	if err != nil {
		t.Fatalf("parseDraftUntilReadyPushOptions() error = %v", err)
	}
	if !got {
		t.Fatal("draft-until-ready did not survive the push-option round trip")
	}
}

func TestFormatDraftUntilReadyPushOptionOmittedWhenOff(t *testing.T) {
	if got := formatDraftUntilReadyPushOption(false); got != "" {
		t.Fatalf("formatDraftUntilReadyPushOption(false) = %q, want empty", got)
	}
}

func TestParseDraftUntilReadyPushOptionsDefaultsOff(t *testing.T) {
	got, err := parseDraftUntilReadyPushOptions([]string{"no-mistakes.skip=test", "ci.skip"})
	if err != nil {
		t.Fatalf("parseDraftUntilReadyPushOptions() error = %v", err)
	}
	if got {
		t.Fatal("draft-until-ready must default to off")
	}
}

func TestParseDraftUntilReadyPushOptionsRejectsGarbage(t *testing.T) {
	if _, err := parseDraftUntilReadyPushOptions([]string{"no-mistakes.draft-until-ready=maybe"}); err == nil {
		t.Fatal("expected an error for an unparseable draft-until-ready value")
	}
}

func TestAxiRunExposesDraftUntilReadyFlag(t *testing.T) {
	cmd := newAxiRunCmd()
	flag := cmd.Flags().Lookup("draft-until-ready")
	if flag == nil {
		t.Fatal("axi run must expose --draft-until-ready")
	}
	if flag.DefValue != "false" {
		t.Fatalf("--draft-until-ready default = %q, want false", flag.DefValue)
	}
	if !strings.Contains(strings.ToLower(flag.Usage), "draft") {
		t.Fatalf("--draft-until-ready usage should describe the draft lifecycle, got %q", flag.Usage)
	}
}

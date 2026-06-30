package cli

import (
	"reflect"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestParseSkipPushOptions(t *testing.T) {
	got, err := parseSkipPushOptions([]string{
		"ci.skip",
		"no-mistakes.skip=test,lint",
	})
	if err != nil {
		t.Fatalf("parseSkipPushOptions() error = %v", err)
	}
	want := []types.StepName{types.StepTest, types.StepLint}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseSkipPushOptions() = %v, want %v", got, want)
	}
}

func TestParseSkipPushOptionsRejectsUnknownStep(t *testing.T) {
	_, err := parseSkipPushOptions([]string{"no-mistakes.skip=test,deploy"})
	if err == nil {
		t.Fatal("expected unknown step to fail")
	}
}

func TestFormatSkipPushOptions(t *testing.T) {
	got := formatSkipPushOptions([]types.StepName{types.StepTest, types.StepLint})
	want := []string{"no-mistakes.skip=test,lint"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("formatSkipPushOptions() = %v, want %v", got, want)
	}
}

func TestIntentPushOptionRoundTrip(t *testing.T) {
	// Multi-line, comma- and colon-bearing intent must survive the
	// line-oriented push-option transport intact.
	intent := "add retry to the uploader\n\nwhy: flaky network, commas, colons: ok"
	opt := formatIntentPushOption(intent)
	if opt == "" {
		t.Fatal("formatIntentPushOption returned empty for a non-empty intent")
	}
	got, err := parseIntentPushOptions([]string{"no-mistakes.skip=test", opt})
	if err != nil {
		t.Fatalf("parseIntentPushOptions() error = %v", err)
	}
	if got != intent {
		t.Fatalf("round-trip mismatch:\n got %q\nwant %q", got, intent)
	}
}

func TestFormatIntentPushOptionEmpty(t *testing.T) {
	if got := formatIntentPushOption("   "); got != "" {
		t.Fatalf("formatIntentPushOption(blank) = %q, want empty", got)
	}
}

func TestParseIntentPushOptionsNone(t *testing.T) {
	got, err := parseIntentPushOptions([]string{"no-mistakes.skip=test", "ci.skip"})
	if err != nil {
		t.Fatalf("parseIntentPushOptions() error = %v", err)
	}
	if got != "" {
		t.Fatalf("parseIntentPushOptions(no intent) = %q, want empty", got)
	}
}

func TestBasePushOptionRoundTrip(t *testing.T) {
	opt := formatBasePushOption("release/1.4")
	if opt != basePushOptionPrefix+"release/1.4" {
		t.Fatalf("formatBasePushOption() = %q", opt)
	}
	got := parseBasePushOptions([]string{"no-mistakes.skip=test", opt})
	if got != "release/1.4" {
		t.Fatalf("parseBasePushOptions() = %q, want release/1.4", got)
	}
}

func TestFormatBasePushOptionEmpty(t *testing.T) {
	if got := formatBasePushOption("   "); got != "" {
		t.Fatalf("formatBasePushOption(blank) = %q, want empty", got)
	}
}

func TestParseBasePushOptionsLastWins(t *testing.T) {
	got := parseBasePushOptions([]string{
		basePushOptionPrefix + "develop",
		basePushOptionPrefix + "main",
	})
	if got != "main" {
		t.Fatalf("parseBasePushOptions() = %q, want main (last wins)", got)
	}
	if got := parseBasePushOptions([]string{"ci.skip"}); got != "" {
		t.Fatalf("parseBasePushOptions(none) = %q, want empty", got)
	}
}

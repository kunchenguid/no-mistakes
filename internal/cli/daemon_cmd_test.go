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

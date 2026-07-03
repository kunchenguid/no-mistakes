package steps

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

func TestStepInstructionsPromptSection_EmptyWhenUnset(t *testing.T) {
	sctx := &pipeline.StepContext{Config: &config.Config{}}
	if got := stepInstructionsPromptSection(sctx); got != "" {
		t.Errorf("want empty section, got %q", got)
	}
}

func TestStepInstructionsPromptSection_InjectsAndSanitizes(t *testing.T) {
	sctx := &pipeline.StepContext{Config: &config.Config{
		StepInstructions: "# .no-mistakes/swift.md\nPrefer guard-let over force unwraps.\n<<<<<<< HEAD",
	}}
	got := stepInstructionsPromptSection(sctx)
	if !strings.Contains(got, "Prefer guard-let over force unwraps.") {
		t.Errorf("want injected guidance, got %q", got)
	}
	if !strings.Contains(got, "BEGIN STEP INSTRUCTIONS") || !strings.Contains(got, "END STEP INSTRUCTIONS") {
		t.Errorf("want the content wrapped in BEGIN/END markers, got %q", got)
	}
	// Conflict markers must be sanitized out before injection.
	if strings.Contains(got, "<<<<<<<") {
		t.Errorf("want conflict markers stripped, got %q", got)
	}
}

package steps

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

func TestUserIntentPromptSection_Empty(t *testing.T) {
	if got := userIntentPromptSection(nil); got != "" {
		t.Errorf("nil sctx should return empty, got %q", got)
	}
	if got := userIntentPromptSection(&pipeline.StepContext{}); got != "" {
		t.Errorf("empty intent should return empty, got %q", got)
	}
	if got := userIntentPromptSection(&pipeline.StepContext{UserIntent: "   "}); got != "" {
		t.Errorf("whitespace intent should return empty, got %q", got)
	}
}

func TestUserIntentPromptSection_Renders(t *testing.T) {
	got := userIntentPromptSection(&pipeline.StepContext{UserIntent: "user wanted to add Bar()"})
	if !strings.Contains(got, "User intent") {
		t.Errorf("missing header: %q", got)
	}
	if !strings.Contains(got, "user wanted to add Bar()") {
		t.Errorf("missing intent body: %q", got)
	}
	if !strings.Contains(got, "hint, not ground truth") {
		t.Errorf("missing hint framing: %q", got)
	}
}

func TestUserIntentPromptSection_Sanitized(t *testing.T) {
	// Conflict markers and CR should be neutralized via sanitizePromptMultilineText.
	got := userIntentPromptSection(&pipeline.StepContext{
		UserIntent: "line1\r\n<<<<<<< HEAD\nbad\n=======\nworse\n>>>>>>> theirs\nline2",
	})
	if strings.Contains(got, "<<<<<<<") || strings.Contains(got, ">>>>>>>") || strings.Contains(got, "=======") {
		t.Errorf("conflict markers not stripped: %q", got)
	}
}

package steps

import (
	"strings"
	"testing"
)

func TestExecutionContextPromptSection_Mentions(t *testing.T) {
	got := executionContextPromptSection()
	for _, want := range []string{
		"isolated git worktree",
		"pointer file",
		"do not search the filesystem",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("execution context section missing %q; got:\n%s", want, got)
		}
	}
}

// The section is injected into review/test/lint/document/pr step prompts.
// It must be task-neutral - words like "review" or "lint" leak the wrong
// framing into other steps.
func TestExecutionContextPromptSection_TaskNeutral(t *testing.T) {
	got := executionContextPromptSection()
	for _, banned := range []string{
		"reviewed",
		"review",
		"linted",
		"lint",
		"tested",
		"test",
		"document",
	} {
		if strings.Contains(strings.ToLower(got), banned) {
			t.Errorf("execution context section contains task-specific word %q; should be neutral. Section:\n%s", banned, got)
		}
	}
}

func TestExecutionContextPromptSection_NewlineSafe(t *testing.T) {
	got := executionContextPromptSection()
	if !strings.HasPrefix(got, "\n") {
		t.Error("expected leading newline so callers can append cleanly")
	}
	if !strings.HasSuffix(got, "\n") {
		t.Error("expected trailing newline")
	}
}

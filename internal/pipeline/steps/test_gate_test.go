package steps

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// These are the diff-class gate's white-box unit tests: the matcher rule, the
// fail-open classification, and the PR line renderer, all of which need
// package internals and cost no subprocesses.
//
// The gate's step-driving tests - the ones that assert the live-evidence
// agent was or was not invoked - live in the sibling package
// internal/pipeline/steps/testgate, which owns the reason.

// TestMatchNonProductPattern covers the one matcher rule that ignore_patterns
// does not have: a leading "**/" matches at any depth, which is how the
// defaults reach a nested testdata or fixtures directory.
func TestMatchNonProductPattern(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		file    string
		pattern string
		want    bool
	}{
		{file: "internal/foo/testdata/golden.txt", pattern: "**/testdata/**", want: true},
		{file: "testdata/golden.txt", pattern: "**/testdata/**", want: true},
		{file: "internal/testdata", pattern: "**/testdata/**", want: true},
		{file: "internal/foo/testdatabase/x.go", pattern: "**/testdata/**", want: false},
		{file: "docs/guide.md", pattern: "docs/**", want: true},
		{file: "internal/docs/guide.md", pattern: "docs/**", want: false},
		{file: "internal/a/b/README.md", pattern: "*.md", want: true},
		{file: "internal/checkout/checkout.go", pattern: "*.md", want: false},
	} {
		if got := matchNonProductPattern(tc.file, tc.pattern); got != tc.want {
			t.Errorf("matchNonProductPattern(%q, %q) = %v, want %v", tc.file, tc.pattern, got, tc.want)
		}
	}
}

// TestIsNonProductPath_NilClassificationRunsTheAgent pins the fail-open
// direction: with no classification resolved, nothing is non-product, so the
// gate never skips a turn it cannot justify skipping.
func TestIsNonProductPath_NilClassificationRunsTheAgent(t *testing.T) {
	t.Parallel()
	if isNonProductPath("docs/guide.md", nil) {
		t.Fatal("an unresolved classification must treat every path as product")
	}
}

// TestRenderLiveValidationLine_NamesTheEvidencePath keeps the PR's Testing
// section honest about WHY it has the verdict it shows: a reused go and a
// freshly driven go otherwise render identically.
func TestRenderLiveValidationLine_NamesTheEvidencePath(t *testing.T) {
	t.Parallel()
	scenarios := []types.TestScenario{{Name: "checkout", Result: types.ScenarioResultPass, Live: true}}

	line := renderLiveValidationLine(scenarios, types.TestVerdictGo, "product files unchanged since abc123; reused from run run-9", types.TestEvidenceSourceReused)
	for _, want := range []string{"go", "1 of 1 scenarios driven live", "reused from run run-9"} {
		if !strings.Contains(line, want) {
			t.Errorf("line %q omits %q", line, want)
		}
	}

	// A pre-gate payload renders exactly as before.
	if got := renderLiveValidationLine(scenarios, types.TestVerdictGo, "", ""); strings.Contains(got, "(") {
		t.Errorf("line %q should carry no evidence note", got)
	}

	// An automatic no-surface has no scenarios at all, so the reason is the
	// only thing worth rendering.
	got := renderLiveValidationLine(nil, types.TestVerdictNoSurface, "no product file in diff", types.TestEvidenceSourceNoProductChange)
	if !strings.Contains(got, "no-surface") || !strings.Contains(got, "no product file in diff") {
		t.Errorf("line %q should name the automatic no-surface and its reason", got)
	}

	if got := renderLiveValidationLine(nil, "", "", ""); got != "" {
		t.Errorf("nothing recorded should render nothing, got %q", got)
	}
}

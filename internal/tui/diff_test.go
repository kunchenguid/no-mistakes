package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestParseDiffLines_Empty(t *testing.T) {
	lines := parseDiffLines("")
	if lines != nil {
		t.Errorf("expected nil for empty input, got %d lines", len(lines))
	}
}

func TestParseDiffLines_Simple(t *testing.T) {
	raw := `diff --git a/main.go b/main.go
index abc1234..def5678 100644
--- a/main.go
+++ b/main.go
@@ -1,3 +1,4 @@
 package main
+import "fmt"
-var x = 1
 func main() {}
`
	lines := parseDiffLines(raw)
	if len(lines) != 9 {
		t.Fatalf("expected 9 lines, got %d", len(lines))
	}

	// Check line types.
	expected := []diffLineType{
		diffLineFileHeader, // diff --git
		diffLineFileHeader, // index
		diffLineFileHeader, // ---
		diffLineFileHeader, // +++
		diffLineHunkHeader, // @@
		diffLineContext,    // package main
		diffLineAddition,   // +import
		diffLineDeletion,   // -var
		diffLineContext,    // func main
	}
	for i, want := range expected {
		if lines[i].Type != want {
			t.Errorf("line %d: expected type %d, got %d (text: %q)", i, want, lines[i].Type, lines[i].Text)
		}
	}
}

func TestParseDiffLines_MultipleFiles(t *testing.T) {
	raw := `diff --git a/a.go b/a.go
--- a/a.go
+++ b/a.go
@@ -1 +1 @@
-old
+new
diff --git a/b.go b/b.go
--- a/b.go
+++ b/b.go
@@ -1 +1 @@
-foo
+bar
`
	lines := parseDiffLines(raw)
	// Count file headers.
	fileHeaders := 0
	for _, l := range lines {
		if l.Type == diffLineFileHeader && strings.HasPrefix(l.Text, "diff --git") {
			fileHeaders++
		}
	}
	if fileHeaders != 2 {
		t.Errorf("expected 2 file headers, got %d", fileHeaders)
	}
}

func TestClassifyDiffLine(t *testing.T) {
	tests := []struct {
		line string
		want diffLineType
	}{
		{"diff --git a/f b/f", diffLineFileHeader},
		{"--- a/f", diffLineFileHeader},
		{"+++ b/f", diffLineFileHeader},
		{"index abc..def 100644", diffLineFileHeader},
		{"@@ -1,3 +1,4 @@", diffLineHunkHeader},
		{"+added", diffLineAddition},
		{"-removed", diffLineDeletion},
		{" context", diffLineContext},
		{"random text", diffLineContext},
	}
	for _, tt := range tests {
		if got := classifyDiffLine(tt.line); got != tt.want {
			t.Errorf("classifyDiffLine(%q) = %d, want %d", tt.line, got, tt.want)
		}
	}
}

func TestDiffStats(t *testing.T) {
	lines := []diffLine{
		{Type: diffLineFileHeader, Text: "diff --git a/main.go b/main.go"},
		{Type: diffLineFileHeader, Text: "--- a/main.go"},
		{Type: diffLineFileHeader, Text: "+++ b/main.go"},
		{Type: diffLineHunkHeader, Text: "@@ -1,3 +1,4 @@"},
		{Type: diffLineContext, Text: " package main"},
		{Type: diffLineAddition, Text: "+import \"fmt\""},
		{Type: diffLineAddition, Text: "+import \"os\""},
		{Type: diffLineDeletion, Text: "-var x = 1"},
	}

	files, adds, dels := diffStats(lines)
	if files != 1 {
		t.Errorf("expected 1 file, got %d", files)
	}
	if adds != 2 {
		t.Errorf("expected 2 additions, got %d", adds)
	}
	if dels != 1 {
		t.Errorf("expected 1 deletion, got %d", dels)
	}
}

func TestDiffStats_MultipleFiles(t *testing.T) {
	lines := []diffLine{
		{Type: diffLineFileHeader, Text: "+++ b/a.go"},
		{Type: diffLineAddition, Text: "+line"},
		{Type: diffLineFileHeader, Text: "+++ b/b.go"},
		{Type: diffLineDeletion, Text: "-line"},
	}

	files, adds, dels := diffStats(lines)
	if files != 2 {
		t.Errorf("expected 2 files, got %d", files)
	}
	if adds != 1 {
		t.Errorf("expected 1 addition, got %d", adds)
	}
	if dels != 1 {
		t.Errorf("expected 1 deletion, got %d", dels)
	}
}

func TestDiffStats_DevNull(t *testing.T) {
	lines := []diffLine{
		{Type: diffLineFileHeader, Text: "+++ /dev/null"},
		{Type: diffLineDeletion, Text: "-removed"},
	}

	files, _, _ := diffStats(lines)
	if files != 0 {
		t.Errorf("expected 0 files (/dev/null excluded), got %d", files)
	}
}

func TestRenderDiff_Empty(t *testing.T) {
	if got := renderDiff("", 80, 20, 0, "", ""); got != "" {
		t.Errorf("expected empty for empty input, got %q", got)
	}
}

func TestRenderDiff_HasStats(t *testing.T) {
	raw := `diff --git a/main.go b/main.go
--- a/main.go
+++ b/main.go
@@ -1 +1,2 @@
 package main
+import "fmt"
`
	got := renderDiff(raw, 80, 0, 0, "", "")
	if !strings.Contains(got, "1 file") {
		t.Error("expected file count in stats")
	}
	if !strings.Contains(got, "+1") {
		t.Error("expected addition count in stats")
	}
}

func TestRenderDiff_ColoredLines(t *testing.T) {
	raw := `diff --git a/main.go b/main.go
--- a/main.go
+++ b/main.go
@@ -1,2 +1,2 @@
-old line
+new line
`
	got := renderDiff(raw, 80, 0, 0, "", "")
	// Lines should be present (rendered with styles, but text should be there).
	if !strings.Contains(got, "old line") {
		t.Error("expected deletion line in output")
	}
	if !strings.Contains(got, "new line") {
		t.Error("expected addition line in output")
	}
}

func TestRenderDiff_Scrolling(t *testing.T) {
	// Build a diff with many lines.
	var b strings.Builder
	b.WriteString("diff --git a/main.go b/main.go\n")
	b.WriteString("--- a/main.go\n")
	b.WriteString("+++ b/main.go\n")
	b.WriteString("@@ -1,20 +1,20 @@\n")
	for i := 0; i < 20; i++ {
		b.WriteString("+line " + strings.Repeat("x", i) + "\n")
	}
	raw := b.String()

	// Render with a small viewport and offset.
	got := renderDiff(raw, 80, 5, 2, "", "")

	// Should show scroll indicator since there are more lines.
	if !strings.Contains(got, "more lines") {
		t.Error("expected scroll indicator for remaining lines")
	}
}

func TestRenderDiff_ScrollEnd(t *testing.T) {
	raw := `diff --git a/main.go b/main.go
--- a/main.go
+++ b/main.go
@@ -1 +1 @@
-old
+new
`
	// Scroll to near the end with a small viewport.
	got := renderDiff(raw, 80, 3, 3, "", "")

	// Should show scroll-up indicator since we scrolled past start.
	if !strings.Contains(got, "↑") {
		t.Error("expected ↑ scroll indicator when scrolled to end")
	}
}

func TestRenderDiff_WrappedInBox(t *testing.T) {
	raw := `diff --git a/main.go b/main.go
--- a/main.go
+++ b/main.go
@@ -1 +1,2 @@
 package main
+import "fmt"
`
	got := stripANSI(renderDiff(raw, 80, 0, 0, "", ""))
	lines := strings.Split(got, "\n")
	if len(lines) == 0 {
		t.Fatal("expected non-empty output")
	}
	// Should have box with "Diff" title.
	if !strings.Contains(lines[0], "Diff") {
		t.Errorf("expected 'Diff' title in top border, got %q", lines[0])
	}
	if !strings.Contains(lines[0], "╭") {
		t.Error("expected rounded top-left corner in diff box")
	}
}

func TestRenderDiff_ScrollIndicatorInBottomBorder(t *testing.T) {
	// Build a diff with many lines.
	var b strings.Builder
	b.WriteString("diff --git a/main.go b/main.go\n")
	b.WriteString("--- a/main.go\n")
	b.WriteString("+++ b/main.go\n")
	b.WriteString("@@ -1,20 +1,20 @@\n")
	for i := 0; i < 20; i++ {
		b.WriteString(fmt.Sprintf("+line %d\n", i))
	}

	got := stripANSI(renderDiff(b.String(), 80, 5, 0, "", ""))
	lines := strings.Split(got, "\n")
	// The last non-empty line should be the bottom border with scroll info.
	lastLine := ""
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			lastLine = lines[i]
			break
		}
	}
	if !strings.Contains(lastLine, "╰") {
		t.Errorf("expected bottom border with ╰, got %q", lastLine)
	}
	if !strings.Contains(lastLine, "more lines") || !strings.Contains(lastLine, "↓") {
		t.Errorf("expected scroll indicator in bottom border, got %q", lastLine)
	}
}

func TestDiffLineStyle_Types(t *testing.T) {
	// Just verify no panics and styles are created.
	types := []diffLineType{
		diffLineContext,
		diffLineAddition,
		diffLineDeletion,
		diffLineFileHeader,
		diffLineHunkHeader,
	}
	for _, dt := range types {
		style := diffLineStyle(dt)
		_ = style.Render("test") // should not panic
	}
}

func TestModel_DiffToggle(t *testing.T) {
	run := testRun()
	m := NewModel("/tmp/sock", nil, run)
	m.steps[0].Status = types.StepStatusFixReview
	m.stepDiffs[types.StepReview] = "+new line\n"

	// Toggle on.
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
	model := updated.(Model)
	if !model.showDiff {
		t.Error("expected showDiff=true after 'd' press")
	}

	// Toggle off.
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
	model = updated.(Model)
	if model.showDiff {
		t.Error("expected showDiff=false after second 'd' press")
	}
}

func TestModel_DiffToggle_NoEffect_NoAwaitingStep(t *testing.T) {
	run := testRun()
	m := NewModel("/tmp/sock", nil, run)

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
	model := updated.(Model)
	if model.showDiff {
		t.Error("expected showDiff=false when no step is awaiting")
	}
}

func TestModel_DiffScroll(t *testing.T) {
	run := testRun()
	m := NewModel("/tmp/sock", nil, run)
	m.showDiff = true

	// Scroll down.
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	model := updated.(Model)
	if model.diffOffset != 1 {
		t.Errorf("expected diffOffset=1, got %d", model.diffOffset)
	}

	// Scroll up.
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	model = updated.(Model)
	if model.diffOffset != 0 {
		t.Errorf("expected diffOffset=0, got %d", model.diffOffset)
	}

	// Can't scroll below 0.
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	model = updated.(Model)
	if model.diffOffset != 0 {
		t.Errorf("expected diffOffset=0, got %d", model.diffOffset)
	}
}

func TestModel_DiffScroll_NoEffectWhenHidden(t *testing.T) {
	run := testRun()
	m := NewModel("/tmp/sock", nil, run)
	m.showDiff = false

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	model := updated.(Model)
	if model.diffOffset != 0 {
		t.Error("expected no scroll when diff is hidden")
	}
}

func TestModel_ApplyEvent_StepCompletedWithDiff(t *testing.T) {
	run := testRun()
	m := NewModel("/tmp/sock", nil, run)
	m.showDiff = true
	m.diffOffset = 5

	diff := "+new line\n-old line\n"
	m.applyEvent(ipc.Event{
		Type:     ipc.EventStepCompleted,
		RunID:    run.ID,
		StepName: ptr(types.StepReview),
		Status:   ptr(string(types.StepStatusFixReview)),
		Diff:     &diff,
	})

	got, ok := m.stepDiffs[types.StepReview]
	if !ok || got != diff {
		t.Error("expected diff stored for review step")
	}
	// showDiff and offset should reset.
	if m.showDiff {
		t.Error("expected showDiff reset to false")
	}
	if m.diffOffset != 0 {
		t.Error("expected diffOffset reset to 0")
	}
}

func TestModel_View_ShowsDiff(t *testing.T) {
	run := testRun()
	m := NewModel("/tmp/sock", nil, run)
	m.steps[0].Status = types.StepStatusFixReview
	m.showDiff = true
	m.stepDiffs[types.StepReview] = `diff --git a/main.go b/main.go
--- a/main.go
+++ b/main.go
@@ -1 +1,2 @@
 package main
+import "fmt"
`
	m.height = 40

	view := m.View()
	if !strings.Contains(view, "1 file") {
		t.Error("expected diff stats in view")
	}
	if !strings.Contains(view, "import") {
		t.Error("expected diff content in view")
	}
}

func TestModel_View_ShowsFindingsNotDiff(t *testing.T) {
	run := testRun()
	m := NewModel("/tmp/sock", nil, run)
	m.steps[0].Status = types.StepStatusAwaitingApproval
	m.showDiff = false
	m.stepFindings[types.StepReview] = `{"findings":[{"severity":"warning","description":"check this"}],"summary":"1 issue"}`
	m.stepDiffs[types.StepReview] = "+some diff\n"

	view := m.View()
	// Should show findings, not diff.
	if !strings.Contains(view, "check this") {
		t.Error("expected findings in view when showDiff is false")
	}
}

func TestRenderPipelineView_DiffKey(t *testing.T) {
	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval
	// Action bar is now rendered outside the pipeline box per DESIGN.md.
	out := stripANSI(renderActionBar(run.Steps, true, true, false, 5, 5, false, true))
	if !strings.Contains(out, "d diff") {
		t.Error("expected d diff in approval prompt")
	}
}

// --- CI view tests ---

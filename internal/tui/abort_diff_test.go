package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/muesli/termenv"
)

func TestAbortConfirmation_FirstPressShowsConfirm(t *testing.T) {
	// First 'x' press should NOT send abort - should set confirmAbort flag
	// and show a confirmation prompt in the action bar.
	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval
	run.Steps[0].FindingsJSON = ptr(`{"summary":"test","items":[{"id":"f1","severity":"error","file":"a.go","line":1,"description":"bug"}]}`)

	m := NewModel("/tmp/sock", nil, run)
	m.width = 80

	// Press 'x' once.
	result, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	updated := result.(Model)

	// confirmAbort should be set.
	if !updated.confirmAbort {
		t.Error("expected confirmAbort to be true after first x press")
	}

	// The action bar should show a confirmation hint.
	view := updated.View()
	stripped := stripANSI(view)
	if !strings.Contains(stripped, "x again to abort") {
		t.Errorf("expected 'x again to abort' in view, got:\n%s", stripped)
	}
}

func TestAbortConfirmation_SecondPressSendsAbort(t *testing.T) {
	// Second 'x' press should actually send the abort command.
	run := testRun()

	m := NewModel("/tmp/sock", nil, run)
	m.width = 80
	m.confirmAbort = true // simulate first press already happened

	// Press 'x' again - this should produce a command (the respond cmd).
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})

	// Should produce a non-nil command (the abort RPC call).
	if cmd == nil {
		t.Error("expected a non-nil command from second x press (abort should be sent)")
	}
}

func TestModel_View_HelpOverlay_ShowsAbortWhenRunning(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	run := testRun()
	m := NewModel("", nil, run)
	m.width = 80
	m.height = 40
	m.showHelp = true

	view := m.View()
	plain := stripANSI(view)

	if !strings.Contains(plain, "x x") || !strings.Contains(plain, "abort pipeline") {
		t.Errorf("help should show top-level abort while pipeline is running, got:\n%s", plain)
	}
}

func TestFindDiffOffset_MatchesFileAndHunk(t *testing.T) {
	// findDiffOffset should return the index of the hunk header
	// that contains the target file and line number.
	raw := "diff --git a/foo.go b/foo.go\n" +
		"--- a/foo.go\n" +
		"+++ b/foo.go\n" +
		"@@ -10,5 +10,7 @@ func foo() {\n" +
		" context\n" +
		"+added\n" +
		" context\n" +
		"@@ -30,3 +32,4 @@ func bar() {\n" +
		" context\n" +
		"+another\n"
	lines := parseDiffLines(raw)

	// Line 12 is in the first hunk (+10,7 covers lines 10-16).
	offset := findDiffOffset(lines, "foo.go", 12)
	if offset != 3 { // index of "@@ -10,5 +10,7 @@" line
		t.Errorf("expected offset=3 for foo.go:12, got %d", offset)
	}

	// Line 33 is in the second hunk (+32,4 covers lines 32-35).
	offset = findDiffOffset(lines, "foo.go", 33)
	if offset != 7 { // index of "@@ -30,3 +32,4 @@" line
		t.Errorf("expected offset=7 for foo.go:33, got %d", offset)
	}
}

func TestFindDiffOffset_FileNotFound(t *testing.T) {
	// Should return 0 when the file doesn't exist in the diff.
	raw := "diff --git a/foo.go b/foo.go\n" +
		"+++ b/foo.go\n" +
		"@@ -1,3 +1,4 @@\n" +
		"+added\n"
	lines := parseDiffLines(raw)

	offset := findDiffOffset(lines, "bar.go", 1)
	if offset != 0 {
		t.Errorf("expected offset=0 for non-existent file, got %d", offset)
	}
}

func TestFindDiffOffset_ScrollsToFileHeader(t *testing.T) {
	// When line=0 or line not in any hunk, should scroll to the file header.
	raw := "diff --git a/foo.go b/foo.go\n" +
		"--- a/foo.go\n" +
		"+++ b/foo.go\n" +
		"@@ -10,3 +10,4 @@\n" +
		"+added\n"
	lines := parseDiffLines(raw)

	// Line 0 means "just show me the file".
	offset := findDiffOffset(lines, "foo.go", 0)
	if offset != 0 { // index of "diff --git a/foo.go" line
		t.Errorf("expected offset=0 for foo.go:0, got %d", offset)
	}

	// Line 99 is beyond any hunk - should still scroll to the file header.
	offset = findDiffOffset(lines, "foo.go", 99)
	if offset != 0 {
		t.Errorf("expected offset=0 for foo.go:99 (beyond all hunks), got %d", offset)
	}
}

func TestDiffToggle_AutoScrollsToFinding(t *testing.T) {
	// When pressing 'd' to switch from findings to diff, diffOffset
	// should auto-scroll to the location of the current finding.
	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval
	run.Steps[0].FindingsJSON = ptr(`{"summary":"test","items":[` +
		`{"id":"f1","severity":"error","file":"foo.go","line":33,"description":"bug1"},` +
		`{"id":"f2","severity":"warning","file":"bar.go","line":5,"description":"bug2"}]}`)

	m := NewModel("/tmp/sock", nil, run)
	m.width = 80
	m.height = 40
	m.stepDiffs[types.StepReview] = "diff --git a/foo.go b/foo.go\n" +
		"--- a/foo.go\n" +
		"+++ b/foo.go\n" +
		"@@ -30,3 +30,4 @@ func bar() {\n" +
		" context\n" +
		"+added\n" +
		"diff --git a/bar.go b/bar.go\n" +
		"--- a/bar.go\n" +
		"+++ b/bar.go\n" +
		"@@ -3,3 +3,4 @@\n" +
		"+new line\n"

	// Cursor is on finding 0 (foo.go:33). Press 'd' to show diff.
	result, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
	model := result.(Model)

	if !model.showDiff {
		t.Fatal("expected showDiff=true")
	}
	// Should auto-scroll to the hunk containing foo.go line 33.
	// The hunk header "@@ -30,3 +30,4 @@" is at index 3.
	if model.diffOffset != 3 {
		t.Errorf("expected diffOffset=3 for foo.go:33, got %d", model.diffOffset)
	}
}

func TestAbortConfirmation_OtherKeyResetsConfirm(t *testing.T) {
	// Pressing any other key after first 'x' should reset confirmAbort.
	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval
	run.Steps[0].FindingsJSON = ptr(`{"summary":"test","items":[{"id":"f1","severity":"error","file":"a.go","line":1,"description":"bug"}]}`)

	m := NewModel("/tmp/sock", nil, run)
	m.width = 80
	m.confirmAbort = true // simulate first press

	// Press 'j' (a navigation key, not 'x').
	result, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	updated := result.(Model)

	if updated.confirmAbort {
		t.Error("expected confirmAbort to be false after pressing a different key")
	}
}

func TestRenderDiff_LongLinesTruncated(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	// Create a diff with a line longer than the box content width.
	longLine := "+" + strings.Repeat("x", 200) // 201 chars total
	raw := "diff --git a/foo.go b/foo.go\n--- a/foo.go\n+++ b/foo.go\n@@ -1,3 +1,3 @@\n context line\n" + longLine + "\n"

	boxWidth := 80
	contentWidth := boxWidth - 4 // 2 border + 2 padding = 76

	result := renderDiff(raw, boxWidth, 0, 0, "", "")
	plain := stripANSI(result)

	// Check that no content line exceeds the box width.
	for _, line := range strings.Split(plain, "\n") {
		if line == "" {
			continue
		}
		// Each line in the box should be exactly boxWidth visual chars wide.
		w := lipgloss.Width(line)
		if w > boxWidth {
			t.Errorf("line exceeds box width (%d > %d): %s", w, boxWidth, line)
		}
	}

	// Verify the long line was truncated by checking the content width.
	// The long line should NOT appear in full inside the box.
	if strings.Contains(plain, strings.Repeat("x", contentWidth+1)) {
		t.Error("expected long diff line to be truncated to fit box content width")
	}
}

func TestRenderDiff_ShortLinesNotTruncated(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	// A short line should appear in full.
	raw := "diff --git a/foo.go b/foo.go\n--- a/foo.go\n+++ b/foo.go\n@@ -1,3 +1,3 @@\n context line\n+short addition\n"

	result := renderDiff(raw, 80, 0, 0, "", "")
	plain := stripANSI(result)

	if !strings.Contains(plain, "short addition") {
		t.Error("expected short diff line to appear in full")
	}
}

func TestRenderDiff_TruncatedLinePreservesPrefix(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	// A long addition line should still start with "+" after truncation.
	longLine := "+" + strings.Repeat("a", 200)
	raw := "diff --git a/foo.go b/foo.go\n--- a/foo.go\n+++ b/foo.go\n@@ -1,3 +1,3 @@\n context\n" + longLine + "\n"

	result := renderDiff(raw, 80, 0, 0, "", "")
	plain := stripANSI(result)

	// The truncated line should still contain "+a" (the diff prefix is preserved).
	// Box lines look like: │ +aaa... │
	found := false
	for _, line := range strings.Split(plain, "\n") {
		// Extract content between box borders.
		if strings.HasPrefix(strings.TrimSpace(line), "│") && strings.Contains(line, "+a") {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected truncated addition line to still start with + prefix")
	}
}

// --- Log line truncation tests ---

func TestModel_View_LogLongLinesWrapped(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	run := testRun()
	m := NewModel("/tmp/sock", nil, run)
	m.width = 80
	// Create a log line much longer than the box content width.
	longLog := "running " + strings.Repeat("x", 200) // well over 80 chars
	m.logs = []string{longLog}

	view := stripANSI(m.View())

	// No line in the rendered output should exceed the box width.
	for _, line := range strings.Split(view, "\n") {
		if line == "" {
			continue
		}
		w := lipgloss.Width(line)
		if w > 80 {
			t.Errorf("log line exceeds box width (%d > 80): %s", w, line)
		}
	}

	if got := strings.Count(view, "x"); got < 200 {
		t.Errorf("expected wrapped log output to preserve all 200 x characters, got %d", got)
	}
}

func TestModel_View_LogShortLinesNotTruncated(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	run := testRun()
	m := NewModel("/tmp/sock", nil, run)
	m.width = 80
	m.logs = []string{"running go test ./..."}

	view := stripANSI(m.View())

	if !strings.Contains(view, "running go test ./...") {
		t.Error("expected short log line to appear in full")
	}
}

func TestRenderCIView_LogLongLinesWrapped(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	run := testRunWithCI()
	run.Steps[5].Status = types.StepStatusRunning
	longLog := "monitoring CI for PR #42 (timeout: 4h)..."
	longLog2 := "running " + strings.Repeat("y", 200)
	logs := []string{longLog, longLog2}

	result := stripANSI(renderCIView(run, run.Steps, "", logs, 80))

	// No line should exceed the box width (80).
	for _, line := range strings.Split(result, "\n") {
		if line == "" {
			continue
		}
		w := lipgloss.Width(line)
		if w > 80 {
			t.Errorf("CI log line exceeds box width (%d > 80): %s", w, line)
		}
	}

	if got := strings.Count(result, "y"); got < 200 {
		t.Errorf("expected wrapped CI log output to preserve all 200 y characters, got %d", got)
	}
}

func TestRenderFindingsWithSelection_LongFilePathTruncated(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)

	// Create a finding with a very long file path that would overflow an 80-width box.
	longPath := "src/internal/very/deeply/nested/package/structure/" + strings.Repeat("x", 100) + "/handler.go"
	raw := fmt.Sprintf(`{"items":[{"id":"f1","severity":"error","file":"%s","line":42,"description":"Missing error check"}]}`, longPath)
	selected := map[string]bool{"f1": true}

	// Width is 76 (box content width = 80 - 4 for border/padding).
	content, _ := renderFindingsWithSelection(raw, 76, 0, selected, 0)
	result := stripANSI(content)

	// No line in the findings content should exceed 76 chars.
	for _, line := range strings.Split(result, "\n") {
		if line == "" {
			continue
		}
		w := lipgloss.Width(line)
		if w > 76 {
			t.Errorf("finding gutter line exceeds content width (%d > 76): %s", w, line)
		}
	}

	// The full long file path should NOT appear.
	if strings.Contains(result, longPath) {
		t.Error("expected long file path to be truncated to fit content width")
	}
}

func TestRenderFindingsWithSelection_ShortFilePathNotTruncated(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)

	raw := `{"items":[{"id":"f1","severity":"error","file":"src/handler.go","line":42,"description":"Missing error check"}]}`
	selected := map[string]bool{"f1": true}

	content, _ := renderFindingsWithSelection(raw, 76, 0, selected, 0)
	result := stripANSI(content)

	// Short file path should appear in full.
	if !strings.Contains(result, "src/handler.go:42") {
		t.Error("expected short file path to appear in full, got:\n" + result)
	}
}

func TestRenderFindingsWithSelection_TruncatedGutterPreservesSeverityIcon(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)

	longPath := strings.Repeat("z", 200) + "/handler.go"
	raw := fmt.Sprintf(`{"items":[{"id":"f1","severity":"error","file":"%s","line":1,"description":"test"}]}`, longPath)
	selected := map[string]bool{"f1": true}

	content, _ := renderFindingsWithSelection(raw, 76, 0, selected, 0)
	result := stripANSI(content)

	// The severity icon and checkbox should still be present even with truncation.
	if !strings.Contains(result, "[x]") {
		t.Error("expected checkbox to survive truncation")
	}
	if !strings.Contains(result, "E") {
		t.Error("expected severity icon to survive truncation")
	}
}

func TestDiffToggle_NoOpWhenNoDiffData(t *testing.T) {
	// Pressing 'd' when the awaiting step has no diff data should NOT toggle showDiff.
	// This prevents the bug where showDiff=true hides selection actions
	// but no diff actually renders.
	run := testRun()
	m := NewModel("/tmp/sock", nil, run)
	m.steps[0].Status = types.StepStatusAwaitingApproval
	m.stepFindings[types.StepReview] = `{"items":[{"id":"f1","severity":"error","file":"a.go","line":1,"description":"bad"}]}`
	// No diff data set for this step.

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
	model := updated.(Model)
	if model.showDiff {
		t.Error("expected showDiff to remain false when no diff data exists for the awaiting step")
	}
}

func TestActionBar_HidesDiffWhenNoDiffData(t *testing.T) {
	// The action bar should NOT show 'd diff' when no diff data exists
	// for the current awaiting step.
	configureTUIColors()
	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval
	m := NewModel("", nil, run)
	m.width = 80
	m.height = 50
	m.stepFindings[types.StepReview] = `{"summary":"test","items":[{"id":"f1","severity":"error","file":"foo.go","line":1,"description":"bad"}]}`
	m.resetFindingSelection(types.StepReview)
	// No diff data set.

	view := stripANSI(m.View())
	if strings.Contains(view, "d diff") {
		t.Error("should NOT show 'd diff' when no diff data exists for the awaiting step")
	}
	if strings.Contains(view, "d findings") {
		t.Error("should NOT show 'd findings' when no diff data exists")
	}
}

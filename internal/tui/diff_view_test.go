package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/muesli/termenv"
)

func TestDiffView_NextFindingKey_MovesCursorAndScrolls(t *testing.T) {
	// Pressing 'n' in diff view should move finding cursor to next finding
	// and auto-scroll the diff to the new finding's hunk location.
	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval
	run.Steps[0].FindingsJSON = ptr(`{"summary":"test","items":[` +
		`{"id":"f1","severity":"error","file":"foo.go","line":12,"description":"bug1"},` +
		`{"id":"f2","severity":"warning","file":"bar.go","line":5,"description":"bug2"}]}`)

	m := NewModel("/tmp/sock", nil, run)
	m.width = 80
	m.height = 40
	m.showDiff = true
	m.stepDiffs[types.StepReview] = "diff --git a/foo.go b/foo.go\n" +
		"--- a/foo.go\n" +
		"+++ b/foo.go\n" +
		"@@ -10,5 +10,7 @@ func foo() {\n" +
		" context\n" +
		"+added\n" +
		"diff --git a/bar.go b/bar.go\n" +
		"--- a/bar.go\n" +
		"+++ b/bar.go\n" +
		"@@ -3,5 +3,6 @@\n" +
		"+new line\n"

	// Cursor starts at finding 0 (foo.go:12). Press 'n' to go to finding 1 (bar.go:5).
	result, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	model := result.(Model)

	cursor := model.findingCursor[types.StepReview]
	if cursor != 1 {
		t.Errorf("expected finding cursor=1 after 'n', got %d", cursor)
	}
	// Diff should auto-scroll to bar.go's hunk header at index 9
	// ("@@ -3,5 +3,6 @@" is the hunk in bar.go).
	if model.diffOffset != 9 {
		t.Errorf("expected diffOffset=9 for bar.go:5, got %d", model.diffOffset)
	}
}

func TestDiffView_PrevFindingKey_MovesCursorAndScrolls(t *testing.T) {
	// Pressing 'p' in diff view should move finding cursor to previous finding
	// and auto-scroll the diff to that finding's hunk location.
	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval
	run.Steps[0].FindingsJSON = ptr(`{"summary":"test","items":[` +
		`{"id":"f1","severity":"error","file":"foo.go","line":12,"description":"bug1"},` +
		`{"id":"f2","severity":"warning","file":"bar.go","line":5,"description":"bug2"}]}`)

	m := NewModel("/tmp/sock", nil, run)
	m.width = 80
	m.height = 40
	m.showDiff = true
	m.findingCursor[types.StepReview] = 1 // start on finding 1 (bar.go:5)
	m.stepDiffs[types.StepReview] = "diff --git a/foo.go b/foo.go\n" +
		"--- a/foo.go\n" +
		"+++ b/foo.go\n" +
		"@@ -10,5 +10,7 @@ func foo() {\n" +
		" context\n" +
		"+added\n" +
		"diff --git a/bar.go b/bar.go\n" +
		"--- a/bar.go\n" +
		"+++ b/bar.go\n" +
		"@@ -3,5 +3,6 @@\n" +
		"+new line\n"

	// Press 'p' to go back to finding 0 (foo.go:12).
	result, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("p")})
	model := result.(Model)

	cursor := model.findingCursor[types.StepReview]
	if cursor != 0 {
		t.Errorf("expected finding cursor=0 after 'p', got %d", cursor)
	}
	// Diff should auto-scroll to foo.go's hunk header at index 3.
	if model.diffOffset != 3 {
		t.Errorf("expected diffOffset=3 for foo.go:12, got %d", model.diffOffset)
	}
}

func TestDiffView_NextFindingKey_NoOpWhenNotInDiffView(t *testing.T) {
	// 'n' and 'p' should be no-ops when not in diff view (showDiff=false).
	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval
	run.Steps[0].FindingsJSON = ptr(`{"summary":"test","items":[` +
		`{"id":"f1","severity":"error","file":"foo.go","line":12,"description":"bug1"},` +
		`{"id":"f2","severity":"warning","file":"bar.go","line":5,"description":"bug2"}]}`)

	m := NewModel("/tmp/sock", nil, run)
	m.width = 80
	m.height = 40
	m.showDiff = false

	// Press 'n' - should not change cursor because we're not in diff mode.
	result, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	model := result.(Model)

	cursor := model.findingCursor[types.StepReview]
	if cursor != 0 {
		t.Errorf("expected finding cursor unchanged at 0 when not in diff view, got %d", cursor)
	}
}

func TestDiffView_ShowsFindingContext(t *testing.T) {
	// When viewing diff with findings, the current finding's info should
	// appear as a context line so users know which finding they're looking at.
	lipgloss.SetColorProfile(termenv.ANSI)
	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval
	run.Steps[0].FindingsJSON = ptr(`{"summary":"test","items":[` +
		`{"id":"f1","severity":"error","file":"foo.go","line":12,"description":"Missing error check"},` +
		`{"id":"f2","severity":"warning","file":"bar.go","line":5,"description":"Unused import"}]}`)

	m := NewModel("/tmp/sock", nil, run)
	m.width = 80
	m.height = 40
	m.stepDiffs[types.StepReview] = "diff --git a/foo.go b/foo.go\n--- a/foo.go\n+++ b/foo.go\n@@ -10,6 +10,8 @@\n context\n+added line\n"
	m.showDiff = true

	output := stripANSI(m.View())

	// Should show the focused finding's file:line and description somewhere in the diff view.
	if !strings.Contains(output, "foo.go:12") {
		t.Errorf("expected diff view to show current finding file:line 'foo.go:12', got:\n%s", output)
	}
	if !strings.Contains(output, "Missing error check") {
		t.Errorf("expected diff view to show current finding description 'Missing error check', got:\n%s", output)
	}
}

func TestDiffView_FindingContextUpdatesOnNavigation(t *testing.T) {
	// When navigating with 'n' key, the finding context should update to show the next finding.
	lipgloss.SetColorProfile(termenv.ANSI)
	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval
	run.Steps[0].FindingsJSON = ptr(`{"summary":"test","items":[` +
		`{"id":"f1","severity":"error","file":"foo.go","line":12,"description":"Missing error check"},` +
		`{"id":"f2","severity":"warning","file":"bar.go","line":5,"description":"Unused import"}]}`)

	m := NewModel("/tmp/sock", nil, run)
	m.width = 80
	m.height = 40
	m.stepDiffs[types.StepReview] = "diff --git a/foo.go b/foo.go\n--- a/foo.go\n+++ b/foo.go\n@@ -10,6 +10,8 @@\n context\n+added line\ndiff --git a/bar.go b/bar.go\n--- a/bar.go\n+++ b/bar.go\n@@ -3,6 +3,8 @@\n context\n+added\n"
	m.showDiff = true

	// Press 'n' to move to second finding.
	result, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	model := result.(Model)

	output := stripANSI(model.View())

	// Should now show the second finding's info.
	if !strings.Contains(output, "bar.go:5") {
		t.Errorf("expected diff view to show navigated finding 'bar.go:5' after pressing n, got:\n%s", output)
	}
	if !strings.Contains(output, "Unused import") {
		t.Errorf("expected diff view to show navigated finding description 'Unused import' after pressing n, got:\n%s", output)
	}
}

func TestDiffView_NoFindingContextWithoutFindings(t *testing.T) {
	// When diff view has no findings, there should be no finding context line.
	lipgloss.SetColorProfile(termenv.ANSI)
	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval

	m := NewModel("/tmp/sock", nil, run)
	m.width = 80
	m.height = 40
	m.stepDiffs[types.StepReview] = "diff --git a/foo.go b/foo.go\n--- a/foo.go\n+++ b/foo.go\n@@ -10,6 +10,8 @@\n context\n+added line\n"
	m.showDiff = true

	output := stripANSI(m.View())

	// Should have the diff content but no finding context (no severity icons in header area).
	// Check that the diff view renders without error.
	if !strings.Contains(output, "Diff") {
		t.Errorf("expected diff view title 'Diff', got:\n%s", output)
	}
	// The diff view should NOT have finding-specific content like severity icons
	// outside of the diff content itself.
	lines := strings.Split(output, "\n")
	for _, line := range lines {
		cleaned := strings.TrimSpace(line)
		// Finding context would appear between stats and diff content.
		// If there's no findings, we shouldn't see a "Finding N/M" style line.
		if strings.HasPrefix(cleaned, "Finding ") {
			t.Errorf("unexpected finding context line without findings: %q", line)
		}
	}
}

func TestModel_EscapeReturnsToDiffFromFindings(t *testing.T) {
	// Pressing Escape while in diff view should return to findings view.
	lipgloss.SetColorProfile(termenv.Ascii)
	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval
	m := NewModel("", nil, run)
	m.showDiff = true
	m.stepDiffs[types.StepReview] = "diff --git a/foo.go b/foo.go\n"

	result, _ := m.Update(tea.KeyMsg{Type: tea.KeyEscape})
	m = result.(Model)

	if m.showDiff {
		t.Fatal("expected Escape to return from diff view to findings view")
	}
}

func TestModel_EscapeResetsDiffOffset(t *testing.T) {
	// Pressing Escape while in diff view should also reset diffOffset.
	lipgloss.SetColorProfile(termenv.Ascii)
	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval
	m := NewModel("", nil, run)
	m.showDiff = true
	m.diffOffset = 42
	m.stepDiffs[types.StepReview] = "diff --git a/foo.go b/foo.go\n"

	result, _ := m.Update(tea.KeyMsg{Type: tea.KeyEscape})
	m = result.(Model)

	if m.diffOffset != 0 {
		t.Fatalf("expected Escape to reset diffOffset to 0, got %d", m.diffOffset)
	}
}

func TestModel_EscapeNoOpWhenNotInDiffOrHelp(t *testing.T) {
	// Pressing Escape when not in diff view and help is closed should be a no-op.
	lipgloss.SetColorProfile(termenv.Ascii)
	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval
	m := NewModel("", nil, run)
	m.showDiff = false
	m.showHelp = false
	m.findingCursor = map[types.StepName]int{types.StepReview: 2}

	result, _ := m.Update(tea.KeyMsg{Type: tea.KeyEscape})
	m = result.(Model)

	// State should be unchanged.
	if m.showDiff {
		t.Fatal("Escape should not toggle diff on")
	}
	if m.showHelp {
		t.Fatal("Escape should not toggle help on")
	}
	if m.findingCursor[types.StepReview] != 2 {
		t.Fatalf("Escape should not change cursor position, got %d", m.findingCursor[types.StepReview])
	}
}

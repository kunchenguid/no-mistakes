package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/muesli/termenv"
)

func TestModel_View_HelpOverlay_HidesDiffToggleWhenNoDiffData(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval
	m := NewModel("", nil, run)
	m.width = 80
	m.height = 40
	m.showHelp = true
	// Step is awaiting approval but no diff data has been set in stepDiffs.

	view := m.View()
	plain := stripANSI(view)

	// Action keys like approve should still be shown.
	if !strings.Contains(plain, "approve") {
		t.Errorf("help should show action keys during approval, got:\n%s", plain)
	}
	// The d toggle should NOT be shown since there's no diff data.
	if strings.Contains(plain, "diff/findings toggle") {
		t.Errorf("help should hide d toggle when no diff data exists, got:\n%s", plain)
	}
}

func TestModel_View_HelpOverlay_ShowsDiffToggleWhenDiffDataExists(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval
	m := NewModel("", nil, run)
	m.width = 80
	m.height = 40
	m.showHelp = true
	m.stepDiffs[types.StepReview] = "diff --git a/foo.go b/foo.go\n--- a/foo.go\n+++ b/foo.go\n@@ -1,3 +1,4 @@\n one\n+two\n three\n"

	view := m.View()
	plain := stripANSI(view)

	// The d toggle should be shown since diff data exists.
	if !strings.Contains(plain, "diff/findings toggle") {
		t.Errorf("help should show d toggle when diff data exists, got:\n%s", plain)
	}
}

func TestModel_View_HelpOverlay_HidesDiffToggleWhenEmptyDiffData(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval
	m := NewModel("", nil, run)
	m.width = 80
	m.height = 40
	m.showHelp = true
	m.stepDiffs[types.StepReview] = "" // empty diff data

	view := m.View()
	plain := stripANSI(view)

	// The d toggle should NOT be shown since diff data is empty.
	if strings.Contains(plain, "diff/findings toggle") {
		t.Errorf("help should hide d toggle when diff data is empty, got:\n%s", plain)
	}
}

func TestModel_View_HelpOverlay_ShowsDetachWhenRunning(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	run := testRun()
	run.Steps[0].Status = types.StepStatusRunning
	m := NewModel("", nil, run)
	m.width = 80
	m.height = 40
	m.showHelp = true
	// Pipeline is still running - done is false (default).

	view := m.View()
	plain := stripANSI(view)

	// Help overlay should show a detach hint (not quit) while the pipeline is running.
	if !strings.Contains(plain, "detach") {
		t.Errorf("help should show detach when pipeline is running, got:\n%s", plain)
	}
	if strings.Contains(plain, "quit") {
		t.Errorf("help should NOT show 'quit' when pipeline is running, got:\n%s", plain)
	}
}

func TestModel_View_HelpOverlay_ShowsQuitWhenDone(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	run := testRun()
	run.Status = types.RunCompleted
	for i := range run.Steps {
		run.Steps[i].Status = types.StepStatusCompleted
	}
	m := NewModel("", nil, run)
	m.width = 80
	m.height = 40
	m.showHelp = true
	m.done = true

	view := m.View()
	plain := stripANSI(view)

	// Help overlay should show "q  quit" (not "detach") when pipeline is done.
	if !strings.Contains(plain, "q  quit") {
		t.Errorf("help should show 'q  quit' when pipeline is done, got:\n%s", plain)
	}
	if strings.Contains(plain, "detach") {
		t.Errorf("help should NOT show 'detach' when pipeline is done, got:\n%s", plain)
	}
}

func TestModel_View_HelpOverlay_NeverShowsCombinedDetachQuit(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval
	m := NewModel("", nil, run)
	m.width = 80
	m.height = 40
	m.showHelp = true
	m.stepDiffs[types.StepReview] = "diff --git a/foo.go b/foo.go\n--- a/foo.go\n+++ b/foo.go\n@@ -1,3 +1,4 @@\n one\n+two\n three\n"

	view := m.View()
	plain := stripANSI(view)

	// Help should never show the combined "detach/quit" label.
	if strings.Contains(plain, "detach/quit") {
		t.Errorf("help should not show combined 'detach/quit', got:\n%s", plain)
	}
}

func TestRenderFindings_FocusedFileRefNotDim(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI)
	// Focused finding's file:line reference should NOT be dim, matching the
	// description treatment from iteration 47 for complete visual contrast.
	raw := `{"findings":[
		{"id":"f1","severity":"error","file":"src/handler.go","line":42,"description":"focused text"},
		{"id":"f2","severity":"warning","file":"src/config.go","line":17,"description":"other text"}
	],"summary":"2 issues"}`

	selected := map[string]bool{"f1": true, "f2": true}
	content, _ := renderFindingsWithSelection(raw, 80, 0, selected, 0) // cursor=0, f1 focused

	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(ansiBrightBlack))

	// Focused file ref should NOT be dim-styled.
	if strings.Contains(content, dimStyle.Render("src/handler.go:42")) {
		t.Error("focused finding file:line reference should not be dim-styled")
	}
	// But it should still appear (in default style).
	if !strings.Contains(stripANSI(content), "src/handler.go:42") {
		t.Error("focused finding file:line reference should appear in output")
	}
}

func TestRenderFindings_UnfocusedFileRefDim(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI)
	// Unfocused finding's file:line reference should be dim, matching the
	// description treatment for visual contrast with the focused finding.
	raw := `{"findings":[
		{"id":"f1","severity":"error","file":"src/handler.go","line":42,"description":"first issue"},
		{"id":"f2","severity":"warning","file":"src/config.go","line":17,"description":"second issue"}
	],"summary":"2 issues"}`

	selected := map[string]bool{"f1": true, "f2": true}
	content, _ := renderFindingsWithSelection(raw, 80, 0, selected, 0) // cursor=0, f2 unfocused

	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(ansiBrightBlack))

	// Unfocused file ref should be dim-styled.
	if !strings.Contains(content, dimStyle.Render("src/config.go:17")) {
		t.Error("unfocused finding file:line reference should be dim-styled")
	}
}

func TestRenderFindings_FocusChangesFileRefStyle(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI)
	// Moving cursor should swap which file:line reference is dim vs default.
	raw := `{"findings":[
		{"id":"f1","severity":"error","file":"src/handler.go","line":42,"description":"first issue"},
		{"id":"f2","severity":"warning","file":"src/config.go","line":17,"description":"second issue"}
	],"summary":"2 issues"}`

	selected := map[string]bool{"f1": true, "f2": true}
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(ansiBrightBlack))

	// Cursor at 0: f1 focused (non-dim ref), f2 unfocused (dim ref).
	content0, _ := renderFindingsWithSelection(raw, 80, 0, selected, 0)
	if strings.Contains(content0, dimStyle.Render("src/handler.go:42")) {
		t.Error("with cursor=0, handler.go ref should NOT be dim")
	}
	if !strings.Contains(content0, dimStyle.Render("src/config.go:17")) {
		t.Error("with cursor=0, config.go ref should be dim")
	}

	// Cursor at 1: f2 focused (non-dim ref), f1 unfocused (dim ref).
	content1, _ := renderFindingsWithSelection(raw, 80, 1, selected, 0)
	if !strings.Contains(content1, dimStyle.Render("src/handler.go:42")) {
		t.Error("with cursor=1, handler.go ref should be dim")
	}
	if strings.Contains(content1, dimStyle.Render("src/config.go:17")) {
		t.Error("with cursor=1, config.go ref should NOT be dim")
	}
}

func TestRenderFindings_FocusedSeverityIconNotDim(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI)
	// Focused finding's severity icon should keep its colored style (not dim),
	// matching the description and file:line ref treatment for the focused item.
	raw := `{"findings":[
		{"id":"f1","severity":"error","file":"src/handler.go","line":42,"description":"focused text"},
		{"id":"f2","severity":"warning","file":"src/config.go","line":17,"description":"other text"}
	],"summary":"2 issues"}`

	selected := map[string]bool{"f1": true, "f2": true}
	content, _ := renderFindingsWithSelection(raw, 80, 0, selected, 0) // cursor=0, f1 focused

	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(ansiBrightBlack))

	// Focused severity icon should NOT be dim-styled.
	if strings.Contains(content, dimStyle.Render("E")) {
		t.Error("focused finding severity icon should not be dim-styled")
	}
	// The colored icon should still be present.
	errStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(ansiRed))
	if !strings.Contains(content, errStyle.Render("E")) {
		t.Error("focused finding severity icon should be styled with its severity color")
	}
}

func TestRenderFindings_UnfocusedSeverityIconDim(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI)
	// Unfocused finding's severity icon should be dim (bright black), matching
	// the description and file:line ref dimming for visual contrast.
	raw := `{"findings":[
		{"id":"f1","severity":"error","file":"src/handler.go","line":42,"description":"first issue"},
		{"id":"f2","severity":"warning","file":"src/config.go","line":17,"description":"second issue"}
	],"summary":"2 issues"}`

	selected := map[string]bool{"f1": true, "f2": true}
	content, _ := renderFindingsWithSelection(raw, 80, 0, selected, 0) // cursor=0, f2 unfocused

	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(ansiBrightBlack))

	// Unfocused severity icon (W for warning) should be dim-styled.
	if !strings.Contains(content, dimStyle.Render("W")) {
		t.Error("unfocused finding severity icon should be dim-styled")
	}
	// The colored warning icon should NOT appear for unfocused findings.
	warnStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(ansiYellow))
	if strings.Contains(content, warnStyle.Render("W")) {
		t.Error("unfocused finding severity icon should not use its severity color")
	}
}

func TestRenderFindings_FocusChangesSeverityIconStyle(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI)
	// Moving cursor should swap which severity icon is colored vs dim.
	raw := `{"findings":[
		{"id":"f1","severity":"error","file":"src/handler.go","line":42,"description":"first issue"},
		{"id":"f2","severity":"warning","file":"src/config.go","line":17,"description":"second issue"}
	],"summary":"2 issues"}`

	selected := map[string]bool{"f1": true, "f2": true}
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(ansiBrightBlack))
	errStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(ansiRed))
	warnStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(ansiYellow))

	// Cursor at 0: f1 focused (colored E), f2 unfocused (dim W).
	content0, _ := renderFindingsWithSelection(raw, 80, 0, selected, 0)
	if !strings.Contains(content0, errStyle.Render("E")) {
		t.Error("with cursor=0, error icon should be colored red")
	}
	if !strings.Contains(content0, dimStyle.Render("W")) {
		t.Error("with cursor=0, warning icon should be dim")
	}

	// Cursor at 1: f2 focused (colored W), f1 unfocused (dim E).
	content1, _ := renderFindingsWithSelection(raw, 80, 1, selected, 0)
	if !strings.Contains(content1, dimStyle.Render("E")) {
		t.Error("with cursor=1, error icon should be dim")
	}
	if !strings.Contains(content1, warnStyle.Render("W")) {
		t.Error("with cursor=1, warning icon should be colored yellow")
	}
}

// --- styleLogLine tests ---

func TestStyleLogLine_PassLineGreen(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI)
	line := "PASS: TestFoo (0.3s)"
	styled := styleLogLine(line)
	greenStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(ansiGreen))
	expected := greenStyle.Render(line)
	if styled != expected {
		t.Errorf("PASS line should be green-styled, got %q", styled)
	}
}

func TestStyleLogLine_FailLineRed(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI)
	line := "FAIL: TestBar (0.1s)"
	styled := styleLogLine(line)
	redStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(ansiRed))
	expected := redStyle.Render(line)
	if styled != expected {
		t.Errorf("FAIL line should be red-styled, got %q", styled)
	}
}

func TestStyleLogLine_DefaultLineDim(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI)
	line := "running go test ./..."
	styled := styleLogLine(line)
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(ansiBrightBlack))
	expected := dimStyle.Render(line)
	if styled != expected {
		t.Errorf("default line should be dim-styled, got %q", styled)
	}
}

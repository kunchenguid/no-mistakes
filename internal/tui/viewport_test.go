package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/muesli/termenv"
)

func TestRenderFindings_ViewportShowsSubset(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI)
	// 10 findings, viewport fits 4 items, cursor at 0 (top).
	raw := makeManyFindings(10)
	selected := map[string]bool{}
	for i := 1; i <= 10; i++ {
		selected[fmt.Sprintf("f%d", i)] = true
	}

	out, _ := renderFindingsWithSelection(raw, 80, 0, selected, 4)
	plain := stripANSI(out)

	// Should show first 4 findings (f1 through f4).
	if !strings.Contains(plain, "finding 1 description") {
		t.Errorf("expected finding 1 visible at cursor=0, got:\n%s", plain)
	}
	if !strings.Contains(plain, "finding 4 description") {
		t.Errorf("expected finding 4 visible at cursor=0, got:\n%s", plain)
	}
	// Should NOT show finding 5 (outside viewport).
	if strings.Contains(plain, "finding 5 description") {
		t.Errorf("finding 5 should not be visible when viewport=4 and cursor=0, got:\n%s", plain)
	}
}

func TestRenderFindings_ViewportScrollDownIndicator(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI)
	raw := makeManyFindings(10)
	selected := map[string]bool{}
	for i := 1; i <= 10; i++ {
		selected[fmt.Sprintf("f%d", i)] = true
	}

	_, scrollFooter := renderFindingsWithSelection(raw, 80, 0, selected, 4)

	// Down indicator is returned as scrollFooter (for embedding in box border).
	if !strings.Contains(scrollFooter, "6 more below") {
		t.Errorf("expected scrollFooter with '6 more below', got: %q", scrollFooter)
	}
}

func TestRenderFindings_ViewportScrollUpIndicator(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI)
	raw := makeManyFindings(10)
	selected := map[string]bool{}
	for i := 1; i <= 10; i++ {
		selected[fmt.Sprintf("f%d", i)] = true
	}

	// Cursor at item 9 (index 9, last item) - up indicator should be in scrollFooter.
	out, scrollFooter := renderFindingsWithSelection(raw, 80, 9, selected, 4)
	plain := stripANSI(out)

	// Should show finding 10 (cursor is on it).
	if !strings.Contains(plain, "finding 10 description") {
		t.Errorf("expected finding 10 visible at cursor=9, got:\n%s", plain)
	}
	// Up indicator should be in scrollFooter, not inline content.
	if !strings.Contains(scrollFooter, "above") {
		t.Errorf("expected up scroll indicator in scrollFooter, got: %q", scrollFooter)
	}
	// Should NOT show down indicator when at bottom.
	if strings.Contains(scrollFooter, "↓") {
		t.Errorf("should not show down indicator at bottom, got: %q", scrollFooter)
	}
}

func TestFindingsBoxTitle_MultipleSeverities(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI)
	run := testRun()
	run.Steps[0].Status = types.StepStatusCompleted
	run.Steps[1].Status = types.StepStatusAwaitingApproval

	findingsJSON := `{"summary":"issues","items":[
		{"id":"f1","severity":"error","file":"a.go","line":1,"description":"err"},
		{"id":"f2","severity":"warning","file":"b.go","line":2,"description":"warn"},
		{"id":"f3","severity":"warning","file":"c.go","line":3,"description":"warn2"}
	]}`
	m := NewModel("/tmp/sock", nil, run)
	m.width = 80
	m.height = 40
	m.stepFindings[types.StepTest] = findingsJSON
	m.resetFindingSelection(types.StepTest)

	view := m.View()
	plain := stripANSI(view)

	// Title should show counts per severity: "Findings - E 1 W 2".
	if !strings.Contains(plain, "Findings - E 1 W 2") {
		t.Errorf("expected findings box title with 'Findings - E 1 W 2', got:\n%s", plain)
	}
}

func TestFindingsBoxTitle_NoCursorPosition(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI)
	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval

	findingsJSON := makeManyFindings(5)
	m := NewModel("/tmp/sock", nil, run)
	m.width = 80
	m.height = 40
	m.stepFindings[types.StepReview] = findingsJSON
	m.resetFindingSelection(types.StepReview)

	// Title should show severity counts, not cursor position.
	m.findingCursor[types.StepReview] = 2
	view := m.View()
	plain := stripANSI(view)
	if !strings.Contains(plain, "Findings - W 5") {
		t.Errorf("expected 'Findings - W 5' in title, got:\n%s", plain)
	}
	// Should NOT contain old-style position indicator.
	if strings.Contains(plain, "(3/5)") {
		t.Errorf("title should not contain cursor position indicator, got:\n%s", plain)
	}
}

func TestModel_View_FindingsViewportApplied(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI)
	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval

	m := NewModel("/tmp/sock", nil, run)
	m.width = 80
	m.height = 30 // small terminal height -> should trigger viewport
	m.stepFindings[types.StepReview] = makeManyFindings(15)
	m.resetFindingSelection(types.StepReview)

	view := m.View()
	plain := stripANSI(view)

	// With 15 findings and height=30, not all should be visible.
	// The viewport should limit visible findings and show a scroll indicator.
	if !strings.Contains(plain, "↓") && !strings.Contains(plain, "more below") {
		t.Errorf("expected scroll-down indicator when findings exceed viewport, got:\n%s", plain)
	}
	// Finding 1 should be visible (cursor starts at 0).
	if !strings.Contains(plain, "finding 1 description") {
		t.Errorf("expected finding 1 visible (cursor at 0), got:\n%s", plain)
	}
}

// --- Diff scroll position indicator tests ---

func TestDiffBoxTitle_ShowsScrollPosition(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI)
	// Create a diff with enough lines to scroll.
	var diffLines []string
	diffLines = append(diffLines, "diff --git a/foo.go b/foo.go", "--- a/foo.go", "+++ b/foo.go", "@@ -1,20 +1,20 @@")
	for i := 1; i <= 30; i++ {
		diffLines = append(diffLines, fmt.Sprintf("+line %d", i))
	}
	raw := strings.Join(diffLines, "\n") + "\n"

	// Render at offset 0 with viewHeight 10. Total lines = 34 (4 headers + 30 additions).
	out := renderDiff(raw, 80, 10, 0, "Review", "")
	plain := stripANSI(out)

	// Title should include scroll position: line 1 of total.
	if !strings.Contains(plain, "Diff - Review (1/34)") {
		t.Errorf("expected 'Diff - Review (1/34)' in title at offset=0, got:\n%s", plain)
	}
}

func TestDiffBoxTitle_ScrollPositionUpdatesWithOffset(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI)
	var diffLines []string
	diffLines = append(diffLines, "diff --git a/foo.go b/foo.go", "--- a/foo.go", "+++ b/foo.go", "@@ -1,20 +1,20 @@")
	for i := 1; i <= 30; i++ {
		diffLines = append(diffLines, fmt.Sprintf("+line %d", i))
	}
	raw := strings.Join(diffLines, "\n") + "\n"

	// Render at offset 15 with viewHeight 10. Total = 34.
	out := renderDiff(raw, 80, 10, 15, "Test", "")
	plain := stripANSI(out)

	// Title should show line 16 (offset+1) of 34.
	if !strings.Contains(plain, "Diff - Test (16/34)") {
		t.Errorf("expected 'Diff - Test (16/34)' at offset=15, got:\n%s", plain)
	}
}

func TestDiffBoxTitle_NoPositionWhenAllVisible(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI)
	// Small diff that fits entirely in the viewport.
	raw := "diff --git a/foo.go b/foo.go\n--- a/foo.go\n+++ b/foo.go\n@@ -1 +1 @@\n-old\n+new\n"

	// viewHeight 0 means show all.
	out := renderDiff(raw, 80, 0, 0, "Review", "")
	plain := stripANSI(out)

	// Should NOT show position indicator when all content is visible.
	if strings.Contains(plain, "(/") || strings.Contains(plain, "(1/") {
		t.Errorf("expected no position indicator when all content visible, got:\n%s", plain)
	}
	// But should still show step name.
	if !strings.Contains(plain, "Diff - Review") {
		t.Errorf("expected 'Diff - Review' in title, got:\n%s", plain)
	}
}

// --- CI box title position indicator tests ---

func TestRenderCIView_TitleNoPositionWithoutFindings(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI)
	run := testRunWithCI()
	run.Steps[5].Status = types.StepStatusRunning
	logs := []string{"monitoring CI for PR #42 (timeout: 4h)..."}

	out := stripANSI(renderCIView(run, run.Steps, "", logs, 80))
	lines := strings.Split(out, "\n")

	// Title should be just "CI" without any position indicator.
	titleLine := lines[0]
	if !strings.Contains(titleLine, "CI") {
		t.Error("expected CI in title")
	}
	if strings.Contains(titleLine, "(") {
		t.Errorf("expected no position indicator when no findings, got: %s", titleLine)
	}
}

func TestRenderCIView_LogTailDuringMonitoring(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI)
	run := testRunWithCI()
	run.Steps[5].Status = types.StepStatusRunning
	logs := []string{
		"monitoring CI for PR #42 (timeout: 4h)...",
		"polling CI status...",
		"all checks passing",
	}

	out := stripANSI(renderCIView(run, run.Steps, "", logs, 80))

	// Log tail lines should appear inside the CI box during monitoring.
	if !strings.Contains(out, "polling CI status") {
		t.Errorf("expected log tail line 'polling CI status' inside CI box, got:\n%s", out)
	}
	if !strings.Contains(out, "all checks passing") {
		t.Errorf("expected log tail line 'all checks passing' inside CI box, got:\n%s", out)
	}
}

func TestModel_View_NoStandaloneLogBoxDuringCI(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI)
	run := testRunWithCI()
	m := NewModel("/tmp/sock", nil, run)
	m.steps = run.Steps
	m.steps[5].Status = types.StepStatusRunning
	m.logs = []string{"monitoring CI for PR #42", "polling CI", "checks passing"}
	m.width = 80

	view := stripANSI(m.View())

	// The standalone Log box should NOT appear when CI is active.
	hasStandaloneLogBox := false
	for _, line := range strings.Split(view, "\n") {
		if strings.Contains(line, "╭") && strings.Contains(line, "Log") && !strings.Contains(line, "CI") {
			hasStandaloneLogBox = true
		}
	}
	if hasStandaloneLogBox {
		t.Error("expected no standalone Log box when CI is active - logs should be inside CI box")
	}

	// But log content should still be visible (inside CI box).
	if !strings.Contains(view, "checks passing") {
		t.Error("expected log content to appear inside CI box")
	}
}

// --- CI adaptive log tail ---

func TestRenderCIView_LogTailFillsAvailableHeight(t *testing.T) {
	// Log tail should dynamically fill available height, not use hardcoded line counts.
	lipgloss.SetColorProfile(termenv.ANSI)
	run := testRunWithCI()
	run.Steps[5].Status = types.StepStatusRunning

	manyLogs := []string{"monitoring CI for PR #42 (timeout: 4h)..."}
	for i := 1; i <= 30; i++ {
		manyLogs = append(manyLogs, fmt.Sprintf("log-line-%d", i))
	}

	// With height=20, more logs should show than with height=10.
	tall := stripANSI(renderCIViewWithSelection(run, run.Steps, "", manyLogs, 80, 20, 0, nil))
	short := stripANSI(renderCIViewWithSelection(run, run.Steps, "", manyLogs, 80, 10, 0, nil))

	countLogLines := func(s string) int {
		n := 0
		for _, line := range strings.Split(s, "\n") {
			if strings.Contains(line, "log-line-") {
				n++
			}
		}
		return n
	}

	tallCount := countLogLines(tall)
	shortCount := countLogLines(short)

	if tallCount <= shortCount {
		t.Errorf("expected more log lines with height=20 (%d) than height=10 (%d)", tallCount, shortCount)
	}
}

func TestRenderCIView_LogTailTinyStillShowsSome(t *testing.T) {
	// Even with very small height, at least 1 log line should show.
	lipgloss.SetColorProfile(termenv.ANSI)
	run := testRunWithCI()
	run.Steps[5].Status = types.StepStatusRunning
	logs := []string{
		"monitoring CI for PR #42 (timeout: 4h)...",
		"line1",
		"line2",
		"all checks passing",
	}

	out := stripANSI(renderCIViewWithSelection(run, run.Steps, "", logs, 80, 10, 0, nil))

	if !strings.Contains(out, "all checks passing") {
		t.Error("expected at least the last log line even in tiny terminal")
	}
}

func TestRenderCIView_ZeroHeightOmitsLogTail(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI)
	run := testRunWithCI()
	run.Steps[5].Status = types.StepStatusRunning
	logs := []string{
		"monitoring CI for PR #42 (timeout: 4h)...",
		"polling CI status...",
		"all checks passing",
	}

	out := stripANSI(renderCIViewWithSelection(run, run.Steps, "", logs, 80, 0, 0, nil))

	if !strings.Contains(out, "Monitoring CI checks") {
		t.Fatalf("expected CI status to remain visible, got:\n%s", out)
	}
	if strings.Contains(out, "polling CI status") || strings.Contains(out, "all checks passing") {
		t.Fatalf("expected zero-height CI view to omit log tail, got:\n%s", out)
	}
}

func TestModel_View_CIShortTerminalKeepsStatusPanel(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)

	run := testRunWithCI()
	run.Steps[5].Status = types.StepStatusRunning
	run.PRURL = ptr("https://github.com/kunchenguid/no-mistakes/pull/42")

	m := NewModel("/tmp/sock", nil, run)
	m.width = 80
	m.height = 17
	m.logs = []string{
		"line1",
		"line2",
		"all checks passing",
	}

	view := stripANSI(m.View())

	if got := lipgloss.Height(view); got > m.height {
		t.Fatalf("expected rendered view height <= terminal height (%d), got %d\n%s", m.height, got, view)
	}
	if !strings.Contains(view, "CI") {
		t.Fatalf("expected CI section to remain visible in short terminal\n%s", view)
	}
	if !strings.Contains(view, "Monitoring CI checks") {
		t.Fatalf("expected CI status panel to remain visible in short terminal\n%s", view)
	}
}

func TestRenderCIView_LogTailScalesWithHeight(t *testing.T) {
	// Larger height should show more log lines.
	lipgloss.SetColorProfile(termenv.ANSI)
	run := testRunWithCI()
	run.Steps[5].Status = types.StepStatusRunning

	manyLogs := []string{"monitoring CI for PR #42 (timeout: 4h)..."}
	for i := 1; i <= 50; i++ {
		manyLogs = append(manyLogs, fmt.Sprintf("log-%d", i))
	}

	tall := stripANSI(renderCIViewWithSelection(run, run.Steps, "", manyLogs, 80, 40, 0, nil))
	compact := stripANSI(renderCIViewWithSelection(run, run.Steps, "", manyLogs, 80, 15, 0, nil))

	countLogLines := func(s string) int {
		n := 0
		for _, line := range strings.Split(s, "\n") {
			if strings.Contains(line, "log-") {
				n++
			}
		}
		return n
	}

	tallCount := countLogLines(tall)
	compactCount := countLogLines(compact)

	if tallCount <= compactCount {
		t.Errorf("expected more log lines at height=40 (%d) than height=15 (%d)", tallCount, compactCount)
	}
}

// --- Action Bar placement per DESIGN.md ---
// DESIGN.md: Action bar "Sits below the pipeline box, above findings/diff"

package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/muesli/termenv"
)

func TestRenderPipelineView_WrappedInBox(t *testing.T) {
	run := testRun()
	run.Steps[0].Status = types.StepStatusCompleted
	run.Steps[0].DurationMS = ptr(int64(1200))
	run.Steps[1].Status = types.StepStatusRunning

	out := stripANSI(renderPipelineView(run, run.Steps, 80, 0, 40))
	// Pipeline view should be wrapped in a box with rounded corners.
	if !strings.Contains(out, "╭") || !strings.Contains(out, "╯") {
		t.Error("expected pipeline view to be wrapped in a box with rounded corners")
	}
	// Title should be "Pipeline" in the top border.
	lines := strings.Split(out, "\n")
	if !strings.Contains(lines[0], "Pipeline") {
		t.Errorf("expected 'Pipeline' title in top border, got %q", lines[0])
	}
}

func TestModel_View_LogTailWrappedInBox(t *testing.T) {
	run := testRun()
	m := NewModel("/tmp/sock", nil, run)
	m.logs = []string{"running go test ./...", "PASS: TestFoo (0.3s)"}

	view := stripANSI(m.View())
	// Log section should have "Log" title in a box.
	if !strings.Contains(view, "Log") {
		t.Error("expected 'Log' section title")
	}
	// The log lines should be inside a box with borders.
	logSection := false
	for _, line := range strings.Split(view, "\n") {
		if strings.Contains(line, "Log") && strings.Contains(line, "╭") {
			logSection = true
		}
		if logSection && strings.Contains(line, "running go test") {
			if !strings.Contains(line, "│") {
				t.Errorf("expected log content inside box borders, got %q", line)
			}
			break
		}
	}
	if !logSection {
		t.Error("expected log section to have a boxed title")
	}
}

// --- Findings gutter alignment tests ---

func TestRenderFindings_GutterFixedWidth(t *testing.T) {
	// DESIGN.md Gutter System: cursor, checkbox, severity icon each get their
	// own fixed-width column. Content never shifts when selection state changes.
	//
	//   > [x] ● src/handler.go:42
	//            Missing error check on db.Close()
	//
	//     [x] ▲ src/config.go:17
	//            Unused import "fmt"

	raw := `{"findings":[
		{"id":"f1","severity":"error","file":"main.go","line":10,"description":"nil pointer"},
		{"id":"f2","severity":"warning","file":"util.go","description":"unused var"}
	],"summary":"2 issues"}`

	allSelected := map[string]bool{"f1": true, "f2": true}
	content, _ := renderFindingsWithSelection(raw, 80, 0, allSelected, 0)
	got := stripANSI(content)

	lines := strings.Split(got, "\n")

	// Find the first finding line (has a checkbox).
	var findingLines []string
	for _, line := range lines {
		if strings.Contains(line, "[x]") || strings.Contains(line, "[ ]") {
			findingLines = append(findingLines, line)
		}
	}

	if len(findingLines) < 2 {
		t.Fatalf("expected at least 2 finding lines, got %d in:\n%s", len(findingLines), got)
	}

	// The gutter should be: "> [x] ● " or "  [x] ● " (8 chars).
	// Cursor (1) + space (1) + checkbox (3) + space (1) + icon (1) + space (1) = 8
	for i, line := range findingLines {
		// Cursor column: position 0 should be ">" or " "
		if line[0] != '>' && line[0] != ' ' {
			t.Errorf("finding %d: expected cursor column at position 0, got %q", i, string(line[0]))
		}
		// Space at position 1
		if line[1] != ' ' {
			t.Errorf("finding %d: expected space at position 1, got %q", i, string(line[1]))
		}
		// Checkbox at positions 2-4: "[x]" or "[ ]"
		cb := line[2:5]
		if cb != "[x]" && cb != "[ ]" {
			t.Errorf("finding %d: expected checkbox at positions 2-4, got %q", i, cb)
		}
		// Space at position 5
		if line[5] != ' ' {
			t.Errorf("finding %d: expected space at position 5, got %q", i, string(line[5]))
		}
	}

	// First finding should have cursor ">"
	if findingLines[0][0] != '>' {
		t.Errorf("expected cursor on first finding, got %q", string(findingLines[0][0]))
	}
	// Second finding should have space (no cursor)
	if findingLines[1][0] != ' ' {
		t.Errorf("expected no cursor on second finding, got %q", string(findingLines[1][0]))
	}
}

func TestRenderFindings_DescriptionClearsGutter(t *testing.T) {
	// Description lines should be indented to clear the gutter (8 chars).
	raw := `{"findings":[{"id":"f1","severity":"error","file":"main.go","line":10,"description":"buffer overflow risk"}],"summary":"1 issue"}`

	selected := map[string]bool{"f1": true}
	content, _ := renderFindingsWithSelection(raw, 80, 0, selected, 0)
	got := stripANSI(content)

	lines := strings.Split(got, "\n")
	// Find the description line (follows the finding line with checkbox).
	var descLine string
	for i, line := range lines {
		if strings.Contains(line, "[x]") && i+1 < len(lines) {
			descLine = lines[i+1]
			break
		}
	}

	if descLine == "" {
		t.Fatalf("could not find description line in:\n%s", got)
	}

	// Description should be indented 8 chars to clear the gutter.
	if len(descLine) < 8 {
		t.Fatalf("description line too short: %q", descLine)
	}
	indent := descLine[:8]
	if strings.TrimSpace(indent) != "" {
		t.Errorf("expected 8-char indent before description, got %q", indent)
	}
	if !strings.Contains(descLine, "buffer overflow risk") {
		t.Errorf("expected description text, got %q", descLine)
	}
}

func TestModel_View_FindingsInBox(t *testing.T) {
	// When findings are shown, they should be wrapped in a "Findings" box.
	run := testRun()
	m := NewModel("/tmp/sock", nil, run)
	m.steps[0].Status = types.StepStatusAwaitingApproval
	m.stepFindings[types.StepReview] = `{"findings":[{"id":"f1","severity":"error","file":"app.go","line":5,"description":"buffer overflow"}],"summary":"1 issue"}`
	m.resetFindingSelection(types.StepReview)
	m.width = 80

	view := stripANSI(m.View())

	// Should have a "Findings" titled box.
	if !strings.Contains(view, "Findings") {
		t.Error("expected Findings title in boxed section")
	}

	// The findings box should have rounded border chars.
	hasTopBorder := false
	hasBottomBorder := false
	for _, line := range strings.Split(view, "\n") {
		if strings.Contains(line, "╭") && strings.Contains(line, "Findings") {
			hasTopBorder = true
		}
		if strings.Contains(line, "╰") && !strings.Contains(line, "Pipeline") && !strings.Contains(line, "Log") && !strings.Contains(line, "Diff") {
			hasBottomBorder = true
		}
	}
	if !hasTopBorder {
		t.Error("expected top border with Findings title")
	}
	if !hasBottomBorder {
		t.Error("expected bottom border for Findings box")
	}
}

func TestRenderCIView_WrappedInBox(t *testing.T) {
	run := testRunWithCI()
	run.Steps[5].Status = types.StepStatusRunning
	logs := []string{"monitoring CI for PR #42 (timeout: 4h)..."}

	out := stripANSI(renderCIView(run, run.Steps, "", logs, 80))

	// Should be wrapped in a box with "CI" title per DESIGN.md.
	lines := strings.Split(out, "\n")
	if len(lines) == 0 {
		t.Fatal("expected non-empty output")
	}
	if !strings.Contains(lines[0], "CI") {
		t.Errorf("expected 'CI' title in top border, got %q", lines[0])
	}
	if !strings.Contains(lines[0], "╭") {
		t.Error("expected rounded top-left corner in CI box")
	}
	// Should have rounded bottom corner.
	hasBottom := false
	for _, line := range lines {
		if strings.Contains(line, "╰") && strings.Contains(line, "╯") {
			hasBottom = true
			break
		}
	}
	if !hasBottom {
		t.Error("expected rounded bottom border in CI box")
	}
}

func TestRenderCIView_NoRedundantHeader(t *testing.T) {
	// The box title "CI" replaces the old "◉ CI Monitor" header.
	run := testRunWithCI()
	run.Steps[5].Status = types.StepStatusRunning
	logs := []string{"monitoring CI for PR #42 (timeout: 4h)..."}

	out := stripANSI(renderCIView(run, run.Steps, "", logs, 80))

	if strings.Contains(out, "CI Monitor") {
		t.Error("expected no redundant 'CI Monitor' header - box title handles it")
	}
}

func TestRenderCIView_ContentInsideBox(t *testing.T) {
	run := testRunWithCI()
	run.Steps[5].Status = types.StepStatusRunning
	logs := []string{"monitoring CI for PR #42 (timeout: 4h)..."}

	out := stripANSI(renderCIView(run, run.Steps, "", logs, 80))

	// State should be inside box borders.
	foundState := false
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "Monitoring") && strings.Contains(line, "│") {
			foundState = true
		}
	}
	if !foundState {
		t.Error("expected state indicator inside box borders")
	}
}

func TestModel_View_CIViewInBox(t *testing.T) {
	run := testRunWithCI()
	m := NewModel("/tmp/sock", nil, run)
	m.steps = run.Steps
	m.steps[5].Status = types.StepStatusRunning
	m.logs = []string{"monitoring CI for PR #42 (timeout: 4h)..."}
	m.width = 80

	view := stripANSI(m.View())

	// The CI section should be in a box with "CI" title.
	hasCIBox := false
	for _, line := range strings.Split(view, "\n") {
		if strings.Contains(line, "╭") && strings.Contains(line, "CI") && !strings.Contains(line, "Pipeline") {
			hasCIBox = true
			break
		}
	}
	if !hasCIBox {
		t.Error("expected 'CI' titled box in full model view")
	}
}

// Spacing Rules: 1 blank line between sections, never more than 1.
func TestModel_View_OneBlankLineBetweenSections(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	defer lipgloss.SetColorProfile(termenv.ANSI)

	findings := `{"findings":[{"severity":"warning","description":"test finding"}],"summary":"1 issue"}`
	run := &ipc.RunInfo{
		ID: "run-001", RepoID: "repo-001", Branch: "main", HeadSHA: "abc123", BaseSHA: "000000",
		Status: types.RunRunning,
		Steps: []ipc.StepResultInfo{
			{ID: "s1", StepName: types.StepReview, StepOrder: 1, Status: types.StepStatusAwaitingApproval, FindingsJSON: &findings},
			{ID: "s2", StepName: types.StepTest, StepOrder: 2, Status: types.StepStatusPending},
		},
	}
	m := NewModel("/tmp/sock", nil, run)
	m.width = 60
	m.logs = []string{"running test"}

	view := m.View()
	plain := stripANSI(view)

	// Between any two box bottom/top borders, there should be exactly 1 blank line.
	// That means: ╯ followed by newline, blank line, then ╭
	lines := strings.Split(plain, "\n")
	for i := 0; i < len(lines)-1; i++ {
		trimmed := strings.TrimSpace(lines[i])
		if strings.HasSuffix(trimmed, "╯") && i+1 < len(lines) {
			// Next box should be separated by 1 blank line
			nextContent := -1
			for j := i + 1; j < len(lines); j++ {
				if strings.TrimSpace(lines[j]) != "" {
					nextContent = j
					break
				}
			}
			if nextContent < 0 {
				continue // no more content, this is the last box
			}
			if strings.Contains(lines[nextContent], "╭") {
				blankCount := nextContent - i - 1
				if blankCount != 1 {
					t.Errorf("expected 1 blank line between sections at lines %d-%d, got %d blank lines\nbetween: %q\nand: %q",
						i, nextContent, blankCount, lines[i], lines[nextContent])
				}
			}
		}
	}
}

// Spacing between Pipeline and CI boxes should also have 1 blank line.
func TestModel_View_OneBlankLineBetweenPipelineAndCI(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	defer lipgloss.SetColorProfile(termenv.ANSI)

	run := &ipc.RunInfo{
		ID: "run-001", RepoID: "repo-001", Branch: "main", HeadSHA: "abc123", BaseSHA: "000000",
		Status: types.RunRunning,
		Steps: []ipc.StepResultInfo{
			{ID: "s1", StepName: types.StepReview, StepOrder: 1, Status: types.StepStatusCompleted},
			{ID: "s2", StepName: types.StepCI, StepOrder: 2, Status: types.StepStatusRunning},
		},
	}
	m := NewModel("/tmp/sock", nil, run)
	m.width = 60
	m.logs = []string{"monitoring CI for PR #42"}

	view := m.View()
	plain := stripANSI(view)
	lines := strings.Split(plain, "\n")

	for i := 0; i < len(lines)-1; i++ {
		trimmed := strings.TrimSpace(lines[i])
		if strings.HasSuffix(trimmed, "╯") {
			nextContent := -1
			for j := i + 1; j < len(lines); j++ {
				if strings.TrimSpace(lines[j]) != "" {
					nextContent = j
					break
				}
			}
			if nextContent < 0 {
				continue
			}
			if strings.Contains(lines[nextContent], "╭") {
				blankCount := nextContent - i - 1
				if blankCount != 1 {
					t.Errorf("expected 1 blank line between sections at lines %d-%d, got %d\nbetween: %q\nand: %q",
						i, nextContent, blankCount, lines[i], lines[nextContent])
				}
			}
		}
	}
}

// Diff stats should match DESIGN.md: "3 files  +42  -17" not "3 file(s) changed"
func TestRenderDiff_StatsPluralization(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	defer lipgloss.SetColorProfile(termenv.ANSI)

	// Multiple files should say "files"
	raw := "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1 +1 @@\n-old\n+new\ndiff --git a/b.go b/b.go\n--- a/b.go\n+++ b/b.go\n@@ -1 +1 @@\n-old2\n+new2\n"
	result := renderDiff(raw, 80, 20, 0, "", "")
	plain := stripANSI(result)
	if !strings.Contains(plain, "2 files") {
		t.Errorf("expected '2 files' (plural) for multiple files, got: %s", plain)
	}

	// Single file should say "file"
	raw2 := "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1 +1 @@\n-old\n+new\n"
	result2 := renderDiff(raw2, 80, 20, 0, "", "")
	plain2 := stripANSI(result2)
	if !strings.Contains(plain2, "1 file") {
		t.Errorf("expected '1 file' (singular) for one file, got: %s", plain2)
	}
	if strings.Contains(plain2, "1 files") {
		t.Errorf("expected '1 file' not '1 files' for one file, got: %s", plain2)
	}
}

func TestRenderDiff_StatsMatchDesign(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	defer lipgloss.SetColorProfile(termenv.ANSI)

	raw := "diff --git a/foo.go b/foo.go\nindex abc..def 100644\n--- a/foo.go\n+++ b/foo.go\n@@ -1,3 +1,4 @@\n context\n+added1\n+added2\n-removed\n"
	result := renderDiff(raw, 80, 20, 0, "", "")
	plain := stripANSI(result)

	// Should say "1 file" (singular) or "3 files" (plural), NOT "file(s) changed"
	if strings.Contains(plain, "file(s)") {
		t.Error("diff stats should not contain 'file(s)' - use 'file'/'files' per DESIGN.md")
	}
	if strings.Contains(plain, "changed") {
		t.Error("diff stats should not contain 'changed' - use compact format per DESIGN.md: '1 file  +2  -1'")
	}
	// Should contain the file count and +/- stats
	if !strings.Contains(plain, "1 file") {
		t.Errorf("expected '1 file' in diff stats, got: %s", plain)
	}
}

func TestRenderFindings_BlankLineBetweenItems(t *testing.T) {
	// DESIGN.md Gutter System shows a blank line between each finding item.
	raw := `{"findings":[
		{"id":"f1","severity":"error","file":"main.go","line":10,"description":"nil pointer"},
		{"id":"f2","severity":"warning","file":"util.go","line":5,"description":"unused var"}
	],"summary":"2 issues"}`

	got := stripANSI(renderFindings(raw, 80))
	lines := strings.Split(got, "\n")

	// Find the description lines by looking for 8-space indented content.
	// After each description line, there should be a blank line before the next finding
	// (except after the last finding).
	foundBlankBetween := false
	for i, line := range lines {
		if strings.TrimSpace(line) == "nil pointer" {
			// After description of first finding, next line should be blank,
			// then the second finding's gutter line follows.
			if i+1 < len(lines) && strings.TrimSpace(lines[i+1]) == "" {
				foundBlankBetween = true
			}
		}
	}
	if !foundBlankBetween {
		t.Errorf("expected blank line between finding items per DESIGN.md, got:\n%s", got)
	}
}

func TestRenderDiff_ScrollUpIndicator(t *testing.T) {
	// When scrolled down (offset > 0) with lines remaining below,
	// the bottom border should show an up arrow indicating lines above.
	var b strings.Builder
	b.WriteString("diff --git a/main.go b/main.go\n")
	b.WriteString("--- a/main.go\n")
	b.WriteString("+++ b/main.go\n")
	b.WriteString("@@ -1,20 +1,20 @@\n")
	for i := 0; i < 20; i++ {
		b.WriteString(fmt.Sprintf("+line %d\n", i))
	}

	// Scroll down 5 lines, view height 5 - should have lines above AND below.
	got := stripANSI(renderDiff(b.String(), 80, 5, 5, "", ""))
	lines := strings.Split(got, "\n")
	lastLine := ""
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			lastLine = lines[i]
			break
		}
	}
	if !strings.Contains(lastLine, "↑") {
		t.Errorf("expected ↑ in bottom border when scrolled down, got %q", lastLine)
	}
	if !strings.Contains(lastLine, "↓") {
		t.Errorf("expected ↓ in bottom border when lines remain below, got %q", lastLine)
	}
}

func TestRenderDiff_ScrollUpOnlyAtBottom(t *testing.T) {
	// When scrolled to the very end, should show ↑ but not ↓.
	var b strings.Builder
	b.WriteString("diff --git a/main.go b/main.go\n")
	b.WriteString("--- a/main.go\n")
	b.WriteString("+++ b/main.go\n")
	b.WriteString("@@ -1,5 +1,5 @@\n")
	for i := 0; i < 5; i++ {
		b.WriteString(fmt.Sprintf("+line %d\n", i))
	}

	// 9 total lines, view height 5, offset 4 - at the bottom.
	got := stripANSI(renderDiff(b.String(), 80, 5, 4, "", ""))
	lines := strings.Split(got, "\n")
	lastLine := ""
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			lastLine = lines[i]
			break
		}
	}
	if !strings.Contains(lastLine, "↑") {
		t.Errorf("expected ↑ in bottom border at end of diff, got %q", lastLine)
	}
	if strings.Contains(lastLine, "↓") {
		t.Errorf("expected no ↓ at end of diff, got %q", lastLine)
	}
}

// --- Color consistency tests per DESIGN.md Color Roles ---

func TestRenderPipelineView_StatusSuffixDim(t *testing.T) {
	// DESIGN.md Typography Scale: "Meta: Dim (bright black). Durations, file
	// references, counts, hints, footer." Status suffixes like "- awaiting approval"
	// are meta-level hints and must be styled dim (bright black).
	run := testRun()
	run.Steps[1].Status = types.StepStatusAwaitingApproval

	got := renderPipelineView(run, run.Steps, 80, 0, 40)

	// The suffix text "- awaiting approval" should be styled dim (contain ANSI codes).
	// When stripped, the text should be present; in the raw output, it should be wrapped
	// in dim styling, not appear as plain unstyled text.
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(ansiBrightBlack))
	styledSuffix := dimStyle.Render("- awaiting approval")

	if !strings.Contains(got, styledSuffix) {
		t.Errorf("expected status suffix '- awaiting approval' to be styled dim (bright black), but it was not found as styled text in output")
	}
}

func TestRenderPipelineView_FailedErrorDim(t *testing.T) {
	// Failed step error messages are also meta-level info and should be dim.
	run := testRun()
	errMsg := "lint failed"
	run.Steps[2].Status = types.StepStatusFailed
	run.Steps[2].Error = &errMsg

	got := renderPipelineView(run, run.Steps, 80, 0, 40)

	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(ansiBrightBlack))
	styledSuffix := dimStyle.Render("- " + errMsg)

	if !strings.Contains(got, styledSuffix) {
		t.Errorf("expected failed error suffix to be styled dim, but it was not found as styled text in output")
	}
}

func TestRenderFindings_CursorStyledBlue(t *testing.T) {
	// DESIGN.md Color Roles: "Primary action/focus: blue - interactive elements."
	// The cursor ">" indicating the focused finding should be styled blue.
	raw := `{"findings":[{"id":"f1","severity":"error","file":"main.go","line":10,"description":"nil pointer"}],"summary":"1 issue"}`
	selected := map[string]bool{"f1": true}

	got, _ := renderFindingsWithSelection(raw, 80, 0, selected, 0)

	blueStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(ansiBlue))
	styledCursor := blueStyle.Render(">")

	if !strings.Contains(got, styledCursor) {
		t.Errorf("expected cursor '>' to be styled blue per DESIGN.md Primary action/focus, but it was not found as styled text")
	}
}

func TestRenderFindings_CheckboxSelectedGreen(t *testing.T) {
	// DESIGN.md Color Roles: "Success: green - completed, additions."
	// Selected checkboxes "[x]" represent a successful/confirmed selection.
	raw := `{"findings":[{"id":"f1","severity":"error","file":"main.go","line":10,"description":"nil pointer"}],"summary":"1 issue"}`
	selected := map[string]bool{"f1": true}

	got, _ := renderFindingsWithSelection(raw, 80, 0, selected, 0)

	greenStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(ansiGreen))
	styledCheckbox := greenStyle.Render("[x]")

	if !strings.Contains(got, styledCheckbox) {
		t.Errorf("expected selected checkbox '[x]' to be styled green per DESIGN.md Success color, but it was not found as styled text")
	}
}

func TestRenderFindings_CheckboxUnselectedDim(t *testing.T) {
	// DESIGN.md Color Roles: "Muted/secondary: bright black."
	// Unselected checkboxes "[ ]" should be dim to de-emphasize.
	raw := `{"findings":[{"id":"f1","severity":"error","file":"main.go","line":10,"description":"nil pointer"}],"summary":"1 issue"}`
	selected := map[string]bool{} // nothing selected

	got, _ := renderFindingsWithSelection(raw, 80, 0, selected, 0)

	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(ansiBrightBlack))
	styledCheckbox := dimStyle.Render("[ ]")

	if !strings.Contains(got, styledCheckbox) {
		t.Errorf("expected unselected checkbox '[ ]' to be styled dim (bright black) per DESIGN.md Muted color, but it was not found as styled text")
	}
}

func TestNewModel_PopulatesStepFindingsFromInitialSteps_DisplaysOnView(t *testing.T) {
	findings := `{"findings":[{"severity":"warning","description":"stale finding from re-attach"}],"summary":"1 issue"}`
	run := &ipc.RunInfo{
		ID:      "run-001",
		RepoID:  "repo-001",
		Branch:  "feature/foo",
		HeadSHA: "abc123",
		BaseSHA: "000000",
		Status:  types.RunRunning,
		Steps: []ipc.StepResultInfo{
			{ID: "s1", StepName: types.StepReview, StepOrder: 1, Status: types.StepStatusAwaitingApproval, FindingsJSON: &findings},
			{ID: "s2", StepName: types.StepTest, StepOrder: 2, Status: types.StepStatusPending},
		},
	}

	m := NewModel("/tmp/sock", nil, run)
	view := m.View()

	// The findings from the initial steps should be visible in the view.
	if !strings.Contains(view, "stale finding from re-attach") {
		t.Error("expected findings from initial step to appear in view on re-attach")
	}
}

// --- Iteration 8: Footer visibility during approval + log line coloring ---

func TestFooter_ShowsDetachDuringApproval(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	run := testRun()
	run.Steps[0].Status = types.StepStatusCompleted
	run.Steps[1].Status = types.StepStatusAwaitingApproval

	m := NewModel("/tmp/sock", nil, run)
	m.width = 80
	view := m.View()
	plain := stripANSI(view)

	// Footer should show "q detach" even when a step is awaiting approval.
	if !strings.Contains(plain, "q detach") {
		t.Errorf("expected 'q detach' footer during approval state, got:\n%s", plain)
	}
}

func TestLogTail_PassLinesStyledGreen(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI)
	run := testRun()
	run.Steps[0].Status = types.StepStatusRunning

	m := NewModel("/tmp/sock", nil, run)
	m.width = 80
	m.logs = []string{"running go test ./...", "PASS: TestFoo (0.3s)"}
	view := m.View()

	// PASS lines should be styled green (ANSI color 2), not just dim.
	greenStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(ansiGreen))
	greenPass := greenStyle.Render("PASS: TestFoo (0.3s)")
	if !strings.Contains(view, greenPass) {
		t.Error("expected PASS log line to be styled green, not dim")
	}
}

func TestLogTail_FailLinesStyledRed(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI)
	run := testRun()
	run.Steps[0].Status = types.StepStatusRunning

	m := NewModel("/tmp/sock", nil, run)
	m.width = 80
	m.logs = []string{"running go test ./...", "FAIL: TestBar (0.1s)"}
	view := m.View()

	// FAIL lines should be styled red (ANSI color 1), not just dim.
	redStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(ansiRed))
	redFail := redStyle.Render("FAIL: TestBar (0.1s)")
	if !strings.Contains(view, redFail) {
		t.Error("expected FAIL log line to be styled red, not dim")
	}
}

func TestLogTail_RegularLineStaysDim(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI)
	run := testRun()
	run.Steps[0].Status = types.StepStatusRunning

	m := NewModel("/tmp/sock", nil, run)
	m.width = 80
	m.logs = []string{"running go test ./..."}
	view := m.View()

	// Regular log lines should remain dim (bright black).
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(ansiBrightBlack))
	dimLine := dimStyle.Render("running go test ./...")
	if !strings.Contains(view, dimLine) {
		t.Error("expected regular log line to remain dim-styled")
	}
}

func TestFindingsBoxTitle_ShowsSeverityCounts(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI)
	run := testRun()
	run.Steps[0].Status = types.StepStatusCompleted
	run.Steps[1].Status = types.StepStatusAwaitingApproval

	findingsJSON := `{"summary":"test issues","items":[{"id":"f1","severity":"error","file":"foo.go","line":1,"description":"bad thing"}]}`
	m := NewModel("/tmp/sock", nil, run)
	m.width = 80
	m.stepFindings[types.StepTest] = findingsJSON
	m.resetFindingSelection(types.StepTest)
	view := m.View()
	plain := stripANSI(view)

	// The findings box title should show severity counts, e.g. "Findings - E 1".
	if !strings.Contains(plain, "Findings - E 1") {
		t.Errorf("expected findings box title with severity counts 'Findings - E 1', got:\n%s", plain)
	}
}

func TestDiffBoxTitle_ReviewStep(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI)
	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval

	m := NewModel("/tmp/sock", nil, run)
	m.width = 80
	m.height = 40
	m.stepDiffs[types.StepReview] = "diff --git a/bar.go b/bar.go\n--- a/bar.go\n+++ b/bar.go\n@@ -1 +1 @@\n-old\n+new\n"
	m.showDiff = true
	view := m.View()
	plain := stripANSI(view)

	// Should say "Diff - Review" for the review step.
	if !strings.Contains(plain, "Diff - Review") {
		t.Errorf("expected 'Diff - Review' in box title, got:\n%s", plain)
	}
}

// --- Findings viewport scrolling tests ---

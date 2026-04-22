package tui

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/muesli/termenv"
)

func TestStaleShowDiff_ResetWhenNewFindingsArrive(t *testing.T) {
	// Bug: if user was viewing diff for step A, then step B arrives with
	// findings but no diff data, showDiff stays true from step A.
	// This causes View() to show neither diff nor findings - a blank state.
	// Fix: reset showDiff when new findings arrive via EventStepCompleted.
	configureTUIColors()
	run := testRun()
	m := NewModel("/tmp/sock", nil, run)
	m.width = 80
	m.height = 50

	// Simulate: Review step has findings + diff, user toggled showDiff on.
	m.steps[0].Status = types.StepStatusAwaitingApproval
	m.stepFindings[types.StepReview] = `{"items":[{"id":"f1","severity":"error","file":"a.go","line":1,"description":"bad"}]}`
	m.stepDiffs[types.StepReview] = "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1 +1 @@\n-old\n+new\n"
	m.showDiff = true

	// Now: user approves Review, Test step completes with findings but NO diff.
	m.steps[0].Status = types.StepStatusCompleted
	m.steps[1].Status = types.StepStatusAwaitingApproval
	findingsJSON := `{"items":[{"id":"f2","severity":"warning","file":"b.go","line":5,"description":"unused var"}]}`
	status := string(types.StepStatusAwaitingApproval)
	stepName := types.StepTest
	m.applyEvent(ipc.Event{
		Type:     ipc.EventStepCompleted,
		StepName: &stepName,
		Status:   &status,
		Findings: &findingsJSON,
		// No Diff field - this step has no diff data.
	})

	if m.showDiff {
		t.Error("showDiff should be reset to false when new findings arrive without diff data")
	}

	// Verify the findings are actually visible in the View.
	view := stripANSI(m.View())
	if !strings.Contains(view, "unused var") {
		t.Errorf("expected Test findings to be visible in view, got:\n%s", view)
	}
}

func TestStaleShowDiff_FindingsVisibleAfterStepTransition(t *testing.T) {
	// Even when showDiff was true from a previous step, the new step's
	// findings should be shown (not hidden by stale diff state).
	configureTUIColors()
	run := testRun()
	m := NewModel("/tmp/sock", nil, run)
	m.width = 80
	m.height = 50

	// Setup: user was viewing diff for Review.
	m.steps[0].Status = types.StepStatusCompleted
	m.steps[1].Status = types.StepStatusAwaitingApproval
	m.stepFindings[types.StepTest] = `{"summary":"Test issues","items":[{"id":"t1","severity":"error","file":"test.go","line":10,"description":"missing assertion"}]}`
	m.resetFindingSelection(types.StepTest)
	m.showDiff = true // stale from previous step

	// Apply the event that should reset showDiff.
	findingsJSON := m.stepFindings[types.StepTest]
	status := string(types.StepStatusAwaitingApproval)
	stepName := types.StepTest
	m.applyEvent(ipc.Event{
		Type:     ipc.EventStepCompleted,
		StepName: &stepName,
		Status:   &status,
		Findings: &findingsJSON,
	})

	view := stripANSI(m.View())
	if !strings.Contains(view, "Findings -") {
		t.Errorf("expected findings box to be visible, got:\n%s", view)
	}
}

func TestStaleShowDiff_DiffResetAlsoResetsOffset(t *testing.T) {
	// When showDiff is reset due to new findings, diffOffset should also reset
	// to prevent stale scroll position carrying over.
	configureTUIColors()
	run := testRun()
	m := NewModel("/tmp/sock", nil, run)
	m.showDiff = true
	m.diffOffset = 42 // stale offset from previous diff

	findingsJSON := `{"items":[{"id":"f1","severity":"info","file":"c.go","line":1,"description":"note"}]}`
	status := string(types.StepStatusAwaitingApproval)
	stepName := types.StepTest
	m.applyEvent(ipc.Event{
		Type:     ipc.EventStepCompleted,
		StepName: &stepName,
		Status:   &status,
		Findings: &findingsJSON,
	})

	if m.diffOffset != 0 {
		t.Errorf("diffOffset should be reset to 0 when new findings arrive, got %d", m.diffOffset)
	}
}

func TestApplyEvent_NewApprovalPayloadClearsPreviousOverrides(t *testing.T) {
	run := testRun()
	m := NewModel("", nil, run)
	m.stepFindings[types.StepReview] = `{"findings":[{"id":"review-1","severity":"error","description":"old","action":"auto-fix"}],"summary":"1"}`
	m.findingInstructions[types.StepReview] = map[string]string{"review-1": "old note"}
	m.addedFindings[types.StepReview] = []types.Finding{{ID: "user-1", Severity: "info", Description: "old added", Source: types.FindingSourceUser, Action: types.ActionAutoFix}}
	m.findingSelections[types.StepReview] = map[string]bool{"review-1": true, "user-1": true}

	findingsJSON := `{"findings":[{"id":"review-2","severity":"warning","description":"new","action":"auto-fix"}],"summary":"1"}`
	status := string(types.StepStatusAwaitingApproval)
	stepName := types.StepReview
	m.applyEvent(ipc.Event{Type: ipc.EventStepCompleted, StepName: &stepName, Status: &status, Findings: &findingsJSON})

	if _, ok := m.findingInstructions[types.StepReview]; ok {
		t.Fatal("expected stale finding instructions to be cleared")
	}
	if len(m.addedFindings[types.StepReview]) != 0 {
		t.Fatalf("expected stale user-added findings to be cleared, got %d", len(m.addedFindings[types.StepReview]))
	}
	selected := m.findingSelections[types.StepReview]
	if len(selected) != 1 || !selected["review-2"] {
		t.Fatalf("expected selection reset to only new finding, got %#v", selected)
	}
}

func TestActionBar_ShowsDiffWhenDiffDataExists(t *testing.T) {
	// The action bar SHOULD show 'd diff' when diff data exists for the current step.
	configureTUIColors()
	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval
	m := NewModel("", nil, run)
	m.width = 80
	m.height = 50
	m.stepFindings[types.StepReview] = `{"summary":"test","items":[{"id":"f1","severity":"error","file":"foo.go","line":1,"description":"bad"}]}`
	m.stepDiffs[types.StepReview] = "diff --git a/foo.go b/foo.go\n--- a/foo.go\n+++ b/foo.go\n@@ -1 +1 @@\n-old\n+new\n"
	m.resetFindingSelection(types.StepReview)

	view := stripANSI(m.View())
	if !strings.Contains(view, "d diff") {
		t.Errorf("expected 'd diff' in action bar when diff data exists, got:\n%s", view)
	}
}

func TestModel_HelpAutoDismiss_NavigationKey(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval
	m := NewModel("", nil, run)
	m.width = 80
	m.height = 40
	m.showHelp = true
	m.stepFindings[types.StepReview] = `{"summary":"test","items":[{"id":"f1","severity":"error","file":"foo.go","line":1,"description":"bad"},{"id":"f2","severity":"warning","file":"bar.go","line":5,"description":"ugly"}]}`
	m.resetFindingSelection(types.StepReview)

	// Press j (navigation) while help is open - should dismiss help.
	result, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	m = result.(Model)
	if m.showHelp {
		t.Fatal("expected help to auto-dismiss when pressing navigation key j")
	}
	// Navigation should still take effect - cursor should have moved.
	if m.findingCursor[types.StepReview] != 1 {
		t.Fatalf("expected cursor to move to 1 after j, got %d", m.findingCursor[types.StepReview])
	}
}

func TestModel_HelpAutoDismiss_ActionKey(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval
	m := NewModel("", nil, run)
	m.width = 80
	m.height = 40
	m.showHelp = true
	m.stepFindings[types.StepReview] = `{"summary":"test","items":[{"id":"f1","severity":"error","file":"foo.go","line":1,"description":"bad"}]}`
	m.resetFindingSelection(types.StepReview)

	// Press d (toggle diff/findings) while help is open - should dismiss help.
	m.stepDiffs[types.StepReview] = "diff --git a/foo.go b/foo.go\n--- a/foo.go\n+++ b/foo.go\n@@ -1 +1 @@\n-old\n+new\n"
	result, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'d'}})
	m = result.(Model)
	if m.showHelp {
		t.Fatal("expected help to auto-dismiss when pressing action key d")
	}
}

func TestModel_HelpAutoDismiss_SelectionKey(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval
	m := NewModel("", nil, run)
	m.width = 80
	m.height = 40
	m.showHelp = true
	m.stepFindings[types.StepReview] = `{"summary":"test","items":[{"id":"f1","severity":"error","file":"foo.go","line":1,"description":"bad"}]}`
	m.resetFindingSelection(types.StepReview)

	// Press space (toggle selection) while help is open - should dismiss help.
	result, _ := m.Update(tea.KeyMsg{Type: tea.KeySpace})
	m = result.(Model)
	if m.showHelp {
		t.Fatal("expected help to auto-dismiss when pressing selection key space")
	}
}

func TestErrorDisplay_WrappedInBox(t *testing.T) {
	run := testRun()
	m := NewModel("/tmp/sock", nil, run)
	m.width = 80
	m.err = &ipc.RPCError{Code: -1, Message: "connection refused"}
	view := m.View()
	plain := stripANSI(view)
	// Error should be in a rounded-border box.
	if !strings.Contains(plain, "╭") || !strings.Contains(plain, "╯") {
		t.Error("expected error to be wrapped in a box with rounded corners")
	}
	// The error message should appear inside the box borders.
	lines := strings.Split(plain, "\n")
	foundInside := false
	for _, line := range lines {
		if strings.Contains(line, "│") && strings.Contains(line, "connection refused") {
			foundInside = true
			break
		}
	}
	if !foundInside {
		t.Error("expected error message to appear inside box borders (between │ chars)")
	}
}

func TestErrorDisplay_HasErrorTitle(t *testing.T) {
	run := testRun()
	m := NewModel("/tmp/sock", nil, run)
	m.width = 80
	m.err = &ipc.RPCError{Code: -1, Message: "subscribe failed"}
	view := m.View()
	plain := stripANSI(view)
	// The box should have "Error" in the top border line.
	if !strings.Contains(plain, "Error") {
		t.Error("expected 'Error' title in the box top border")
	}
	// Specifically, "Error" should be on the same line as the top-left corner.
	lines := strings.Split(plain, "\n")
	found := false
	for _, line := range lines {
		if strings.Contains(line, "╭") && strings.Contains(line, "Error") {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected 'Error' title in top border line with ╭")
	}
}

func TestErrorDisplay_RedStyledMessage(t *testing.T) {
	run := testRun()
	m := NewModel("/tmp/sock", nil, run)
	m.width = 80
	m.err = &ipc.RPCError{Code: -1, Message: "event stream closed"}
	view := m.View()
	// Verify the error message content is styled red inside the box.
	redStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(ansiRed))
	// The message text itself should be red-styled inside the error box.
	styledMsg := redStyle.Render("event stream closed")
	if !strings.Contains(view, styledMsg) {
		t.Error("expected error message text to be styled red inside the error box")
	}
}

func TestModel_ApplyEvent_ClearsTransientErrorOnNextStateEvent(t *testing.T) {
	run := testRun()
	m := NewModel("/tmp/sock", nil, run)
	m.err = &ipc.RPCError{Code: -1, Message: "no step awaiting approval"}

	status := string(types.StepStatusFixReview)
	stepName := types.StepReview
	m.applyEvent(ipc.Event{
		Type:     ipc.EventStepCompleted,
		StepName: &stepName,
		Status:   &status,
	})

	if m.err != nil {
		t.Fatalf("expected transient error to clear on next state event, got %v", m.err)
	}
}

// --- Pipeline content truncation tests ---

func TestPipelineView_LongErrorTruncated(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	run := testRun()
	longError := strings.Repeat("x", 200)
	run.Steps[1].Status = types.StepStatusFailed
	run.Steps[1].Error = &longError

	boxWidth := 80
	result := renderPipelineView(run, run.Steps, boxWidth, 0, 40)
	plain := stripANSI(result)

	// No line in the pipeline box should exceed the box width.
	for _, line := range strings.Split(plain, "\n") {
		if line == "" {
			continue
		}
		w := lipgloss.Width(line)
		if w > boxWidth {
			t.Errorf("pipeline line exceeds box width (%d > %d): %s", w, boxWidth, line)
		}
	}

	// The full long error should NOT appear - it should be truncated.
	if strings.Contains(plain, longError) {
		t.Error("expected long error message to be truncated to fit box content width")
	}
}

func TestPipelineView_LongBranchTruncated(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	longBranch := "feature/" + strings.Repeat("very-long-name-", 10)
	run := testRun()
	run.Branch = longBranch

	boxWidth := 80
	result := renderPipelineView(run, run.Steps, boxWidth, 0, 40)
	plain := stripANSI(result)

	// No line should exceed the box width.
	for _, line := range strings.Split(plain, "\n") {
		if line == "" {
			continue
		}
		w := lipgloss.Width(line)
		if w > boxWidth {
			t.Errorf("pipeline line exceeds box width (%d > %d): %s", w, boxWidth, line)
		}
	}

	// The full long branch should NOT appear.
	if strings.Contains(plain, longBranch) {
		t.Error("expected long branch name to be truncated to fit box content width")
	}
}

func TestPipelineView_ShortErrorNotTruncated(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	run := testRun()
	shortError := "exit code 1"
	run.Steps[1].Status = types.StepStatusFailed
	run.Steps[1].Error = &shortError

	result := renderPipelineView(run, run.Steps, 80, 0, 40)
	plain := stripANSI(result)

	// Short error should appear in full.
	if !strings.Contains(plain, shortError) {
		t.Error("expected short error message to appear in full")
	}
}

func TestRenderCIView_ShowsShortPRContextInPanel(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	run := testRunWithCI()
	longURL := "https://github.com/some-very-long-organization-name/some-very-long-repository-name-that-goes-on-and-on/pull/12345"
	run.PRURL = &longURL
	run.Steps[5].Status = types.StepStatusRunning

	result := stripANSI(renderCIView(run, run.Steps, "", nil, 80))

	if !strings.Contains(result, "PR #12345") {
		t.Fatalf("expected CI panel to show short PR context, got: %s", result)
	}
	if strings.Contains(result, longURL) {
		t.Fatal("expected CI panel to avoid rendering the full PR URL")
	}
}

func TestRenderCIView_LongLastEventTruncated(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	run := testRunWithCI()
	run.Steps[5].Status = types.StepStatusRunning
	longEvent := "monitoring CI for PR #42 with a very long description " + strings.Repeat("z", 200)
	logs := []string{longEvent}

	result := stripANSI(renderCIView(run, run.Steps, "", logs, 80))

	// No line should exceed the box width (80).
	for _, line := range strings.Split(result, "\n") {
		if line == "" {
			continue
		}
		w := lipgloss.Width(line)
		if w > 80 {
			t.Errorf("CI LastEvent line exceeds box width (%d > 80): %s", w, line)
		}
	}

	// The full long event text should NOT appear in the output.
	if strings.Contains(result, strings.Repeat("z", 77)) {
		t.Error("expected long LastEvent to be truncated to fit box content width")
	}
}

func TestRenderCIView_ShowsPRContext(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	run := testRunWithCI()
	shortURL := "https://github.com/user/repo/pull/99"
	run.PRURL = &shortURL
	run.Steps[5].Status = types.StepStatusRunning

	result := stripANSI(renderCIView(run, run.Steps, "", nil, 60))
	if !strings.Contains(result, "PR #99") {
		t.Fatalf("expected CI panel to show PR context, got: %s", result)
	}
}

func TestModel_View_ErrorBox_LongMessageTruncated(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)

	m := Model{
		run:   testRun(),
		steps: testRun().Steps,
		width: 80,
		err:   fmt.Errorf("%s", strings.Repeat("x", 200)),
	}

	result := m.View()
	lines := strings.Split(result, "\n")

	// No line should exceed the box width of 80.
	for _, line := range lines {
		w := lipgloss.Width(line)
		if w > 80 {
			t.Errorf("error box line exceeds box width (%d > 80): %s", w, line)
		}
	}

	// The full 200-char error text should NOT appear in the output.
	if strings.Contains(result, strings.Repeat("x", 200)) {
		t.Error("expected long error message to be truncated to fit inside box")
	}
}

func TestModel_View_ErrorBox_ShortMessageNotTruncated(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)

	errText := "connection refused"
	m := Model{
		run:   testRun(),
		steps: testRun().Steps,
		width: 80,
		err:   fmt.Errorf("%s", errText),
	}

	result := stripANSI(m.View())

	// Short error message should appear in full.
	if !strings.Contains(result, errText) {
		t.Error("expected short error message to appear in full")
	}
}

func TestModel_View_ErrorBox_MultiLineMessageTruncated(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)

	longLine := strings.Repeat("y", 200)
	m := Model{
		run:   testRun(),
		steps: testRun().Steps,
		width: 80,
		err:   fmt.Errorf("line1\n%s", longLine),
	}

	result := m.View()
	lines := strings.Split(result, "\n")

	// No line should exceed the box width of 80.
	for _, line := range lines {
		w := lipgloss.Width(line)
		if w > 80 {
			t.Errorf("error box line exceeds box width (%d > 80): %s", w, line)
		}
	}
}

func TestRenderPipelineView_RunStatusColoredBlue(t *testing.T) {
	run := testRun()
	run.Status = types.RunRunning
	steps := []ipc.StepResultInfo{
		{StepName: types.StepReview, Status: types.StepStatusRunning},
		{StepName: types.StepTest, Status: types.StepStatusPending},
	}
	view := renderPipelineView(run, steps, 80, 0, 40)

	// The run status should stand out as the primary signal in the header.
	blueStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(ansiBlue))
	blueRunning := blueStyle.Render("running")
	if !strings.Contains(view, blueRunning) {
		t.Errorf("expected run status 'running' to be styled blue, got:\n%s", view)
	}
}

func TestRenderPipelineView_RunStatusColoredGreen(t *testing.T) {
	run := testRun()
	run.Status = types.RunCompleted
	steps := []ipc.StepResultInfo{
		{StepName: types.StepReview, Status: types.StepStatusCompleted},
		{StepName: types.StepTest, Status: types.StepStatusCompleted},
	}
	view := renderPipelineView(run, steps, 80, 0, 40)

	greenStyle := lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(ansiGreen))
	greenCompleted := greenStyle.Render("completed")
	if !strings.Contains(view, greenCompleted) {
		t.Errorf("expected run status 'completed' to be styled green, got:\n%s", view)
	}
}

func TestRenderPipelineView_BranchStyledDim(t *testing.T) {
	run := testRun()
	view := renderPipelineView(run, run.Steps, 80, 0, 40)

	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(ansiBrightBlack))
	dimBranch := dimStyle.Render(run.Branch)
	if !strings.Contains(view, dimBranch) {
		t.Errorf("expected branch name to be styled dim, got:\n%s", view)
	}
}

func TestRenderPipelineView_HidesStepProgress(t *testing.T) {
	run := testRun()
	run.Status = types.RunRunning
	steps := []ipc.StepResultInfo{
		{StepName: types.StepReview, Status: types.StepStatusCompleted},
		{StepName: types.StepTest, Status: types.StepStatusRunning},
		{StepName: types.StepLint, Status: types.StepStatusPending},
		{StepName: types.StepPush, Status: types.StepStatusPending},
		{StepName: types.StepPR, Status: types.StepStatusPending},
	}
	view := renderPipelineView(run, steps, 80, 0, 40)
	plain := stripANSI(view)

	if strings.Contains(plain, "2/5") {
		t.Errorf("expected step progress count to be hidden from pipeline header, got:\n%s", plain)
	}
}

func TestPipelineView_LongBranchKeepsStatusVisible(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	longBranch := "feature/" + strings.Repeat("very-long-name-", 10)
	run := testRun()
	run.Branch = longBranch

	result := stripANSI(renderPipelineView(run, run.Steps, 40, 0, 40))
	if !strings.Contains(result, "running") {
		t.Fatalf("expected running status to remain visible when branch is truncated, got:\n%s", result)
	}
	if strings.Contains(result, longBranch) {
		t.Fatal("expected long branch name to be truncated before it reaches the status")
	}
}

func TestRenderBoxWithFooter_FitsRequestedWidth(t *testing.T) {
	plain := stripANSI(renderBoxWithFooter("Findings", "content", 40, "↓ 1 more below (j/k)"))
	for _, line := range strings.Split(plain, "\n") {
		if line == "" {
			continue
		}
		if got := lipgloss.Width(line); got != 40 {
			t.Fatalf("expected every box line to be width 40, got %d for %q", got, line)
		}
	}
}

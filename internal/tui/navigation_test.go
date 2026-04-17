package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/muesli/termenv"
)

func TestModel_View_HelpOverlay_ShowsEscBackInDiffMode(t *testing.T) {
	// Help overlay in diff mode should show Esc as "back to findings".
	lipgloss.SetColorProfile(termenv.Ascii)
	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval
	m := NewModel("", nil, run)
	m.width = 80
	m.height = 40
	m.showHelp = true
	m.showDiff = true
	m.stepDiffs[types.StepReview] = "diff --git a/foo.go b/foo.go\n"
	m.stepFindings[types.StepReview] = `[{"id":"1","file":"foo.go","line":10,"description":"test","severity":"error"}]`

	output := stripANSI(m.View())

	if !strings.Contains(output, "esc") || !strings.Contains(output, "back") {
		t.Errorf("help overlay in diff mode should show 'esc' with 'back' description, got:\n%s", output)
	}
}

func TestModel_View_HelpOverlay_NoEscBackOutsideDiffMode(t *testing.T) {
	// Help overlay NOT in diff mode should NOT show the "back to findings" hint.
	lipgloss.SetColorProfile(termenv.Ascii)
	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval
	m := NewModel("", nil, run)
	m.width = 80
	m.height = 40
	m.showHelp = true
	m.showDiff = false

	output := stripANSI(m.View())

	// Should not contain "esc" + "back" combination.
	if strings.Contains(output, "back to findings") {
		t.Errorf("help overlay should NOT show 'back to findings' when not in diff mode, got:\n%s", output)
	}
}

// Severity count badges tests removed - counts are now in the box title, not body.

// --- Space toggle auto-advance tests ---

func TestSpaceToggle_AutoAdvancesToNextFinding(t *testing.T) {
	run := testRun()
	m := NewModel("/tmp/sock", nil, run)
	m.steps[0].Status = types.StepStatusAwaitingApproval
	m.stepFindings[types.StepReview] = `{"findings":[{"id":"f1","severity":"error","description":"first"},{"id":"f2","severity":"warning","description":"second"},{"id":"f3","severity":"info","description":"third"}],"summary":"3 issues"}`
	m.ensureFindingSelection(types.StepReview)

	// Cursor starts at 0.
	if m.findingCursor[types.StepReview] != 0 {
		t.Fatalf("cursor should start at 0, got %d", m.findingCursor[types.StepReview])
	}

	// Press space to toggle finding at cursor 0, then cursor should auto-advance to 1.
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeySpace, Runes: []rune(" ")})
	model := updated.(Model)

	if model.findingCursor[types.StepReview] != 1 {
		t.Fatalf("cursor should auto-advance to 1 after space, got %d", model.findingCursor[types.StepReview])
	}
}

func TestSpaceToggle_StaysOnLastFinding(t *testing.T) {
	run := testRun()
	m := NewModel("/tmp/sock", nil, run)
	m.steps[0].Status = types.StepStatusAwaitingApproval
	m.stepFindings[types.StepReview] = `{"findings":[{"id":"f1","severity":"error","description":"first"},{"id":"f2","severity":"warning","description":"second"}],"summary":"2 issues"}`
	m.ensureFindingSelection(types.StepReview)

	// Move cursor to last item (index 1).
	m.findingCursor[types.StepReview] = 1

	// Press space - cursor should stay at 1 (last item, can't advance).
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeySpace, Runes: []rune(" ")})
	model := updated.(Model)

	if model.findingCursor[types.StepReview] != 1 {
		t.Fatalf("cursor should stay at 1 (last item), got %d", model.findingCursor[types.StepReview])
	}
}

func TestSpaceToggle_TogglesOriginalNotAdvanced(t *testing.T) {
	run := testRun()
	m := NewModel("/tmp/sock", nil, run)
	m.steps[0].Status = types.StepStatusAwaitingApproval
	m.stepFindings[types.StepReview] = `{"findings":[{"id":"f1","severity":"error","description":"first"},{"id":"f2","severity":"warning","description":"second"},{"id":"f3","severity":"info","description":"third"}],"summary":"3 issues"}`
	m.ensureFindingSelection(types.StepReview)

	// All 3 are selected. Press space at cursor 0.
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeySpace, Runes: []rune(" ")})
	model := updated.(Model)

	// f1 (cursor was at 0) should be toggled off, f2 and f3 should remain selected.
	sel := model.findingSelections[types.StepReview]
	if sel["f1"] {
		t.Fatal("f1 should have been toggled off")
	}
	if !sel["f2"] {
		t.Fatal("f2 should still be selected (not toggled)")
	}
	if !sel["f3"] {
		t.Fatal("f3 should still be selected (not toggled)")
	}
}

func TestModel_View_HelpOverlay_HidesNavigationWhenNoAwaitingStep(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	run := testRun()
	// All steps pending - no step awaiting approval, no diff available.
	m := NewModel("", nil, run)
	m.width = 80
	m.height = 40
	m.showHelp = true

	view := m.View()
	plain := stripANSI(view)

	// Navigation keys should NOT be shown since they're no-ops without an awaiting step.
	if strings.Contains(plain, "j/k") {
		t.Errorf("help should hide j/k navigation when no step awaiting, got:\n%s", plain)
	}
	if strings.Contains(plain, "g/G") {
		t.Errorf("help should hide g/G when no step awaiting, got:\n%s", plain)
	}
	if strings.Contains(plain, "Ctrl+d/u") {
		t.Errorf("help should hide Ctrl+d/u when no step awaiting, got:\n%s", plain)
	}
	// "Navigation" section title should NOT appear.
	if strings.Contains(plain, "Navigation") {
		t.Errorf("help should hide Navigation section when no step awaiting, got:\n%s", plain)
	}
}

func TestModel_View_HelpOverlay_ShowsOnlyQAndHelpWhenNoActions(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	run := testRun()
	// Pipeline running, no step awaiting - minimal help content.
	m := NewModel("", nil, run)
	m.width = 80
	m.height = 40
	m.showHelp = true

	view := m.View()
	plain := stripANSI(view)

	// Should still show q and ? since those always work.
	if !strings.Contains(plain, "detach") {
		t.Errorf("help should show q detach even without actions, got:\n%s", plain)
	}
	if !strings.Contains(plain, "close help") {
		t.Errorf("help should show ? close help even without actions, got:\n%s", plain)
	}
	// Should be in a Help box.
	if !strings.Contains(plain, "Help") {
		t.Errorf("help should still be in a Help titled box, got:\n%s", plain)
	}
}

func TestModel_View_HelpOverlay_ShowsNavigationWhenAwaitingStep(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval
	m := NewModel("", nil, run)
	m.width = 80
	m.height = 40
	m.showHelp = true

	view := m.View()
	plain := stripANSI(view)

	// Navigation should be shown when a step is awaiting approval.
	if !strings.Contains(plain, "j/k") {
		t.Errorf("help should show j/k navigation when step is awaiting, got:\n%s", plain)
	}
	if !strings.Contains(plain, "Navigation") {
		t.Errorf("help should show Navigation section when step is awaiting, got:\n%s", plain)
	}
}

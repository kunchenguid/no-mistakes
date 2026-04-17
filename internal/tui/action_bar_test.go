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

func TestActionBar_BetweenPipelineAndFindings(t *testing.T) {
	// In the full model View(), the action bar should appear between the pipeline box
	// bottom border (╰) and the findings box top border (╭).
	configureTUIColors()
	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval
	m := NewModel("", nil, run)
	m.width = 80
	m.height = 50
	m.stepFindings[types.StepReview] = `{"summary":"test","items":[{"id":"f1","severity":"error","file":"foo.go","line":1,"description":"bad"}]}`
	m.resetFindingSelection(types.StepReview)

	view := stripANSI(m.View())
	lines := strings.Split(view, "\n")

	// Find: pipeline box bottom border, action bar, findings box top border - in that order.
	pipelineEnd := -1
	actionBarLine := -1
	findingsStart := -1
	for i, line := range lines {
		if strings.Contains(line, "╰") && pipelineEnd == -1 {
			pipelineEnd = i
		}
		if strings.Contains(line, "a approve") && actionBarLine == -1 {
			actionBarLine = i
		}
		if strings.Contains(line, "╭") && strings.Contains(line, "Findings") {
			findingsStart = i
		}
	}

	if pipelineEnd == -1 {
		t.Fatal("could not find pipeline box bottom border")
	}
	if actionBarLine == -1 {
		t.Fatal("could not find action bar")
	}
	if findingsStart == -1 {
		t.Fatal("could not find findings box top border")
	}

	if actionBarLine <= pipelineEnd {
		t.Errorf("action bar (line %d) should be AFTER pipeline box bottom (line %d)", actionBarLine, pipelineEnd)
	}
	if actionBarLine >= findingsStart {
		t.Errorf("action bar (line %d) should be BEFORE findings box top (line %d)", actionBarLine, findingsStart)
	}
}

func TestActionBar_IncludesAwaitingLabel(t *testing.T) {
	// The action bar section outside the pipeline box should include the "X awaiting action:" label.
	configureTUIColors()
	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval
	m := NewModel("", nil, run)
	m.width = 80
	m.height = 50
	m.stepFindings[types.StepReview] = `{"summary":"test","items":[{"id":"f1","severity":"error","file":"foo.go","line":1,"description":"bad"}]}`
	m.resetFindingSelection(types.StepReview)

	view := stripANSI(m.View())

	// The "awaiting action:" label should appear outside the pipeline box.
	if !strings.Contains(view, "Review awaiting action") {
		t.Error("expected 'Review awaiting action' label in view")
	}
	if !strings.Contains(view, "a approve") {
		t.Error("expected approve action key in full view")
	}
	if !strings.Contains(view, "f fix") {
		t.Error("expected fix action in full view when findings are selected")
	}

	// It should NOT be inside the pipeline box.
	pipelineOut := stripANSI(renderPipelineView(run, run.Steps, 80, 0, 40))
	if strings.Contains(pipelineOut, "a approve") {
		t.Error("approve action should not be inside the pipeline box")
	}
	if strings.Contains(pipelineOut, "f fix") {
		t.Error("fix action should not be inside the pipeline box")
	}
	if strings.Contains(pipelineOut, "awaiting action") {
		t.Error("'awaiting action' label should not be inside the pipeline box")
	}
}

func TestActionBar_FixReviewPromptInView(t *testing.T) {
	// Integration test: full model view shows "Review - review fix:" prompt (not "awaiting action").
	configureTUIColors()
	run := testRun()
	run.Steps[0].Status = types.StepStatusFixReview
	m := NewModel("", nil, run)
	m.width = 80
	m.height = 50
	m.stepFindings[types.StepReview] = `{"summary":"test","items":[{"id":"f1","severity":"error","file":"foo.go","line":1,"description":"bad"}]}`
	m.resetFindingSelection(types.StepReview)

	view := stripANSI(m.View())
	// The action bar prompt should say "Review - review fix:" not "Review awaiting action:".
	if !strings.Contains(view, "Review - review fix:") {
		t.Errorf("expected 'Review - review fix:' in full view for FixReview step, got:\n%s", view)
	}
	if strings.Contains(view, "Review awaiting action") {
		t.Errorf("did not expect awaiting-action prompt in full view for FixReview step, got:\n%s", view)
	}
}

// --- Run Outcome Banner Tests ---

func TestOutcomeBanner_SuccessShowsCheckmark(t *testing.T) {
	run := testRun()
	run.Status = types.RunCompleted
	steps := []ipc.StepResultInfo{
		{StepName: types.StepReview, Status: types.StepStatusCompleted},
		{StepName: types.StepTest, Status: types.StepStatusCompleted},
		{StepName: types.StepPush, Status: types.StepStatusCompleted},
	}
	banner := stripANSI(renderOutcomeBanner(run, steps))
	if !strings.Contains(banner, "✓") {
		t.Error("expected ✓ in success banner")
	}
	if !strings.Contains(banner, "Pipeline passed") {
		t.Errorf("expected 'Pipeline passed' in banner, got: %s", banner)
	}
}

func TestOutcomeBanner_FailureShowsX(t *testing.T) {
	run := testRun()
	run.Status = types.RunFailed
	steps := []ipc.StepResultInfo{
		{StepName: types.StepReview, Status: types.StepStatusCompleted},
		{StepName: types.StepTest, Status: types.StepStatusFailed, Error: ptr("exit code 1")},
		{StepName: types.StepLint, Status: types.StepStatusPending},
	}
	banner := stripANSI(renderOutcomeBanner(run, steps))
	if !strings.Contains(banner, "✗") {
		t.Error("expected ✗ in failure banner")
	}
	if !strings.Contains(banner, "Test failed") {
		t.Errorf("expected 'Test failed' in banner, got: %s", banner)
	}
}

func TestOutcomeBanner_EmptyWhenRunning(t *testing.T) {
	run := testRun()
	run.Status = types.RunRunning
	banner := renderOutcomeBanner(run, run.Steps)
	if banner != "" {
		t.Errorf("expected empty banner when running, got: %q", banner)
	}
}

func TestActionBar_DiffModeShowsFindings(t *testing.T) {
	// When viewing the diff, the 'd' key should say "findings" not "diff"
	// since pressing d will toggle back to findings view.
	configureTUIColors()
	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval
	m := NewModel("", nil, run)
	m.width = 80
	m.height = 50
	m.showDiff = true
	m.stepFindings[types.StepReview] = `{"summary":"test","items":[{"id":"f1","severity":"error","file":"foo.go","line":1,"description":"bad"}]}`
	m.stepDiffs[types.StepReview] = "diff --git a/foo.go b/foo.go\n--- a/foo.go\n+++ b/foo.go\n@@ -1 +1 @@\n-old\n+new\n"
	m.resetFindingSelection(types.StepReview)

	view := stripANSI(m.View())
	if !strings.Contains(view, "d findings") {
		t.Errorf("expected 'd findings' in action bar when viewing diff, got:\n%s", view)
	}
	if strings.Contains(view, "d diff") {
		t.Error("should NOT show 'd diff' when already viewing diff")
	}
}

func TestActionBar_FindingsModeShowsDiff(t *testing.T) {
	// When viewing findings (default), the 'd' key should say "diff".
	configureTUIColors()
	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval
	m := NewModel("", nil, run)
	m.width = 80
	m.height = 50
	m.showDiff = false
	m.stepFindings[types.StepReview] = `{"summary":"test","items":[{"id":"f1","severity":"error","file":"foo.go","line":1,"description":"bad"}]}`
	m.stepDiffs[types.StepReview] = "diff --git a/foo.go b/foo.go\n--- a/foo.go\n+++ b/foo.go\n@@ -1 +1 @@\n-old\n+new\n"
	m.resetFindingSelection(types.StepReview)

	view := stripANSI(m.View())
	if !strings.Contains(view, "d diff") {
		t.Errorf("expected 'd diff' in action bar when viewing findings, got:\n%s", view)
	}
}

func TestActionBar_HidesSelectionInDiffMode(t *testing.T) {
	// Selection actions (toggle/A/N) should be hidden when viewing diff
	// since those keys don't work in diff mode.
	configureTUIColors()
	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval
	m := NewModel("", nil, run)
	m.width = 80
	m.height = 50
	m.showDiff = true
	m.stepFindings[types.StepReview] = `{"summary":"test","items":[{"id":"f1","severity":"error","file":"foo.go","line":1,"description":"bad"}]}`
	m.stepDiffs[types.StepReview] = "diff --git a/foo.go b/foo.go\n--- a/foo.go\n+++ b/foo.go\n@@ -1 +1 @@\n-old\n+new\n"
	m.resetFindingSelection(types.StepReview)

	view := stripANSI(m.View())
	if strings.Contains(view, "toggle") {
		t.Error("selection action 'toggle' should NOT appear when viewing diff")
	}
	if strings.Contains(view, "A all") {
		t.Error("selection action 'A all' should NOT appear when viewing diff")
	}
	if strings.Contains(view, "N none") {
		t.Error("selection action 'N none' should NOT appear when viewing diff")
	}
}

func TestActionBar_ShowsNavHintsInDiffMode(t *testing.T) {
	// When viewing diff, the action bar should show n/p navigation hints
	// so users can discover finding-to-finding navigation without opening help.
	configureTUIColors()
	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval
	m := NewModel("", nil, run)
	m.width = 80
	m.height = 50
	m.showDiff = true
	m.stepFindings[types.StepReview] = `{"summary":"test","items":[{"id":"f1","severity":"error","file":"foo.go","line":1,"description":"bad"},{"id":"f2","severity":"warning","file":"bar.go","line":5,"description":"warn"}]}`
	m.stepDiffs[types.StepReview] = "diff --git a/foo.go b/foo.go\n--- a/foo.go\n+++ b/foo.go\n@@ -1 +1 @@\n-old\n+new\n"
	m.resetFindingSelection(types.StepReview)

	view := stripANSI(m.View())
	if !strings.Contains(view, "│") {
		t.Errorf("expected separator before diff navigation hints, got:\n%s", view)
	}
	if !strings.Contains(view, "n next") {
		t.Errorf("expected 'n next' hint in action bar when viewing diff, got:\n%s", view)
	}
	if !strings.Contains(view, "p prev") {
		t.Errorf("expected 'p prev' hint in action bar when viewing diff, got:\n%s", view)
	}
}

func TestActionBar_HidesNavHintsInFindingsMode(t *testing.T) {
	// When viewing findings (not diff), n/p hints should NOT appear
	// since j/k is the primary navigation in findings view.
	configureTUIColors()
	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval
	m := NewModel("", nil, run)
	m.width = 80
	m.height = 50
	m.showDiff = false
	m.stepFindings[types.StepReview] = `{"summary":"test","items":[{"id":"f1","severity":"error","file":"foo.go","line":1,"description":"bad"},{"id":"f2","severity":"warning","file":"bar.go","line":5,"description":"warn"}]}`
	m.stepDiffs[types.StepReview] = "diff --git a/foo.go b/foo.go\n--- a/foo.go\n+++ b/foo.go\n@@ -1 +1 @@\n-old\n+new\n"
	m.resetFindingSelection(types.StepReview)

	view := stripANSI(m.View())
	if strings.Contains(view, "n next") {
		t.Error("'n next' hint should NOT appear when viewing findings")
	}
	if strings.Contains(view, "p prev") {
		t.Error("'p prev' hint should NOT appear when viewing findings")
	}
}

func TestActionBar_FixShowsSelectionCount(t *testing.T) {
	// When findings are selected for fix, the action bar should show
	// the selection count like "f fix (3/5)" so users know what they're sending.
	configureTUIColors()
	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval
	m := NewModel("", nil, run)
	m.width = 80
	m.height = 50
	m.stepFindings[types.StepReview] = `{"summary":"test","items":[
		{"id":"f1","severity":"error","file":"a.go","line":1,"description":"one"},
		{"id":"f2","severity":"error","file":"b.go","line":2,"description":"two"},
		{"id":"f3","severity":"warning","file":"c.go","line":3,"description":"three"},
		{"id":"f4","severity":"warning","file":"d.go","line":4,"description":"four"},
		{"id":"f5","severity":"info","file":"e.go","line":5,"description":"five"}
	]}`
	m.resetFindingSelection(types.StepReview)
	// Deselect 2 findings, leaving 3 selected.
	delete(m.findingSelections[types.StepReview], "f2")
	delete(m.findingSelections[types.StepReview], "f4")

	view := stripANSI(m.View())
	if !strings.Contains(view, "f fix (3/5)") {
		t.Errorf("expected 'f fix (3/5)' in action bar, got:\n%s", view)
	}
}

func TestActionBar_FixAllSelectedNoCount(t *testing.T) {
	// When ALL findings are selected, show "f fix" without count since
	// the count adds no information (it's the default state).
	configureTUIColors()
	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval
	m := NewModel("", nil, run)
	m.width = 80
	m.height = 50
	m.stepFindings[types.StepReview] = `{"summary":"test","items":[
		{"id":"f1","severity":"error","file":"a.go","line":1,"description":"one"},
		{"id":"f2","severity":"error","file":"b.go","line":2,"description":"two"},
		{"id":"f3","severity":"warning","file":"c.go","line":3,"description":"three"}
	]}`
	m.resetFindingSelection(types.StepReview) // all 3 selected

	view := stripANSI(m.View())
	// Should have "f fix" but NOT "f fix (3/3)" or similar count
	if !strings.Contains(view, "f fix") {
		t.Errorf("expected 'f fix' in action bar, got:\n%s", view)
	}
	if strings.Contains(view, "f fix (") {
		t.Errorf("should NOT show count when all findings are selected, got:\n%s", view)
	}
}

func TestActionBar_FixCountUpdatesOnDeselect(t *testing.T) {
	// Toggling a finding off should update the count in the action bar.
	configureTUIColors()
	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval
	m := NewModel("", nil, run)
	m.width = 80
	m.height = 50
	m.stepFindings[types.StepReview] = `{"summary":"test","items":[
		{"id":"f1","severity":"error","file":"a.go","line":1,"description":"one"},
		{"id":"f2","severity":"error","file":"b.go","line":2,"description":"two"},
		{"id":"f3","severity":"warning","file":"c.go","line":3,"description":"three"}
	]}`
	m.resetFindingSelection(types.StepReview) // all 3 selected
	// Deselect one finding.
	delete(m.findingSelections[types.StepReview], "f1")

	view := stripANSI(m.View())
	if !strings.Contains(view, "f fix (2/3)") {
		t.Errorf("expected 'f fix (2/3)' after deselecting one finding, got:\n%s", view)
	}
}

func TestOutcomeBanner_InViewWhenDone(t *testing.T) {
	run := testRun()
	run.Status = types.RunCompleted
	run.Steps = []ipc.StepResultInfo{
		{StepName: types.StepReview, Status: types.StepStatusCompleted},
		{StepName: types.StepTest, Status: types.StepStatusCompleted},
	}
	m := NewModel("", nil, run)
	m.width = 80
	m.height = 40
	m.done = true
	view := stripANSI(m.View())
	if !strings.Contains(view, "Pipeline passed") {
		t.Errorf("expected 'Pipeline passed' in done view, got:\n%s", view)
	}
}

func TestModel_HandleKey_JumpToTopDiff(t *testing.T) {
	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval
	m := NewModel("/tmp/sock", nil, run)
	m.showDiff = true
	m.diffOffset = 15

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("g")})
	model := updated.(Model)
	if model.diffOffset != 0 {
		t.Errorf("expected diffOffset=0 after 'g', got %d", model.diffOffset)
	}
}

func TestModel_HandleKey_JumpToBottomDiff(t *testing.T) {
	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval
	m := NewModel("/tmp/sock", nil, run)
	m.showDiff = true
	m.diffOffset = 0

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("G")})
	model := updated.(Model)
	// G sets diffOffset to a large value; renderDiff will clamp it.
	if model.diffOffset == 0 {
		t.Error("expected diffOffset > 0 after 'G'")
	}
}

func TestModel_HandleKey_JumpToTopFindings(t *testing.T) {
	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval
	m := NewModel("/tmp/sock", nil, run)
	m.stepFindings[types.StepReview] = `{"findings":[{"id":"f1","severity":"error","description":"a"},{"id":"f2","severity":"warning","description":"b"},{"id":"f3","severity":"info","description":"c"},{"id":"f4","severity":"error","description":"d"},{"id":"f5","severity":"warning","description":"e"}]}`
	m.findingCursor[types.StepReview] = 4

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("g")})
	model := updated.(Model)
	if model.findingCursor[types.StepReview] != 0 {
		t.Errorf("expected findingCursor=0 after 'g', got %d", model.findingCursor[types.StepReview])
	}
}

func TestModel_HandleKey_JumpToBottomFindings(t *testing.T) {
	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval
	m := NewModel("/tmp/sock", nil, run)
	m.stepFindings[types.StepReview] = `{"findings":[{"id":"f1","severity":"error","description":"a"},{"id":"f2","severity":"warning","description":"b"},{"id":"f3","severity":"info","description":"c"},{"id":"f4","severity":"error","description":"d"},{"id":"f5","severity":"warning","description":"e"}]}`
	m.findingCursor[types.StepReview] = 0

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("G")})
	model := updated.(Model)
	if model.findingCursor[types.StepReview] != 4 {
		t.Errorf("expected findingCursor=4 after 'G', got %d", model.findingCursor[types.StepReview])
	}
}

func TestModel_HandleKey_HalfPageDownDiff(t *testing.T) {
	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval
	m := NewModel("/tmp/sock", nil, run)
	m.showDiff = true
	m.diffOffset = 0
	m.height = 40 // viewHeight = 40 - 15 = 25, half page = 12

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlD})
	model := updated.(Model)
	if model.diffOffset != 12 {
		t.Errorf("expected diffOffset=12 after ctrl+d with height=40, got %d", model.diffOffset)
	}
}

func TestModel_HandleKey_HalfPageUpDiff(t *testing.T) {
	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval
	m := NewModel("/tmp/sock", nil, run)
	m.showDiff = true
	m.diffOffset = 20
	m.height = 40 // half page = 12

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlU})
	model := updated.(Model)
	if model.diffOffset != 8 {
		t.Errorf("expected diffOffset=8 after ctrl+u from 20 with height=40, got %d", model.diffOffset)
	}
}

func TestModel_HandleKey_HalfPageDownFindings(t *testing.T) {
	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval
	m := NewModel("/tmp/sock", nil, run)
	// 10 findings so cursor can move meaningfully.
	items := make([]string, 10)
	for i := range items {
		items[i] = fmt.Sprintf(`{"id":"f%d","severity":"error","description":"finding %d"}`, i+1, i+1)
	}
	m.stepFindings[types.StepReview] = `{"findings":[` + strings.Join(items, ",") + `]}`
	m.findingCursor[types.StepReview] = 0
	m.height = 40 // half page for findings = (40 - 20) / 3 / 2 = ~3

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlD})
	model := updated.(Model)
	cursor := model.findingCursor[types.StepReview]
	// Half page = max(halfViewport, 3) where viewport = (40-20)/3 = 6, half = 3.
	if cursor != 3 {
		t.Errorf("expected findingCursor=3 after ctrl+d, got %d", cursor)
	}
}

func TestModel_HandleKey_HalfPageUpFindings(t *testing.T) {
	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval
	m := NewModel("/tmp/sock", nil, run)
	items := make([]string, 10)
	for i := range items {
		items[i] = fmt.Sprintf(`{"id":"f%d","severity":"error","description":"finding %d"}`, i+1, i+1)
	}
	m.stepFindings[types.StepReview] = `{"findings":[` + strings.Join(items, ",") + `]}`
	m.findingCursor[types.StepReview] = 8
	m.height = 40

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyCtrlU})
	model := updated.(Model)
	cursor := model.findingCursor[types.StepReview]
	if cursor != 5 {
		t.Errorf("expected findingCursor=5 after ctrl+u from 8, got %d", cursor)
	}
}

// --- Findings scroll indicator key hints ---

func TestRenderFindings_ScrollDownIncludesKeyHint(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI)
	raw := makeManyFindings(10)
	selected := map[string]bool{}
	for i := 1; i <= 10; i++ {
		selected[fmt.Sprintf("f%d", i)] = true
	}

	_, scrollFooter := renderFindingsWithSelection(raw, 80, 0, selected, 4)

	// Down indicator (in scrollFooter) should include (j/k) key hint.
	if !strings.Contains(scrollFooter, "more below (j/k)") {
		t.Errorf("expected scrollFooter with 'more below (j/k)' key hint, got: %q", scrollFooter)
	}
}

func TestRenderFindings_ScrollUpIncludesKeyHint(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI)
	raw := makeManyFindings(10)
	selected := map[string]bool{}
	for i := 1; i <= 10; i++ {
		selected[fmt.Sprintf("f%d", i)] = true
	}

	// Cursor at bottom - up indicator should be in scrollFooter with key hint.
	_, scrollFooter := renderFindingsWithSelection(raw, 80, 9, selected, 4)

	if !strings.Contains(scrollFooter, "above (j/k)") {
		t.Errorf("expected 'above (j/k)' in scrollFooter, got: %q", scrollFooter)
	}
}

func TestRenderFindings_BothIndicatorsIncludeKeyHints(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI)
	raw := makeManyFindings(10)
	selected := map[string]bool{}
	for i := 1; i <= 10; i++ {
		selected[fmt.Sprintf("f%d", i)] = true
	}

	// Cursor in the middle - both indicators should be in scrollFooter.
	_, scrollFooter := renderFindingsWithSelection(raw, 80, 5, selected, 4)

	// Both up and down indicators in scrollFooter.
	if !strings.Contains(scrollFooter, "above") {
		t.Errorf("expected 'above' in scrollFooter, got: %q", scrollFooter)
	}
	if !strings.Contains(scrollFooter, "more below (j/k)") {
		t.Errorf("expected 'more below (j/k)' in scrollFooter, got: %q", scrollFooter)
	}
}

func TestRenderFindings_NoScrollStillShowsJKHint(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI)
	// 2 findings that all fit on screen - no scrolling needed.
	raw := makeManyFindings(2)
	selected := map[string]bool{"f1": true, "f2": true}

	// Large maxVisible so everything fits without scrolling.
	_, scrollFooter := renderFindingsWithSelection(raw, 80, 0, selected, 10)

	if !strings.Contains(scrollFooter, "j/k") {
		t.Errorf("expected j/k hint even when all findings fit, got: %q", scrollFooter)
	}
}

func TestFindingsBox_ScrollDownInBottomBorder(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI)
	run := testRun()
	run.Steps[0].Status = types.StepStatusCompleted
	run.Steps[1].Status = types.StepStatusAwaitingApproval

	findingsJSON := makeManyFindings(10)
	m := NewModel("/tmp/sock", nil, run)
	m.width = 80
	m.height = 40
	m.stepFindings[types.StepTest] = findingsJSON
	m.resetFindingSelection(types.StepTest)
	m.findingCursor[types.StepTest] = 0

	view := m.View()
	plain := stripANSI(view)

	// The findings box bottom border should contain the down scroll indicator,
	// matching how diff embeds its scroll hint in the border.
	lines := strings.Split(plain, "\n")
	foundBorder := false
	for _, line := range lines {
		if strings.Contains(line, "╰") && strings.Contains(line, "more below") {
			foundBorder = true
			break
		}
	}
	if !foundBorder {
		t.Errorf("expected findings scroll indicator in bottom border (╰...more below...╯), got:\n%s", plain)
	}
}

func TestFindingsBox_ScrollUpInBorder(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI)
	run := testRun()
	run.Steps[0].Status = types.StepStatusCompleted
	run.Steps[1].Status = types.StepStatusAwaitingApproval

	findingsJSON := makeManyFindings(10)
	m := NewModel("/tmp/sock", nil, run)
	m.width = 80
	m.height = 40
	m.stepFindings[types.StepTest] = findingsJSON
	m.resetFindingSelection(types.StepTest)
	// Move to bottom so we get an up indicator.
	m.findingCursor[types.StepTest] = 9

	view := m.View()
	plain := stripANSI(view)

	// The up indicator should be in the bottom border (╰ line), not inline.
	lines := strings.Split(plain, "\n")
	foundUpInBorder := false
	for _, line := range lines {
		if strings.Contains(line, "╰") && strings.Contains(line, "above") {
			foundUpInBorder = true
			break
		}
	}
	if !foundUpInBorder {
		t.Errorf("expected up scroll indicator in bottom border, got:\n%s", plain)
	}
	// Should NOT appear inline (not in border).
	for _, line := range lines {
		if strings.Contains(line, "above") && !strings.Contains(line, "╰") {
			t.Errorf("up scroll indicator should not be inline, got line: %s", line)
		}
	}
}

func TestFindingsBox_BothScrollIndicatorsInBorder(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI)
	run := testRun()
	run.Steps[0].Status = types.StepStatusCompleted
	run.Steps[1].Status = types.StepStatusAwaitingApproval

	findingsJSON := makeManyFindings(10)
	m := NewModel("/tmp/sock", nil, run)
	m.width = 80
	m.height = 40
	m.stepFindings[types.StepTest] = findingsJSON
	m.resetFindingSelection(types.StepTest)
	// Cursor in middle: both up and down should be in the bottom border.
	m.findingCursor[types.StepTest] = 5

	view := m.View()
	plain := stripANSI(view)

	// Both indicators in the bottom border line.
	lines := strings.Split(plain, "\n")
	foundBorderLine := false
	for _, line := range lines {
		if strings.Contains(line, "╰") && strings.Contains(line, "above") && strings.Contains(line, "below") {
			foundBorderLine = true
			break
		}
	}
	if !foundBorderLine {
		t.Errorf("expected both scroll indicators in bottom border, got:\n%s", plain)
	}
}

// --- Findings scroll-up in footer tests ---

func TestRenderFindings_ScrollUpInFooter(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI)
	raw := makeManyFindings(10)
	selected := map[string]bool{}
	for i := 1; i <= 10; i++ {
		selected[fmt.Sprintf("f%d", i)] = true
	}

	// Cursor at bottom - scroll-up should be in scrollFooter, not inline content.
	content, scrollFooter := renderFindingsWithSelection(raw, 80, 9, selected, 4)
	plain := stripANSI(content)

	// Up indicator should NOT appear inline in the content.
	if strings.Contains(plain, "above") {
		t.Errorf("scroll-up indicator should not be inline, got:\n%s", plain)
	}
	// Up indicator should be in the scrollFooter string.
	if !strings.Contains(scrollFooter, "above") {
		t.Errorf("expected scroll-up in scrollFooter, got: %q", scrollFooter)
	}
}

func TestRenderFindings_BothIndicatorsInFooter(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI)
	raw := makeManyFindings(10)
	selected := map[string]bool{}
	for i := 1; i <= 10; i++ {
		selected[fmt.Sprintf("f%d", i)] = true
	}

	// Cursor in middle - both up and down should be in scrollFooter.
	content, scrollFooter := renderFindingsWithSelection(raw, 80, 5, selected, 4)
	plain := stripANSI(content)

	// No inline scroll indicators.
	if strings.Contains(plain, "above") {
		t.Errorf("scroll-up should not be inline, got:\n%s", plain)
	}
	// Footer should have both directions.
	if !strings.Contains(scrollFooter, "above") {
		t.Errorf("expected 'above' in scrollFooter, got: %q", scrollFooter)
	}
	if !strings.Contains(scrollFooter, "below") {
		t.Errorf("expected 'below' in scrollFooter, got: %q", scrollFooter)
	}
}

func TestRenderFindings_NoScrollUpInFooterAtTop(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI)
	raw := makeManyFindings(10)
	selected := map[string]bool{}
	for i := 1; i <= 10; i++ {
		selected[fmt.Sprintf("f%d", i)] = true
	}

	// Cursor at top - should have down but no up indicator.
	_, scrollFooter := renderFindingsWithSelection(raw, 80, 0, selected, 4)

	if strings.Contains(scrollFooter, "above") {
		t.Errorf("should not have 'above' when at top, got: %q", scrollFooter)
	}
	if !strings.Contains(scrollFooter, "below") {
		t.Errorf("expected 'below' when items exist below, got: %q", scrollFooter)
	}
}

// --- Help Overlay Tests ---

func TestModel_HelpToggle(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	run := testRun()
	m := NewModel("", nil, run)

	// Initially showHelp is false.
	if m.showHelp {
		t.Fatal("expected showHelp to be false initially")
	}

	// Press ? to toggle help on.
	result, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'?'}})
	m = result.(Model)
	if !m.showHelp {
		t.Fatal("expected showHelp to be true after pressing ?")
	}

	// Press ? again to toggle help off.
	result, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'?'}})
	m = result.(Model)
	if m.showHelp {
		t.Fatal("expected showHelp to be false after pressing ? again")
	}
}

func TestModel_View_HelpOverlay(t *testing.T) {
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

	// Should show navigation keys.
	if !strings.Contains(plain, "j/k") {
		t.Errorf("help overlay should show j/k navigation, got:\n%s", plain)
	}
	// Should show g/G keys.
	if !strings.Contains(plain, "g/G") {
		t.Errorf("help overlay should show g/G jump keys, got:\n%s", plain)
	}
	// Should show Ctrl+d/u keys.
	if !strings.Contains(plain, "Ctrl+d/u") {
		t.Errorf("help overlay should show Ctrl+d/u half-page keys, got:\n%s", plain)
	}
	// Should show action keys (aligned with padding).
	for _, key := range []string{"approve", "fix", "skip", "abort (press twice)"} {
		if !strings.Contains(plain, key) {
			t.Errorf("help overlay should show %q, got:\n%s", key, plain)
		}
	}
	// Should show toggle key.
	if !strings.Contains(plain, "diff/findings toggle") {
		t.Errorf("help overlay should show diff/findings toggle, got:\n%s", plain)
	}
	// Should show selection keys.
	if !strings.Contains(plain, "toggle") {
		t.Errorf("help overlay should show toggle selection, got:\n%s", plain)
	}
	// Should be in a box titled "Help".
	if !strings.Contains(plain, "Help") {
		t.Errorf("help overlay should be in a box titled Help, got:\n%s", plain)
	}
}

func TestModel_View_FooterShowsHelpHint(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	run := testRun()
	m := NewModel("", nil, run)
	m.width = 80
	m.height = 40

	view := m.View()
	plain := stripANSI(view)

	// Footer should include ? help hint.
	lines := strings.Split(plain, "\n")
	foundHelpHint := false
	for _, line := range lines {
		if strings.Contains(line, "?") && strings.Contains(line, "help") {
			foundHelpHint = true
			break
		}
	}
	if !foundHelpHint {
		t.Errorf("footer should show ? help hint, got:\n%s", plain)
	}
}

func TestModel_EscapeDismissesHelp(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	run := testRun()
	m := NewModel("", nil, run)
	m.showHelp = true

	// Press Escape to dismiss help.
	result, _ := m.Update(tea.KeyMsg{Type: tea.KeyEscape})
	m = result.(Model)
	if m.showHelp {
		t.Fatal("expected Escape to dismiss help overlay")
	}
}

func TestModel_View_HelpOverlay_HidesActionsWhenNoApproval(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	run := testRun()
	// All steps pending - no step awaiting approval.
	m := NewModel("", nil, run)
	m.width = 80
	m.height = 40
	m.showHelp = true

	view := m.View()
	plain := stripANSI(view)

	// Navigation should be hidden (no awaiting step means j/k etc. are no-ops).
	if strings.Contains(plain, "j/k") {
		t.Errorf("help should hide navigation keys without approval, got:\n%s", plain)
	}
	// Action keys should NOT be shown since no step is awaiting approval.
	if strings.Contains(plain, "approve") {
		t.Errorf("help should hide action keys when no step awaiting approval, got:\n%s", plain)
	}
	// Selection keys should NOT be shown.
	if strings.Contains(plain, "select all") {
		t.Errorf("help should hide selection keys when no step awaiting approval, got:\n%s", plain)
	}
}

func TestModel_View_HelpOverlay_HidesSelectionInDiffMode(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval
	m := NewModel("", nil, run)
	m.width = 80
	m.height = 40
	m.showHelp = true
	m.showDiff = true
	m.stepDiffs[types.StepReview] = "diff --git a/foo.go b/foo.go\n--- a/foo.go\n+++ b/foo.go\n@@ -1,3 +1,4 @@\n one\n+two\n three\n"

	view := m.View()
	plain := stripANSI(view)

	// Action keys should be shown (approval is active).
	if !strings.Contains(plain, "approve") {
		t.Errorf("help should show action keys during approval, got:\n%s", plain)
	}
	// Selection keys should NOT be shown in diff mode.
	if strings.Contains(plain, "select all") {
		t.Errorf("help should hide selection keys in diff mode, got:\n%s", plain)
	}
	// d toggle should still be shown.
	if !strings.Contains(plain, "diff/findings toggle") {
		t.Errorf("help should show d toggle in diff mode, got:\n%s", plain)
	}
}

func TestOutcomeBanner_SuccessShowsElapsedTime(t *testing.T) {
	run := testRun()
	run.Status = types.RunCompleted
	steps := []ipc.StepResultInfo{
		{StepName: types.StepReview, Status: types.StepStatusCompleted, DurationMS: ptr(int64(1200))},
		{StepName: types.StepTest, Status: types.StepStatusCompleted, DurationMS: ptr(int64(3400))},
		{StepName: types.StepPush, Status: types.StepStatusCompleted, DurationMS: ptr(int64(800))},
	}
	banner := stripANSI(renderOutcomeBanner(run, steps))
	// Total = 1200 + 3400 + 800 = 5400ms = 5.4s
	if !strings.Contains(banner, "5.4s") {
		t.Errorf("expected elapsed time '5.4s' in success banner, got: %s", banner)
	}
}

func TestOutcomeBanner_FailureShowsElapsedTime(t *testing.T) {
	run := testRun()
	run.Status = types.RunFailed
	steps := []ipc.StepResultInfo{
		{StepName: types.StepReview, Status: types.StepStatusCompleted, DurationMS: ptr(int64(2000))},
		{StepName: types.StepTest, Status: types.StepStatusFailed, DurationMS: ptr(int64(6500))},
		{StepName: types.StepLint, Status: types.StepStatusPending},
	}
	banner := stripANSI(renderOutcomeBanner(run, steps))
	// Total = 2000 + 6500 = 8500ms = 8.5s
	if !strings.Contains(banner, "8.5s") {
		t.Errorf("expected elapsed time '8.5s' in failure banner, got: %s", banner)
	}
}

// boxContentLine extracts the content between box border │ chars on a line.

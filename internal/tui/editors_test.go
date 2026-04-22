package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/muesli/termenv"
)

func keyMsg(s string) tea.KeyMsg {
	switch s {
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "ctrl+s":
		return tea.KeyMsg{Type: tea.KeyCtrlS}
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case "shift+tab":
		return tea.KeyMsg{Type: tea.KeyShiftTab}
	case "left":
		return tea.KeyMsg{Type: tea.KeyLeft}
	case "right":
		return tea.KeyMsg{Type: tea.KeyRight}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "space":
		return tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

func newAwaitingModel(t *testing.T, findings string) Model {
	t.Helper()
	lipgloss.SetColorProfile(termenv.Ascii)
	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval
	m := NewModel("", nil, run)
	m.width = 120
	m.height = 40
	if findings != "" {
		m.stepFindings[types.StepReview] = findings
		m.resetFindingSelection(types.StepReview)
	}
	return m
}

func TestEditInstruction_OpensEditorWithExistingNote(t *testing.T) {
	findings := `{"findings":[{"id":"review-1","severity":"error","file":"a.go","line":1,"description":"bug","action":"auto-fix"}],"summary":"1 issue"}`
	m := newAwaitingModel(t, findings)
	m.findingInstructions = map[types.StepName]map[string]string{
		types.StepReview: {"review-1": "focus on parser.go"},
	}

	next, _ := m.handleKey(keyMsg("e"))
	updated := next.(Model)
	if !updated.editorActive() {
		t.Fatal("expected editor to be active after pressing 'e'")
	}
	if updated.editor.kind != editorInstruction {
		t.Errorf("expected instruction editor, got %d", updated.editor.kind)
	}
	if updated.editor.findingID != "review-1" {
		t.Errorf("expected editor to target review-1, got %q", updated.editor.findingID)
	}
	if updated.editor.instruction.Value() != "focus on parser.go" {
		t.Errorf("expected existing instruction to be pre-filled, got %q", updated.editor.instruction.Value())
	}
}

func TestEditInstruction_EscCancelsWithoutSaving(t *testing.T) {
	findings := `{"findings":[{"id":"review-1","severity":"error","description":"bug","action":"auto-fix"}],"summary":"1"}`
	m := newAwaitingModel(t, findings)
	next, _ := m.handleKey(keyMsg("e"))
	m = next.(Model)
	m.editor.instruction.SetValue("draft note")
	next, _ = m.handleKey(keyMsg("esc"))
	m = next.(Model)
	if m.editorActive() {
		t.Fatal("expected editor to close on esc")
	}
	if got := m.findingInstructions[types.StepReview]["review-1"]; got != "" {
		t.Errorf("expected no saved instruction after cancel, got %q", got)
	}
}

func TestEditInstruction_CtrlSSavesInstruction(t *testing.T) {
	findings := `{"findings":[{"id":"review-1","severity":"error","description":"bug","action":"auto-fix"}],"summary":"1"}`
	m := newAwaitingModel(t, findings)
	next, _ := m.handleKey(keyMsg("e"))
	m = next.(Model)
	m.editor.instruction.SetValue("only touch handler.go")
	next, _ = m.handleKey(keyMsg("ctrl+s"))
	m = next.(Model)
	if m.editorActive() {
		t.Fatal("expected editor to close on save")
	}
	got := m.findingInstructions[types.StepReview]["review-1"]
	if got != "only touch handler.go" {
		t.Errorf("expected instruction saved, got %q", got)
	}
}

func TestEditInstruction_ClearingRemovesEntry(t *testing.T) {
	findings := `{"findings":[{"id":"review-1","severity":"error","description":"bug","action":"auto-fix"}],"summary":"1"}`
	m := newAwaitingModel(t, findings)
	m.findingInstructions = map[types.StepName]map[string]string{
		types.StepReview: {"review-1": "old note"},
	}
	next, _ := m.handleKey(keyMsg("e"))
	m = next.(Model)
	m.editor.instruction.SetValue("   ")
	next, _ = m.handleKey(keyMsg("ctrl+s"))
	m = next.(Model)
	if _, ok := m.findingInstructions[types.StepReview]; ok {
		t.Error("expected findingInstructions entry to be cleared")
	}
}

func TestAddFinding_OpensEditorEmpty(t *testing.T) {
	m := newAwaitingModel(t, `{"findings":[{"id":"review-1","severity":"error","description":"bug","action":"auto-fix"}],"summary":"1"}`)
	next, _ := m.handleKey(keyMsg("+"))
	m = next.(Model)
	if !m.editorActive() || m.editor.kind != editorAddFinding {
		t.Fatal("expected add-finding editor to be active")
	}
	if m.editor.addFocus != addFieldSeverity {
		t.Error("expected initial focus on severity field")
	}
}

func TestAddFinding_SaveAppendsUserFinding(t *testing.T) {
	m := newAwaitingModel(t, `{"findings":[{"id":"review-1","severity":"error","description":"bug","action":"auto-fix"}],"summary":"1"}`)
	next, _ := m.handleKey(keyMsg("+"))
	m = next.(Model)
	// Change severity to info (index 2).
	m.editor.addSeverityIdx = 2
	m.editor.addDesc.SetValue("also audit logger init path")
	m.editor.addInstr.SetValue("prefer slog")
	next, _ = m.handleKey(keyMsg("ctrl+s"))
	m = next.(Model)

	if m.editorActive() {
		t.Fatal("expected editor to close after save")
	}
	added := m.addedFindings[types.StepReview]
	if len(added) != 1 {
		t.Fatalf("expected 1 user-added finding, got %d", len(added))
	}
	if added[0].Severity != "info" {
		t.Errorf("expected severity=info, got %q", added[0].Severity)
	}
	if added[0].Description != "also audit logger init path" {
		t.Errorf("expected description saved, got %q", added[0].Description)
	}
	if added[0].UserInstructions != "prefer slog" {
		t.Errorf("expected instruction saved, got %q", added[0].UserInstructions)
	}
	if added[0].Source != types.FindingSourceUser {
		t.Errorf("expected source=user, got %q", added[0].Source)
	}
	if added[0].ID == "" || !strings.HasPrefix(added[0].ID, "user-") {
		t.Errorf("expected user-N ID, got %q", added[0].ID)
	}
	// User-added finding should be auto-selected so 'f' includes it.
	if !m.findingSelections[types.StepReview][added[0].ID] {
		t.Error("expected user-added finding to be auto-selected")
	}
}

func TestAddFinding_RequiresDescription(t *testing.T) {
	m := newAwaitingModel(t, `{"findings":[],"summary":"0"}`)
	next, _ := m.handleKey(keyMsg("+"))
	m = next.(Model)
	next, _ = m.handleKey(keyMsg("ctrl+s"))
	m = next.(Model)
	if !m.editorActive() {
		t.Fatal("expected editor to stay open without description")
	}
	if m.editor.errorMsg == "" {
		t.Error("expected validation error message")
	}
	if len(m.addedFindings[types.StepReview]) != 0 {
		t.Error("expected no finding appended when validation fails")
	}
}

func TestAddFinding_TabCyclesFocus(t *testing.T) {
	m := newAwaitingModel(t, `{"findings":[],"summary":"0"}`)
	next, _ := m.handleKey(keyMsg("+"))
	m = next.(Model)
	next, _ = m.handleKey(keyMsg("tab"))
	m = next.(Model)
	if m.editor.addFocus != addFieldFile {
		t.Errorf("expected focus on File after tab, got %d", m.editor.addFocus)
	}
	for i := 0; i < int(addFieldCount); i++ {
		next, _ = m.handleKey(keyMsg("tab"))
		m = next.(Model)
	}
	if m.editor.addFocus != addFieldFile {
		t.Errorf("expected focus to wrap around to File, got %d", m.editor.addFocus)
	}
}

func TestDeleteUserFinding_RemovesOnlyUserAuthored(t *testing.T) {
	findings := `{"findings":[{"id":"review-1","severity":"error","description":"agent bug","action":"auto-fix"}],"summary":"1"}`
	m := newAwaitingModel(t, findings)
	// Manually add a user finding.
	m.addedFindings[types.StepReview] = []types.Finding{
		{ID: "user-1", Severity: "info", Description: "user idea", Source: types.FindingSourceUser, Action: types.ActionAutoFix},
	}
	m.findingSelections[types.StepReview]["user-1"] = true
	m.findingCursor[types.StepReview] = 1 // cursor on user-added

	next, _ := m.handleKey(keyMsg("D"))
	m = next.(Model)

	if len(m.addedFindings[types.StepReview]) != 0 {
		t.Errorf("expected user finding removed, still have %d", len(m.addedFindings[types.StepReview]))
	}

	// Now try deleting the agent finding - D should be a no-op.
	m.findingCursor[types.StepReview] = 0
	next, _ = m.handleKey(keyMsg("D"))
	m = next.(Model)
	if len(m.findingItems(types.StepReview)) != 1 {
		t.Error("expected agent finding to be preserved when D pressed on it")
	}
}

func TestAddFinding_RendersEvenOnSmallTerminal(t *testing.T) {
	// Regression: the editor used to compete with findings/logs for the
	// content budget and get dropped on smaller terminals, making the UI
	// feel frozen. It must always render when active.
	m := newAwaitingModel(t, `{"findings":[{"id":"review-1","severity":"error","description":"bug","action":"auto-fix"}],"summary":"1"}`)
	m.height = 24
	next, _ := m.handleKey(keyMsg("+"))
	m = next.(Model)
	plain := stripANSI(m.View())
	if !strings.Contains(plain, "Add finding") {
		t.Errorf("expected Add finding editor to render, got:\n%s", plain)
	}
	if !strings.Contains(plain, "Severity") {
		t.Errorf("expected Severity label to render, got:\n%s", plain)
	}
}

func TestEditInstruction_RendersEvenOnSmallTerminal(t *testing.T) {
	m := newAwaitingModel(t, `{"findings":[{"id":"review-1","severity":"error","description":"bug","action":"auto-fix"}],"summary":"1"}`)
	m.height = 24
	next, _ := m.handleKey(keyMsg("e"))
	m = next.(Model)
	plain := stripANSI(m.View())
	if !strings.Contains(plain, "Instruction for review-1") {
		t.Errorf("expected Instruction editor to render, got:\n%s", plain)
	}
}

func TestActionBar_SelectionOrder_EditAddBeforeAllNone(t *testing.T) {
	findings := `{"findings":[{"id":"review-1","severity":"error","description":"bug","action":"auto-fix"}],"summary":"1"}`
	m := newAwaitingModel(t, findings)
	plain := stripANSI(m.View())
	eIdx := strings.Index(plain, "e edit")
	plusIdx := strings.Index(plain, "+ add")
	aIdx := strings.Index(plain, "A all")
	nIdx := strings.Index(plain, "N none")
	if eIdx < 0 || plusIdx < 0 || aIdx < 0 || nIdx < 0 {
		t.Fatalf("expected all selection actions visible, got:\n%s", plain)
	}
	if !(eIdx < aIdx && plusIdx < aIdx) {
		t.Errorf("expected 'e edit' and '+ add' to appear before 'A all'; got e@%d + @%d A@%d", eIdx, plusIdx, aIdx)
	}
	if !(plusIdx < nIdx) {
		t.Errorf("expected '+ add' before 'N none'; got + @%d N@%d", plusIdx, nIdx)
	}
}

func TestRespondCmd_IncludesInstructionsAndAddedFindings(t *testing.T) {
	findings := `{"findings":[{"id":"review-1","severity":"error","description":"bug","action":"auto-fix"}],"summary":"1"}`
	m := newAwaitingModel(t, findings)
	m.findingInstructions = map[types.StepName]map[string]string{
		types.StepReview: {"review-1": "only touch parser.go"},
	}
	m.addedFindings[types.StepReview] = []types.Finding{
		{ID: "user-1", Severity: "warning", Description: "also logger", Source: types.FindingSourceUser, Action: types.ActionAutoFix},
	}
	m.findingSelections[types.StepReview] = map[string]bool{
		"review-1": true,
		"user-1":   true,
	}

	ids := m.selectedFindingIDs(types.StepReview)
	if len(ids) != 1 || ids[0] != "review-1" {
		t.Fatalf("expected only agent ID in selectedFindingIDs, got %v", ids)
	}
	added := m.selectedUserAddedFindings(types.StepReview)
	if len(added) != 1 || added[0].ID != "user-1" {
		t.Fatalf("expected one user-added finding, got %v", added)
	}
	// Instructions that don't match a selected ID should be dropped.
	if _, ok := m.findingInstructions[types.StepReview]["review-99"]; ok {
		t.Fatal("test setup bug")
	}
}

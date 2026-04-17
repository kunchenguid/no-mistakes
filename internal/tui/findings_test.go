package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/muesli/termenv"
)

func TestParseFindings_Empty(t *testing.T) {
	f, err := parseFindings("")
	if err != nil {
		t.Fatal(err)
	}
	if f != nil {
		t.Error("expected nil for empty string")
	}
}

func TestParseFindings_Valid(t *testing.T) {
	raw := `{"findings":[{"severity":"error","file":"main.go","line":10,"description":"nil pointer"}],"summary":"1 issue found"}`
	f, err := parseFindings(raw)
	if err != nil {
		t.Fatal(err)
	}
	if f == nil {
		t.Fatal("expected non-nil findings")
	}
	if len(f.Items) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(f.Items))
	}
	if f.Items[0].Severity != "error" {
		t.Errorf("expected error severity, got %s", f.Items[0].Severity)
	}
	if f.Items[0].File != "main.go" {
		t.Errorf("expected main.go, got %s", f.Items[0].File)
	}
	if f.Items[0].Line != 10 {
		t.Errorf("expected line 10, got %d", f.Items[0].Line)
	}
	if f.Summary != "1 issue found" {
		t.Errorf("expected '1 issue found', got %s", f.Summary)
	}
}

func TestParseFindings_WithRiskAssessment(t *testing.T) {
	raw := `{"findings":[{"severity":"error","description":"bug"}],"risk_level":"high","risk_rationale":"Critical bug."}`
	f, err := parseFindings(raw)
	if err != nil {
		t.Fatal(err)
	}
	if f.RiskLevel != "high" {
		t.Errorf("expected risk_level 'high', got %q", f.RiskLevel)
	}
	if f.RiskRationale != "Critical bug." {
		t.Errorf("expected risk_rationale, got %q", f.RiskRationale)
	}
}

func TestParseFindings_InvalidJSON(t *testing.T) {
	_, err := parseFindings("{bad json")
	if err == nil {
		t.Error("expected error for invalid JSON")
	}
}

func TestSeverityIcon(t *testing.T) {
	tests := []struct {
		severity string
		icon     string
	}{
		{"error", "E"},
		{"warning", "W"},
		{"info", "I"},
		{"unknown", "·"},
	}
	for _, tt := range tests {
		if got := severityIcon(tt.severity); got != tt.icon {
			t.Errorf("severityIcon(%s) = %q, want %q", tt.severity, got, tt.icon)
		}
	}
}

func TestRenderFindings_Empty(t *testing.T) {
	if got := renderFindings("", 80); got != "" {
		t.Errorf("expected empty string for empty input, got %q", got)
	}
}

func TestRenderFindings_NoItems(t *testing.T) {
	raw := `{"findings":[],"summary":""}`
	if got := renderFindings(raw, 80); got != "" {
		t.Errorf("expected empty string for no findings, got %q", got)
	}
}

func TestRenderFindings_SummaryOnly(t *testing.T) {
	raw := `{"findings":[],"summary":"All clear"}`
	got := renderFindings(raw, 80)
	if !strings.Contains(got, "All clear") {
		t.Error("expected summary in output")
	}
}

func TestRenderFindings_RiskAssessment(t *testing.T) {
	raw := `{"findings":[{"severity":"error","file":"main.go","line":10,"description":"nil pointer"}],"risk_level":"high","risk_rationale":"Critical concurrency bug could cause data corruption."}`
	got := renderFindings(raw, 80)
	plain := stripANSI(got)

	// Should show "Risk: HIGH" instead of a summary.
	if !strings.Contains(plain, "Risk: HIGH") {
		t.Errorf("expected 'Risk: HIGH' in output, got:\n%s", plain)
	}
	// Should include rationale.
	if !strings.Contains(plain, "Critical concurrency bug") {
		t.Errorf("expected risk rationale in output, got:\n%s", plain)
	}
}

func TestRenderFindings_RiskAssessmentLow(t *testing.T) {
	raw := `{"findings":[{"severity":"info","description":"minor style issue"}],"risk_level":"low","risk_rationale":"Straightforward cosmetic change."}`
	got := renderFindings(raw, 80)
	plain := stripANSI(got)

	if !strings.Contains(plain, "Risk: LOW") {
		t.Errorf("expected 'Risk: LOW' in output, got:\n%s", plain)
	}
}

func TestRenderFindings_RiskOverridesSummary(t *testing.T) {
	// When both risk_level and summary are present, risk takes precedence.
	raw := `{"findings":[{"severity":"warning","description":"check this"}],"summary":"1 issue found","risk_level":"medium","risk_rationale":"Moderate impact."}`
	got := renderFindings(raw, 80)
	plain := stripANSI(got)

	if !strings.Contains(plain, "Risk: MEDIUM") {
		t.Errorf("expected risk assessment, got:\n%s", plain)
	}
	if strings.Contains(plain, "1 issue found") {
		t.Errorf("summary should not appear when risk assessment is present, got:\n%s", plain)
	}
}

func TestRenderFindings_SelectionFooter(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI)
	raw := `{"findings":[
		{"id":"f1","severity":"error","file":"a.go","line":1,"description":"err"},
		{"id":"f2","severity":"warning","file":"b.go","line":2,"description":"warn"},
		{"id":"f3","severity":"info","file":"c.go","line":3,"description":"note"}
	],"summary":"3 issues"}`

	// When some findings are deselected, footer should show selected counts.
	selected := map[string]bool{"f1": true, "f3": true} // f2 (warning) deselected
	_, footer := renderFindingsWithSelection(raw, 80, 0, selected, 0)
	plain := stripANSI(footer)

	if !strings.Contains(plain, "E 1 I 1 selected") {
		t.Errorf("expected selection footer 'E 1 I 1 selected', got: %q", plain)
	}
}

func TestRenderFindings_SelectionFooter_AllSelected(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI)
	raw := `{"findings":[
		{"id":"f1","severity":"error","description":"err"},
		{"id":"f2","severity":"warning","description":"warn"}
	],"summary":"2 issues"}`

	// When all are selected, no selection footer.
	selected := map[string]bool{"f1": true, "f2": true}
	_, footer := renderFindingsWithSelection(raw, 80, 0, selected, 0)

	if strings.Contains(stripANSI(footer), "selected") {
		t.Errorf("should not show selection footer when all selected, got: %q", footer)
	}
}

func TestRenderFindings_SelectionFooter_NilSelected(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI)
	raw := `{"findings":[
		{"id":"f1","severity":"error","description":"err"}
	],"summary":"1 issue"}`

	// nil selected means all selected (default state).
	_, footer := renderFindingsWithSelection(raw, 80, 0, nil, 0)

	if strings.Contains(stripANSI(footer), "selected") {
		t.Errorf("should not show selection footer when selected is nil, got: %q", footer)
	}
}

func TestRenderFindings_SelectionFooter_AllDeselected(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI)
	raw := `{"findings":[
		{"id":"f1","severity":"error","description":"err"},
		{"id":"f2","severity":"warning","description":"warn"}
	],"summary":"2 issues"}`

	// All deselected: selected map present but no IDs true.
	selected := map[string]bool{"f1": false, "f2": false}
	_, footer := renderFindingsWithSelection(raw, 80, 0, selected, 0)
	plain := stripANSI(footer)

	// Should not show "selected" at all when nothing is selected.
	if strings.Contains(plain, "selected") {
		t.Errorf("should not show selection footer when all deselected, got: %q", plain)
	}
}

func TestRenderFindings_WithFindings(t *testing.T) {
	raw := `{"findings":[
		{"severity":"error","file":"main.go","line":10,"description":"nil pointer dereference"},
		{"severity":"warning","file":"util.go","description":"unused variable"},
		{"severity":"info","description":"consider adding docs"}
	],"summary":"3 issues found"}`

	got := renderFindings(raw, 80)

	// Summary present.
	if !strings.Contains(got, "3 issues found") {
		t.Error("expected summary")
	}

	// Severity counts are in the box title now, not the body.

	// File references.
	if !strings.Contains(got, "main.go:10") {
		t.Error("expected file:line reference")
	}
	if !strings.Contains(got, "util.go") {
		t.Error("expected file reference without line")
	}

	// Descriptions.
	if !strings.Contains(got, "nil pointer dereference") {
		t.Error("expected error description")
	}
	if !strings.Contains(got, "unused variable") {
		t.Error("expected warning description")
	}
	if !strings.Contains(got, "consider adding docs") {
		t.Error("expected info description")
	}
}

func TestRenderFindings_InvalidJSON(t *testing.T) {
	// Should return empty rather than crash.
	if got := renderFindings("{bad", 80); got != "" {
		t.Errorf("expected empty for invalid JSON, got %q", got)
	}
}

func TestRenderFindings_WrapsLongDescriptions(t *testing.T) {
	raw := `{"findings":[{"severity":"warning","description":"this is a very long finding description that should wrap to fit inside the available review pane width instead of getting cut off at the edge of the terminal"}],"summary":"1 issue"}`

	got := renderFindings(raw, 40)
	for _, line := range strings.Split(strings.TrimSuffix(got, "\n"), "\n") {
		if len([]rune(stripANSI(line))) > 40 {
			t.Fatalf("expected wrapped findings output, got overlong line %q", stripANSI(line))
		}
	}
}

func TestConfigureTUIColors_UsesANSIProfile(t *testing.T) {
	prev := lipgloss.ColorProfile()
	defer lipgloss.SetColorProfile(prev)

	lipgloss.SetColorProfile(termenv.TrueColor)
	configureTUIColors()

	if lipgloss.ColorProfile() != termenv.ANSI {
		t.Fatalf("ColorProfile = %v, want %v", lipgloss.ColorProfile(), termenv.ANSI)
	}
}

func TestModel_ApplyEvent_StepCompletedWithFindings(t *testing.T) {
	run := testRun()
	m := NewModel("/tmp/sock", nil, run)

	findingsJSON := `{"findings":[{"severity":"warning","description":"test"}],"summary":"1 issue"}`
	m.applyEvent(ipc.Event{
		Type:     ipc.EventStepCompleted,
		RunID:    run.ID,
		StepName: ptr(types.StepReview),
		Status:   ptr(string(types.StepStatusAwaitingApproval)),
		Findings: &findingsJSON,
	})

	if m.steps[0].Status != types.StepStatusAwaitingApproval {
		t.Errorf("expected awaiting_approval, got %s", m.steps[0].Status)
	}
	if got, ok := m.stepFindings[types.StepReview]; !ok || got != findingsJSON {
		t.Error("expected findings stored for review step")
	}
}

func TestModel_View_ShowsFindingsWhenAwaiting(t *testing.T) {
	run := testRun()
	m := NewModel("/tmp/sock", nil, run)
	m.steps[0].Status = types.StepStatusAwaitingApproval
	m.stepFindings[types.StepReview] = `{"findings":[{"id":"review-1","severity":"error","file":"app.go","line":5,"description":"buffer overflow risk"}],"summary":"1 critical issue"}`

	view := m.View()
	if !strings.Contains(view, "1 critical issue") {
		t.Error("expected findings summary in view")
	}
	if !strings.Contains(view, "[x]") {
		t.Error("expected findings to start selected")
	}
	if !strings.Contains(view, "buffer overflow risk") {
		t.Error("expected finding description in view")
	}
	if !strings.Contains(view, "app.go:5") {
		t.Error("expected file reference in view")
	}
}

func TestModel_ApplyEvent_PausedStepPreselectsAllFindings(t *testing.T) {
	run := testRun()
	m := NewModel("/tmp/sock", nil, run)

	findingsJSON := `{"findings":[{"id":"review-1","severity":"warning","description":"first"},{"id":"review-2","severity":"error","description":"second"}],"summary":"2 issues"}`
	m.applyEvent(ipc.Event{
		Type:     ipc.EventStepCompleted,
		RunID:    run.ID,
		StepName: ptr(types.StepReview),
		Status:   ptr(string(types.StepStatusAwaitingApproval)),
		Findings: &findingsJSON,
	})

	ids := m.selectedFindingIDs(types.StepReview)
	if len(ids) != 2 || ids[0] != "review-1" || ids[1] != "review-2" {
		t.Fatalf("expected all findings selected, got %#v", ids)
	}
}

func TestModel_FindingSelectionToggleAndCursor(t *testing.T) {
	run := testRun()
	m := NewModel("/tmp/sock", nil, run)
	m.steps[0].Status = types.StepStatusAwaitingApproval
	m.stepFindings[types.StepReview] = `{"findings":[{"id":"review-1","severity":"warning","description":"first"},{"id":"review-2","severity":"error","description":"second"}],"summary":"2 issues"}`
	m.ensureFindingSelection(types.StepReview)

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeySpace, Runes: []rune(" ")})
	model := updated.(Model)
	ids := model.selectedFindingIDs(types.StepReview)
	if len(ids) != 1 || ids[0] != "review-2" {
		t.Fatalf("expected first finding toggled off, got %#v", ids)
	}

	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	model = updated.(Model)
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeySpace, Runes: []rune(" ")})
	model = updated.(Model)
	ids = model.selectedFindingIDs(types.StepReview)
	if len(ids) != 0 {
		t.Fatalf("expected both findings toggled off, got %#v", ids)
	}

	_, cmd := model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("f")})
	if cmd != nil {
		t.Fatal("expected fix to be blocked when no findings are selected")
	}

	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("A")})
	model = updated.(Model)
	ids = model.selectedFindingIDs(types.StepReview)
	if len(ids) != 2 {
		t.Fatalf("expected select-all to restore both findings, got %#v", ids)
	}

	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("N")})
	model = updated.(Model)
	ids = model.selectedFindingIDs(types.StepReview)
	if len(ids) != 0 {
		t.Fatalf("expected clear-all to remove selections, got %#v", ids)
	}
}

func TestModel_View_HidesFixActionWhenNoFindingsSelected(t *testing.T) {
	run := testRun()
	m := NewModel("/tmp/sock", nil, run)
	m.steps[0].Status = types.StepStatusAwaitingApproval
	m.stepFindings[types.StepReview] = `{"findings":[{"id":"review-1","severity":"warning","description":"first"},{"id":"review-2","severity":"error","description":"second"}],"summary":"2 issues"}`
	m.ensureFindingSelection(types.StepReview)
	m.clearAllFindings(types.StepReview)

	view := stripANSI(m.View())
	if strings.Contains(view, "f fix") {
		t.Fatal("expected fix action to be hidden when no findings are selected")
	}
	if !strings.Contains(view, "toggle") {
		t.Fatal("expected selection controls to remain visible")
	}
}

func TestModel_View_NoFindingsWhenNotAwaiting(t *testing.T) {
	run := testRun()
	m := NewModel("/tmp/sock", nil, run)
	// Store findings but step is completed (not awaiting).
	m.steps[0].Status = types.StepStatusCompleted
	m.stepFindings[types.StepReview] = `{"findings":[{"severity":"error","description":"should not appear"}],"summary":"hidden"}`

	view := m.View()
	if strings.Contains(view, "should not appear") {
		t.Error("findings should not appear when step is not awaiting approval")
	}
}

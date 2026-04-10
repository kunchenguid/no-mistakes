package tui

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/muesli/termenv"
)

var ansiRegexp = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func stripANSI(s string) string {
	return ansiRegexp.ReplaceAllString(s, "")
}

func ptr[T any](v T) *T { return &v }

func testRun() *ipc.RunInfo {
	return &ipc.RunInfo{
		ID:      "run-001",
		RepoID:  "repo-001",
		Branch:  "feature/foo",
		HeadSHA: "abc12345def67890",
		BaseSHA: "000000000000",
		Status:  types.RunRunning,
		Steps: []ipc.StepResultInfo{
			{ID: "s1", StepName: types.StepReview, StepOrder: 1, Status: types.StepStatusPending},
			{ID: "s2", StepName: types.StepTest, StepOrder: 2, Status: types.StepStatusPending},
			{ID: "s3", StepName: types.StepLint, StepOrder: 3, Status: types.StepStatusPending},
			{ID: "s4", StepName: types.StepPush, StepOrder: 4, Status: types.StepStatusPending},
			{ID: "s5", StepName: types.StepPR, StepOrder: 5, Status: types.StepStatusPending},
		},
	}
}

func TestStepStatusIcon(t *testing.T) {
	tests := []struct {
		status types.StepStatus
		icon   string
	}{
		{types.StepStatusPending, "○"},
		{types.StepStatusRunning, spinnerFrames[0]},
		{types.StepStatusAwaitingApproval, "⏸"},
		{types.StepStatusFixing, spinnerFrames[0]},
		{types.StepStatusCompleted, "✓"},
		{types.StepStatusSkipped, "–"},
		{types.StepStatusFailed, "✗"},
	}
	for _, tt := range tests {
		if got := stepStatusIcon(tt.status); got != tt.icon {
			t.Errorf("stepStatusIcon(%s) = %q, want %q", tt.status, got, tt.icon)
		}
	}
}

func TestStepLabel(t *testing.T) {
	tests := []struct {
		name  types.StepName
		label string
	}{
		{types.StepReview, "Review"},
		{types.StepTest, "Test"},
		{types.StepLint, "Lint"},
		{types.StepPush, "Push"},
		{types.StepPR, "PR"},
		{types.StepBabysit, "Babysit"},
	}
	for _, tt := range tests {
		if got := stepLabel(tt.name); got != tt.label {
			t.Errorf("stepLabel(%s) = %q, want %q", tt.name, got, tt.label)
		}
	}
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		ms   int64
		want string
	}{
		{500, "500ms"},
		{1000, "1.0s"},
		{2500, "2.5s"},
		{60000, "60.0s"},
	}
	for _, tt := range tests {
		if got := formatDuration(tt.ms); got != tt.want {
			t.Errorf("formatDuration(%d) = %q, want %q", tt.ms, got, tt.want)
		}
	}
}

func TestRenderPipelineView_NilRun(t *testing.T) {
	out := renderPipelineView(nil, nil, 80, 0, false, false)
	if out != "No active run." {
		t.Errorf("expected 'No active run.', got %q", out)
	}
}

func TestRenderPipelineView_ShowsSteps(t *testing.T) {
	run := testRun()
	run.Steps[0].Status = types.StepStatusCompleted
	run.Steps[0].DurationMS = ptr(int64(1200))
	run.Steps[1].Status = types.StepStatusRunning

	out := renderPipelineView(run, run.Steps, 80, 0, false, false)
	if !strings.Contains(out, "feature/foo") {
		t.Error("expected branch name in output")
	}
	if !strings.Contains(out, "abc12345") {
		t.Error("expected truncated SHA in output")
	}
	if !strings.Contains(out, "Review") {
		t.Error("expected Review step")
	}
	if !strings.Contains(out, "1.2s") {
		t.Error("expected duration for completed step")
	}
	if !strings.Contains(out, "Test") {
		t.Error("expected Test step")
	}
}

func TestRenderPipelineView_ConnectorsBetweenSteps(t *testing.T) {
	run := testRun()
	run.Steps[0].Status = types.StepStatusCompleted
	run.Steps[0].DurationMS = ptr(int64(1200))
	run.Steps[1].Status = types.StepStatusRunning

	out := stripANSI(renderPipelineView(run, run.Steps, 80, 0, false, false))
	lines := strings.Split(out, "\n")

	// Find step lines by looking for step label keywords (inside box borders).
	stepIcons := []string{"✓", "⠋", "○", "⏸", "✗", "–"}
	var stepLineIndices []int
	for i, line := range lines {
		// Strip box border prefix (│ ) to find step content.
		content := strings.TrimPrefix(strings.TrimSpace(line), "│")
		content = strings.TrimSpace(content)
		// Remove trailing box border.
		if idx := strings.LastIndex(content, "│"); idx > 0 {
			content = strings.TrimSpace(content[:idx])
		}
		for _, icon := range stepIcons {
			if strings.HasPrefix(content, icon) {
				stepLineIndices = append(stepLineIndices, i)
				break
			}
		}
	}
	if len(stepLineIndices) < 2 {
		t.Fatalf("expected at least 2 step lines, found %d in:\n%s", len(stepLineIndices), out)
	}
	// Between each pair of consecutive step lines, there should be a connector.
	// Inside the box, connector lines contain multiple │ chars (box borders + connector).
	for i := 0; i < len(stepLineIndices)-1; i++ {
		connectorFound := false
		for j := stepLineIndices[i] + 1; j < stepLineIndices[i+1]; j++ {
			// Connector line has 3 │ chars: left border, connector, right border.
			if strings.Count(lines[j], "│") >= 3 {
				connectorFound = true
				break
			}
		}
		if !connectorFound {
			t.Errorf("expected connector │ between step lines %d and %d", stepLineIndices[i], stepLineIndices[i+1])
		}
	}
}

func TestRenderPipelineView_ApprovalPrompt(t *testing.T) {
	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval

	out := renderPipelineView(run, run.Steps, 80, 0, true, true)
	if !strings.Contains(out, "awaiting action") {
		t.Error("expected approval prompt")
	}
	if !strings.Contains(stripANSI(out), "a approve") {
		t.Error("expected action keys")
	}
	if !strings.Contains(stripANSI(out), "f fix") {
		t.Error("expected fix action when findings are selected")
	}
}

func TestRenderPipelineView_Error(t *testing.T) {
	run := testRun()
	run.Error = ptr("something broke")
	out := renderPipelineView(run, run.Steps, 80, 0, false, false)
	if !strings.Contains(out, "something broke") {
		t.Error("expected error message in output")
	}
}

func TestRenderPipelineView_StepError(t *testing.T) {
	run := testRun()
	run.Steps[1].Status = types.StepStatusFailed
	run.Steps[1].Error = ptr("tests failed")

	out := renderPipelineView(run, run.Steps, 80, 0, false, false)
	if !strings.Contains(out, "tests failed") {
		t.Error("expected step error in output")
	}
}

func TestRenderApprovalActions_FormatWithSeparator(t *testing.T) {
	out := stripANSI(renderApprovalActions(true, true))
	// Keys should not be bracket-wrapped - design uses "a approve" not "[a] approve".
	if strings.Contains(out, "[a]") {
		t.Error("expected bare key format 'a approve', not '[a] approve'")
	}
	// Should have │ separator between primary actions and selection actions.
	if !strings.Contains(out, "│") {
		t.Error("expected │ separator between primary and selection action groups")
	}
	// Selection actions should use ␣ for space.
	if !strings.Contains(out, "toggle") {
		t.Error("expected toggle in selection actions")
	}
}

func TestRenderApprovalActions_NoSelectionActions(t *testing.T) {
	out := stripANSI(renderApprovalActions(false, true))
	// Without selection actions, no │ separator should appear.
	if strings.Contains(out, "│") {
		t.Error("expected no │ separator when no selection actions")
	}
	if strings.Contains(out, "toggle") {
		t.Error("expected no selection actions")
	}
}

func TestRenderPipelineView_HidesFixActionWhenDisabled(t *testing.T) {
	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval

	out := stripANSI(renderPipelineView(run, run.Steps, 80, 0, true, false))
	if strings.Contains(out, "f fix") {
		t.Fatal("expected fix action to be hidden when disabled")
	}
	if !strings.Contains(out, "toggle") {
		t.Fatal("expected selection controls when findings are present")
	}
}

func TestRenderPipelineView_HidesSelectionControlsWithoutFindings(t *testing.T) {
	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval

	out := stripANSI(renderPipelineView(run, run.Steps, 80, 0, false, false))
	if strings.Contains(out, "f fix") {
		t.Fatal("expected fix action to be hidden without findings")
	}
	if strings.Contains(out, "toggle") {
		t.Fatal("expected selection controls to be hidden without findings")
	}
}

func TestAwaitingStep(t *testing.T) {
	run := testRun()

	// No step awaiting.
	if got := awaitingStep(run.Steps); got != nil {
		t.Error("expected nil when no step awaiting")
	}

	// Set review to awaiting.
	run.Steps[0].Status = types.StepStatusAwaitingApproval
	got := awaitingStep(run.Steps)
	if got == nil {
		t.Fatal("expected non-nil step")
	}
	if got.StepName != types.StepReview {
		t.Errorf("expected review step, got %s", got.StepName)
	}

	// Fix review also counts.
	run.Steps[0].Status = types.StepStatusCompleted
	run.Steps[1].Status = types.StepStatusFixReview
	got = awaitingStep(run.Steps)
	if got == nil || got.StepName != types.StepTest {
		t.Error("expected test step in fix_review")
	}
}

func TestModel_ApplyEvent_StepStarted(t *testing.T) {
	run := testRun()
	m := NewModel("/tmp/sock", nil, run)

	m.applyEvent(ipc.Event{
		Type:     ipc.EventStepStarted,
		RunID:    run.ID,
		StepName: ptr(types.StepReview),
	})

	if m.steps[0].Status != types.StepStatusRunning {
		t.Errorf("expected running, got %s", m.steps[0].Status)
	}
}

func TestModel_ApplyEvent_StepCompleted(t *testing.T) {
	run := testRun()
	m := NewModel("/tmp/sock", nil, run)

	m.applyEvent(ipc.Event{
		Type:     ipc.EventStepCompleted,
		RunID:    run.ID,
		StepName: ptr(types.StepReview),
		Status:   ptr(string(types.StepStatusCompleted)),
	})

	if m.steps[0].Status != types.StepStatusCompleted {
		t.Errorf("expected completed, got %s", m.steps[0].Status)
	}
}

func TestModel_ApplyEvent_RunCompleted(t *testing.T) {
	run := testRun()
	m := NewModel("/tmp/sock", nil, run)

	m.applyEvent(ipc.Event{
		Type:   ipc.EventRunCompleted,
		RunID:  run.ID,
		Status: ptr(string(types.RunCompleted)),
	})

	if !m.done {
		t.Error("expected done=true after run_completed")
	}
	if m.run.Status != types.RunCompleted {
		t.Errorf("expected completed status, got %s", m.run.Status)
	}
}

func TestModel_ApplyEvent_LogChunk(t *testing.T) {
	run := testRun()
	m := NewModel("/tmp/sock", nil, run)

	m.applyEvent(ipc.Event{
		Type:    ipc.EventLogChunk,
		RunID:   run.ID,
		Content: ptr("line1\nline2\n"),
	})

	if len(m.logs) != 2 {
		t.Fatalf("expected 2 log lines, got %d", len(m.logs))
	}
	if m.logs[0] != "line1" || m.logs[1] != "line2" {
		t.Errorf("unexpected log lines: %v", m.logs)
	}
}

func TestModel_ApplyEvent_LogChunk_Truncation(t *testing.T) {
	run := testRun()
	m := NewModel("/tmp/sock", nil, run)

	// Add 110 log lines.
	for i := 0; i < 110; i++ {
		m.applyEvent(ipc.Event{
			Type:    ipc.EventLogChunk,
			RunID:   run.ID,
			Content: ptr("line\n"),
		})
	}

	if len(m.logs) != 100 {
		t.Errorf("expected 100 log lines (truncated), got %d", len(m.logs))
	}
}

func TestModel_HandleKey_Quit(t *testing.T) {
	run := testRun()
	m := NewModel("/tmp/sock", nil, run)

	updated, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	model := updated.(Model)
	if !model.quitting {
		t.Error("expected quitting=true after 'q'")
	}
	if cmd == nil {
		t.Error("expected quit command")
	}
}

func TestModel_HandleKey_ApprovalActions_NoAwaitingStep(t *testing.T) {
	run := testRun()
	m := NewModel("/tmp/sock", nil, run)

	// No step awaiting → approval keys should return nil cmd.
	for _, key := range []string{"a", "f", "s", "x"} {
		_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(key)})
		if cmd != nil {
			t.Errorf("key %q should return nil cmd when no step awaiting", key)
		}
	}
}

func TestModel_Update_SpinnerTickAdvancesRunningStepIcon(t *testing.T) {
	run := testRun()
	run.Steps[0].Status = types.StepStatusRunning
	m := NewModel("/tmp/sock", nil, run)
	m.width = 80

	before := m.View()
	updated, cmd := m.Update(spinnerTickMsg{})
	after := updated.(Model).View()

	if before == after {
		t.Fatal("expected spinner tick to change the rendered view")
	}
	if cmd == nil {
		t.Fatal("expected spinner tick to schedule another tick")
	}
}

func TestModel_Update_SpinnerTickStopsWhenIdle(t *testing.T) {
	run := testRun()
	m := NewModel("/tmp/sock", nil, run)
	m.width = 80

	before := m.View()
	updated, cmd := m.Update(spinnerTickMsg{})
	after := updated.(Model).View()

	if before != after {
		t.Fatal("expected idle spinner tick to leave the view unchanged")
	}
	if cmd != nil {
		t.Fatal("expected idle spinner tick to stop rescheduling")
	}
}

func TestModel_Update_StepStartedBeginsSpinnerLoop(t *testing.T) {
	run := testRun()
	m := NewModel("/tmp/sock", nil, run)

	updated, cmd := m.Update(eventMsg(ipc.Event{
		Type:     ipc.EventStepStarted,
		RunID:    run.ID,
		StepName: ptr(types.StepReview),
	}))
	model := updated.(Model)

	if model.steps[0].Status != types.StepStatusRunning {
		t.Fatalf("expected review step running, got %s", model.steps[0].Status)
	}
	if cmd == nil {
		t.Fatal("expected step start to schedule spinner ticks")
	}
}

func TestModel_View_NoActiveRun(t *testing.T) {
	m := Model{}
	view := m.View()
	if !strings.Contains(view, "No active run") {
		t.Error("expected 'No active run' in view")
	}
}

func TestModel_View_DetachMessage(t *testing.T) {
	run := testRun()
	m := NewModel("/tmp/sock", nil, run)
	view := stripANSI(m.View())
	if !strings.Contains(view, "q detach") {
		t.Error("expected minimal 'q detach' hint when pipeline is running")
	}
	// Should NOT use verbose phrasing.
	if strings.Contains(view, "Press q") {
		t.Error("expected minimal footer, not verbose 'Press q...' phrasing")
	}
}

func TestModel_View_DoneMessage(t *testing.T) {
	run := testRun()
	m := NewModel("/tmp/sock", nil, run)
	m.done = true
	view := stripANSI(m.View())
	if !strings.Contains(view, "q quit") {
		t.Error("expected minimal 'q quit' hint when done")
	}
	// Should NOT use verbose phrasing.
	if strings.Contains(view, "Press q") {
		t.Error("expected minimal footer, not verbose 'Press q...' phrasing")
	}
}

func TestModel_View_LogTail(t *testing.T) {
	run := testRun()
	m := NewModel("/tmp/sock", nil, run)
	m.logs = []string{"log line 1", "log line 2", "log line 3"}
	view := m.View()
	if !strings.Contains(view, "log line 1") {
		t.Error("expected log lines in view")
	}
}

func TestModel_View_LogTailTruncated(t *testing.T) {
	run := testRun()
	m := NewModel("/tmp/sock", nil, run)
	for i := 0; i < 10; i++ {
		m.logs = append(m.logs, "log line")
	}
	view := m.View()
	// View should only show last 5 lines, so count occurrences.
	count := strings.Count(view, "log line")
	if count != 5 {
		t.Errorf("expected 5 log lines in view, got %d", count)
	}
}

func TestModel_View_Error(t *testing.T) {
	run := testRun()
	m := NewModel("/tmp/sock", nil, run)
	m.err = &ipc.RPCError{Code: -1, Message: "test error"}
	view := m.View()
	if !strings.Contains(view, "test error") {
		t.Error("expected error in view")
	}
}

func TestModel_ConnectedMsg(t *testing.T) {
	run := testRun()
	m := NewModel("/tmp/sock", nil, run)

	ch := make(chan ipc.Event, 1)
	cancel := func() {}

	updated, _ := m.Update(connectedMsg{events: ch, cancelSub: cancel})
	model := updated.(Model)
	if model.events == nil {
		t.Error("expected events channel to be set")
	}
}

func TestModel_WindowSize(t *testing.T) {
	run := testRun()
	m := NewModel("/tmp/sock", nil, run)

	updated, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	model := updated.(Model)
	if model.width != 120 || model.height != 40 {
		t.Errorf("expected 120x40, got %dx%d", model.width, model.height)
	}
}

// --- Review/Findings tests ---

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
		{"error", "●"},
		{"warning", "▲"},
		{"info", "○"},
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

	// Severity counts present.
	if !strings.Contains(got, "1 error") {
		t.Error("expected error count")
	}
	if !strings.Contains(got, "1 warning") {
		t.Error("expected warning count")
	}
	if !strings.Contains(got, "1 info") {
		t.Error("expected info count")
	}

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

// --- Diff viewer tests ---

func TestParseDiffLines_Empty(t *testing.T) {
	lines := parseDiffLines("")
	if lines != nil {
		t.Errorf("expected nil for empty input, got %d lines", len(lines))
	}
}

func TestParseDiffLines_Simple(t *testing.T) {
	raw := `diff --git a/main.go b/main.go
index abc1234..def5678 100644
--- a/main.go
+++ b/main.go
@@ -1,3 +1,4 @@
 package main
+import "fmt"
-var x = 1
 func main() {}
`
	lines := parseDiffLines(raw)
	if len(lines) != 9 {
		t.Fatalf("expected 9 lines, got %d", len(lines))
	}

	// Check line types.
	expected := []diffLineType{
		diffLineFileHeader, // diff --git
		diffLineFileHeader, // index
		diffLineFileHeader, // ---
		diffLineFileHeader, // +++
		diffLineHunkHeader, // @@
		diffLineContext,    // package main
		diffLineAddition,   // +import
		diffLineDeletion,   // -var
		diffLineContext,    // func main
	}
	for i, want := range expected {
		if lines[i].Type != want {
			t.Errorf("line %d: expected type %d, got %d (text: %q)", i, want, lines[i].Type, lines[i].Text)
		}
	}
}

func TestParseDiffLines_MultipleFiles(t *testing.T) {
	raw := `diff --git a/a.go b/a.go
--- a/a.go
+++ b/a.go
@@ -1 +1 @@
-old
+new
diff --git a/b.go b/b.go
--- a/b.go
+++ b/b.go
@@ -1 +1 @@
-foo
+bar
`
	lines := parseDiffLines(raw)
	// Count file headers.
	fileHeaders := 0
	for _, l := range lines {
		if l.Type == diffLineFileHeader && strings.HasPrefix(l.Text, "diff --git") {
			fileHeaders++
		}
	}
	if fileHeaders != 2 {
		t.Errorf("expected 2 file headers, got %d", fileHeaders)
	}
}

func TestClassifyDiffLine(t *testing.T) {
	tests := []struct {
		line string
		want diffLineType
	}{
		{"diff --git a/f b/f", diffLineFileHeader},
		{"--- a/f", diffLineFileHeader},
		{"+++ b/f", diffLineFileHeader},
		{"index abc..def 100644", diffLineFileHeader},
		{"@@ -1,3 +1,4 @@", diffLineHunkHeader},
		{"+added", diffLineAddition},
		{"-removed", diffLineDeletion},
		{" context", diffLineContext},
		{"random text", diffLineContext},
	}
	for _, tt := range tests {
		if got := classifyDiffLine(tt.line); got != tt.want {
			t.Errorf("classifyDiffLine(%q) = %d, want %d", tt.line, got, tt.want)
		}
	}
}

func TestDiffStats(t *testing.T) {
	lines := []diffLine{
		{Type: diffLineFileHeader, Text: "diff --git a/main.go b/main.go"},
		{Type: diffLineFileHeader, Text: "--- a/main.go"},
		{Type: diffLineFileHeader, Text: "+++ b/main.go"},
		{Type: diffLineHunkHeader, Text: "@@ -1,3 +1,4 @@"},
		{Type: diffLineContext, Text: " package main"},
		{Type: diffLineAddition, Text: "+import \"fmt\""},
		{Type: diffLineAddition, Text: "+import \"os\""},
		{Type: diffLineDeletion, Text: "-var x = 1"},
	}

	files, adds, dels := diffStats(lines)
	if files != 1 {
		t.Errorf("expected 1 file, got %d", files)
	}
	if adds != 2 {
		t.Errorf("expected 2 additions, got %d", adds)
	}
	if dels != 1 {
		t.Errorf("expected 1 deletion, got %d", dels)
	}
}

func TestDiffStats_MultipleFiles(t *testing.T) {
	lines := []diffLine{
		{Type: diffLineFileHeader, Text: "+++ b/a.go"},
		{Type: diffLineAddition, Text: "+line"},
		{Type: diffLineFileHeader, Text: "+++ b/b.go"},
		{Type: diffLineDeletion, Text: "-line"},
	}

	files, adds, dels := diffStats(lines)
	if files != 2 {
		t.Errorf("expected 2 files, got %d", files)
	}
	if adds != 1 {
		t.Errorf("expected 1 addition, got %d", adds)
	}
	if dels != 1 {
		t.Errorf("expected 1 deletion, got %d", dels)
	}
}

func TestDiffStats_DevNull(t *testing.T) {
	lines := []diffLine{
		{Type: diffLineFileHeader, Text: "+++ /dev/null"},
		{Type: diffLineDeletion, Text: "-removed"},
	}

	files, _, _ := diffStats(lines)
	if files != 0 {
		t.Errorf("expected 0 files (/dev/null excluded), got %d", files)
	}
}

func TestRenderDiff_Empty(t *testing.T) {
	if got := renderDiff("", 80, 20, 0); got != "" {
		t.Errorf("expected empty for empty input, got %q", got)
	}
}

func TestRenderDiff_HasStats(t *testing.T) {
	raw := `diff --git a/main.go b/main.go
--- a/main.go
+++ b/main.go
@@ -1 +1,2 @@
 package main
+import "fmt"
`
	got := renderDiff(raw, 80, 0, 0)
	if !strings.Contains(got, "1 file(s) changed") {
		t.Error("expected file count in stats")
	}
	if !strings.Contains(got, "+1") {
		t.Error("expected addition count in stats")
	}
}

func TestRenderDiff_ColoredLines(t *testing.T) {
	raw := `diff --git a/main.go b/main.go
--- a/main.go
+++ b/main.go
@@ -1,2 +1,2 @@
-old line
+new line
`
	got := renderDiff(raw, 80, 0, 0)
	// Lines should be present (rendered with styles, but text should be there).
	if !strings.Contains(got, "old line") {
		t.Error("expected deletion line in output")
	}
	if !strings.Contains(got, "new line") {
		t.Error("expected addition line in output")
	}
}

func TestRenderDiff_Scrolling(t *testing.T) {
	// Build a diff with many lines.
	var b strings.Builder
	b.WriteString("diff --git a/main.go b/main.go\n")
	b.WriteString("--- a/main.go\n")
	b.WriteString("+++ b/main.go\n")
	b.WriteString("@@ -1,20 +1,20 @@\n")
	for i := 0; i < 20; i++ {
		b.WriteString("+line " + strings.Repeat("x", i) + "\n")
	}
	raw := b.String()

	// Render with a small viewport and offset.
	got := renderDiff(raw, 80, 5, 2)

	// Should show scroll indicator since there are more lines.
	if !strings.Contains(got, "more lines") {
		t.Error("expected scroll indicator for remaining lines")
	}
}

func TestRenderDiff_ScrollEnd(t *testing.T) {
	raw := `diff --git a/main.go b/main.go
--- a/main.go
+++ b/main.go
@@ -1 +1 @@
-old
+new
`
	// Scroll to near the end with a small viewport.
	got := renderDiff(raw, 80, 3, 3)

	// Should show end-of-diff indicator since we scrolled past start.
	if !strings.Contains(got, "end of diff") {
		t.Error("expected end-of-diff indicator")
	}
}

func TestRenderDiff_WrappedInBox(t *testing.T) {
	raw := `diff --git a/main.go b/main.go
--- a/main.go
+++ b/main.go
@@ -1 +1,2 @@
 package main
+import "fmt"
`
	got := stripANSI(renderDiff(raw, 80, 0, 0))
	lines := strings.Split(got, "\n")
	if len(lines) == 0 {
		t.Fatal("expected non-empty output")
	}
	// Should have box with "Diff" title.
	if !strings.Contains(lines[0], "Diff") {
		t.Errorf("expected 'Diff' title in top border, got %q", lines[0])
	}
	if !strings.Contains(lines[0], "╭") {
		t.Error("expected rounded top-left corner in diff box")
	}
}

func TestRenderDiff_ScrollIndicatorInBottomBorder(t *testing.T) {
	// Build a diff with many lines.
	var b strings.Builder
	b.WriteString("diff --git a/main.go b/main.go\n")
	b.WriteString("--- a/main.go\n")
	b.WriteString("+++ b/main.go\n")
	b.WriteString("@@ -1,20 +1,20 @@\n")
	for i := 0; i < 20; i++ {
		b.WriteString(fmt.Sprintf("+line %d\n", i))
	}

	got := stripANSI(renderDiff(b.String(), 80, 5, 0))
	lines := strings.Split(got, "\n")
	// The last non-empty line should be the bottom border with scroll info.
	lastLine := ""
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			lastLine = lines[i]
			break
		}
	}
	if !strings.Contains(lastLine, "╰") {
		t.Errorf("expected bottom border with ╰, got %q", lastLine)
	}
	if !strings.Contains(lastLine, "more lines") || !strings.Contains(lastLine, "↓") {
		t.Errorf("expected scroll indicator in bottom border, got %q", lastLine)
	}
}

func TestDiffLineStyle_Types(t *testing.T) {
	// Just verify no panics and styles are created.
	types := []diffLineType{
		diffLineContext,
		diffLineAddition,
		diffLineDeletion,
		diffLineFileHeader,
		diffLineHunkHeader,
	}
	for _, dt := range types {
		style := diffLineStyle(dt)
		_ = style.Render("test") // should not panic
	}
}

func TestModel_DiffToggle(t *testing.T) {
	run := testRun()
	m := NewModel("/tmp/sock", nil, run)
	m.steps[0].Status = types.StepStatusFixReview
	m.stepDiffs[types.StepReview] = "+new line\n"

	// Toggle on.
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
	model := updated.(Model)
	if !model.showDiff {
		t.Error("expected showDiff=true after 'd' press")
	}

	// Toggle off.
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
	model = updated.(Model)
	if model.showDiff {
		t.Error("expected showDiff=false after second 'd' press")
	}
}

func TestModel_DiffToggle_NoEffect_NoAwaitingStep(t *testing.T) {
	run := testRun()
	m := NewModel("/tmp/sock", nil, run)

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
	model := updated.(Model)
	if model.showDiff {
		t.Error("expected showDiff=false when no step is awaiting")
	}
}

func TestModel_DiffScroll(t *testing.T) {
	run := testRun()
	m := NewModel("/tmp/sock", nil, run)
	m.showDiff = true

	// Scroll down.
	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	model := updated.(Model)
	if model.diffOffset != 1 {
		t.Errorf("expected diffOffset=1, got %d", model.diffOffset)
	}

	// Scroll up.
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	model = updated.(Model)
	if model.diffOffset != 0 {
		t.Errorf("expected diffOffset=0, got %d", model.diffOffset)
	}

	// Can't scroll below 0.
	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	model = updated.(Model)
	if model.diffOffset != 0 {
		t.Errorf("expected diffOffset=0, got %d", model.diffOffset)
	}
}

func TestModel_DiffScroll_NoEffectWhenHidden(t *testing.T) {
	run := testRun()
	m := NewModel("/tmp/sock", nil, run)
	m.showDiff = false

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
	model := updated.(Model)
	if model.diffOffset != 0 {
		t.Error("expected no scroll when diff is hidden")
	}
}

func TestModel_ApplyEvent_StepCompletedWithDiff(t *testing.T) {
	run := testRun()
	m := NewModel("/tmp/sock", nil, run)
	m.showDiff = true
	m.diffOffset = 5

	diff := "+new line\n-old line\n"
	m.applyEvent(ipc.Event{
		Type:     ipc.EventStepCompleted,
		RunID:    run.ID,
		StepName: ptr(types.StepReview),
		Status:   ptr(string(types.StepStatusFixReview)),
		Diff:     &diff,
	})

	got, ok := m.stepDiffs[types.StepReview]
	if !ok || got != diff {
		t.Error("expected diff stored for review step")
	}
	// showDiff and offset should reset.
	if m.showDiff {
		t.Error("expected showDiff reset to false")
	}
	if m.diffOffset != 0 {
		t.Error("expected diffOffset reset to 0")
	}
}

func TestModel_View_ShowsDiff(t *testing.T) {
	run := testRun()
	m := NewModel("/tmp/sock", nil, run)
	m.steps[0].Status = types.StepStatusFixReview
	m.showDiff = true
	m.stepDiffs[types.StepReview] = `diff --git a/main.go b/main.go
--- a/main.go
+++ b/main.go
@@ -1 +1,2 @@
 package main
+import "fmt"
`
	m.height = 40

	view := m.View()
	if !strings.Contains(view, "1 file(s) changed") {
		t.Error("expected diff stats in view")
	}
	if !strings.Contains(view, "import") {
		t.Error("expected diff content in view")
	}
}

func TestModel_View_ShowsFindingsNotDiff(t *testing.T) {
	run := testRun()
	m := NewModel("/tmp/sock", nil, run)
	m.steps[0].Status = types.StepStatusAwaitingApproval
	m.showDiff = false
	m.stepFindings[types.StepReview] = `{"findings":[{"severity":"warning","description":"check this"}],"summary":"1 issue"}`
	m.stepDiffs[types.StepReview] = "+some diff\n"

	view := m.View()
	// Should show findings, not diff.
	if !strings.Contains(view, "check this") {
		t.Error("expected findings in view when showDiff is false")
	}
}

func TestRenderPipelineView_DiffKey(t *testing.T) {
	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval
	out := stripANSI(renderPipelineView(run, run.Steps, 80, 0, true, true))
	if !strings.Contains(out, "d diff") {
		t.Error("expected d diff in approval prompt")
	}
}

// --- Babysit view tests ---

func testRunWithBabysit() *ipc.RunInfo {
	run := testRun()
	run.Steps = append(run.Steps, ipc.StepResultInfo{
		ID: "s6", StepName: types.StepBabysit, StepOrder: 6, Status: types.StepStatusPending,
	})
	return run
}

func TestIsBabysitActive(t *testing.T) {
	run := testRunWithBabysit()

	// Pending → not active.
	if isBabysitActive(run.Steps) {
		t.Error("expected false when babysit is pending")
	}

	// Running → active.
	run.Steps[5].Status = types.StepStatusRunning
	if !isBabysitActive(run.Steps) {
		t.Error("expected true when babysit is running")
	}

	// Fixing → active.
	run.Steps[5].Status = types.StepStatusFixing
	if !isBabysitActive(run.Steps) {
		t.Error("expected true when babysit is fixing")
	}

	// Awaiting approval → active.
	run.Steps[5].Status = types.StepStatusAwaitingApproval
	if !isBabysitActive(run.Steps) {
		t.Error("expected true when babysit is awaiting approval")
	}

	// Fix review → active.
	run.Steps[5].Status = types.StepStatusFixReview
	if !isBabysitActive(run.Steps) {
		t.Error("expected true when babysit is in fix review")
	}

	// Completed → not active.
	run.Steps[5].Status = types.StepStatusCompleted
	if isBabysitActive(run.Steps) {
		t.Error("expected false when babysit is completed")
	}
}

func TestIsBabysitActive_NoBabysitStep(t *testing.T) {
	run := testRun() // no babysit step
	if isBabysitActive(run.Steps) {
		t.Error("expected false when no babysit step exists")
	}
}

func TestBabysitStepStatus(t *testing.T) {
	run := testRunWithBabysit()
	run.Steps[5].Status = types.StepStatusRunning

	if got := babysitStepStatus(run.Steps); got != types.StepStatusRunning {
		t.Errorf("expected running, got %s", got)
	}
}

func TestBabysitStepStatus_NoBabysitStep(t *testing.T) {
	run := testRun()
	if got := babysitStepStatus(run.Steps); got != types.StepStatusPending {
		t.Errorf("expected pending (default), got %s", got)
	}
}

func TestExtractPRFromLogs(t *testing.T) {
	tests := []struct {
		name string
		logs []string
		want string
	}{
		{
			name: "standard babysit message",
			logs: []string{"babysitting PR #42 (timeout: 4h)..."},
			want: "42",
		},
		{
			name: "multiple logs",
			logs: []string{
				"some other log",
				"babysitting PR #123 (timeout: 4h)...",
				"CI failures detected",
			},
			want: "123",
		},
		{
			name: "no PR reference",
			logs: []string{"running agent...", "completed"},
			want: "",
		},
		{
			name: "empty logs",
			logs: nil,
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := extractPRFromLogs(tt.logs); got != tt.want {
				t.Errorf("extractPRFromLogs() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestParseBabysitActivity(t *testing.T) {
	t.Run("empty logs", func(t *testing.T) {
		a := parseBabysitActivity(nil)
		if a.CIFixes != 0 || a.AutoFixing || a.LastEvent != "" {
			t.Error("expected zero activity for empty logs")
		}
	})

	t.Run("polling", func(t *testing.T) {
		a := parseBabysitActivity([]string{"babysitting PR #42 (timeout: 4h)..."})
		if a.LastEvent == "" {
			t.Error("expected last event set")
		}
	})

	t.Run("ci failure detected", func(t *testing.T) {
		a := parseBabysitActivity([]string{
			"babysitting PR #42 (timeout: 4h)...",
			"CI failures detected: test — auto-fixing...",
		})
		if a.CIFixes != 1 {
			t.Errorf("expected 1 CI fix, got %d", a.CIFixes)
		}
		if !a.AutoFixing {
			t.Error("expected auto-fixing to be true")
		}
	})

	t.Run("ci fix completed", func(t *testing.T) {
		a := parseBabysitActivity([]string{
			"CI failures detected: test — auto-fixing...",
			"running agent to fix CI failures...",
			"committed and pushed fixes",
		})
		if a.CIFixes != 1 {
			t.Errorf("expected 1 CI fix, got %d", a.CIFixes)
		}
		if a.AutoFixing {
			t.Error("expected auto-fixing to be false after push")
		}
	})

	t.Run("multiple ci fixes", func(t *testing.T) {
		a := parseBabysitActivity([]string{
			"CI failures detected: test",
			"committed and pushed fixes",
			"CI failures detected: lint",
			"running agent to fix CI failures...",
		})
		if a.CIFixes != 2 {
			t.Errorf("expected 2 CI fixes, got %d", a.CIFixes)
		}
	})

	t.Run("pr merged", func(t *testing.T) {
		a := parseBabysitActivity([]string{
			"babysitting PR #42 (timeout: 4h)...",
			"PR has been merged!",
		})
		if !strings.Contains(a.LastEvent, "merged") {
			t.Error("expected merged as last event")
		}
	})

	t.Run("pr closed", func(t *testing.T) {
		a := parseBabysitActivity([]string{"PR has been closed"})
		if !strings.Contains(a.LastEvent, "closed") {
			t.Error("expected closed as last event")
		}
	})

	t.Run("timeout", func(t *testing.T) {
		a := parseBabysitActivity([]string{"babysit timeout reached"})
		if !strings.Contains(a.LastEvent, "timeout") {
			t.Error("expected timeout as last event")
		}
	})
}

func TestRenderBabysitView_Monitoring(t *testing.T) {
	run := testRunWithBabysit()
	run.Steps[5].Status = types.StepStatusRunning
	logs := []string{"babysitting PR #42 (timeout: 4h)..."}

	out := renderBabysitView(run, run.Steps, "", logs, 80)

	if !strings.Contains(out, "Babysit Monitor") {
		t.Error("expected header")
	}
	if !strings.Contains(out, "Monitoring") {
		t.Error("expected monitoring state")
	}
	if !strings.Contains(out, "PR #42") {
		t.Error("expected PR number from logs")
	}
}

func TestRenderBabysitView_WithPRURL(t *testing.T) {
	run := testRunWithBabysit()
	run.PRURL = ptr("https://github.com/user/repo/pull/99")
	run.Steps[5].Status = types.StepStatusRunning

	out := renderBabysitView(run, run.Steps, "", nil, 80)

	if !strings.Contains(out, "https://github.com/user/repo/pull/99") {
		t.Error("expected full PR URL")
	}
}

func TestRenderBabysitView_AutoFixing(t *testing.T) {
	run := testRunWithBabysit()
	run.Steps[5].Status = types.StepStatusRunning
	logs := []string{
		"babysitting PR #42 (timeout: 4h)...",
		"CI failures detected: test — auto-fixing...",
		"running agent to fix CI failures...",
	}

	out := renderBabysitView(run, run.Steps, "", logs, 80)

	if !strings.Contains(out, "Auto-fixing CI") {
		t.Error("expected auto-fixing state indicator")
	}
	if !strings.Contains(out, "CI auto-fixes: 1") {
		t.Error("expected CI fix count")
	}
}

func TestRenderBabysitView_FixingComments(t *testing.T) {
	run := testRunWithBabysit()
	run.Steps[5].Status = types.StepStatusFixing

	out := renderBabysitView(run, run.Steps, "", nil, 80)

	if !strings.Contains(out, "addressing PR comments") {
		t.Error("expected addressing comments state")
	}
}

func TestRenderBabysitView_AwaitingWithFindings(t *testing.T) {
	run := testRunWithBabysit()
	run.Steps[5].Status = types.StepStatusAwaitingApproval
	findings := `{"findings":[{"severity":"info","description":"@alice: Please add more tests"}],"summary":"1 PR comment(s) to review"}`

	out := renderBabysitView(run, run.Steps, findings, nil, 80)

	if !strings.Contains(out, "review below") {
		t.Error("expected review prompt")
	}
	if !strings.Contains(out, "1 PR comment(s) to review") {
		t.Error("expected findings summary")
	}
	if !strings.Contains(out, "Please add more tests") {
		t.Error("expected comment content in findings")
	}
}

func TestRenderBabysitView_AwaitingNoFindings(t *testing.T) {
	run := testRunWithBabysit()
	run.Steps[5].Status = types.StepStatusAwaitingApproval

	out := renderBabysitView(run, run.Steps, "", nil, 80)

	if !strings.Contains(out, "review below") {
		t.Error("expected review prompt even without findings")
	}
}

func TestRenderBabysitView_LastActivity(t *testing.T) {
	run := testRunWithBabysit()
	run.Steps[5].Status = types.StepStatusRunning
	logs := []string{
		"babysitting PR #42 (timeout: 4h)...",
		"committed and pushed fixes",
	}

	out := renderBabysitView(run, run.Steps, "", logs, 80)

	if !strings.Contains(out, "Latest:") {
		t.Error("expected latest activity line")
	}
	if !strings.Contains(out, "committed and pushed fixes") {
		t.Error("expected last event text")
	}
}

func TestModel_View_BabysitViewWhenActive(t *testing.T) {
	run := testRunWithBabysit()
	m := NewModel("/tmp/sock", nil, run)
	m.steps = run.Steps
	m.steps[5].Status = types.StepStatusRunning
	m.logs = []string{"babysitting PR #42 (timeout: 4h)..."}

	view := m.View()

	if !strings.Contains(view, "Babysit Monitor") {
		t.Error("expected babysit view in model output")
	}
	if !strings.Contains(view, "Monitoring") {
		t.Error("expected monitoring state in model output")
	}
}

func TestModel_View_BabysitAwaitingShowsFindings(t *testing.T) {
	run := testRunWithBabysit()
	m := NewModel("/tmp/sock", nil, run)
	m.steps = run.Steps
	m.steps[5].Status = types.StepStatusAwaitingApproval
	m.stepFindings[types.StepBabysit] = `{"findings":[{"severity":"info","description":"@bob: fix the typo"}],"summary":"1 comment"}`

	view := m.View()

	if !strings.Contains(view, "Babysit Monitor") {
		t.Error("expected babysit view header")
	}
	if !strings.Contains(view, "fix the typo") {
		t.Error("expected comment finding in babysit view")
	}
}

func TestModel_View_NonBabysitStepUsesGenericFindings(t *testing.T) {
	run := testRun() // no babysit step
	m := NewModel("/tmp/sock", nil, run)
	m.steps[0].Status = types.StepStatusAwaitingApproval
	m.stepFindings[types.StepReview] = `{"findings":[{"severity":"error","description":"critical bug"}],"summary":"1 issue"}`

	view := m.View()

	// Should use generic findings, not babysit view.
	if strings.Contains(view, "Babysit Monitor") {
		t.Error("expected generic findings view, not babysit view")
	}
	if !strings.Contains(view, "critical bug") {
		t.Error("expected generic findings content")
	}
}

func TestNewModel_PopulatesStepFindingsFromInitialSteps(t *testing.T) {
	findings := `{"findings":[{"severity":"warning","description":"potential null deref"}],"summary":"1 issue"}`
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

	// stepFindings should be populated from the initial steps' FindingsJSON.
	got, ok := m.stepFindings[types.StepReview]
	if !ok {
		t.Fatal("expected stepFindings to contain review step findings")
	}
	if got != findings {
		t.Errorf("stepFindings[review] = %q, want %q", got, findings)
	}
	// Step without findings should not appear in the map.
	if _, ok := m.stepFindings[types.StepTest]; ok {
		t.Error("expected stepFindings to NOT contain test step (no findings)")
	}
}

// --- Boxed section tests ---

func TestRenderBox_HasRoundedCorners(t *testing.T) {
	out := renderBox("Title", "content", 40)
	if !strings.Contains(out, "╭") || !strings.Contains(out, "╮") {
		t.Error("expected rounded top corners ╭ and ╮")
	}
	if !strings.Contains(out, "╰") || !strings.Contains(out, "╯") {
		t.Error("expected rounded bottom corners ╰ and ╯")
	}
}

func TestRenderBox_TitleInTopBorder(t *testing.T) {
	out := stripANSI(renderBox("Pipeline", "step content", 40))
	lines := strings.Split(out, "\n")
	if len(lines) == 0 {
		t.Fatal("expected non-empty output")
	}
	if !strings.Contains(lines[0], "Pipeline") {
		t.Errorf("expected title 'Pipeline' in top border line, got %q", lines[0])
	}
}

func TestRenderBox_ContentInsideBorders(t *testing.T) {
	out := stripANSI(renderBox("Test", "hello world", 40))
	lines := strings.Split(out, "\n")
	// Find content line (between top and bottom border).
	foundContent := false
	for _, line := range lines[1:] {
		if strings.Contains(line, "hello world") {
			foundContent = true
			// Content lines should start with │.
			if !strings.HasPrefix(strings.TrimSpace(line), "│") {
				t.Errorf("expected content line to start with │, got %q", line)
			}
			break
		}
	}
	if !foundContent {
		t.Error("expected 'hello world' inside box")
	}
}

func TestRenderBox_HorizontalPadding(t *testing.T) {
	out := stripANSI(renderBox("Test", "X", 20))
	lines := strings.Split(out, "\n")
	for _, line := range lines {
		if strings.Contains(line, "X") {
			// Content should have at least 1 space padding from border.
			if strings.Contains(line, "│X") || strings.Contains(line, "X│") {
				t.Errorf("expected horizontal padding between content and border, got %q", line)
			}
			break
		}
	}
}

func TestRenderBox_FillsWidth(t *testing.T) {
	out := stripANSI(renderBox("Title", "content", 50))
	lines := strings.Split(out, "\n")
	for _, line := range lines {
		if line == "" {
			continue
		}
		w := lipgloss.Width(line)
		if w != 50 {
			t.Errorf("expected line width 50, got %d for line %q", w, line)
		}
	}
}

func TestRenderBox_MultilineContent(t *testing.T) {
	out := stripANSI(renderBox("Test", "line1\nline2\nline3", 40))
	if !strings.Contains(out, "line1") || !strings.Contains(out, "line2") || !strings.Contains(out, "line3") {
		t.Error("expected all content lines in output")
	}
}

func TestRenderPipelineView_WrappedInBox(t *testing.T) {
	run := testRun()
	run.Steps[0].Status = types.StepStatusCompleted
	run.Steps[0].DurationMS = ptr(int64(1200))
	run.Steps[1].Status = types.StepStatusRunning

	out := stripANSI(renderPipelineView(run, run.Steps, 80, 0, false, false))
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

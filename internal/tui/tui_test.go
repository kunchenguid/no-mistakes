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
	out := renderPipelineView(nil, nil, 80, 0)
	if out != "No active run." {
		t.Errorf("expected 'No active run.', got %q", out)
	}
}

func TestRenderPipelineView_ShowsSteps(t *testing.T) {
	run := testRun()
	run.Steps[0].Status = types.StepStatusCompleted
	run.Steps[0].DurationMS = ptr(int64(1200))
	run.Steps[1].Status = types.StepStatusRunning

	out := renderPipelineView(run, run.Steps, 80, 0)
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

	out := stripANSI(renderPipelineView(run, run.Steps, 80, 0))
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

	// Action bar is now rendered outside the pipeline box per DESIGN.md.
	out := renderActionBar(run.Steps, true, true, false, 5, 5)
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
	out := renderPipelineView(run, run.Steps, 80, 0)
	if !strings.Contains(out, "something broke") {
		t.Error("expected error message in output")
	}
}

func TestRenderPipelineView_StepError(t *testing.T) {
	run := testRun()
	run.Steps[1].Status = types.StepStatusFailed
	run.Steps[1].Error = ptr("tests failed")

	out := renderPipelineView(run, run.Steps, 80, 0)
	if !strings.Contains(out, "tests failed") {
		t.Error("expected step error in output")
	}
}

func TestRenderApprovalActions_FormatWithSeparator(t *testing.T) {
	out := stripANSI(renderApprovalActions(true, true, false, 5, 5))
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
	out := stripANSI(renderApprovalActions(false, true, false, 5, 5))
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

	// Action bar is now rendered outside the pipeline box per DESIGN.md.
	out := stripANSI(renderActionBar(run.Steps, true, false, false, 0, 5))
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

	out := stripANSI(renderPipelineView(run, run.Steps, 80, 0))
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
	if got := renderDiff("", 80, 20, 0, ""); got != "" {
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
	got := renderDiff(raw, 80, 0, 0, "")
	if !strings.Contains(got, "1 file") {
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
	got := renderDiff(raw, 80, 0, 0, "")
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
	got := renderDiff(raw, 80, 5, 2, "")

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
	got := renderDiff(raw, 80, 3, 3, "")

	// Should show scroll-up indicator since we scrolled past start.
	if !strings.Contains(got, "↑") {
		t.Error("expected ↑ scroll indicator when scrolled to end")
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
	got := stripANSI(renderDiff(raw, 80, 0, 0, ""))
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

	got := stripANSI(renderDiff(b.String(), 80, 5, 0, ""))
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
	if !strings.Contains(view, "1 file") {
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
	// Action bar is now rendered outside the pipeline box per DESIGN.md.
	out := stripANSI(renderActionBar(run.Steps, true, true, false, 5, 5))
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

	if !strings.Contains(stripANSI(out), "Babysit") {
		t.Error("expected Babysit box title")
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

	if !strings.Contains(stripANSI(view), "Babysit") {
		t.Error("expected babysit box in model output")
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

	if !strings.Contains(stripANSI(view), "Babysit") {
		t.Error("expected babysit box title")
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
	// Check that no "Babysit" titled box appears (only Pipeline/Findings boxes).
	hasBabysitBox := false
	for _, line := range strings.Split(stripANSI(view), "\n") {
		if strings.Contains(line, "╭") && strings.Contains(line, "Babysit") {
			hasBabysitBox = true
		}
	}
	if hasBabysitBox {
		t.Error("expected generic findings view, not babysit box")
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

	out := stripANSI(renderPipelineView(run, run.Steps, 80, 0))
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
	got := stripANSI(renderFindingsWithSelection(raw, 80, 0, allSelected, 0))

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
	got := stripANSI(renderFindingsWithSelection(raw, 80, 0, selected, 0))

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

func TestRenderBabysitView_WrappedInBox(t *testing.T) {
	run := testRunWithBabysit()
	run.Steps[5].Status = types.StepStatusRunning
	logs := []string{"babysitting PR #42 (timeout: 4h)..."}

	out := stripANSI(renderBabysitView(run, run.Steps, "", logs, 80))

	// Should be wrapped in a box with "Babysit" title per DESIGN.md.
	lines := strings.Split(out, "\n")
	if len(lines) == 0 {
		t.Fatal("expected non-empty output")
	}
	if !strings.Contains(lines[0], "Babysit") {
		t.Errorf("expected 'Babysit' title in top border, got %q", lines[0])
	}
	if !strings.Contains(lines[0], "╭") {
		t.Error("expected rounded top-left corner in babysit box")
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
		t.Error("expected rounded bottom border in babysit box")
	}
}

func TestRenderBabysitView_NoRedundantHeader(t *testing.T) {
	// The box title "Babysit" replaces the old "◉ Babysit Monitor" header.
	run := testRunWithBabysit()
	run.Steps[5].Status = types.StepStatusRunning
	logs := []string{"babysitting PR #42 (timeout: 4h)..."}

	out := stripANSI(renderBabysitView(run, run.Steps, "", logs, 80))

	if strings.Contains(out, "Babysit Monitor") {
		t.Error("expected no redundant 'Babysit Monitor' header - box title handles it")
	}
}

func TestRenderBabysitView_ContentInsideBox(t *testing.T) {
	run := testRunWithBabysit()
	run.Steps[5].Status = types.StepStatusRunning
	logs := []string{"babysitting PR #42 (timeout: 4h)..."}

	out := stripANSI(renderBabysitView(run, run.Steps, "", logs, 80))

	// PR info and state should be inside box borders.
	foundPR := false
	foundState := false
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "PR #42") && strings.Contains(line, "│") {
			foundPR = true
		}
		if strings.Contains(line, "Monitoring") && strings.Contains(line, "│") {
			foundState = true
		}
	}
	if !foundPR {
		t.Error("expected PR info inside box borders")
	}
	if !foundState {
		t.Error("expected state indicator inside box borders")
	}
}

func TestModel_View_BabysitViewInBox(t *testing.T) {
	run := testRunWithBabysit()
	m := NewModel("/tmp/sock", nil, run)
	m.steps = run.Steps
	m.steps[5].Status = types.StepStatusRunning
	m.logs = []string{"babysitting PR #42 (timeout: 4h)..."}
	m.width = 80

	view := stripANSI(m.View())

	// The babysit section should be in a box with "Babysit" title.
	hasBabysitBox := false
	for _, line := range strings.Split(view, "\n") {
		if strings.Contains(line, "╭") && strings.Contains(line, "Babysit") && !strings.Contains(line, "Pipeline") {
			hasBabysitBox = true
			break
		}
	}
	if !hasBabysitBox {
		t.Error("expected 'Babysit' titled box in full model view")
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

// Spacing between Pipeline and Babysit boxes should also have 1 blank line.
func TestModel_View_OneBlankLineBetweenPipelineAndBabysit(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	defer lipgloss.SetColorProfile(termenv.ANSI)

	run := &ipc.RunInfo{
		ID: "run-001", RepoID: "repo-001", Branch: "main", HeadSHA: "abc123", BaseSHA: "000000",
		Status: types.RunRunning,
		Steps: []ipc.StepResultInfo{
			{ID: "s1", StepName: types.StepReview, StepOrder: 1, Status: types.StepStatusCompleted},
			{ID: "s2", StepName: types.StepBabysit, StepOrder: 2, Status: types.StepStatusRunning},
		},
	}
	m := NewModel("/tmp/sock", nil, run)
	m.width = 60
	m.logs = []string{"babysitting PR #42"}

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
	result := renderDiff(raw, 80, 20, 0, "")
	plain := stripANSI(result)
	if !strings.Contains(plain, "2 files") {
		t.Errorf("expected '2 files' (plural) for multiple files, got: %s", plain)
	}

	// Single file should say "file"
	raw2 := "diff --git a/a.go b/a.go\n--- a/a.go\n+++ b/a.go\n@@ -1 +1 @@\n-old\n+new\n"
	result2 := renderDiff(raw2, 80, 20, 0, "")
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
	result := renderDiff(raw, 80, 20, 0, "")
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
	got := stripANSI(renderDiff(b.String(), 80, 5, 5, ""))
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
	got := stripANSI(renderDiff(b.String(), 80, 5, 4, ""))
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

	got := renderPipelineView(run, run.Steps, 80, 0)

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

	got := renderPipelineView(run, run.Steps, 80, 0)

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

	got := renderFindingsWithSelection(raw, 80, 0, selected, 0)

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

	got := renderFindingsWithSelection(raw, 80, 0, selected, 0)

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

	got := renderFindingsWithSelection(raw, 80, 0, selected, 0)

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

func TestDiffBoxTitle_IncludesStepName(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI)
	run := testRun()
	run.Steps[0].Status = types.StepStatusCompleted
	run.Steps[1].Status = types.StepStatusAwaitingApproval

	m := NewModel("/tmp/sock", nil, run)
	m.width = 80
	m.height = 40
	m.stepDiffs[types.StepTest] = "diff --git a/foo.go b/foo.go\n--- a/foo.go\n+++ b/foo.go\n@@ -1 +1 @@\n-old\n+new\n"
	m.showDiff = true
	view := m.View()
	plain := stripANSI(view)

	// The diff box title should include the step name, e.g. "Diff - Test".
	if !strings.Contains(plain, "Diff - Test") {
		t.Errorf("expected diff box title to include step name 'Diff - Test', got:\n%s", plain)
	}
}

func TestFindingsBoxTitle_IncludesStepName(t *testing.T) {
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

	// The findings box title should include the step name, e.g. "Findings - Test".
	if !strings.Contains(plain, "Findings - Test") {
		t.Errorf("expected findings box title to include step name 'Findings - Test', got:\n%s", plain)
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

func makeManyFindings(n int) string {
	var items []string
	for i := 1; i <= n; i++ {
		items = append(items, fmt.Sprintf(`{"id":"f%d","severity":"warning","file":"file%d.go","line":%d,"description":"finding %d description"}`, i, i, i, i))
	}
	return fmt.Sprintf(`{"summary":"test summary","items":[%s]}`, strings.Join(items, ","))
}

func TestRenderFindings_ViewportShowsSubset(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI)
	// 10 findings, viewport fits 4 items, cursor at 0 (top).
	raw := makeManyFindings(10)
	selected := map[string]bool{}
	for i := 1; i <= 10; i++ {
		selected[fmt.Sprintf("f%d", i)] = true
	}

	out := renderFindingsWithSelection(raw, 80, 0, selected, 4)
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

	out := renderFindingsWithSelection(raw, 80, 0, selected, 4)
	plain := stripANSI(out)

	// Should show down indicator for 6 more items below.
	if !strings.Contains(plain, "↓ 6 more") {
		t.Errorf("expected '↓ 6 more' scroll indicator, got:\n%s", plain)
	}
}

func TestRenderFindings_ViewportScrollUpIndicator(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI)
	raw := makeManyFindings(10)
	selected := map[string]bool{}
	for i := 1; i <= 10; i++ {
		selected[fmt.Sprintf("f%d", i)] = true
	}

	// Cursor at item 9 (index 9, last item) - should show up indicator.
	out := renderFindingsWithSelection(raw, 80, 9, selected, 4)
	plain := stripANSI(out)

	// Should show finding 10 (cursor is on it).
	if !strings.Contains(plain, "finding 10 description") {
		t.Errorf("expected finding 10 visible at cursor=9, got:\n%s", plain)
	}
	// Should show up indicator for items above.
	if !strings.Contains(plain, "↑") {
		t.Errorf("expected up scroll indicator when cursor at bottom, got:\n%s", plain)
	}
	// Should NOT show down indicator when at bottom.
	if strings.Contains(plain, "↓") {
		t.Errorf("should not show down indicator at bottom, got:\n%s", plain)
	}
}

func TestFindingsBoxTitle_ShowsPositionIndicator(t *testing.T) {
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

	// Move cursor to 3rd item (index 2).
	m.findingCursor[types.StepTest] = 2

	view := m.View()
	plain := stripANSI(view)

	// The findings box title should show position: "Findings - Test (3/10)".
	if !strings.Contains(plain, "Findings - Test (3/10)") {
		t.Errorf("expected findings box title with position '(3/10)', got:\n%s", plain)
	}
}

func TestFindingsBoxTitle_PositionUpdatesWithCursor(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI)
	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval

	findingsJSON := makeManyFindings(5)
	m := NewModel("/tmp/sock", nil, run)
	m.width = 80
	m.height = 40
	m.stepFindings[types.StepReview] = findingsJSON
	m.resetFindingSelection(types.StepReview)

	// Cursor at first item (index 0) -> should show (1/5).
	m.findingCursor[types.StepReview] = 0
	view := m.View()
	plain := stripANSI(view)
	if !strings.Contains(plain, "(1/5)") {
		t.Errorf("expected position (1/5) at cursor 0, got:\n%s", plain)
	}

	// Cursor at last item (index 4) -> should show (5/5).
	m.findingCursor[types.StepReview] = 4
	view = m.View()
	plain = stripANSI(view)
	if !strings.Contains(plain, "(5/5)") {
		t.Errorf("expected position (5/5) at cursor 4, got:\n%s", plain)
	}
}

func TestFindingsBoxTitle_SingleFinding(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI)
	run := testRun()
	run.Steps[0].Status = types.StepStatusCompleted
	run.Steps[1].Status = types.StepStatusAwaitingApproval

	findingsJSON := `{"summary":"one issue","items":[{"id":"f1","severity":"error","file":"foo.go","line":1,"description":"bad"}]}`
	m := NewModel("/tmp/sock", nil, run)
	m.width = 80
	m.height = 40
	m.stepFindings[types.StepTest] = findingsJSON
	m.resetFindingSelection(types.StepTest)

	view := m.View()
	plain := stripANSI(view)

	// Single finding: should show (1/1).
	if !strings.Contains(plain, "(1/1)") {
		t.Errorf("expected position (1/1) for single finding, got:\n%s", plain)
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
	out := renderDiff(raw, 80, 10, 0, "Review")
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
	out := renderDiff(raw, 80, 10, 15, "Test")
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
	out := renderDiff(raw, 80, 0, 0, "Review")
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

// --- Babysit box title position indicator tests ---

func TestRenderBabysitView_TitleShowsPositionWhenFindings(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI)
	run := testRunWithBabysit()
	run.Steps[5].Status = types.StepStatusAwaitingApproval
	findings := `{"findings":[
		{"id":"f1","severity":"info","description":"comment 1"},
		{"id":"f2","severity":"info","description":"comment 2"},
		{"id":"f3","severity":"info","description":"comment 3"}
	],"summary":"3 comments"}`

	out := stripANSI(renderBabysitViewWithSelection(run, run.Steps, findings, nil, 80, 0, 1, nil))

	if !strings.Contains(out, "Babysit (2/3)") {
		t.Errorf("expected position indicator 'Babysit (2/3)' in title, got:\n%s", out)
	}
}

func TestRenderBabysitView_TitleNoPositionWithoutFindings(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI)
	run := testRunWithBabysit()
	run.Steps[5].Status = types.StepStatusRunning
	logs := []string{"babysitting PR #42 (timeout: 4h)..."}

	out := stripANSI(renderBabysitView(run, run.Steps, "", logs, 80))
	lines := strings.Split(out, "\n")

	// Title should be just "Babysit" without any position indicator.
	titleLine := lines[0]
	if !strings.Contains(titleLine, "Babysit") {
		t.Error("expected Babysit in title")
	}
	if strings.Contains(titleLine, "(") {
		t.Errorf("expected no position indicator when no findings, got: %s", titleLine)
	}
}

func TestRenderBabysitView_PositionUpdatesWithCursor(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI)
	run := testRunWithBabysit()
	run.Steps[5].Status = types.StepStatusAwaitingApproval
	findings := `{"findings":[
		{"id":"f1","severity":"info","description":"c1"},
		{"id":"f2","severity":"info","description":"c2"},
		{"id":"f3","severity":"info","description":"c3"},
		{"id":"f4","severity":"info","description":"c4"},
		{"id":"f5","severity":"info","description":"c5"}
	],"summary":"5 comments"}`

	// Cursor at start.
	out1 := stripANSI(renderBabysitViewWithSelection(run, run.Steps, findings, nil, 80, 0, 0, nil))
	if !strings.Contains(out1, "Babysit (1/5)") {
		t.Errorf("expected 'Babysit (1/5)' at start, got:\n%s", out1)
	}

	// Cursor at end.
	out2 := stripANSI(renderBabysitViewWithSelection(run, run.Steps, findings, nil, 80, 0, 4, nil))
	if !strings.Contains(out2, "Babysit (5/5)") {
		t.Errorf("expected 'Babysit (5/5)' at end, got:\n%s", out2)
	}
}

func TestModel_View_BabysitFindingsViewportApplied(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI)
	run := testRunWithBabysit()
	m := NewModel("/tmp/sock", nil, run)
	m.steps = run.Steps
	m.steps[5].Status = types.StepStatusAwaitingApproval

	// Create 10 findings.
	var items []string
	for i := 1; i <= 10; i++ {
		items = append(items, fmt.Sprintf(`{"id":"f%d","severity":"info","description":"comment %d"}`, i, i))
	}
	m.stepFindings[types.StepBabysit] = `{"findings":[` + strings.Join(items, ",") + `],"summary":"10 comments"}`
	m.resetFindingSelection(types.StepBabysit)

	// Set a terminal height that forces viewport (height - 25 reserve = 15, /3 = 5 max visible).
	m.width = 80
	m.height = 40

	view := stripANSI(m.View())

	// With height=30, not all 10 findings should be visible.
	// Verify scroll indicators appear.
	if !strings.Contains(view, "more below") {
		t.Errorf("expected '↓ N more below' scroll indicator when findings overflow viewport, got:\n%s", view)
	}
}

func TestRenderBabysitView_LogTailDuringMonitoring(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI)
	run := testRunWithBabysit()
	run.Steps[5].Status = types.StepStatusRunning
	logs := []string{
		"babysitting PR #42 (timeout: 4h)...",
		"polling CI status...",
		"all checks passing",
	}

	out := stripANSI(renderBabysitView(run, run.Steps, "", logs, 80))

	// Log tail lines should appear inside the babysit box during monitoring.
	if !strings.Contains(out, "polling CI status") {
		t.Errorf("expected log tail line 'polling CI status' inside babysit box, got:\n%s", out)
	}
	if !strings.Contains(out, "all checks passing") {
		t.Errorf("expected log tail line 'all checks passing' inside babysit box, got:\n%s", out)
	}
}

func TestRenderBabysitView_NoLogTailDuringApproval(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI)
	run := testRunWithBabysit()
	run.Steps[5].Status = types.StepStatusAwaitingApproval
	logs := []string{
		"babysitting PR #42 (timeout: 4h)...",
		"polling CI status...",
		"all checks passing",
	}
	findings := `{"findings":[{"severity":"info","description":"@bob: fix the typo"}],"summary":"1 comment"}`

	out := stripANSI(renderBabysitViewWithSelection(run, run.Steps, findings, logs, 80, 0, 0, nil))

	// During approval, log tail should NOT appear - findings take priority.
	if strings.Contains(out, "polling CI status") {
		t.Error("expected no log tail lines inside babysit box during approval state")
	}
	// But findings should still show.
	if !strings.Contains(out, "fix the typo") {
		t.Error("expected findings to still show during approval")
	}
}

func TestModel_View_NoStandaloneLogBoxDuringBabysit(t *testing.T) {
	lipgloss.SetColorProfile(termenv.ANSI)
	run := testRunWithBabysit()
	m := NewModel("/tmp/sock", nil, run)
	m.steps = run.Steps
	m.steps[5].Status = types.StepStatusRunning
	m.logs = []string{"babysitting PR #42", "polling CI", "checks passing"}
	m.width = 80

	view := stripANSI(m.View())

	// The standalone Log box should NOT appear when babysit is active.
	hasStandaloneLogBox := false
	for _, line := range strings.Split(view, "\n") {
		if strings.Contains(line, "╭") && strings.Contains(line, "Log") && !strings.Contains(line, "Babysit") {
			hasStandaloneLogBox = true
		}
	}
	if hasStandaloneLogBox {
		t.Error("expected no standalone Log box when babysit is active - logs should be inside babysit box")
	}

	// But log content should still be visible (inside babysit box).
	if !strings.Contains(view, "checks passing") {
		t.Error("expected log content to appear inside babysit box")
	}
}

// --- Action Bar placement per DESIGN.md ---
// DESIGN.md: Action bar "Sits below the pipeline box, above findings/diff"

func TestActionBar_OutsidePipelineBox(t *testing.T) {
	// The pipeline box should NOT contain action bar keys when a step is awaiting approval.
	// The action bar should be rendered separately, outside the box.
	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval

	pipelineOut := stripANSI(renderPipelineView(run, run.Steps, 80, 0))

	// The pipeline box content (between ╭ and ╰) should NOT contain action bar keys.
	if strings.Contains(pipelineOut, "a approve") {
		t.Error("action bar keys should NOT be inside the pipeline box - DESIGN.md says it sits below")
	}
	if strings.Contains(pipelineOut, "awaiting action") {
		t.Error("approval prompt should NOT be inside the pipeline box - DESIGN.md says it sits below")
	}
}

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

	// It should NOT be inside the pipeline box.
	pipelineOut := stripANSI(renderPipelineView(run, run.Steps, 80, 0))
	if strings.Contains(pipelineOut, "awaiting action") {
		t.Error("'awaiting action' label should not be inside the pipeline box")
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

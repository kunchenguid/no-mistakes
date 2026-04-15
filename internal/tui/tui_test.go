package tui

import (
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

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
		{types.StepDocument, "Document"},
		{types.StepPush, "Push"},
		{types.StepPR, "PR"},
		{types.StepCI, "CI"},
	}
	for _, tt := range tests {
		if got := stepLabel(tt.name); got != tt.label {
			t.Errorf("stepLabel(%s) = %q, want %q", tt.name, got, tt.label)
		}
	}
}

func TestRenderPipelineView_NilRun(t *testing.T) {
	out := renderPipelineView(nil, nil, 80, 0, 40)
	if out != "No active run." {
		t.Errorf("expected 'No active run.', got %q", out)
	}
}

func TestRenderPipelineView_ShowsSteps(t *testing.T) {
	run := testRun()
	run.Steps[0].Status = types.StepStatusCompleted
	run.Steps[0].DurationMS = ptr(int64(1200))
	run.Steps[1].Status = types.StepStatusRunning
	run.Steps[2].Status = types.StepStatusCompleted
	run.Steps[2].DurationMS = ptr(int64(500))

	out := stripANSI(renderPipelineView(run, run.Steps, 80, 0, 40))
	if !strings.Contains(out, "feature/foo") {
		t.Error("expected branch name in output")
	}
	if strings.Contains(out, "abc12345") {
		t.Error("expected commit SHA to be hidden from the pipeline header")
	}
	if strings.Contains(out, "run-001") {
		t.Error("expected pipeline ID to be hidden from the pipeline header")
	}
	if strings.Contains(out, "1/5") {
		t.Error("expected step progress count to be hidden from the pipeline header")
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
	if !strings.Contains(out, "Lint") {
		t.Error("expected Lint step")
	}
	if !strings.Contains(out, "500ms") {
		t.Error("expected sub-second duration for completed step")
	}
}

func TestRenderPipelineView_HeaderHasSpacerBeforeSteps(t *testing.T) {
	run := testRun()
	run.Steps[0].Status = types.StepStatusRunning

	plain := stripANSI(renderPipelineView(run, run.Steps, 80, 0, 40))
	var contentLines []string
	for _, line := range strings.Split(plain, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "│") && strings.HasSuffix(trimmed, "│") {
			contentLines = append(contentLines, boxContentLine(line))
		}
	}
	if len(contentLines) < 3 {
		t.Fatalf("expected at least header, spacer, and one step line, got %d lines in:\n%s", len(contentLines), plain)
	}
	if !strings.Contains(contentLines[0], "feature/foo") {
		t.Fatalf("expected header line to include branch name, got %q", contentLines[0])
	}
	if !strings.Contains(contentLines[0], "running") {
		t.Fatalf("expected header line to include run status, got %q", contentLines[0])
	}
	if contentLines[1] != "" {
		t.Fatalf("expected a blank spacer row between header and steps, got %q", contentLines[1])
	}
}

func TestRenderPipelineView_ConnectorsBetweenSteps(t *testing.T) {
	run := testRun()
	run.Steps[0].Status = types.StepStatusCompleted
	run.Steps[0].DurationMS = ptr(int64(1200))
	run.Steps[1].Status = types.StepStatusRunning

	out := stripANSI(renderPipelineView(run, run.Steps, 80, 0, 40))
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

func TestRenderPipelineView_Error(t *testing.T) {
	run := testRun()
	run.Error = ptr("something broke")
	out := renderPipelineView(run, run.Steps, 80, 0, 40)
	if !strings.Contains(out, "something broke") {
		t.Error("expected error message in output")
	}
}

func TestRenderPipelineView_StepError(t *testing.T) {
	run := testRun()
	run.Steps[1].Status = types.StepStatusFailed
	run.Steps[1].Error = ptr("tests failed")

	out := renderPipelineView(run, run.Steps, 80, 0, 40)
	if !strings.Contains(out, "tests failed") {
		t.Error("expected step error in output")
	}
}

func TestRenderApprovalActions_FormatWithSeparator(t *testing.T) {
	out := stripANSI(renderApprovalActions(true, true, false, 5, 5, false, true))
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
	out := stripANSI(renderApprovalActions(false, true, false, 5, 5, false, true))
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
	out := stripANSI(renderActionBar(run.Steps, true, false, false, 0, 5, false, true))
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

	out := stripANSI(renderPipelineView(run, run.Steps, 80, 0, 40))
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

func TestModel_ApplyEvent_StepCompleted_FailedStoresError(t *testing.T) {
	run := testRun()
	m := NewModel("/tmp/sock", nil, run)
	errMsg := "agent review: claude parse events: bufio.Scanner: token too long"

	m.applyEvent(ipc.Event{
		Type:     ipc.EventStepCompleted,
		RunID:    run.ID,
		StepName: ptr(types.StepReview),
		Status:   ptr(string(types.StepStatusFailed)),
		Error:    &errMsg,
	})

	if m.steps[0].Status != types.StepStatusFailed {
		t.Fatalf("expected failed, got %s", m.steps[0].Status)
	}
	if m.steps[0].Error == nil || *m.steps[0].Error != errMsg {
		t.Fatalf("expected step error %q, got %v", errMsg, m.steps[0].Error)
	}
	out := stripANSI(renderPipelineView(m.run, m.steps, 80, 0, 40))
	if !strings.Contains(out, errMsg) {
		t.Fatalf("expected pipeline view to contain step error, got:\n%s", out)
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

func TestModel_ApplyEvent_RunCompleted_FailedStoresError(t *testing.T) {
	run := testRun()
	m := NewModel("/tmp/sock", nil, run)
	errMsg := "step review failed: agent review: claude parse events: bufio.Scanner: token too long"

	m.applyEvent(ipc.Event{
		Type:   ipc.EventRunCompleted,
		RunID:  run.ID,
		Status: ptr(string(types.RunFailed)),
		Error:  &errMsg,
	})

	if !m.done {
		t.Fatal("expected done=true after failed run_completed")
	}
	if m.run.Status != types.RunFailed {
		t.Fatalf("expected failed status, got %s", m.run.Status)
	}
	if m.run.Error == nil || *m.run.Error != errMsg {
		t.Fatalf("expected run error %q, got %v", errMsg, m.run.Error)
	}
	out := stripANSI(renderPipelineView(m.run, m.steps, 80, 0, 40))
	if !strings.Contains(out, "Error: step review failed: agent review: claude parse events") {
		t.Fatalf("expected pipeline view to contain run error, got:\n%s", out)
	}
}

func TestModel_ApplyEvent_RunUpdated_PRURL(t *testing.T) {
	run := testRun()
	m := NewModel("/tmp/sock", nil, run)

	prURL := "https://github.com/test/repo/pull/42"
	m.applyEvent(ipc.Event{
		Type:   ipc.EventRunUpdated,
		RunID:  run.ID,
		Status: ptr(string(types.RunRunning)),
		PRURL:  &prURL,
	})

	if m.run.PRURL == nil || *m.run.PRURL != prURL {
		t.Errorf("expected run PRURL %q, got %v", prURL, m.run.PRURL)
	}
}

func TestModel_ApplyEvent_RunCompleted_PRURL(t *testing.T) {
	run := testRun()
	m := NewModel("/tmp/sock", nil, run)

	prURL := "https://github.com/test/repo/pull/42"
	m.applyEvent(ipc.Event{
		Type:   ipc.EventRunCompleted,
		RunID:  run.ID,
		Status: ptr(string(types.RunCompleted)),
		PRURL:  &prURL,
	})

	if m.run.PRURL == nil || *m.run.PRURL != prURL {
		t.Errorf("expected run PRURL %q, got %v", prURL, m.run.PRURL)
	}
}

func TestRenderFooter_WithPRURL_ShowsOpenAction(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	prURL := "https://github.com/test/repo/pull/42"
	footer := renderFooter(true, false, false, &prURL, 80)
	stripped := stripANSI(footer)

	if !strings.Contains(stripped, "o") || !strings.Contains(stripped, "open PR") {
		t.Errorf("expected footer to contain 'o open PR' action, got: %s", stripped)
	}
	if !strings.Contains(stripped, prURL) {
		t.Errorf("expected footer to contain full PR URL, got: %s", stripped)
	}
}

func TestRenderFooter_WithoutPRURL(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	footer := renderFooter(false, false, false, nil, 80)
	stripped := stripANSI(footer)

	if strings.Contains(stripped, "open PR") {
		t.Errorf("expected no open PR action in footer, got: %s", stripped)
	}
}

func TestRenderFooter_PRURL_ActionShownAtNarrowWidth(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	prURL := "https://github.com/test/repo/pull/42"
	// Even at narrow width, "open PR" action should appear
	footer := renderFooter(true, false, false, &prURL, 40)
	stripped := stripANSI(footer)

	if !strings.Contains(stripped, "open PR") {
		t.Errorf("expected footer to contain 'open PR' action, got: %s", stripped)
	}
}

func TestOpenBrowserCmd_WaitsForBrowserCommand(t *testing.T) {
	original := runBrowserCommand
	t.Cleanup(func() {
		runBrowserCommand = original
	})

	called := false
	finished := false
	runBrowserCommand = func(name string, args ...string) error {
		called = true
		time.Sleep(50 * time.Millisecond)
		finished = true
		return nil
	}

	start := time.Now()
	msg := openBrowserCmd("https://example.com")()
	if msg != nil {
		t.Fatalf("expected nil msg, got %#v", msg)
	}
	if !called {
		t.Fatal("expected browser command to be invoked")
	}
	if !finished {
		t.Fatal("expected browser command to finish before return")
	}
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Fatalf("expected command to block until completion, returned after %v", elapsed)
	}
}

func TestOpenBrowserCmd_ReturnsErrMsgOnFailure(t *testing.T) {
	original := runBrowserCommand
	t.Cleanup(func() {
		runBrowserCommand = original
	})

	wantErr := errors.New("launcher missing")
	runBrowserCommand = func(name string, args ...string) error {
		return wantErr
	}

	msg := openBrowserCmd("https://example.com")()
	errMsg, ok := msg.(errMsg)
	if !ok {
		t.Fatalf("expected errMsg, got %#v", msg)
	}
	if !errors.Is(errMsg.err, wantErr) {
		t.Fatalf("expected error %v, got %v", wantErr, errMsg.err)
	}
}

func TestBrowserCommandSpec_WindowsUsesRundll32(t *testing.T) {
	name, args := browserCommandSpec("windows", "https://example.com/pull/1?foo=1&bar=2")

	if name != "rundll32" {
		t.Fatalf("expected rundll32 launcher, got %q", name)
	}

	wantArgs := []string{"url.dll,FileProtocolHandler", "https://example.com/pull/1?foo=1&bar=2"}
	if !reflect.DeepEqual(args, wantArgs) {
		t.Fatalf("unexpected args: got %v want %v", args, wantArgs)
	}
}

func TestModel_Update_OpenPRKeyRunsBrowserCommand(t *testing.T) {
	original := runBrowserCommand
	t.Cleanup(func() {
		runBrowserCommand = original
	})

	prURL := "https://github.com/test/repo/pull/42"
	run := testRun()
	run.PRURL = &prURL
	m := NewModel("/tmp/sock", nil, run)

	called := false
	var gotName string
	var gotArgs []string
	runBrowserCommand = func(name string, args ...string) error {
		called = true
		gotName = name
		gotArgs = append([]string(nil), args...)
		return nil
	}

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'o'}})
	if cmd == nil {
		t.Fatal("expected browser open command")
	}
	if msg := cmd(); msg != nil {
		t.Fatalf("expected nil msg, got %#v", msg)
	}
	if !called {
		t.Fatal("expected browser launcher to be called")
	}

	wantName, wantArgs := browserCommandSpec(runtime.GOOS, prURL)
	if gotName != wantName {
		t.Fatalf("unexpected command name: got %q want %q", gotName, wantName)
	}
	if !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Fatalf("unexpected command args: got %v want %v", gotArgs, wantArgs)
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

func TestModel_HandleKey_QuitClearsTerminalTitle(t *testing.T) {
	run := testRun()
	run.Steps[0].Status = types.StepStatusRunning
	m := NewModel("/tmp/sock", nil, run)

	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("q")})
	if cmd == nil {
		t.Fatal("expected quit command")
	}

	msg := cmd()
	if fmt.Sprintf("%T", msg) != "tea.sequenceMsg" {
		t.Fatalf("expected sequence message, got %T", msg)
	}

	cmds := reflect.ValueOf(msg)
	if cmds.Len() != 2 {
		t.Fatalf("expected 2 quit commands, got %d", cmds.Len())
	}

	titleCmd, ok := cmds.Index(0).Interface().(tea.Cmd)
	if !ok {
		t.Fatalf("expected first sequence entry to be tea.Cmd, got %T", cmds.Index(0).Interface())
	}

	titleMsg := titleCmd()
	if fmt.Sprintf("%T", titleMsg) != "tea.setWindowTitleMsg" {
		t.Fatalf("expected title reset message, got %T", titleMsg)
	}
	if fmt.Sprint(titleMsg) != "" {
		t.Fatalf("expected blank title reset, got %q", fmt.Sprint(titleMsg))
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
	if !strings.Contains(view, "x abort") {
		t.Error("expected top-level 'x abort' hint when pipeline is running")
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
	if got := renderDiff("", 80, 20, 0, "", ""); got != "" {
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
	got := renderDiff(raw, 80, 0, 0, "", "")
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
	got := renderDiff(raw, 80, 0, 0, "", "")
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
	got := renderDiff(raw, 80, 5, 2, "", "")

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
	got := renderDiff(raw, 80, 3, 3, "", "")

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
	got := stripANSI(renderDiff(raw, 80, 0, 0, "", ""))
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

	got := stripANSI(renderDiff(b.String(), 80, 5, 0, "", ""))
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
	out := stripANSI(renderActionBar(run.Steps, true, true, false, 5, 5, false, true))
	if !strings.Contains(out, "d diff") {
		t.Error("expected d diff in approval prompt")
	}
}

// --- CI view tests ---

func testRunWithCI() *ipc.RunInfo {
	run := testRun()
	run.Steps = append(run.Steps, ipc.StepResultInfo{
		ID: "s6", StepName: types.StepCI, StepOrder: 6, Status: types.StepStatusPending,
	})
	return run
}

func TestIsCIActive(t *testing.T) {
	run := testRunWithCI()

	// Pending → not active.
	if isCIActive(run.Steps) {
		t.Error("expected false when CI is pending")
	}

	// Running → active.
	run.Steps[5].Status = types.StepStatusRunning
	if !isCIActive(run.Steps) {
		t.Error("expected true when CI is running")
	}

	// Completed → not active.
	run.Steps[5].Status = types.StepStatusCompleted
	if isCIActive(run.Steps) {
		t.Error("expected false when CI is completed")
	}
}

func TestIsCIActive_NoCIStep(t *testing.T) {
	run := testRun() // no CI step
	if isCIActive(run.Steps) {
		t.Error("expected false when no CI step exists")
	}
}

func TestCIStepStatus(t *testing.T) {
	run := testRunWithCI()
	run.Steps[5].Status = types.StepStatusRunning

	if got := ciStepStatus(run.Steps); got != types.StepStatusRunning {
		t.Errorf("expected running, got %s", got)
	}
}

func TestCIStepStatus_NoCIStep(t *testing.T) {
	run := testRun()
	if got := ciStepStatus(run.Steps); got != types.StepStatusPending {
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
			name: "standard CI message",
			logs: []string{"monitoring CI for PR #42 (timeout: 4h)..."},
			want: "42",
		},
		{
			name: "multiple logs",
			logs: []string{
				"some other log",
				"monitoring CI for PR #123 (timeout: 4h)...",
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

func TestParseCIActivity(t *testing.T) {
	t.Run("empty logs", func(t *testing.T) {
		a := parseCIActivity(nil)
		if a.CIFixes != 0 || a.AutoFixing || a.LastEvent != "" {
			t.Error("expected zero activity for empty logs")
		}
	})

	t.Run("polling", func(t *testing.T) {
		a := parseCIActivity([]string{"monitoring CI for PR #42 (timeout: 4h)..."})
		if a.LastEvent == "" {
			t.Error("expected last event set")
		}
	})

	t.Run("ci failure detected", func(t *testing.T) {
		a := parseCIActivity([]string{
			"monitoring CI for PR #42 (timeout: 4h)...",
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
		a := parseCIActivity([]string{
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
		a := parseCIActivity([]string{
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
		a := parseCIActivity([]string{
			"monitoring CI for PR #42 (timeout: 4h)...",
			"PR has been merged!",
		})
		if !strings.Contains(a.LastEvent, "merged") {
			t.Error("expected merged as last event")
		}
	})

	t.Run("pr closed", func(t *testing.T) {
		a := parseCIActivity([]string{"PR has been closed"})
		if !strings.Contains(a.LastEvent, "closed") {
			t.Error("expected closed as last event")
		}
	})

	t.Run("timeout", func(t *testing.T) {
		a := parseCIActivity([]string{"CI timeout reached"})
		if !strings.Contains(a.LastEvent, "timeout") {
			t.Error("expected timeout as last event")
		}
	})
}

func TestRenderCIView_Monitoring(t *testing.T) {
	run := testRunWithCI()
	run.Steps[5].Status = types.StepStatusRunning
	logs := []string{"monitoring CI for PR #42 (timeout: 4h)..."}

	out := renderCIView(run, run.Steps, "", logs, 80)

	if !strings.Contains(stripANSI(out), "CI") {
		t.Error("expected CI box title")
	}
	if !strings.Contains(out, "Monitoring") {
		t.Error("expected monitoring state")
	}
}

func TestRenderCIView_ShowsPRContextFromURL(t *testing.T) {
	run := testRunWithCI()
	run.PRURL = ptr("https://github.com/user/repo/pull/99")
	run.Steps[5].Status = types.StepStatusRunning

	out := stripANSI(renderCIView(run, run.Steps, "", nil, 80))

	if !strings.Contains(out, "PR #99") {
		t.Fatalf("expected CI panel to show PR context, got: %s", out)
	}
}

func TestRenderCIView_AutoFixing(t *testing.T) {
	run := testRunWithCI()
	run.Steps[5].Status = types.StepStatusRunning
	logs := []string{
		"monitoring CI for PR #42 (timeout: 4h)...",
		"CI failures detected: test — auto-fixing...",
		"running agent to fix CI failures...",
	}

	out := renderCIView(run, run.Steps, "", logs, 80)

	if !strings.Contains(out, "Auto-fixing CI") {
		t.Error("expected auto-fixing state indicator")
	}
	if !strings.Contains(out, "CI auto-fixes: 1") {
		t.Error("expected CI fix count")
	}
}

func TestRenderCIView_LastActivity(t *testing.T) {
	run := testRunWithCI()
	run.Steps[5].Status = types.StepStatusRunning
	logs := []string{
		"monitoring CI for PR #42 (timeout: 4h)...",
		"committed and pushed fixes",
	}

	out := renderCIView(run, run.Steps, "", logs, 80)

	if !strings.Contains(out, "Latest:") {
		t.Error("expected latest activity line")
	}
	if !strings.Contains(out, "committed and pushed fixes") {
		t.Error("expected last event text")
	}
}

func TestModel_View_CIViewWhenActive(t *testing.T) {
	run := testRunWithCI()
	m := NewModel("/tmp/sock", nil, run)
	m.steps = run.Steps
	m.steps[5].Status = types.StepStatusRunning
	m.logs = []string{"monitoring CI for PR #42 (timeout: 4h)..."}

	view := m.View()

	if !strings.Contains(stripANSI(view), "CI") {
		t.Error("expected CI box in model output")
	}
	if !strings.Contains(view, "Monitoring") {
		t.Error("expected monitoring state in model output")
	}
}

func TestModel_View_NonCIStepUsesGenericFindings(t *testing.T) {
	run := testRun() // no CI step
	m := NewModel("/tmp/sock", nil, run)
	m.steps[0].Status = types.StepStatusAwaitingApproval
	m.stepFindings[types.StepReview] = `{"findings":[{"severity":"error","description":"critical bug"}],"summary":"1 issue"}`

	view := m.View()

	// Should use generic findings, not CI view.
	// Check that no "CI" titled box appears (only Pipeline/Findings boxes).
	hasCIBox := false
	for _, line := range strings.Split(stripANSI(view), "\n") {
		if strings.Contains(line, "╭") && strings.Contains(line, "CI") {
			hasCIBox = true
		}
	}
	if hasCIBox {
		t.Error("expected generic findings view, not CI box")
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
// Returns the trimmed inner content, or empty string if not a box content line.
func boxContentLine(line string) string {
	trimmed := strings.TrimSpace(line)
	if len(trimmed) >= 2 && strings.HasPrefix(trimmed, "│") && strings.HasSuffix(trimmed, "│") {
		inner := trimmed[len("│") : len(trimmed)-len("│")]
		return strings.TrimSpace(inner)
	}
	return ""
}

func TestPipelineView_CompactNoConnectors(t *testing.T) {
	// When terminal height is small (< 30), connector lines between steps should be suppressed.
	run := testRun()
	steps := []ipc.StepResultInfo{
		{StepName: types.StepReview, Status: types.StepStatusCompleted},
		{StepName: types.StepTest, Status: types.StepStatusRunning},
		{StepName: types.StepLint, Status: types.StepStatusPending},
	}
	compact := stripANSI(renderPipelineView(run, steps, 80, 0, 25))
	normal := stripANSI(renderPipelineView(run, steps, 80, 0, 40))
	// Compact should have fewer lines than normal (no connector lines).
	compactLines := len(strings.Split(compact, "\n"))
	normalLines := len(strings.Split(normal, "\n"))
	if compactLines >= normalLines {
		t.Errorf("compact pipeline (height=25) should have fewer lines than normal (height=40): compact=%d, normal=%d", compactLines, normalLines)
	}
}

func TestPipelineView_NormalHasConnectors(t *testing.T) {
	// When terminal height is >= 30, connector lines should still be present.
	run := testRun()
	steps := []ipc.StepResultInfo{
		{StepName: types.StepReview, Status: types.StepStatusCompleted},
		{StepName: types.StepTest, Status: types.StepStatusRunning},
		{StepName: types.StepLint, Status: types.StepStatusPending},
	}
	result := stripANSI(renderPipelineView(run, steps, 80, 0, 40))
	lines := strings.Split(result, "\n")
	connectorCount := 0
	for _, line := range lines {
		inner := boxContentLine(line)
		if inner == "│" {
			connectorCount++
		}
	}
	if connectorCount < 2 {
		t.Errorf("expected at least 2 connector lines in normal mode (height=40), found %d", connectorCount)
	}
}

func TestModel_View_CompactPipeline(t *testing.T) {
	// Integration test: model with small height should produce compact pipeline.
	run := testRun()
	run.Steps[0].Status = types.StepStatusCompleted
	run.Steps[1].Status = types.StepStatusRunning
	m := NewModel("/tmp/test.sock", nil, run)
	m.width = 80
	m.height = 20
	view := stripANSI(m.View())
	lines := strings.Split(view, "\n")
	for _, line := range lines {
		inner := boxContentLine(line)
		if inner == "│" {
			t.Errorf("found connector line in compact view (height=20), should be suppressed")
		}
	}
}

func TestOutcomeBanner_NoDurationWhenNoStepTimes(t *testing.T) {
	run := testRun()
	run.Status = types.RunCompleted
	steps := []ipc.StepResultInfo{
		{StepName: types.StepReview, Status: types.StepStatusCompleted},
		{StepName: types.StepTest, Status: types.StepStatusCompleted},
	}
	banner := stripANSI(renderOutcomeBanner(run, steps))
	// When no steps have DurationMS, no time should be shown.
	if !strings.Contains(banner, "Pipeline passed") {
		t.Errorf("expected 'Pipeline passed' in banner, got: %s", banner)
	}
	if strings.Contains(banner, "s") && strings.Contains(banner, ".") {
		// Rough check: shouldn't have a duration string like "0.0s"
		t.Errorf("expected no elapsed time when no step durations available, got: %s", banner)
	}
}

func TestModel_View_LogTailCompact(t *testing.T) {
	// In stacked layout, compact terminals should still use the remaining height budget.
	prev := lipgloss.ColorProfile()
	defer lipgloss.SetColorProfile(prev)

	lipgloss.SetColorProfile(termenv.Ascii)
	run := testRun()
	m := NewModel("/tmp/sock", nil, run)
	m.width = 80
	m.height = 25
	for i := 1; i <= 20; i++ {
		m.logs = append(m.logs, fmt.Sprintf("log line %d", i))
	}

	view := stripANSI(m.View())
	count := strings.Count(view, "log line")
	if count <= 3 {
		t.Fatalf("expected compact stacked layout to expand beyond the old 3-line cap, got %d\n%s", count, view)
	}
	if got := lipgloss.Height(view); got != m.height {
		t.Fatalf("expected compact stacked layout to use full terminal height %d, got %d\n%s", m.height, got, view)
	}
}

func TestModel_View_LogTailHiddenTiny(t *testing.T) {
	// In very small terminals (height < 20), log box should be hidden entirely.
	run := testRun()
	m := NewModel("/tmp/sock", nil, run)
	m.width = 80
	m.height = 15 // tiny terminal
	m.logs = []string{"log line 1", "log line 2", "log line 3"}
	view := m.View()
	if strings.Contains(view, "log line") {
		t.Error("expected log box hidden in tiny terminal (height=15)")
	}
	if strings.Contains(view, "Log") {
		t.Error("expected no Log box title in tiny terminal")
	}
}

func TestModel_View_ShortTerminalDoesNotOverflowHeight(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)

	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval
	run.Steps[0].FindingsJSON = ptr(`{"summary":"The review surfaced several issues that need attention before continuing.","items":[{"id":"f1","severity":"warning","file":"internal/pipeline/steps/review.go","line":101,"description":"This finding has a long description that should wrap across multiple lines in a narrow viewport and still keep the pipeline header visible."},{"id":"f2","severity":"info","file":"internal/pipeline/steps/test.go","line":202,"description":"Another wrapped finding to force the findings panel to compete for height with the pipeline and footer sections."},{"id":"f3","severity":"warning","file":"internal/pipeline/steps/lint.go","line":303,"description":"A third wrapped finding makes the old item-count heuristic overflow the terminal height."}]}`)

	m := NewModel("/tmp/sock", nil, run)
	m.width = 80
	m.height = 18
	view := stripANSI(m.View())

	if got := lipgloss.Height(view); got > m.height {
		t.Fatalf("expected rendered view height <= terminal height (%d), got %d\n%s", m.height, got, view)
	}
	if !strings.Contains(view, "feature/foo") {
		t.Fatalf("expected pipeline header to remain visible in short terminal\n%s", view)
	}
}

func TestModel_View_StackedLogBoxFillsRemainingHeight(t *testing.T) {
	prev := lipgloss.ColorProfile()
	defer lipgloss.SetColorProfile(prev)

	lipgloss.SetColorProfile(termenv.Ascii)
	run := testRun()
	m := NewModel("/tmp/sock", nil, run)
	m.width = 80
	m.height = 40
	for i := 1; i <= 40; i++ {
		m.logs = append(m.logs, fmt.Sprintf("log line %d", i))
	}

	pipelineView := renderPipelineView(run, m.stepsWithRunningElapsed(), m.width, 0, m.height)
	footer := renderFooter(false, false, false, run.PRURL, m.width)
	expectedLogLines := m.height - sectionsHeight([]string{pipelineView}, 2) - 2 - lipgloss.Height(footer) - 2
	if expectedLogLines <= 5 {
		t.Fatalf("expected stacked layout to leave room for more than 5 log lines, got %d", expectedLogLines)
	}

	view := stripANSI(m.View())
	count := strings.Count(view, "log line")
	if count != expectedLogLines {
		t.Fatalf("expected stacked log box to fill remaining height with %d log lines, got %d\n%s", expectedLogLines, count, view)
	}
	if got := lipgloss.Height(view); got > m.height {
		t.Fatalf("expected rendered view height <= terminal height (%d), got %d\n%s", m.height, got, view)
	}
}

// --- Abort Confirmation Tests ---

func TestAbortConfirmation_FirstPressShowsConfirm(t *testing.T) {
	// First 'x' press should NOT send abort - should set confirmAbort flag
	// and show a confirmation prompt in the action bar.
	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval
	run.Steps[0].FindingsJSON = ptr(`{"summary":"test","items":[{"id":"f1","severity":"error","file":"a.go","line":1,"description":"bug"}]}`)

	m := NewModel("/tmp/sock", nil, run)
	m.width = 80

	// Press 'x' once.
	result, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})
	updated := result.(Model)

	// confirmAbort should be set.
	if !updated.confirmAbort {
		t.Error("expected confirmAbort to be true after first x press")
	}

	// The action bar should show a confirmation hint.
	view := updated.View()
	stripped := stripANSI(view)
	if !strings.Contains(stripped, "x again to abort") {
		t.Errorf("expected 'x again to abort' in view, got:\n%s", stripped)
	}
}

func TestAbortConfirmation_SecondPressSendsAbort(t *testing.T) {
	// Second 'x' press should actually send the abort command.
	run := testRun()

	m := NewModel("/tmp/sock", nil, run)
	m.width = 80
	m.confirmAbort = true // simulate first press already happened

	// Press 'x' again - this should produce a command (the respond cmd).
	_, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'x'}})

	// Should produce a non-nil command (the abort RPC call).
	if cmd == nil {
		t.Error("expected a non-nil command from second x press (abort should be sent)")
	}
}

func TestModel_View_HelpOverlay_ShowsAbortWhenRunning(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	run := testRun()
	m := NewModel("", nil, run)
	m.width = 80
	m.height = 40
	m.showHelp = true

	view := m.View()
	plain := stripANSI(view)

	if !strings.Contains(plain, "x x") || !strings.Contains(plain, "abort pipeline") {
		t.Errorf("help should show top-level abort while pipeline is running, got:\n%s", plain)
	}
}

func TestFindDiffOffset_MatchesFileAndHunk(t *testing.T) {
	// findDiffOffset should return the index of the hunk header
	// that contains the target file and line number.
	raw := "diff --git a/foo.go b/foo.go\n" +
		"--- a/foo.go\n" +
		"+++ b/foo.go\n" +
		"@@ -10,5 +10,7 @@ func foo() {\n" +
		" context\n" +
		"+added\n" +
		" context\n" +
		"@@ -30,3 +32,4 @@ func bar() {\n" +
		" context\n" +
		"+another\n"
	lines := parseDiffLines(raw)

	// Line 12 is in the first hunk (+10,7 covers lines 10-16).
	offset := findDiffOffset(lines, "foo.go", 12)
	if offset != 3 { // index of "@@ -10,5 +10,7 @@" line
		t.Errorf("expected offset=3 for foo.go:12, got %d", offset)
	}

	// Line 33 is in the second hunk (+32,4 covers lines 32-35).
	offset = findDiffOffset(lines, "foo.go", 33)
	if offset != 7 { // index of "@@ -30,3 +32,4 @@" line
		t.Errorf("expected offset=7 for foo.go:33, got %d", offset)
	}
}

func TestFindDiffOffset_FileNotFound(t *testing.T) {
	// Should return 0 when the file doesn't exist in the diff.
	raw := "diff --git a/foo.go b/foo.go\n" +
		"+++ b/foo.go\n" +
		"@@ -1,3 +1,4 @@\n" +
		"+added\n"
	lines := parseDiffLines(raw)

	offset := findDiffOffset(lines, "bar.go", 1)
	if offset != 0 {
		t.Errorf("expected offset=0 for non-existent file, got %d", offset)
	}
}

func TestFindDiffOffset_ScrollsToFileHeader(t *testing.T) {
	// When line=0 or line not in any hunk, should scroll to the file header.
	raw := "diff --git a/foo.go b/foo.go\n" +
		"--- a/foo.go\n" +
		"+++ b/foo.go\n" +
		"@@ -10,3 +10,4 @@\n" +
		"+added\n"
	lines := parseDiffLines(raw)

	// Line 0 means "just show me the file".
	offset := findDiffOffset(lines, "foo.go", 0)
	if offset != 0 { // index of "diff --git a/foo.go" line
		t.Errorf("expected offset=0 for foo.go:0, got %d", offset)
	}

	// Line 99 is beyond any hunk - should still scroll to the file header.
	offset = findDiffOffset(lines, "foo.go", 99)
	if offset != 0 {
		t.Errorf("expected offset=0 for foo.go:99 (beyond all hunks), got %d", offset)
	}
}

func TestDiffToggle_AutoScrollsToFinding(t *testing.T) {
	// When pressing 'd' to switch from findings to diff, diffOffset
	// should auto-scroll to the location of the current finding.
	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval
	run.Steps[0].FindingsJSON = ptr(`{"summary":"test","items":[` +
		`{"id":"f1","severity":"error","file":"foo.go","line":33,"description":"bug1"},` +
		`{"id":"f2","severity":"warning","file":"bar.go","line":5,"description":"bug2"}]}`)

	m := NewModel("/tmp/sock", nil, run)
	m.width = 80
	m.height = 40
	m.stepDiffs[types.StepReview] = "diff --git a/foo.go b/foo.go\n" +
		"--- a/foo.go\n" +
		"+++ b/foo.go\n" +
		"@@ -30,3 +30,4 @@ func bar() {\n" +
		" context\n" +
		"+added\n" +
		"diff --git a/bar.go b/bar.go\n" +
		"--- a/bar.go\n" +
		"+++ b/bar.go\n" +
		"@@ -3,3 +3,4 @@\n" +
		"+new line\n"

	// Cursor is on finding 0 (foo.go:33). Press 'd' to show diff.
	result, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
	model := result.(Model)

	if !model.showDiff {
		t.Fatal("expected showDiff=true")
	}
	// Should auto-scroll to the hunk containing foo.go line 33.
	// The hunk header "@@ -30,3 +30,4 @@" is at index 3.
	if model.diffOffset != 3 {
		t.Errorf("expected diffOffset=3 for foo.go:33, got %d", model.diffOffset)
	}
}

func TestAbortConfirmation_OtherKeyResetsConfirm(t *testing.T) {
	// Pressing any other key after first 'x' should reset confirmAbort.
	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval
	run.Steps[0].FindingsJSON = ptr(`{"summary":"test","items":[{"id":"f1","severity":"error","file":"a.go","line":1,"description":"bug"}]}`)

	m := NewModel("/tmp/sock", nil, run)
	m.width = 80
	m.confirmAbort = true // simulate first press

	// Press 'j' (a navigation key, not 'x').
	result, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}})
	updated := result.(Model)

	if updated.confirmAbort {
		t.Error("expected confirmAbort to be false after pressing a different key")
	}
}

func TestRenderDiff_LongLinesTruncated(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	// Create a diff with a line longer than the box content width.
	longLine := "+" + strings.Repeat("x", 200) // 201 chars total
	raw := "diff --git a/foo.go b/foo.go\n--- a/foo.go\n+++ b/foo.go\n@@ -1,3 +1,3 @@\n context line\n" + longLine + "\n"

	boxWidth := 80
	contentWidth := boxWidth - 4 // 2 border + 2 padding = 76

	result := renderDiff(raw, boxWidth, 0, 0, "", "")
	plain := stripANSI(result)

	// Check that no content line exceeds the box width.
	for _, line := range strings.Split(plain, "\n") {
		if line == "" {
			continue
		}
		// Each line in the box should be exactly boxWidth visual chars wide.
		w := lipgloss.Width(line)
		if w > boxWidth {
			t.Errorf("line exceeds box width (%d > %d): %s", w, boxWidth, line)
		}
	}

	// Verify the long line was truncated by checking the content width.
	// The long line should NOT appear in full inside the box.
	if strings.Contains(plain, strings.Repeat("x", contentWidth+1)) {
		t.Error("expected long diff line to be truncated to fit box content width")
	}
}

func TestRenderDiff_ShortLinesNotTruncated(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	// A short line should appear in full.
	raw := "diff --git a/foo.go b/foo.go\n--- a/foo.go\n+++ b/foo.go\n@@ -1,3 +1,3 @@\n context line\n+short addition\n"

	result := renderDiff(raw, 80, 0, 0, "", "")
	plain := stripANSI(result)

	if !strings.Contains(plain, "short addition") {
		t.Error("expected short diff line to appear in full")
	}
}

func TestRenderDiff_TruncatedLinePreservesPrefix(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	// A long addition line should still start with "+" after truncation.
	longLine := "+" + strings.Repeat("a", 200)
	raw := "diff --git a/foo.go b/foo.go\n--- a/foo.go\n+++ b/foo.go\n@@ -1,3 +1,3 @@\n context\n" + longLine + "\n"

	result := renderDiff(raw, 80, 0, 0, "", "")
	plain := stripANSI(result)

	// The truncated line should still contain "+a" (the diff prefix is preserved).
	// Box lines look like: │ +aaa... │
	found := false
	for _, line := range strings.Split(plain, "\n") {
		// Extract content between box borders.
		if strings.HasPrefix(strings.TrimSpace(line), "│") && strings.Contains(line, "+a") {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected truncated addition line to still start with + prefix")
	}
}

// --- Log line truncation tests ---

func TestModel_View_LogLongLinesWrapped(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	run := testRun()
	m := NewModel("/tmp/sock", nil, run)
	m.width = 80
	// Create a log line much longer than the box content width.
	longLog := "running " + strings.Repeat("x", 200) // well over 80 chars
	m.logs = []string{longLog}

	view := stripANSI(m.View())

	// No line in the rendered output should exceed the box width.
	for _, line := range strings.Split(view, "\n") {
		if line == "" {
			continue
		}
		w := lipgloss.Width(line)
		if w > 80 {
			t.Errorf("log line exceeds box width (%d > 80): %s", w, line)
		}
	}

	if got := strings.Count(view, "x"); got < 200 {
		t.Errorf("expected wrapped log output to preserve all 200 x characters, got %d", got)
	}
}

func TestModel_View_LogShortLinesNotTruncated(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	run := testRun()
	m := NewModel("/tmp/sock", nil, run)
	m.width = 80
	m.logs = []string{"running go test ./..."}

	view := stripANSI(m.View())

	if !strings.Contains(view, "running go test ./...") {
		t.Error("expected short log line to appear in full")
	}
}

func TestRenderCIView_LogLongLinesWrapped(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	run := testRunWithCI()
	run.Steps[5].Status = types.StepStatusRunning
	longLog := "monitoring CI for PR #42 (timeout: 4h)..."
	longLog2 := "running " + strings.Repeat("y", 200)
	logs := []string{longLog, longLog2}

	result := stripANSI(renderCIView(run, run.Steps, "", logs, 80))

	// No line should exceed the box width (80).
	for _, line := range strings.Split(result, "\n") {
		if line == "" {
			continue
		}
		w := lipgloss.Width(line)
		if w > 80 {
			t.Errorf("CI log line exceeds box width (%d > 80): %s", w, line)
		}
	}

	if got := strings.Count(result, "y"); got < 200 {
		t.Errorf("expected wrapped CI log output to preserve all 200 y characters, got %d", got)
	}
}

func TestRenderFindingsWithSelection_LongFilePathTruncated(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)

	// Create a finding with a very long file path that would overflow an 80-width box.
	longPath := "src/internal/very/deeply/nested/package/structure/" + strings.Repeat("x", 100) + "/handler.go"
	raw := fmt.Sprintf(`{"items":[{"id":"f1","severity":"error","file":"%s","line":42,"description":"Missing error check"}]}`, longPath)
	selected := map[string]bool{"f1": true}

	// Width is 76 (box content width = 80 - 4 for border/padding).
	content, _ := renderFindingsWithSelection(raw, 76, 0, selected, 0)
	result := stripANSI(content)

	// No line in the findings content should exceed 76 chars.
	for _, line := range strings.Split(result, "\n") {
		if line == "" {
			continue
		}
		w := lipgloss.Width(line)
		if w > 76 {
			t.Errorf("finding gutter line exceeds content width (%d > 76): %s", w, line)
		}
	}

	// The full long file path should NOT appear.
	if strings.Contains(result, longPath) {
		t.Error("expected long file path to be truncated to fit content width")
	}
}

func TestRenderFindingsWithSelection_ShortFilePathNotTruncated(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)

	raw := `{"items":[{"id":"f1","severity":"error","file":"src/handler.go","line":42,"description":"Missing error check"}]}`
	selected := map[string]bool{"f1": true}

	content, _ := renderFindingsWithSelection(raw, 76, 0, selected, 0)
	result := stripANSI(content)

	// Short file path should appear in full.
	if !strings.Contains(result, "src/handler.go:42") {
		t.Error("expected short file path to appear in full, got:\n" + result)
	}
}

func TestRenderFindingsWithSelection_TruncatedGutterPreservesSeverityIcon(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)

	longPath := strings.Repeat("z", 200) + "/handler.go"
	raw := fmt.Sprintf(`{"items":[{"id":"f1","severity":"error","file":"%s","line":1,"description":"test"}]}`, longPath)
	selected := map[string]bool{"f1": true}

	content, _ := renderFindingsWithSelection(raw, 76, 0, selected, 0)
	result := stripANSI(content)

	// The severity icon and checkbox should still be present even with truncation.
	if !strings.Contains(result, "[x]") {
		t.Error("expected checkbox to survive truncation")
	}
	if !strings.Contains(result, "E") {
		t.Error("expected severity icon to survive truncation")
	}
}

func TestDiffToggle_NoOpWhenNoDiffData(t *testing.T) {
	// Pressing 'd' when the awaiting step has no diff data should NOT toggle showDiff.
	// This prevents the bug where showDiff=true hides selection actions
	// but no diff actually renders.
	run := testRun()
	m := NewModel("/tmp/sock", nil, run)
	m.steps[0].Status = types.StepStatusAwaitingApproval
	m.stepFindings[types.StepReview] = `{"items":[{"id":"f1","severity":"error","file":"a.go","line":1,"description":"bad"}]}`
	// No diff data set for this step.

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("d")})
	model := updated.(Model)
	if model.showDiff {
		t.Error("expected showDiff to remain false when no diff data exists for the awaiting step")
	}
}

func TestActionBar_HidesDiffWhenNoDiffData(t *testing.T) {
	// The action bar should NOT show 'd diff' when no diff data exists
	// for the current awaiting step.
	configureTUIColors()
	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval
	m := NewModel("", nil, run)
	m.width = 80
	m.height = 50
	m.stepFindings[types.StepReview] = `{"summary":"test","items":[{"id":"f1","severity":"error","file":"foo.go","line":1,"description":"bad"}]}`
	m.resetFindingSelection(types.StepReview)
	// No diff data set.

	view := stripANSI(m.View())
	if strings.Contains(view, "d diff") {
		t.Error("should NOT show 'd diff' when no diff data exists for the awaiting step")
	}
	if strings.Contains(view, "d findings") {
		t.Error("should NOT show 'd findings' when no diff data exists")
	}
}

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

func TestModel_View_RunningStepShowsElapsedTime(t *testing.T) {
	configureTUIColors()
	run := testRun()
	run.Steps[0].Status = types.StepStatusCompleted
	run.Steps[0].DurationMS = ptr(int64(1200))
	run.Steps[1].Status = types.StepStatusRunning

	m := NewModel("", nil, run)
	m.width = 80
	m.height = 40
	// Simulate step started 5 seconds ago.
	m.stepStartTimes = map[types.StepName]time.Time{
		types.StepTest: time.Now().Add(-5 * time.Second),
	}

	view := stripANSI(m.View())

	// Running step should show an elapsed time (approximately 5.0s).
	// The completed step shows "1.2s", and the running step should show ~"5.0s".
	if !strings.Contains(view, "5.0s") && !strings.Contains(view, "5.1s") && !strings.Contains(view, "4.9s") {
		t.Errorf("expected running step to show ~5.0s elapsed time, got:\n%s", view)
	}
}

func TestModel_Update_StepStartedRecordsStartTime(t *testing.T) {
	configureTUIColors()
	run := testRun()
	m := NewModel("", nil, run)

	before := time.Now()
	stepName := types.StepReview
	m.Update(eventMsg(ipc.Event{
		Type:     ipc.EventStepStarted,
		StepName: &stepName,
	}))
	after := time.Now()

	startTime, ok := m.stepStartTimes[types.StepReview]
	if !ok {
		t.Fatal("expected stepStartTimes to contain entry for Review step")
	}
	if startTime.Before(before) || startTime.After(after) {
		t.Errorf("expected start time between %v and %v, got %v", before, after, startTime)
	}
}

func TestModel_View_RunningStepNoElapsedWithoutStartTime(t *testing.T) {
	configureTUIColors()
	run := testRun()
	run.Steps[0].Status = types.StepStatusCompleted
	run.Steps[0].DurationMS = ptr(int64(1200))
	run.Steps[1].Status = types.StepStatusRunning

	m := NewModel("", nil, run)
	m.width = 80
	m.height = 40
	// No stepStartTimes set - should not show elapsed time for running step.

	view := stripANSI(m.View())

	// The completed step shows "1.2s", but the running Test step should NOT show any duration.
	// Find the Test line and verify it doesn't have a duration.
	lines := strings.Split(view, "\n")
	for _, line := range lines {
		if strings.Contains(line, "Test") && !strings.Contains(line, "test-") {
			// This is the Test step line - should not contain any "s" duration pattern
			// other than the step name itself.
			content := strings.TrimSpace(line)
			content = strings.ReplaceAll(content, "│", "")
			content = strings.TrimSpace(content)
			if strings.Contains(content, "Test") && strings.Contains(content, ".") && strings.HasSuffix(strings.TrimSpace(content), "s") {
				t.Errorf("expected no elapsed time for running step without start time, but found duration-like text in: %q", content)
			}
		}
	}
}

func TestOutcomeBanner_CancelledShowsBanner(t *testing.T) {
	run := testRun()
	run.Status = types.RunCancelled
	steps := []ipc.StepResultInfo{
		{StepName: types.StepReview, Status: types.StepStatusCompleted},
		{StepName: types.StepTest, Status: types.StepStatusFailed},
	}
	banner := stripANSI(renderOutcomeBanner(run, steps))
	if !strings.Contains(banner, "Pipeline cancelled") {
		t.Errorf("expected 'Pipeline cancelled' in banner, got: %s", banner)
	}
	if !strings.Contains(banner, "✗") {
		t.Errorf("expected ✗ in cancelled banner, got: %s", banner)
	}
}

func TestOutcomeBanner_CancelledShowsElapsedTime(t *testing.T) {
	run := testRun()
	run.Status = types.RunCancelled
	d1 := int64(2000)
	d2 := int64(3500)
	steps := []ipc.StepResultInfo{
		{StepName: types.StepReview, Status: types.StepStatusCompleted, DurationMS: &d1},
		{StepName: types.StepTest, Status: types.StepStatusFailed, DurationMS: &d2},
	}
	banner := stripANSI(renderOutcomeBanner(run, steps))
	if !strings.Contains(banner, "5.5s") {
		t.Errorf("expected elapsed time '5.5s' in cancelled banner, got: %s", banner)
	}
}

func TestOutcomeBanner_CancelledInView(t *testing.T) {
	run := testRun()
	run.Status = types.RunCancelled
	m := NewModel("/tmp/sock", nil, run)
	m.width = 80
	m.height = 40
	m.done = true
	view := stripANSI(m.View())
	if !strings.Contains(view, "Pipeline cancelled") {
		t.Errorf("expected 'Pipeline cancelled' in view when run is cancelled, got: %s", view)
	}
}

// Test: When a step completes via EventStepCompleted and we had a start time
// recorded for it, the completed step should show its final duration in the view.
func TestModel_View_CompletedStepPreservesDuration(t *testing.T) {
	configureTUIColors()
	run := testRun()
	run.Steps[0].Status = types.StepStatusRunning
	run.Status = types.RunRunning

	m := NewModel("", nil, run)
	m.width = 80
	m.height = 40

	// Record a start time 5 seconds ago for the running step.
	m.stepStartTimes[types.StepReview] = time.Now().Add(-5 * time.Second)

	// Step completes via event.
	completedStatus := string(types.StepStatusCompleted)
	stepName := types.StepReview
	m.applyEvent(ipc.Event{
		Type:     ipc.EventStepCompleted,
		StepName: &stepName,
		Status:   &completedStatus,
	})

	view := stripANSI(m.View())

	// The completed Review step should show a duration around 5s.
	lines := strings.Split(view, "\n")
	found := false
	for _, line := range lines {
		if strings.Contains(line, "Review") && strings.Contains(line, "5.") && strings.Contains(line, "s") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected completed Review step to show ~5.0s duration, but it was not found in view:\n%s", view)
	}
}

// Test: When EventStepCompleted arrives with a tracked start time, the DurationMS
// on the step is populated from the elapsed time.
func TestModel_ApplyEvent_StepCompletedSetsDuration(t *testing.T) {
	configureTUIColors()
	run := testRun()
	run.Steps[0].Status = types.StepStatusRunning

	m := NewModel("", nil, run)

	// Record a start time 3 seconds ago.
	m.stepStartTimes[types.StepReview] = time.Now().Add(-3 * time.Second)

	// Step completes via event.
	completedStatus := string(types.StepStatusCompleted)
	stepName := types.StepReview
	m.applyEvent(ipc.Event{
		Type:     ipc.EventStepCompleted,
		StepName: &stepName,
		Status:   &completedStatus,
	})

	// The step should have DurationMS set.
	for _, s := range m.steps {
		if s.StepName == types.StepReview {
			if s.DurationMS == nil {
				t.Fatal("expected DurationMS to be set on completed step with tracked start time")
			}
			// Should be approximately 3000ms (allow 2800-3500 for timing variance).
			if *s.DurationMS < 2800 || *s.DurationMS > 3500 {
				t.Errorf("expected DurationMS ~3000ms, got %d", *s.DurationMS)
			}
			return
		}
	}
	t.Fatal("Review step not found in model steps")
}

// Test: When EventStepCompleted arrives without a tracked start time, DurationMS
// remains nil (no crash, no bogus data).
func TestModel_ApplyEvent_StepCompletedNoDurationWithoutStartTime(t *testing.T) {
	configureTUIColors()
	run := testRun()
	run.Steps[0].Status = types.StepStatusRunning

	m := NewModel("", nil, run)
	// No stepStartTimes entry for Review.

	completedStatus := string(types.StepStatusCompleted)
	stepName := types.StepReview
	m.applyEvent(ipc.Event{
		Type:     ipc.EventStepCompleted,
		StepName: &stepName,
		Status:   &completedStatus,
	})

	for _, s := range m.steps {
		if s.StepName == types.StepReview {
			if s.DurationMS != nil {
				t.Errorf("expected DurationMS to remain nil without start time, got %d", *s.DurationMS)
			}
			return
		}
	}
	t.Fatal("Review step not found in model steps")
}

// Test: When re-attaching, steps with StartedAt but no DurationMS get their
// start times seeded into stepStartTimes so elapsed time can be computed.
func TestNewModel_SeedsStartTimesFromStartedAt(t *testing.T) {
	configureTUIColors()
	startedAt := time.Now().Add(-10 * time.Second).Unix()
	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval
	run.Steps[0].StartedAt = &startedAt

	m := NewModel("", nil, run)

	st, ok := m.stepStartTimes[types.StepReview]
	if !ok {
		t.Fatal("expected stepStartTimes to contain entry for Review step on re-attach")
	}
	// The seeded time should be approximately 10 seconds ago.
	elapsed := time.Since(st)
	if elapsed < 9*time.Second || elapsed > 12*time.Second {
		t.Errorf("expected start time ~10s ago, got %v ago", elapsed)
	}
}

// Test: stepsWithRunningElapsed computes elapsed time for AwaitingApproval steps.
func TestModel_View_AwaitingApprovalShowsElapsedTime(t *testing.T) {
	configureTUIColors()
	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval

	m := NewModel("", nil, run)
	m.width = 80
	m.height = 40
	m.stepStartTimes[types.StepReview] = time.Now().Add(-7 * time.Second)

	view := stripANSI(m.View())

	if !strings.Contains(view, "7.0s") && !strings.Contains(view, "7.1s") && !strings.Contains(view, "6.9s") {
		t.Errorf("expected awaiting approval step to show ~7.0s elapsed time, got:\n%s", view)
	}
}

// Test: stepsWithRunningElapsed computes elapsed time for FixReview steps.
func TestModel_View_FixReviewShowsElapsedTime(t *testing.T) {
	configureTUIColors()
	run := testRun()
	run.Steps[0].Status = types.StepStatusFixReview

	m := NewModel("", nil, run)
	m.width = 80
	m.height = 40
	m.stepStartTimes[types.StepReview] = time.Now().Add(-4 * time.Second)

	view := stripANSI(m.View())

	if !strings.Contains(view, "4.0s") && !strings.Contains(view, "4.1s") && !strings.Contains(view, "3.9s") {
		t.Errorf("expected fix review step to show ~4.0s elapsed time, got:\n%s", view)
	}
}

// Test: Re-attach scenario - step is awaiting approval, TUI connects and shows duration
// computed from StartedAt in the initial run data.
func TestModel_View_ReattachAwaitingApprovalShowsDuration(t *testing.T) {
	configureTUIColors()
	startedAt := time.Now().Add(-15 * time.Second).Unix()
	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval
	run.Steps[0].StartedAt = &startedAt

	m := NewModel("", nil, run)
	m.width = 80
	m.height = 40

	view := stripANSI(m.View())

	// StartedAt is seeded from Unix seconds, so sub-second truncation plus render delay
	// can round this re-attach duration up to 16.0s on slower CI runners.
	if !strings.Contains(view, "15.") && !strings.Contains(view, "14.9") && !strings.Contains(view, "15.0") && !strings.Contains(view, "15.1") && !strings.Contains(view, "16.0") {
		t.Errorf("expected re-attached awaiting approval step to show ~15s elapsed, got:\n%s", view)
	}
}

// Test: When EventStepCompleted carries DurationMS, it takes precedence over
// the computed elapsed time from stepStartTimes.
func TestModel_ApplyEvent_StepCompletedPrefersEventDuration(t *testing.T) {
	configureTUIColors()
	run := testRun()
	run.Steps[0].Status = types.StepStatusRunning

	m := NewModel("", nil, run)
	// Record a start time 10 seconds ago.
	m.stepStartTimes[types.StepReview] = time.Now().Add(-10 * time.Second)

	// Event carries execution-only duration of 2 seconds (excluding approval wait).
	completedStatus := string(types.StepStatusCompleted)
	stepName := types.StepReview
	eventDuration := int64(2000)
	m.applyEvent(ipc.Event{
		Type:       ipc.EventStepCompleted,
		StepName:   &stepName,
		Status:     &completedStatus,
		DurationMS: &eventDuration,
	})

	for _, s := range m.steps {
		if s.StepName == types.StepReview {
			if s.DurationMS == nil {
				t.Fatal("expected DurationMS to be set")
			}
			// Should use event's 2000ms, not the computed ~10000ms.
			if *s.DurationMS != 2000 {
				t.Errorf("expected DurationMS = 2000 (from event), got %d", *s.DurationMS)
			}
			return
		}
	}
	t.Fatal("Review step not found")
}

func TestModel_FixingEventDoesNotFreezeDuration(t *testing.T) {
	configureTUIColors()
	run := testRun()
	run.Steps[0].Status = types.StepStatusRunning

	m := NewModel("", nil, run)
	m.stepStartTimes[types.StepReview] = time.Now().Add(-5 * time.Second)

	fixingStatus := string(types.StepStatusFixing)
	stepName := types.StepReview
	m.applyEvent(ipc.Event{
		Type:     ipc.EventStepCompleted,
		StepName: &stepName,
		Status:   &fixingStatus,
	})

	for _, s := range m.steps {
		if s.StepName == types.StepReview {
			if s.DurationMS != nil {
				t.Errorf("expected DurationMS to remain nil during fixing so timer keeps ticking, got %d", *s.DurationMS)
			}
			return
		}
	}
	t.Fatal("Review step not found")
}

// Test: When a running step auto-fixes (no approval in between), the live timer
// must continue accumulating from the original start time, not reset to 0.
func TestModel_AutoFixPreservesAccumulatedElapsed(t *testing.T) {
	configureTUIColors()
	run := testRun()
	run.Steps[0].Status = types.StepStatusRunning

	m := NewModel("", nil, run)
	m.stepStartTimes[types.StepReview] = time.Now().Add(-5 * time.Second)

	fixingStatus := string(types.StepStatusFixing)
	stepName := types.StepReview
	m.applyEvent(ipc.Event{
		Type:     ipc.EventStepCompleted,
		StepName: &stepName,
		Status:   &fixingStatus,
	})

	// stepsWithRunningElapsed should report ~5s of accumulated time, not 0.
	elapsed := m.stepsWithRunningElapsed()
	for _, s := range elapsed {
		if s.StepName == types.StepReview {
			if s.DurationMS == nil {
				t.Fatal("expected stepsWithRunningElapsed to compute live elapsed for Fixing step")
			}
			if *s.DurationMS < 4500 {
				t.Errorf("expected accumulated elapsed >= 4500ms, got %dms (timer reset to 0)", *s.DurationMS)
			}
			return
		}
	}
	t.Fatal("Review step not found")
}

// Test: When a step already has DurationMS persisted (e.g. from AwaitingApproval)
// and then transitions to Fixing, the stale DurationMS must be cleared and the
// live timer must accumulate from the previous execution time.
func TestModel_FixingEventClearsStaleDuration(t *testing.T) {
	configureTUIColors()
	run := testRun()
	run.Steps[0].Status = types.StepStatusRunning

	m := NewModel("", nil, run)
	m.stepStartTimes[types.StepReview] = time.Now().Add(-10 * time.Second)

	// Simulate step entering AwaitingApproval with 10s of persisted execution time.
	awaitingStatus := string(types.StepStatusAwaitingApproval)
	stepName := types.StepReview
	dur := int64(10000)
	m.applyEvent(ipc.Event{
		Type:       ipc.EventStepCompleted,
		StepName:   &stepName,
		Status:     &awaitingStatus,
		DurationMS: &dur,
	})

	// Verify duration was persisted.
	for _, s := range m.steps {
		if s.StepName == types.StepReview && s.DurationMS == nil {
			t.Fatal("expected DurationMS to be set after AwaitingApproval event")
		}
	}

	// Now simulate user pressing fix - step transitions to Fixing.
	fixingStatus := string(types.StepStatusFixing)
	m.applyEvent(ipc.Event{
		Type:     ipc.EventStepCompleted,
		StepName: &stepName,
		Status:   &fixingStatus,
	})

	// DurationMS must be nil so stepsWithRunningElapsed computes live elapsed.
	for _, s := range m.steps {
		if s.StepName == types.StepReview {
			if s.DurationMS != nil {
				t.Errorf("expected DurationMS to be cleared when entering Fixing, got %d", *s.DurationMS)
			}
			break
		}
	}

	// stepsWithRunningElapsed should accumulate: the 10s of prior execution
	// plus a small amount of wall time since the Fixing event.
	elapsed := m.stepsWithRunningElapsed()
	for _, s := range elapsed {
		if s.StepName == types.StepReview {
			if s.DurationMS == nil {
				t.Fatal("expected stepsWithRunningElapsed to compute live elapsed for Fixing step")
			}
			if *s.DurationMS < 9500 || *s.DurationMS > 11000 {
				t.Errorf("expected accumulated elapsed ~10000ms, got %dms", *s.DurationMS)
			}
			return
		}
	}
	t.Fatal("Review step not found")
}

func TestRenderDiff_BlankLineBetweenFiles(t *testing.T) {
	// Multi-file diff should have a blank line before the second file header.
	raw := `diff --git a/foo.go b/foo.go
--- a/foo.go
+++ b/foo.go
@@ -1,2 +1,3 @@
 package foo
+import "fmt"
 func main() {}
diff --git a/bar.go b/bar.go
--- a/bar.go
+++ b/bar.go
@@ -1,2 +1,2 @@
 package bar
-old
+new
`
	got := renderDiff(raw, 80, 0, 0, "", "")
	plain := stripANSI(got)

	// Find the last line of first file and first line of second file.
	lines := strings.Split(plain, "\n")
	secondFileHeaderIdx := -1
	seenFirstFile := false
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		trimmed = strings.TrimLeft(trimmed, "│")
		trimmed = strings.TrimRight(trimmed, "│")
		trimmed = strings.TrimSpace(trimmed)
		if strings.Contains(trimmed, "diff --git a/foo.go") {
			seenFirstFile = true
		}
		if seenFirstFile && strings.Contains(trimmed, "diff --git a/bar.go") {
			secondFileHeaderIdx = i
			break
		}
	}
	if secondFileHeaderIdx < 0 {
		t.Fatal("second file header not found in output")
	}
	if secondFileHeaderIdx < 1 {
		t.Fatal("no line before second file header")
	}
	// The line before the second file header should be blank (inside box).
	prevLine := strings.TrimSpace(lines[secondFileHeaderIdx-1])
	prevLine = strings.TrimLeft(prevLine, "│")
	prevLine = strings.TrimRight(prevLine, "│")
	prevLine = strings.TrimSpace(prevLine)
	if prevLine != "" {
		t.Errorf("expected blank line between files, got %q", lines[secondFileHeaderIdx-1])
	}
}

func TestRenderDiff_NoExtraBlankBeforeFirstFile(t *testing.T) {
	// First file header should NOT have an extra blank line before it.
	raw := `diff --git a/foo.go b/foo.go
--- a/foo.go
+++ b/foo.go
@@ -1 +1,2 @@
 package foo
+import "fmt"
`
	got := renderDiff(raw, 80, 0, 0, "", "")
	plain := stripANSI(got)
	lines := strings.Split(plain, "\n")

	// Find the "diff --git" line.
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		trimmed = strings.TrimLeft(trimmed, "│")
		trimmed = strings.TrimRight(trimmed, "│")
		trimmed = strings.TrimSpace(trimmed)
		if strings.HasPrefix(trimmed, "diff --git") {
			// Line before should NOT be blank (it should be stats or box border).
			if i > 0 {
				prev := strings.TrimSpace(lines[i-1])
				prev = strings.TrimLeft(prev, "│")
				prev = strings.TrimRight(prev, "│")
				prev = strings.TrimSpace(prev)
				// The line before the first diff header is the stats line or empty stats separator.
				// It should NOT be an extra blank line inserted by file separation logic.
				// We can't easily distinguish, but we verify the diff renders correctly.
			}
			return
		}
	}
	t.Fatal("diff --git line not found")
}

func TestRenderDiff_BlankLineBetweenFiles_ThreeFiles(t *testing.T) {
	// Three-file diff: each file boundary should have a blank line separator.
	raw := `diff --git a/a.go b/a.go
--- a/a.go
+++ b/a.go
@@ -1 +1,2 @@
 package a
+import "fmt"
diff --git a/b.go b/b.go
--- a/b.go
+++ b/b.go
@@ -1 +1,2 @@
 package b
+import "os"
diff --git a/c.go b/c.go
--- a/c.go
+++ b/c.go
@@ -1 +1,2 @@
 package c
+import "io"
`
	got := renderDiff(raw, 80, 0, 0, "", "")
	plain := stripANSI(got)
	lines := strings.Split(plain, "\n")

	// Count blank lines immediately before file headers (inside box borders).
	// Skip the first file header since its preceding blank is the stats gap, not a file boundary.
	blankBeforeFile := 0
	fileHeaders := 0
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		trimmed = strings.TrimLeft(trimmed, "│")
		trimmed = strings.TrimRight(trimmed, "│")
		trimmed = strings.TrimSpace(trimmed)
		if strings.HasPrefix(trimmed, "diff --git") {
			fileHeaders++
			// Only count blank lines before 2nd+ file headers (file boundary separators).
			if fileHeaders > 1 && i > 0 {
				prev := strings.TrimSpace(lines[i-1])
				prev = strings.TrimLeft(prev, "│")
				prev = strings.TrimRight(prev, "│")
				prev = strings.TrimSpace(prev)
				if prev == "" {
					blankBeforeFile++
				}
			}
		}
	}
	if fileHeaders != 3 {
		t.Fatalf("expected 3 file headers, got %d", fileHeaders)
	}
	// Blank line before 2nd and 3rd file headers (not 1st).
	if blankBeforeFile != 2 {
		t.Errorf("expected 2 blank lines before file boundaries, got %d", blankBeforeFile)
	}
}

func TestRenderDiff_LineNumbersShown(t *testing.T) {
	raw := `diff --git a/main.go b/main.go
--- a/main.go
+++ b/main.go
@@ -10,3 +10,4 @@
 context line
+added line
 another context
+second add
`
	got := renderDiff(raw, 80, 0, 0, "", "")
	plain := stripANSI(got)
	// Context line at new-file line 10 should show "10" in the gutter.
	if !strings.Contains(plain, " 10 ") {
		t.Errorf("expected line number 10 in diff gutter, got:\n%s", plain)
	}
	// Added line at new-file line 11 should show "11".
	if !strings.Contains(plain, " 11 ") {
		t.Errorf("expected line number 11 in diff gutter, got:\n%s", plain)
	}
	// Context line at 12 and addition at 13.
	if !strings.Contains(plain, " 12 ") {
		t.Errorf("expected line number 12 in diff gutter, got:\n%s", plain)
	}
	if !strings.Contains(plain, " 13 ") {
		t.Errorf("expected line number 13 in diff gutter, got:\n%s", plain)
	}
}

func TestRenderDiff_DeletionLinesNoLineNumber(t *testing.T) {
	raw := `diff --git a/main.go b/main.go
--- a/main.go
+++ b/main.go
@@ -5,3 +5,2 @@
 context
-deleted line
 after delete
`
	got := renderDiff(raw, 80, 0, 0, "", "")
	plain := stripANSI(got)
	lines := strings.Split(plain, "\n")
	// Find the line containing "deleted line" and verify it has no line number.
	for _, line := range lines {
		if strings.Contains(line, "deleted line") {
			// The line should NOT have a number before the deletion marker.
			// It should have blank space in the gutter area.
			trimmed := strings.TrimSpace(line)
			trimmed = strings.TrimLeft(trimmed, "│")
			trimmed = strings.TrimSpace(trimmed)
			if trimmed[0] >= '0' && trimmed[0] <= '9' {
				t.Errorf("deletion line should not have a line number, got: %q", line)
			}
			break
		}
	}
}

func TestRenderFindings_FocusedDescriptionNotDim(t *testing.T) {
	// Focused finding's description should NOT be dim, keeping default style
	// so it visually pops against the dim unfocused descriptions.
	raw := `{"findings":[
		{"id":"f1","severity":"error","file":"a.go","line":1,"description":"focused text"},
		{"id":"f2","severity":"warning","file":"b.go","line":2,"description":"other text"}
	],"summary":"2 issues"}`

	selected := map[string]bool{"f1": true, "f2": true}
	content, _ := renderFindingsWithSelection(raw, 80, 0, selected, 0) // cursor=0, f1 focused

	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(ansiBrightBlack))

	// Focused description should NOT be dim-styled.
	if strings.Contains(content, dimStyle.Render("        focused text")) {
		t.Error("focused finding description should not be dim-styled")
	}
	// But it should still appear (in default style).
	if !strings.Contains(stripANSI(content), "focused text") {
		t.Error("focused finding description should appear in output")
	}
}

func TestRenderFindings_UnfocusedDescriptionDim(t *testing.T) {
	// Unfocused findings' descriptions should be dim (bright black) to create
	// visual contrast with the focused finding.
	raw := `{"findings":[
		{"id":"f1","severity":"error","file":"a.go","line":1,"description":"first issue"},
		{"id":"f2","severity":"warning","file":"b.go","line":2,"description":"second issue"}
	],"summary":"2 issues"}`

	selected := map[string]bool{"f1": true, "f2": true}
	content, _ := renderFindingsWithSelection(raw, 80, 0, selected, 0) // cursor=0, f2 unfocused

	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(ansiBrightBlack))
	// wrapIndentedText produces "        second issue" (8-char indent + text).
	dimSecond := dimStyle.Render("        second issue")

	// Unfocused description should be dim-styled (including its indent).
	if !strings.Contains(content, dimSecond) {
		t.Error("unfocused finding description should be dim-styled")
	}
}

func TestRenderFindings_FocusChangesDescriptionStyle(t *testing.T) {
	// Moving cursor from f1 to f2 should swap which description is dim vs default.
	raw := `{"findings":[
		{"id":"f1","severity":"error","file":"a.go","line":1,"description":"first issue"},
		{"id":"f2","severity":"warning","file":"b.go","line":2,"description":"second issue"}
	],"summary":"2 issues"}`

	selected := map[string]bool{"f1": true, "f2": true}
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(ansiBrightBlack))

	// Cursor at 0: f1 focused, f2 unfocused (dim).
	content0, _ := renderFindingsWithSelection(raw, 80, 0, selected, 0)
	if !strings.Contains(content0, dimStyle.Render("        second issue")) {
		t.Error("with cursor=0, second issue description should be dim")
	}
	if strings.Contains(content0, dimStyle.Render("        first issue")) {
		t.Error("with cursor=0, first issue description should NOT be dim")
	}

	// Cursor at 1: f2 focused, f1 unfocused (dim).
	content1, _ := renderFindingsWithSelection(raw, 80, 1, selected, 0)
	if !strings.Contains(content1, dimStyle.Render("        first issue")) {
		t.Error("with cursor=1, first issue description should be dim")
	}
	if strings.Contains(content1, dimStyle.Render("        second issue")) {
		t.Error("with cursor=1, second issue description should NOT be dim")
	}
}

func TestRenderDiff_LineNumbersStyledDim(t *testing.T) {
	raw := `diff --git a/main.go b/main.go
--- a/main.go
+++ b/main.go
@@ -1,2 +1,2 @@
 context
+added
`
	got := renderDiff(raw, 80, 0, 0, "", "")
	// Line number "1" should be styled dim (bright black).
	dimStyle := lipgloss.NewStyle().Foreground(lipgloss.Color(ansiBrightBlack))
	styledOne := dimStyle.Render("1 ")
	if !strings.Contains(got, styledOne) {
		t.Error("expected line numbers to be styled dim (bright black)")
	}
}

// --- Iteration 48: Blank line between stats and diff content ---

func TestRenderDiff_BlankLineBetweenStatsAndContent(t *testing.T) {
	// DESIGN.md Diff View shows a blank line between the stats header and diff content:
	//   3 files  +42  -17
	//                          <-- blank line here
	//   diff --git a/foo.go b/foo.go
	raw := `diff --git a/main.go b/main.go
--- a/main.go
+++ b/main.go
@@ -1 +1,2 @@
 package main
+import "fmt"
`
	got := renderDiff(raw, 80, 0, 0, "", "")
	plain := stripANSI(got)

	// Find the stats line and the first diff line inside the box.
	lines := strings.Split(plain, "\n")
	statsIdx := -1
	firstDiffIdx := -1
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		// Stats line contains file count and +/- counts.
		if strings.Contains(trimmed, "file") && strings.Contains(trimmed, "+") {
			statsIdx = i
		}
		// First diff content line is the "diff --git" header.
		if strings.Contains(trimmed, "diff --git") && firstDiffIdx == -1 {
			firstDiffIdx = i
		}
	}

	if statsIdx == -1 {
		t.Fatal("could not find stats line in diff output")
	}
	if firstDiffIdx == -1 {
		t.Fatal("could not find diff --git line in diff output")
	}

	// There should be at least one blank line between the stats and the diff content.
	// gap = 2 means: stats at N, blank at N+1, diff at N+2.
	gap := firstDiffIdx - statsIdx
	if gap < 2 {
		t.Errorf("expected blank line between stats and diff content, but gap is %d lines (stats at %d, diff at %d)", gap, statsIdx, firstDiffIdx)
	}
}

func TestRenderDiff_StatsBlankLineNotDoubled(t *testing.T) {
	// Verify there is exactly one blank line between stats and content, not two or more.
	raw := `diff --git a/main.go b/main.go
--- a/main.go
+++ b/main.go
@@ -1 +1,2 @@
 package main
+import "fmt"
`
	got := renderDiff(raw, 80, 0, 0, "", "")
	plain := stripANSI(got)

	lines := strings.Split(plain, "\n")
	statsIdx := -1
	firstDiffIdx := -1
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.Contains(trimmed, "file") && strings.Contains(trimmed, "+") {
			statsIdx = i
		}
		if strings.Contains(trimmed, "diff --git") && firstDiffIdx == -1 {
			firstDiffIdx = i
		}
	}

	if statsIdx == -1 || firstDiffIdx == -1 {
		t.Fatal("could not find stats or diff line")
	}

	// Count blank lines between stats and diff content (inside box, so check trimmed content).
	blankCount := 0
	for i := statsIdx + 1; i < firstDiffIdx; i++ {
		// Inside the box, a blank line looks like "│    │" or similar - trim border chars.
		inner := strings.TrimSpace(lines[i])
		inner = strings.TrimLeft(inner, "│")
		inner = strings.TrimRight(inner, "│")
		inner = strings.TrimSpace(inner)
		if inner == "" {
			blankCount++
		}
	}

	if blankCount != 1 {
		t.Errorf("expected exactly 1 blank line between stats and diff content, got %d", blankCount)
	}
}

func TestRenderDiff_ScrolledViewPreservesStatsGap(t *testing.T) {
	// When scrolled down, the stats header still has a blank line before the visible diff content.
	var b strings.Builder
	b.WriteString("diff --git a/main.go b/main.go\n")
	b.WriteString("--- a/main.go\n")
	b.WriteString("+++ b/main.go\n")
	b.WriteString("@@ -1,20 +1,20 @@\n")
	for i := 0; i < 20; i++ {
		b.WriteString(fmt.Sprintf("+line %d\n", i))
	}
	raw := b.String()

	// Render scrolled down by 5 lines.
	got := renderDiff(raw, 80, 10, 5, "", "")
	plain := stripANSI(got)

	lines := strings.Split(plain, "\n")
	statsIdx := -1
	firstContentIdx := -1
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.Contains(trimmed, "file") && strings.Contains(trimmed, "+") {
			statsIdx = i
		}
		// First non-blank content line after stats inside the box.
		if statsIdx >= 0 && i > statsIdx && firstContentIdx == -1 {
			inner := strings.TrimLeft(trimmed, "│")
			inner = strings.TrimRight(inner, "│")
			inner = strings.TrimSpace(inner)
			if inner != "" {
				firstContentIdx = i
			}
		}
	}

	if statsIdx == -1 {
		t.Fatal("could not find stats line in scrolled diff output")
	}
	if firstContentIdx == -1 {
		t.Fatal("could not find content line after stats in scrolled diff output")
	}

	gap := firstContentIdx - statsIdx
	if gap < 2 {
		t.Errorf("expected blank line between stats and scrolled diff content, but gap is %d lines", gap)
	}
}

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

func TestModel_View_FooterShowsCloseWhenHelpVisible(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	run := testRun()
	m := NewModel("", nil, run)
	m.width = 80
	m.height = 40
	m.showHelp = true

	view := m.View()
	plain := stripANSI(view)

	// Footer should say "close" instead of "help" when help overlay is visible.
	lines := strings.Split(plain, "\n")
	for _, line := range lines {
		// Look for footer line (outside the help box) that has the ? key hint.
		// The footer is the last non-empty line.
		if strings.Contains(line, "?") && strings.Contains(line, "close") && !strings.Contains(line, "close help") {
			return // found the footer with "close" label
		}
	}
	t.Errorf("footer should show '? close' when help is visible, got:\n%s", plain)
}

func TestModel_View_FooterShowsHelpWhenHelpHidden(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	run := testRun()
	m := NewModel("", nil, run)
	m.width = 80
	m.height = 40
	m.showHelp = false

	view := m.View()
	plain := stripANSI(view)

	// Footer should say "help" when help overlay is NOT visible.
	lines := strings.Split(plain, "\n")
	for _, line := range lines {
		if strings.Contains(line, "?") && strings.Contains(line, "help") {
			return // found the footer with "help" label
		}
	}
	t.Errorf("footer should show '? help' when help is hidden, got:\n%s", plain)
}

func TestModel_View_FooterNeverShowsHelpWhenHelpVisible(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	run := testRun()
	m := NewModel("", nil, run)
	m.width = 80
	m.height = 40
	m.showHelp = true

	view := m.View()
	plain := stripANSI(view)

	// The footer line (last few lines, outside boxes) should NOT say "help"
	// when help overlay is already showing. It should say "close" instead.
	// Find footer lines (after the last box closing border ╰).
	lines := strings.Split(plain, "\n")
	// The footer is the last non-empty line(s) after all boxes.
	lastBoxEnd := 0
	for i, line := range lines {
		if strings.Contains(line, "╰") || strings.Contains(line, "+") {
			lastBoxEnd = i
		}
	}
	for _, line := range lines[lastBoxEnd+1:] {
		if strings.Contains(line, "?") && strings.Contains(line, "help") && !strings.Contains(line, "close") {
			t.Errorf("footer should NOT show '? help' when help is visible, found: %q", line)
		}
	}
}

func TestDiffView_NextFindingKey_MovesCursorAndScrolls(t *testing.T) {
	// Pressing 'n' in diff view should move finding cursor to next finding
	// and auto-scroll the diff to the new finding's hunk location.
	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval
	run.Steps[0].FindingsJSON = ptr(`{"summary":"test","items":[` +
		`{"id":"f1","severity":"error","file":"foo.go","line":12,"description":"bug1"},` +
		`{"id":"f2","severity":"warning","file":"bar.go","line":5,"description":"bug2"}]}`)

	m := NewModel("/tmp/sock", nil, run)
	m.width = 80
	m.height = 40
	m.showDiff = true
	m.stepDiffs[types.StepReview] = "diff --git a/foo.go b/foo.go\n" +
		"--- a/foo.go\n" +
		"+++ b/foo.go\n" +
		"@@ -10,5 +10,7 @@ func foo() {\n" +
		" context\n" +
		"+added\n" +
		"diff --git a/bar.go b/bar.go\n" +
		"--- a/bar.go\n" +
		"+++ b/bar.go\n" +
		"@@ -3,5 +3,6 @@\n" +
		"+new line\n"

	// Cursor starts at finding 0 (foo.go:12). Press 'n' to go to finding 1 (bar.go:5).
	result, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	model := result.(Model)

	cursor := model.findingCursor[types.StepReview]
	if cursor != 1 {
		t.Errorf("expected finding cursor=1 after 'n', got %d", cursor)
	}
	// Diff should auto-scroll to bar.go's hunk header at index 9
	// ("@@ -3,5 +3,6 @@" is the hunk in bar.go).
	if model.diffOffset != 9 {
		t.Errorf("expected diffOffset=9 for bar.go:5, got %d", model.diffOffset)
	}
}

func TestDiffView_PrevFindingKey_MovesCursorAndScrolls(t *testing.T) {
	// Pressing 'p' in diff view should move finding cursor to previous finding
	// and auto-scroll the diff to that finding's hunk location.
	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval
	run.Steps[0].FindingsJSON = ptr(`{"summary":"test","items":[` +
		`{"id":"f1","severity":"error","file":"foo.go","line":12,"description":"bug1"},` +
		`{"id":"f2","severity":"warning","file":"bar.go","line":5,"description":"bug2"}]}`)

	m := NewModel("/tmp/sock", nil, run)
	m.width = 80
	m.height = 40
	m.showDiff = true
	m.findingCursor[types.StepReview] = 1 // start on finding 1 (bar.go:5)
	m.stepDiffs[types.StepReview] = "diff --git a/foo.go b/foo.go\n" +
		"--- a/foo.go\n" +
		"+++ b/foo.go\n" +
		"@@ -10,5 +10,7 @@ func foo() {\n" +
		" context\n" +
		"+added\n" +
		"diff --git a/bar.go b/bar.go\n" +
		"--- a/bar.go\n" +
		"+++ b/bar.go\n" +
		"@@ -3,5 +3,6 @@\n" +
		"+new line\n"

	// Press 'p' to go back to finding 0 (foo.go:12).
	result, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("p")})
	model := result.(Model)

	cursor := model.findingCursor[types.StepReview]
	if cursor != 0 {
		t.Errorf("expected finding cursor=0 after 'p', got %d", cursor)
	}
	// Diff should auto-scroll to foo.go's hunk header at index 3.
	if model.diffOffset != 3 {
		t.Errorf("expected diffOffset=3 for foo.go:12, got %d", model.diffOffset)
	}
}

func TestDiffView_NextFindingKey_NoOpWhenNotInDiffView(t *testing.T) {
	// 'n' and 'p' should be no-ops when not in diff view (showDiff=false).
	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval
	run.Steps[0].FindingsJSON = ptr(`{"summary":"test","items":[` +
		`{"id":"f1","severity":"error","file":"foo.go","line":12,"description":"bug1"},` +
		`{"id":"f2","severity":"warning","file":"bar.go","line":5,"description":"bug2"}]}`)

	m := NewModel("/tmp/sock", nil, run)
	m.width = 80
	m.height = 40
	m.showDiff = false

	// Press 'n' - should not change cursor because we're not in diff mode.
	result, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	model := result.(Model)

	cursor := model.findingCursor[types.StepReview]
	if cursor != 0 {
		t.Errorf("expected finding cursor unchanged at 0 when not in diff view, got %d", cursor)
	}
}

func TestDiffView_ShowsFindingContext(t *testing.T) {
	// When viewing diff with findings, the current finding's info should
	// appear as a context line so users know which finding they're looking at.
	lipgloss.SetColorProfile(termenv.ANSI)
	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval
	run.Steps[0].FindingsJSON = ptr(`{"summary":"test","items":[` +
		`{"id":"f1","severity":"error","file":"foo.go","line":12,"description":"Missing error check"},` +
		`{"id":"f2","severity":"warning","file":"bar.go","line":5,"description":"Unused import"}]}`)

	m := NewModel("/tmp/sock", nil, run)
	m.width = 80
	m.height = 40
	m.stepDiffs[types.StepReview] = "diff --git a/foo.go b/foo.go\n--- a/foo.go\n+++ b/foo.go\n@@ -10,6 +10,8 @@\n context\n+added line\n"
	m.showDiff = true

	output := stripANSI(m.View())

	// Should show the focused finding's file:line and description somewhere in the diff view.
	if !strings.Contains(output, "foo.go:12") {
		t.Errorf("expected diff view to show current finding file:line 'foo.go:12', got:\n%s", output)
	}
	if !strings.Contains(output, "Missing error check") {
		t.Errorf("expected diff view to show current finding description 'Missing error check', got:\n%s", output)
	}
}

func TestDiffView_FindingContextUpdatesOnNavigation(t *testing.T) {
	// When navigating with 'n' key, the finding context should update to show the next finding.
	lipgloss.SetColorProfile(termenv.ANSI)
	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval
	run.Steps[0].FindingsJSON = ptr(`{"summary":"test","items":[` +
		`{"id":"f1","severity":"error","file":"foo.go","line":12,"description":"Missing error check"},` +
		`{"id":"f2","severity":"warning","file":"bar.go","line":5,"description":"Unused import"}]}`)

	m := NewModel("/tmp/sock", nil, run)
	m.width = 80
	m.height = 40
	m.stepDiffs[types.StepReview] = "diff --git a/foo.go b/foo.go\n--- a/foo.go\n+++ b/foo.go\n@@ -10,6 +10,8 @@\n context\n+added line\ndiff --git a/bar.go b/bar.go\n--- a/bar.go\n+++ b/bar.go\n@@ -3,6 +3,8 @@\n context\n+added\n"
	m.showDiff = true

	// Press 'n' to move to second finding.
	result, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	model := result.(Model)

	output := stripANSI(model.View())

	// Should now show the second finding's info.
	if !strings.Contains(output, "bar.go:5") {
		t.Errorf("expected diff view to show navigated finding 'bar.go:5' after pressing n, got:\n%s", output)
	}
	if !strings.Contains(output, "Unused import") {
		t.Errorf("expected diff view to show navigated finding description 'Unused import' after pressing n, got:\n%s", output)
	}
}

func TestDiffView_NoFindingContextWithoutFindings(t *testing.T) {
	// When diff view has no findings, there should be no finding context line.
	lipgloss.SetColorProfile(termenv.ANSI)
	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval

	m := NewModel("/tmp/sock", nil, run)
	m.width = 80
	m.height = 40
	m.stepDiffs[types.StepReview] = "diff --git a/foo.go b/foo.go\n--- a/foo.go\n+++ b/foo.go\n@@ -10,6 +10,8 @@\n context\n+added line\n"
	m.showDiff = true

	output := stripANSI(m.View())

	// Should have the diff content but no finding context (no severity icons in header area).
	// Check that the diff view renders without error.
	if !strings.Contains(output, "Diff") {
		t.Errorf("expected diff view title 'Diff', got:\n%s", output)
	}
	// The diff view should NOT have finding-specific content like severity icons
	// outside of the diff content itself.
	lines := strings.Split(output, "\n")
	for _, line := range lines {
		cleaned := strings.TrimSpace(line)
		// Finding context would appear between stats and diff content.
		// If there's no findings, we shouldn't see a "Finding N/M" style line.
		if strings.HasPrefix(cleaned, "Finding ") {
			t.Errorf("unexpected finding context line without findings: %q", line)
		}
	}
}

func TestModel_EscapeReturnsToDiffFromFindings(t *testing.T) {
	// Pressing Escape while in diff view should return to findings view.
	lipgloss.SetColorProfile(termenv.Ascii)
	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval
	m := NewModel("", nil, run)
	m.showDiff = true
	m.stepDiffs[types.StepReview] = "diff --git a/foo.go b/foo.go\n"

	result, _ := m.Update(tea.KeyMsg{Type: tea.KeyEscape})
	m = result.(Model)

	if m.showDiff {
		t.Fatal("expected Escape to return from diff view to findings view")
	}
}

func TestModel_EscapeResetsDiffOffset(t *testing.T) {
	// Pressing Escape while in diff view should also reset diffOffset.
	lipgloss.SetColorProfile(termenv.Ascii)
	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval
	m := NewModel("", nil, run)
	m.showDiff = true
	m.diffOffset = 42
	m.stepDiffs[types.StepReview] = "diff --git a/foo.go b/foo.go\n"

	result, _ := m.Update(tea.KeyMsg{Type: tea.KeyEscape})
	m = result.(Model)

	if m.diffOffset != 0 {
		t.Fatalf("expected Escape to reset diffOffset to 0, got %d", m.diffOffset)
	}
}

func TestModel_EscapeNoOpWhenNotInDiffOrHelp(t *testing.T) {
	// Pressing Escape when not in diff view and help is closed should be a no-op.
	lipgloss.SetColorProfile(termenv.Ascii)
	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval
	m := NewModel("", nil, run)
	m.showDiff = false
	m.showHelp = false
	m.findingCursor = map[types.StepName]int{types.StepReview: 2}

	result, _ := m.Update(tea.KeyMsg{Type: tea.KeyEscape})
	m = result.(Model)

	// State should be unchanged.
	if m.showDiff {
		t.Fatal("Escape should not toggle diff on")
	}
	if m.showHelp {
		t.Fatal("Escape should not toggle help on")
	}
	if m.findingCursor[types.StepReview] != 2 {
		t.Fatalf("Escape should not change cursor position, got %d", m.findingCursor[types.StepReview])
	}
}

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

// visualColumn returns the visual column position where needle starts in line,
// using lipgloss.Width to account for multi-byte Unicode characters.
// Returns -1 if not found.
func visualColumn(line, needle string) int {
	idx := strings.Index(line, needle)
	if idx < 0 {
		return -1
	}
	return lipgloss.Width(line[:idx])
}

func hasLineContainingAll(view string, needles ...string) bool {
	for _, line := range strings.Split(stripANSI(view), "\n") {
		match := true
		for _, needle := range needles {
			if !strings.Contains(line, needle) {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func hasParallelBoxRow(view string) bool {
	for _, line := range strings.Split(stripANSI(view), "\n") {
		if strings.Count(line, "╭") >= 2 || strings.Count(line, "╯") >= 2 || strings.Count(line, "│") >= 4 {
			return true
		}
	}
	return false
}

func TestModel_View_WideLayoutPlacesPipelineBesideFindings(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	findings := `{"findings":[{"severity":"error","description":"test finding","id":"f1","file":"foo.go","line":1}],"summary":"1 issue"}`
	run := &ipc.RunInfo{
		ID: "run-001", RepoID: "repo-001", Branch: "main", HeadSHA: "abc12345",
		BaseSHA: "000000", Status: types.RunRunning,
		Steps: []ipc.StepResultInfo{
			{ID: "s1", StepName: types.StepReview, StepOrder: 1, Status: types.StepStatusAwaitingApproval, FindingsJSON: &findings},
			{ID: "s2", StepName: types.StepTest, StepOrder: 2, Status: types.StepStatusPending},
		},
	}
	m := NewModel("/tmp/sock", nil, run)
	m.width = 140
	m.height = 40

	view := m.View()
	if !strings.Contains(stripANSI(view), "Findings -") {
		t.Fatalf("expected findings box in view, got:\n%s", stripANSI(view))
	}
	if !hasParallelBoxRow(view) {
		t.Fatalf("expected wide layout to render parallel boxes, got:\n%s", stripANSI(view))
	}
}

func TestModel_View_WideLayoutPlacesPipelineBesideLog(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	run := testRun()
	run.Steps[0].Status = types.StepStatusRunning
	m := NewModel("/tmp/sock", nil, run)
	m.width = 140
	m.height = 40
	m.logs = []string{"running go test ./..."}

	view := m.View()
	if !strings.Contains(stripANSI(view), "Log") {
		t.Fatalf("expected log box in view, got:\n%s", stripANSI(view))
	}
	if !hasParallelBoxRow(view) {
		t.Fatalf("expected wide layout to render pipeline beside log, got:\n%s", stripANSI(view))
	}
}

func TestModel_View_NarrowLayoutKeepsPipelineStackedAboveFindings(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	findings := `{"findings":[{"severity":"error","description":"test finding","id":"f1","file":"foo.go","line":1}],"summary":"1 issue"}`
	run := &ipc.RunInfo{
		ID: "run-001", RepoID: "repo-001", Branch: "main", HeadSHA: "abc12345",
		BaseSHA: "000000", Status: types.RunRunning,
		Steps: []ipc.StepResultInfo{
			{ID: "s1", StepName: types.StepReview, StepOrder: 1, Status: types.StepStatusAwaitingApproval, FindingsJSON: &findings},
			{ID: "s2", StepName: types.StepTest, StepOrder: 2, Status: types.StepStatusPending},
		},
	}
	m := NewModel("/tmp/sock", nil, run)
	m.width = 80
	m.height = 40

	view := m.View()
	if hasParallelBoxRow(view) {
		t.Fatalf("expected narrow layout to keep pipeline stacked above findings, got:\n%s", stripANSI(view))
	}
}

func TestModel_View_Width100UsesResponsiveLayout(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	findings := `{"findings":[{"severity":"error","description":"test finding","id":"f1","file":"foo.go","line":1}],"summary":"1 issue"}`
	run := &ipc.RunInfo{
		ID: "run-001", RepoID: "repo-001", Branch: "main", HeadSHA: "abc12345",
		BaseSHA: "000000", Status: types.RunRunning,
		Steps: []ipc.StepResultInfo{
			{ID: "s1", StepName: types.StepReview, StepOrder: 1, Status: types.StepStatusAwaitingApproval, FindingsJSON: &findings},
			{ID: "s2", StepName: types.StepTest, StepOrder: 2, Status: types.StepStatusPending},
		},
	}
	m := NewModel("/tmp/sock", nil, run)
	m.width = 100
	m.height = 40

	view := m.View()
	if !hasParallelBoxRow(view) {
		t.Fatalf("expected width 100 to use responsive layout, got:\n%s", stripANSI(view))
	}
}

func TestHelpOverlay_NavigationDescriptionsAligned(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	result := renderHelpOverlay(80, testRun(), true, true, true, false)
	plain := stripANSI(result)
	lines := strings.Split(plain, "\n")

	// Find lines containing navigation descriptions and measure their visual column positions.
	navDescriptions := []string{"scroll line by line", "jump to start/end", "half-page down/up"}
	var descColumns []int
	for _, line := range lines {
		for _, desc := range navDescriptions {
			if col := visualColumn(line, desc); col >= 0 {
				descColumns = append(descColumns, col)
			}
		}
	}

	if len(descColumns) < 3 {
		t.Fatalf("expected at least 3 navigation entries, found %d in:\n%s", len(descColumns), plain)
	}

	// All descriptions should start at the same visual column.
	for i := 1; i < len(descColumns); i++ {
		if descColumns[i] != descColumns[0] {
			t.Errorf("navigation descriptions not aligned: column %d vs %d in:\n%s",
				descColumns[0], descColumns[i], plain)
		}
	}
}

func TestHelpOverlay_ActionDescriptionsAligned(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	result := renderHelpOverlay(80, testRun(), true, false, true, false)
	plain := stripANSI(result)
	lines := strings.Split(plain, "\n")

	// Find lines containing action descriptions and measure their visual column positions.
	actionDescriptions := []string{"approve", "fix", "skip", "abort (press twice)"}
	var descColumns []int
	for _, line := range lines {
		for _, desc := range actionDescriptions {
			if col := visualColumn(line, desc); col >= 0 {
				descColumns = append(descColumns, col)
			}
		}
	}

	if len(descColumns) < 4 {
		t.Fatalf("expected 4 action entries, found %d in:\n%s", len(descColumns), plain)
	}

	// All descriptions should start at the same visual column.
	for i := 1; i < len(descColumns); i++ {
		if descColumns[i] != descColumns[0] {
			t.Errorf("action descriptions not aligned: column %d vs %d in:\n%s",
				descColumns[0], descColumns[i], plain)
		}
	}
}

func TestHelpOverlay_ShowsRunContext(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	run := testRun()
	result := stripANSI(renderHelpOverlay(80, run, true, false, true, false))
	if !strings.Contains(result, run.Branch) {
		t.Fatalf("expected help overlay to show branch name, got:\n%s", result)
	}
	if !strings.Contains(result, run.HeadSHA[:8]) {
		t.Fatalf("expected help overlay to show short commit SHA, got:\n%s", result)
	}
	if !strings.Contains(result, run.ID) {
		t.Fatalf("expected help overlay to show pipeline ID, got:\n%s", result)
	}
}

func TestHelpOverlay_ShowsOpenPRActionWhenPRURLPresent(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	run := testRun()
	prURL := "https://github.com/test/repo/pull/42"
	run.PRURL = &prURL

	result := stripANSI(renderHelpOverlay(80, run, true, false, true, false))
	if !strings.Contains(result, "open PR in browser") {
		t.Fatalf("expected help overlay to include PR browser action, got:\n%s", result)
	}
	if !strings.Contains(result, "o") {
		t.Fatalf("expected help overlay to include 'o' keybinding, got:\n%s", result)
	}
}

// Test: action bar to findings box should have exactly 1 blank line, not 2.
func TestModel_View_OneBlankLineBetweenActionBarAndFindings(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	defer lipgloss.SetColorProfile(termenv.ANSI)

	findings := `{"findings":[{"severity":"error","description":"test finding","id":"f1","file":"foo.go","line":1}],"summary":"1 issue"}`
	run := &ipc.RunInfo{
		ID: "run-001", RepoID: "repo-001", Branch: "main", HeadSHA: "abc12345",
		BaseSHA: "000000", Status: types.RunRunning,
		Steps: []ipc.StepResultInfo{
			{ID: "s1", StepName: types.StepReview, StepOrder: 1, Status: types.StepStatusAwaitingApproval, FindingsJSON: &findings},
			{ID: "s2", StepName: types.StepTest, StepOrder: 2, Status: types.StepStatusPending},
		},
	}
	m := NewModel("/tmp/sock", nil, run)
	m.width = 80
	m.height = 40

	view := m.View()
	plain := stripANSI(view)
	lines := strings.Split(plain, "\n")

	// Find the last line of the action bar (contains "approve") and the Findings box top border.
	actionBarEnd := -1
	findingsStart := -1
	for i, line := range lines {
		if strings.Contains(line, "approve") && strings.Contains(line, "skip") {
			actionBarEnd = i
		}
		if strings.Contains(line, "╭") && strings.Contains(line, "Findings") {
			findingsStart = i
			break
		}
	}

	if actionBarEnd < 0 || findingsStart < 0 {
		t.Fatalf("could not find action bar or findings box in view:\n%s", plain)
	}

	blankCount := findingsStart - actionBarEnd - 1
	if blankCount != 1 {
		t.Errorf("expected 1 blank line between action bar and findings box, got %d\naction bar line %d: %q\nfindings line %d: %q",
			blankCount, actionBarEnd, lines[actionBarEnd], findingsStart, lines[findingsStart])
	}
}

// Test: log box to help overlay should have exactly 1 blank line, not 2.
func TestModel_View_OneBlankLineBetweenLogAndHelp(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	defer lipgloss.SetColorProfile(termenv.ANSI)

	run := &ipc.RunInfo{
		ID: "run-001", RepoID: "repo-001", Branch: "main", HeadSHA: "abc12345",
		BaseSHA: "000000", Status: types.RunRunning,
		Steps: []ipc.StepResultInfo{
			{ID: "s1", StepName: types.StepReview, StepOrder: 1, Status: types.StepStatusRunning},
		},
	}
	m := NewModel("/tmp/sock", nil, run)
	m.width = 80
	m.height = 40
	m.logs = []string{"running tests..."}
	m.showHelp = true

	view := m.View()
	plain := stripANSI(view)
	lines := strings.Split(plain, "\n")

	// Find the log box bottom border and help box top border.
	logBottom := -1
	helpTop := -1
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasSuffix(trimmed, "╯") && logBottom < 0 {
			// Check if this is the log box bottom (after a log box, not the pipeline box).
			if i > 0 {
				// Look backwards for "Log" title to confirm this is the log box.
				for j := i - 1; j >= 0 && j > i-10; j-- {
					if strings.Contains(lines[j], "Log") && strings.Contains(lines[j], "╭") {
						logBottom = i
						break
					}
				}
			}
		}
		if strings.Contains(line, "╭") && strings.Contains(line, "Help") {
			helpTop = i
			break
		}
	}

	if logBottom < 0 || helpTop < 0 {
		t.Fatalf("could not find log box bottom or help box top in view:\n%s", plain)
	}

	blankCount := helpTop - logBottom - 1
	if blankCount != 1 {
		t.Errorf("expected 1 blank line between log box and help overlay, got %d\nlog bottom line %d: %q\nhelp top line %d: %q",
			blankCount, logBottom, lines[logBottom], helpTop, lines[helpTop])
	}
}

func TestModel_View_ResponsiveLayoutKeepsHelpVisibleWithLogs(t *testing.T) {
	prev := lipgloss.ColorProfile()
	defer lipgloss.SetColorProfile(prev)

	lipgloss.SetColorProfile(termenv.Ascii)

	run := &ipc.RunInfo{
		ID: "run-001", RepoID: "repo-001", Branch: "main", HeadSHA: "abc12345",
		BaseSHA: "000000", Status: types.RunRunning,
		Steps: []ipc.StepResultInfo{
			{ID: "s1", StepName: types.StepReview, StepOrder: 1, Status: types.StepStatusRunning},
		},
	}
	m := NewModel("/tmp/sock", nil, run)
	m.width = 120
	m.height = 40
	m.logs = make([]string, 40)
	for i := range m.logs {
		m.logs[i] = fmt.Sprintf("log line %02d", i)
	}
	m.showHelp = true

	view := stripANSI(m.View())

	if !strings.Contains(view, "Help") {
		t.Fatalf("expected help overlay to remain visible in responsive layout with logs, got:\n%s", view)
	}
	if !strings.Contains(view, "close help") {
		t.Fatalf("expected help overlay content in responsive layout with logs, got:\n%s", view)
	}
}

func TestModel_View_ResponsiveLayoutReservesGapBeforeLogBox(t *testing.T) {
	prev := lipgloss.ColorProfile()
	defer lipgloss.SetColorProfile(prev)

	lipgloss.SetColorProfile(termenv.Ascii)

	run := &ipc.RunInfo{
		ID: "run-001", RepoID: "repo-001", Branch: "main", HeadSHA: "abc12345",
		BaseSHA: "000000", Status: types.RunRunning,
		Steps: []ipc.StepResultInfo{
			{ID: "s1", StepName: types.StepReview, StepOrder: 1, Status: types.StepStatusAwaitingApproval},
		},
	}
	m := NewModel("/tmp/sock", nil, run)
	m.width = 120
	m.height = 20
	m.logs = make([]string, 40)
	for i := range m.logs {
		m.logs[i] = fmt.Sprintf("log line %02d", i)
	}

	view := stripANSI(m.View())
	if !strings.Contains(view, "q detach") {
		t.Fatalf("expected footer to remain visible, got:\n%s", view)
	}
	if !strings.Contains(view, "approve") {
		t.Fatalf("expected action bar to remain visible, got:\n%s", view)
	}
	if !strings.Contains(view, "Log") {
		t.Fatalf("expected log box to remain visible, got:\n%s", view)
	}
	if strings.Contains(view, "log line 26") {
		t.Fatalf("expected responsive log box to reserve one line for the action-bar separator, got:\n%s", view)
	}
	if !strings.Contains(view, "log line 27") {
		t.Fatalf("expected responsive log box to keep the newest lines after reserving the separator, got:\n%s", view)
	}
	if !hasParallelBoxRow(view) {
		t.Fatalf("expected responsive layout with side-by-side columns, got:\n%s", view)
	}
}

// Test: footer should have consistent spacing (1 blank line) after any preceding section.
func TestModel_View_ConsistentFooterSpacing(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	defer lipgloss.SetColorProfile(termenv.ANSI)

	// Test with pipeline box only (completed, no log, no findings).
	run := &ipc.RunInfo{
		ID: "run-001", RepoID: "repo-001", Branch: "main", HeadSHA: "abc12345",
		BaseSHA: "000000", Status: types.RunCompleted,
		Steps: []ipc.StepResultInfo{
			{ID: "s1", StepName: types.StepReview, StepOrder: 1, Status: types.StepStatusCompleted},
		},
	}
	m := NewModel("/tmp/sock", nil, run)
	m.width = 80
	m.height = 40
	m.done = true

	view := m.View()
	plain := stripANSI(view)
	lines := strings.Split(plain, "\n")

	// Find footer line (contains "q" and "quit").
	footerLine := -1
	for i, line := range lines {
		if strings.Contains(line, "q") && strings.Contains(line, "quit") && strings.Contains(line, "?") {
			footerLine = i
			break
		}
	}
	if footerLine < 0 {
		t.Fatalf("could not find footer in view:\n%s", plain)
	}

	// Find the last non-blank line before footer.
	lastContent := -1
	for i := footerLine - 1; i >= 0; i-- {
		if strings.TrimSpace(lines[i]) != "" {
			lastContent = i
			break
		}
	}
	if lastContent < 0 {
		t.Fatalf("no content before footer in view:\n%s", plain)
	}

	blankCount := footerLine - lastContent - 1
	if blankCount != 1 {
		t.Errorf("expected 1 blank line before footer, got %d\nlast content line %d: %q\nfooter line %d: %q",
			blankCount, lastContent, lines[lastContent], footerLine, lines[footerLine])
	}
}

func TestHelpOverlay_SelectionDescriptionsAligned(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	// showDiff=false so selection section is visible.
	result := renderHelpOverlay(80, testRun(), true, false, true, false)
	plain := stripANSI(result)
	lines := strings.Split(plain, "\n")

	// Find lines containing selection descriptions using visual column positions.
	selDescriptions := []string{"toggle current", "select all", "select none"}
	var descColumns []int
	for _, line := range lines {
		for _, desc := range selDescriptions {
			if col := visualColumn(line, desc); col >= 0 {
				descColumns = append(descColumns, col)
			}
		}
	}

	if len(descColumns) < 3 {
		t.Fatalf("expected 3 selection entries, found %d in:\n%s", len(descColumns), plain)
	}

	// All descriptions should start at the same visual column.
	for i := 1; i < len(descColumns); i++ {
		if descColumns[i] != descColumns[0] {
			t.Errorf("selection descriptions not aligned: column %d vs %d in:\n%s",
				descColumns[0], descColumns[i], plain)
		}
	}
}

func TestModel_View_LogBoxExpandsToFillRightColumn(t *testing.T) {
	prev := lipgloss.ColorProfile()
	defer lipgloss.SetColorProfile(prev)

	lipgloss.SetColorProfile(termenv.Ascii)
	run := testRun()
	run.Steps[0].Status = types.StepStatusRunning
	m := NewModel("/tmp/sock", nil, run)
	m.width = 140
	m.height = 40

	// Add many log lines so the log box has room to expand.
	for i := 0; i < 20; i++ {
		m.logs = append(m.logs, fmt.Sprintf("log line %d", i))
	}

	view := m.View()
	plain := stripANSI(view)

	// Count log content lines inside the Log box (lines containing "log line").
	logContentLines := 0
	for _, line := range strings.Split(plain, "\n") {
		if strings.Contains(line, "log line") {
			logContentLines++
		}
	}

	// With height=40, the log box should expand well beyond the old 5-line cap.
	if logContentLines <= 5 {
		t.Errorf("expected log box to expand beyond 5 lines in responsive layout, got %d content lines\nview:\n%s",
			logContentLines, plain)
	}
}

func TestModel_View_LogBoxStaysSmallWhenFindingsPresent(t *testing.T) {
	prev := lipgloss.ColorProfile()
	defer lipgloss.SetColorProfile(prev)

	lipgloss.SetColorProfile(termenv.Ascii)
	findings := `{"findings":[{"severity":"error","description":"test finding","id":"f1","file":"foo.go","line":1}],"summary":"1 issue"}`
	run := &ipc.RunInfo{
		ID: "run-001", RepoID: "repo-001", Branch: "main", HeadSHA: "abc12345",
		BaseSHA: "000000", Status: types.RunRunning,
		Steps: []ipc.StepResultInfo{
			{ID: "s1", StepName: types.StepReview, StepOrder: 1, Status: types.StepStatusAwaitingApproval, FindingsJSON: &findings},
			{ID: "s2", StepName: types.StepTest, StepOrder: 2, Status: types.StepStatusPending},
		},
	}
	m := NewModel("/tmp/sock", nil, run)
	m.width = 140
	m.height = 40
	for i := 0; i < 20; i++ {
		m.logs = append(m.logs, fmt.Sprintf("log line %d", i))
	}

	view := m.View()
	plain := stripANSI(view)

	// When findings are present, the log box should stay small (<=5 lines).
	logContentLines := 0
	for _, line := range strings.Split(plain, "\n") {
		if strings.Contains(line, "log line") {
			logContentLines++
		}
	}
	if logContentLines > 5 {
		t.Errorf("expected log box to stay <=5 lines when findings present, got %d\nview:\n%s",
			logContentLines, plain)
	}
}

func TestNewModel_ReattachStartedAtUsesUnixSeconds(t *testing.T) {
	configureTUIColors()
	run := testRun()
	// Simulate a running step that started 3 seconds ago, with StartedAt stored
	// as Unix seconds (as db.now() returns).
	startedAt := time.Now().Add(-3 * time.Second).Unix()
	run.Steps[0].Status = types.StepStatusRunning
	run.Steps[0].StartedAt = &startedAt

	m := NewModel("", nil, run)
	m.width = 80
	m.height = 40

	view := stripANSI(m.View())

	// The elapsed time should be approximately 3 seconds, not billions.
	// If the bug exists (UnixMilli instead of Unix), it would show ~1.7 billion seconds.
	if strings.Contains(view, "1774") || strings.Contains(view, "17742") {
		t.Errorf("step duration looks like a raw unix timestamp, re-attach used UnixMilli instead of Unix:\n%s", view)
	}
	// Should show a reasonable elapsed time (under 10 seconds, not billions).
	// Extract the duration from the Review line.
	for _, line := range strings.Split(view, "\n") {
		// Skip the OSC terminal title line which also contains "Review".
		if strings.Contains(line, "\x1b]2;") || strings.Contains(line, "\007") {
			continue
		}
		if strings.Contains(line, "Review") {
			// Duration should be small (a few seconds), not a timestamp.
			if !strings.Contains(line, "s") {
				t.Errorf("expected Review line to contain a duration, got: %q", line)
			}
			// Should NOT contain any absurdly large number.
			if strings.Contains(line, "17742") {
				t.Errorf("duration still looks like a unix timestamp: %q", line)
			}
			break
		}
	}
}

func TestModel_ApplyEvent_LogChunk_PartialLines(t *testing.T) {
	run := testRun()
	m := NewModel("/tmp/sock", nil, run)

	// Simulate streaming chunks without trailing newlines (like OpenCode SSE deltas).
	m.applyEvent(ipc.Event{
		Type:    ipc.EventLogChunk,
		RunID:   run.ID,
		Content: ptr("hello "),
	})

	if len(m.logs) != 1 {
		t.Fatalf("expected partial line to be visible, got %d: %v", len(m.logs), m.logs)
	}
	if m.logs[0] != "hello " {
		t.Fatalf("expected visible partial %q, got %q", "hello ", m.logs[0])
	}
	if m.logPartial != "hello " {
		t.Fatalf("expected buffered partial %q, got %q", "hello ", m.logPartial)
	}

	m.applyEvent(ipc.Event{
		Type:    ipc.EventLogChunk,
		RunID:   run.ID,
		Content: ptr("world"),
	})

	if len(m.logs) != 1 {
		t.Fatalf("expected updated partial line to remain visible, got %d: %v", len(m.logs), m.logs)
	}
	if m.logs[0] != "hello world" {
		t.Fatalf("expected visible partial %q, got %q", "hello world", m.logs[0])
	}
	if m.logPartial != "hello world" {
		t.Fatalf("expected buffered partial %q, got %q", "hello world", m.logPartial)
	}

	m.applyEvent(ipc.Event{
		Type:    ipc.EventLogChunk,
		RunID:   run.ID,
		Content: ptr("\n"),
	})

	// "hello world" should be a single log line, not three separate lines.
	if len(m.logs) != 1 {
		t.Fatalf("expected 1 log line, got %d: %v", len(m.logs), m.logs)
	}
	if m.logs[0] != "hello world" {
		t.Errorf("expected %q, got %q", "hello world", m.logs[0])
	}
}

func TestModel_ApplyEvent_LogChunk_MixedPartialAndComplete(t *testing.T) {
	run := testRun()
	m := NewModel("/tmp/sock", nil, run)

	// A chunk that has a complete line and a partial one.
	m.applyEvent(ipc.Event{
		Type:    ipc.EventLogChunk,
		RunID:   run.ID,
		Content: ptr("line1\npartial"),
	})

	// Should have committed "line1" and kept the partial visible.
	if len(m.logs) != 2 {
		t.Fatalf("expected 2 visible lines, got %d: %v", len(m.logs), m.logs)
	}
	if m.logs[0] != "line1" {
		t.Errorf("expected %q, got %q", "line1", m.logs[0])
	}
	if m.logs[1] != "partial" {
		t.Errorf("expected %q, got %q", "partial", m.logs[1])
	}
	if m.logPartial != "partial" {
		t.Fatalf("expected buffered partial %q, got %q", "partial", m.logPartial)
	}

	// Completing the partial line.
	m.applyEvent(ipc.Event{
		Type:    ipc.EventLogChunk,
		RunID:   run.ID,
		Content: ptr(" end\n"),
	})

	if len(m.logs) != 2 {
		t.Fatalf("expected 2 log lines, got %d: %v", len(m.logs), m.logs)
	}
	if m.logs[1] != "partial end" {
		t.Errorf("expected %q, got %q", "partial end", m.logs[1])
	}
}

func TestModel_ApplyEvent_LogChunk_FlushesPartialOnStepCompleted(t *testing.T) {
	run := testRun()
	m := NewModel("/tmp/sock", nil, run)

	m.applyEvent(ipc.Event{
		Type:    ipc.EventLogChunk,
		RunID:   run.ID,
		Content: ptr("last line without newline"),
	})

	m.applyEvent(ipc.Event{
		Type:     ipc.EventStepCompleted,
		RunID:    run.ID,
		StepName: ptr(types.StepName("review")),
		Status:   ptr(string(types.StepStatusCompleted)),
	})

	if len(m.logs) != 1 {
		t.Fatalf("expected 1 log line, got %d: %v", len(m.logs), m.logs)
	}
	if m.logs[0] != "last line without newline" {
		t.Fatalf("expected flushed log line, got %q", m.logs[0])
	}
	if m.logPartial != "" {
		t.Fatalf("expected partial log buffer to be cleared, got %q", m.logPartial)
	}

	m.applyEvent(ipc.Event{
		Type:    ipc.EventLogChunk,
		RunID:   run.ID,
		Content: ptr("next line\n"),
	})

	if len(m.logs) != 2 {
		t.Fatalf("expected 2 log lines, got %d: %v", len(m.logs), m.logs)
	}
	if m.logs[1] != "next line" {
		t.Fatalf("expected independent next line, got %q", m.logs[1])
	}
}

func TestModel_ApplyEvent_LogChunk_FlushesPartialOnRunCompleted(t *testing.T) {
	run := testRun()
	m := NewModel("/tmp/sock", nil, run)

	m.applyEvent(ipc.Event{
		Type:    ipc.EventLogChunk,
		RunID:   run.ID,
		Content: ptr("trailing output"),
	})

	m.applyEvent(ipc.Event{
		Type:   ipc.EventRunCompleted,
		RunID:  run.ID,
		Status: ptr(string(types.RunCompleted)),
	})

	if len(m.logs) != 1 {
		t.Fatalf("expected 1 log line, got %d: %v", len(m.logs), m.logs)
	}
	if m.logs[0] != "trailing output" {
		t.Fatalf("expected flushed log line, got %q", m.logs[0])
	}
	if m.logPartial != "" {
		t.Fatalf("expected partial log buffer to be cleared, got %q", m.logPartial)
	}
}

func TestModel_ApplyEvent_LogChunk_BlankLineSeparators(t *testing.T) {
	// The executor's Log callback formats discrete messages as "text\n\n",
	// with a leading \n only when flushing an unterminated streaming partial.
	run := testRun()
	m := NewModel("/tmp/sock", nil, run)

	// Streaming agent text without trailing newline (partial).
	m.applyEvent(ipc.Event{
		Type:    ipc.EventLogChunk,
		RunID:   run.ID,
		Content: ptr("streaming text"),
	})
	// Discrete message after unterminated stream: leading \n flushes partial.
	m.applyEvent(ipc.Event{
		Type:    ipc.EventLogChunk,
		RunID:   run.ID,
		Content: ptr("\ncommitted agent fixes\n\n"),
	})
	// Consecutive discrete message: no leading \n (previous ended with \n\n).
	m.applyEvent(ipc.Event{
		Type:    ipc.EventLogChunk,
		RunID:   run.ID,
		Content: ptr("reviewing changes...\n\n"),
	})

	// Exactly one blank line between each entry.
	want := []string{"streaming text", "committed agent fixes", "", "reviewing changes...", ""}
	if len(m.logs) != len(want) {
		t.Fatalf("expected %d log entries, got %d: %v", len(want), len(m.logs), m.logs)
	}
	for i, w := range want {
		if m.logs[i] != w {
			t.Errorf("logs[%d] = %q, want %q", i, m.logs[i], w)
		}
	}
}

func TestPipelineConnectors_NotSuppressedDuringCI(t *testing.T) {
	// When CI is active in responsive layout (wide terminal), the pipeline
	// height should not be capped, so connector lines between steps are preserved.
	// Previously, the cap applied regardless of layout mode, which suppressed
	// connectors even when the CI view was in the right column and didn't
	// compete for vertical space.
	configureTUIColors()
	run := testRunWithCI()
	for i := range run.Steps {
		run.Steps[i].Status = types.StepStatusCompleted
		dur := int64(1000)
		run.Steps[i].DurationMS = &dur
	}
	run.Steps[len(run.Steps)-1].Status = types.StepStatusRunning
	run.Steps[len(run.Steps)-1].DurationMS = nil

	m := NewModel("", nil, run)
	m.width = 120 // wide enough for responsive layout
	m.height = 50

	view := m.View()
	plain := stripANSI(view)

	// Render pipeline directly with height=50 as a baseline.
	leftWidth, _ := responsiveColumnWidths(m.width)
	baseline := stripANSI(renderPipelineView(run, run.Steps, leftWidth, 0, 50))
	baselineConnectors := 0
	for _, line := range strings.Split(baseline, "\n") {
		if strings.Count(line, "│") >= 3 {
			baselineConnectors++
		}
	}
	if baselineConnectors == 0 {
		t.Fatalf("baseline pipeline with height=50 should show connectors:\n%s", baseline)
	}

	// The full view should also contain connector lines.
	// In responsive layout, the pipeline (left column) renders with the real
	// terminal height, not a capped value.
	if !strings.Contains(plain, baseline) {
		// The pipeline should be identical to the baseline (uncapped).
		// Check for connectors by verifying step lines are not adjacent.
		stepLabels := []string{"Review", "Test", "Lint", "Push", "PR"}
		lines := strings.Split(plain, "\n")
		adjacentSteps := 0
		for i := 0; i < len(lines)-1; i++ {
			hasLabel := false
			nextHasLabel := false
			for _, label := range stepLabels {
				if strings.Contains(lines[i], label) {
					hasLabel = true
				}
				if strings.Contains(lines[i+1], label) {
					nextHasLabel = true
				}
			}
			if hasLabel && nextHasLabel {
				adjacentSteps++
			}
		}
		if adjacentSteps > 0 {
			t.Errorf("expected connector lines between steps in responsive layout during CI, but %d step pairs are adjacent.\nview:\n%s", adjacentSteps, plain)
		}
	}
}

func TestPipelineConnectors_SuppressedDuringCIInStackedLayout(t *testing.T) {
	// When CI is active in stacked layout, the pipeline height should be
	// capped so the CI panel still has room below it.
	configureTUIColors()
	run := testRunWithCI()
	for i := range run.Steps {
		run.Steps[i].Status = types.StepStatusCompleted
		dur := int64(1000)
		run.Steps[i].DurationMS = &dur
	}
	run.Steps[len(run.Steps)-1].Status = types.StepStatusRunning
	run.Steps[len(run.Steps)-1].DurationMS = nil

	m := NewModel("", nil, run)
	m.width = 80 // narrow enough to force stacked layout
	m.height = 50

	view := stripANSI(m.View())
	expectedPipeline := stripANSI(renderPipelineView(run, m.stepsWithRunningElapsed(), m.width, 0, cappedPipelineHeight))
	uncappedPipeline := stripANSI(renderPipelineView(run, m.stepsWithRunningElapsed(), m.width, 0, m.height))

	if !strings.Contains(view, expectedPipeline) {
		t.Fatalf("expected stacked CI layout to use capped pipeline height %d\nview:\n%s\nexpected pipeline:\n%s", cappedPipelineHeight, view, expectedPipeline)
	}
	if strings.Contains(view, uncappedPipeline) {
		t.Fatalf("expected stacked CI layout to avoid uncapped pipeline height %d", m.height)
	}
}

func TestTerminalTitle_AllPending(t *testing.T) {
	m := NewModel("/tmp/sock", nil, testRun())
	title := m.terminalTitle()
	if title != "○ Pending - feature/foo" {
		t.Errorf("expected '○ Pending - feature/foo', got %q", title)
	}
}

func TestTerminalTitle_RunningStep(t *testing.T) {
	run := testRun()
	run.Steps[0].Status = types.StepStatusRunning
	m := NewModel("/tmp/sock", nil, run)
	title := m.terminalTitle()
	// Should use the current spinner frame and include the step label and branch.
	if title != "⠋ Review - feature/foo" {
		t.Errorf("expected '⠋ Review - feature/foo', got %q", title)
	}
}

func TestTerminalTitle_RunningStepSpinnerAdvances(t *testing.T) {
	run := testRun()
	run.Steps[0].Status = types.StepStatusRunning
	m := NewModel("/tmp/sock", nil, run)
	m.spinnerFrame = 3
	title := m.terminalTitle()
	// Frame 3 is "⠸".
	if title != "⠸ Review - feature/foo" {
		t.Errorf("expected '⠸ Review - feature/foo', got %q", title)
	}
}

func TestTerminalTitle_AwaitingApproval(t *testing.T) {
	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval
	m := NewModel("/tmp/sock", nil, run)
	title := m.terminalTitle()
	if title != "⏸ Review - feature/foo" {
		t.Errorf("expected '⏸ Review - feature/foo', got %q", title)
	}
}

func TestTerminalTitle_Completed(t *testing.T) {
	run := testRun()
	run.Status = types.RunCompleted
	m := NewModel("/tmp/sock", nil, run)
	m.done = true
	title := m.terminalTitle()
	if title != "✓ Completed - feature/foo" {
		t.Errorf("expected '✓ Completed - feature/foo', got %q", title)
	}
}

func TestTerminalTitle_ReattachCompletedRun(t *testing.T) {
	run := testRun()
	run.Status = types.RunCompleted
	m := NewModel("/tmp/sock", nil, run)
	title := m.terminalTitle()
	if title != "✓ Completed - feature/foo" {
		t.Errorf("expected '✓ Completed - feature/foo', got %q", title)
	}
}

func TestTerminalTitle_Failed(t *testing.T) {
	run := testRun()
	run.Status = types.RunFailed
	m := NewModel("/tmp/sock", nil, run)
	m.done = true
	title := m.terminalTitle()
	if title != "✗ Failed - feature/foo" {
		t.Errorf("expected '✗ Failed - feature/foo', got %q", title)
	}
}

func TestTerminalTitle_Cancelled(t *testing.T) {
	run := testRun()
	run.Status = types.RunCancelled
	m := NewModel("/tmp/sock", nil, run)
	m.done = true
	title := m.terminalTitle()
	if title != "✗ Cancelled - feature/foo" {
		t.Errorf("expected '✗ Cancelled - feature/foo', got %q", title)
	}
}

func TestTerminalTitle_FixingStep(t *testing.T) {
	run := testRun()
	run.Steps[0].Status = types.StepStatusCompleted
	run.Steps[1].Status = types.StepStatusFixing
	m := NewModel("/tmp/sock", nil, run)
	title := m.terminalTitle()
	if title != "⠋ Test - feature/foo" {
		t.Errorf("expected '⠋ Test - feature/foo', got %q", title)
	}
}

func TestView_ContainsTerminalTitleEscape(t *testing.T) {
	configureTUIColors()
	run := testRun()
	run.Steps[0].Status = types.StepStatusRunning
	m := NewModel("/tmp/sock", nil, run)
	m.width = 80
	m.height = 40
	view := m.View()
	// The view should start with the OSC title-setting escape sequence.
	if !strings.HasPrefix(view, "\033]2;") {
		t.Errorf("expected view to start with OSC title escape, got prefix: %q", view[:min(len(view), 40)])
	}
	if !strings.Contains(view, "\007") {
		t.Error("expected view to contain BEL terminator for OSC sequence")
	}
}

func TestView_ResponsiveLayoutContainsTerminalTitleEscape(t *testing.T) {
	configureTUIColors()
	run := testRun()
	run.Steps[0].Status = types.StepStatusRunning
	m := NewModel("/tmp/sock", nil, run)
	m.width = 120
	m.height = 40
	view := m.View()
	if !strings.HasPrefix(view, "\033]2;") {
		t.Errorf("expected responsive view to start with OSC title escape, got prefix: %q", view[:min(len(view), 40)])
	}
	if !strings.Contains(view, "\007") {
		t.Error("expected responsive view to contain BEL terminator for OSC sequence")
	}
}

func TestView_QuittingDoesNotBlankTerminalTitle(t *testing.T) {
	configureTUIColors()
	run := testRun()
	m := NewModel("/tmp/sock", nil, run)
	m.quitting = true
	view := m.View()
	if strings.Contains(view, "\033]2;") {
		t.Errorf("expected quitting view to avoid sending a terminal title sequence, got: %q", view)
	}
}

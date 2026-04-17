package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/muesli/termenv"
)

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
	run := testRun()
	run.Status = types.RunCompleted
	run.PRURL = &prURL
	footer := renderFooter(true, false, false, run, "", 80)
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
	footer := renderFooter(false, false, false, nil, "", 80)
	stripped := stripANSI(footer)

	if strings.Contains(stripped, "open PR") {
		t.Errorf("expected no open PR action in footer, got: %s", stripped)
	}
}

func TestRenderFooter_FailedRun_ShowsRerun(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	run := testRun()
	run.Status = types.RunFailed
	footer := renderFooter(true, false, false, run, "", 80)
	stripped := stripANSI(footer)

	if !strings.Contains(stripped, "rerun") {
		t.Fatalf("expected footer to contain rerun action, got: %s", stripped)
	}
}

func TestRenderFooter_PRURL_ActionShownAtNarrowWidth(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	prURL := "https://github.com/test/repo/pull/42"
	// Even at narrow width, "open PR" action should appear
	run := testRun()
	run.Status = types.RunCompleted
	run.PRURL = &prURL
	footer := renderFooter(true, false, false, run, "", 40)
	stripped := stripANSI(footer)

	if !strings.Contains(stripped, "open PR") {
		t.Errorf("expected footer to contain 'open PR' action, got: %s", stripped)
	}
}

func TestRenderFooter_WithAvailableUpdate_ShowsIndicator(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	footer := renderFooter(false, false, false, nil, "v1.2.3", 80)
	stripped := stripANSI(footer)

	if !strings.Contains(stripped, "v1.2.3 available") {
		t.Fatalf("expected footer to contain update indicator, got: %s", stripped)
	}

	if !strings.HasSuffix(strings.TrimRight(stripped, " "), "v1.2.3 available") {
		t.Fatalf("expected update indicator at right edge, got: %s", stripped)
	}
}

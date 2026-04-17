package tui

import (
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

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

package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/muesli/termenv"
)

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
	footer := renderFooter(false, false, false, run, "", m.width)
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

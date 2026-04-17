package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/muesli/termenv"
)

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

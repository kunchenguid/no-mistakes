package tui

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

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

	updated, cmd := m.Update(eventMsg{event: ipc.Event{
		Type:     ipc.EventStepStarted,
		RunID:    run.ID,
		StepName: ptr(types.StepReview),
	}, subscriptionID: m.subscriptionID})
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

	updated, _ := m.Update(connectedMsg{events: ch, cancelSub: cancel, subscriptionID: m.subscriptionID})
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

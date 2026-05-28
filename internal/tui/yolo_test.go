package tui

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/muesli/termenv"
)

func TestModel_Update_YoloKeyTogglesMode(t *testing.T) {
	run := testRun()
	m := NewModel("/tmp/sock", nil, run)
	if m.yoloMode {
		t.Fatal("expected yolo mode off by default")
	}

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	model := updated.(Model)
	if !model.yoloMode {
		t.Fatal("expected first y press to enable yolo mode")
	}

	updated, _ = model.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'y'}})
	model = updated.(Model)
	if model.yoloMode {
		t.Fatal("expected second y press to disable yolo mode")
	}
}

func TestModel_Yolo_AutoApprovesAwaitingStep(t *testing.T) {
	sock := testSocketPath(t)
	srv := startTestIPCServer(t, sock)

	var mu sync.Mutex
	var calls []ipc.RespondParams
	srv.Handle(ipc.MethodRespond, func(_ context.Context, raw json.RawMessage) (interface{}, error) {
		var params ipc.RespondParams
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, err
		}
		mu.Lock()
		calls = append(calls, params)
		mu.Unlock()
		return &ipc.RespondResult{}, nil
	})

	client, err := ipc.Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval
	m := NewModel(sock, client, run)
	m.yoloMode = true

	cmd := m.maybeAutoApproveCmd()
	if cmd == nil {
		t.Fatal("expected auto-approve command when yolo on and step awaiting")
	}
	if msg := cmd(); msg != nil {
		t.Fatalf("expected nil msg from auto-approve, got %#v", msg)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 1 {
		t.Fatalf("expected exactly 1 respond call, got %d", len(calls))
	}
	if calls[0].Action != types.ActionApprove {
		t.Fatalf("action = %s, want %s", calls[0].Action, types.ActionApprove)
	}
	if calls[0].Step != types.StepReview {
		t.Fatalf("step = %s, want %s", calls[0].Step, types.StepReview)
	}
}

func TestModel_Yolo_DoesNotAutoApproveTwiceForSameStep(t *testing.T) {
	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval
	m := NewModel("/tmp/sock", nil, run)
	m.yoloMode = true

	if cmd := m.maybeAutoApproveCmd(); cmd == nil {
		t.Fatal("expected first auto-approve command")
	}
	if cmd := m.maybeAutoApproveCmd(); cmd != nil {
		t.Fatal("expected no second auto-approve command for the same awaiting step")
	}
}

func TestModel_Yolo_NoAutoApproveWhenOff(t *testing.T) {
	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval
	m := NewModel("/tmp/sock", nil, run)

	if cmd := m.maybeAutoApproveCmd(); cmd != nil {
		t.Fatal("expected no auto-approve command when yolo off")
	}
}

func TestModel_View_FooterShowsYoloLabel(t *testing.T) {
	lipgloss.SetColorProfile(termenv.Ascii)
	run := testRun()
	m := NewModel("", nil, run)
	m.width = 120
	m.height = 40

	plain := stripANSI(m.View())
	if !footerContains(plain, "y", "yolo") {
		t.Errorf("footer should show 'y yolo' when yolo off, got:\n%s", plain)
	}

	m.yoloMode = true
	plain = stripANSI(m.View())
	if !footerContains(plain, "y", "end yolo") {
		t.Errorf("footer should show 'y end yolo' when yolo on, got:\n%s", plain)
	}
}

func footerContains(plain string, needles ...string) bool {
	for _, line := range strings.Split(plain, "\n") {
		all := true
		for _, n := range needles {
			if !strings.Contains(line, n) {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}

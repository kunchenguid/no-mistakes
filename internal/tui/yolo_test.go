package tui

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
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
		return &ipc.RespondResult{OK: true}, nil
	})
	srv.Handle(ipc.MethodGetRun, func(_ context.Context, _ json.RawMessage) (interface{}, error) {
		return &ipc.GetRunResult{Run: testRun()}, nil
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

func TestModel_Yolo_DoesNotConsentWhenRunRefreshFails(t *testing.T) {
	sock := testSocketPath(t)
	srv := startTestIPCServer(t, sock)

	checkErr := errors.New("repair projection unavailable")
	srv.Handle(ipc.MethodGetRun, func(_ context.Context, _ json.RawMessage) (interface{}, error) {
		return nil, checkErr
	})
	var responses int
	srv.Handle(ipc.MethodRespond, func(_ context.Context, _ json.RawMessage) (interface{}, error) {
		responses++
		return &ipc.RespondResult{OK: true}, nil
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

	msg := m.maybeAutoApproveCmd()()
	got, ok := msg.(errMsg)
	if !ok || !strings.Contains(got.err.Error(), checkErr.Error()) {
		t.Fatalf("yolo refresh result = %#v, want errMsg containing %q", msg, checkErr)
	}
	if responses != 0 {
		t.Fatalf("respond calls = %d, want none when run state is unknown", responses)
	}
}

func TestModel_Yolo_SurfacesAbortFailureWithoutApproving(t *testing.T) {
	sock := testSocketPath(t)
	srv := startTestIPCServer(t, sock)

	run := testRun()
	run.Steps[0].Status = types.StepStatusFixReview
	run.BlockingRepairUnresolved = true
	srv.Handle(ipc.MethodGetRun, func(_ context.Context, _ json.RawMessage) (interface{}, error) {
		return &ipc.GetRunResult{Run: run}, nil
	})
	abortErr := errors.New("abort transport failed")
	var actions []types.ApprovalAction
	srv.Handle(ipc.MethodRespond, func(_ context.Context, raw json.RawMessage) (interface{}, error) {
		var params ipc.RespondParams
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, err
		}
		actions = append(actions, params.Action)
		return nil, abortErr
	})

	client, err := ipc.Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	m := NewModel(sock, client, run)
	m.yoloMode = true

	msg := m.maybeAutoApproveCmd()()
	got, ok := msg.(errMsg)
	if !ok || !strings.Contains(got.err.Error(), abortErr.Error()) {
		t.Fatalf("yolo abort result = %#v, want errMsg containing %q", msg, abortErr)
	}
	if !slices.Equal(actions, []types.ApprovalAction{types.ActionAbort}) {
		t.Fatalf("response actions = %v, want only abort", actions)
	}
}

// captureRespond wires a model-facing IPC server that records every Respond
// call, returning the connected client plus accessors for the captured params.
func captureRespond(t *testing.T) (string, *ipc.Client, func() []ipc.RespondParams) {
	t.Helper()
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
		return &ipc.RespondResult{OK: true}, nil
	})
	srv.Handle(ipc.MethodGetRun, func(_ context.Context, _ json.RawMessage) (interface{}, error) {
		return &ipc.GetRunResult{Run: testRun()}, nil
	})

	client, err := ipc.Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })

	return sock, client, func() []ipc.RespondParams {
		mu.Lock()
		defer mu.Unlock()
		return append([]ipc.RespondParams(nil), calls...)
	}
}

func TestModel_Yolo_FixesActionableFindings(t *testing.T) {
	sock, client, snapshot := captureRespond(t)

	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval
	fj := `{"findings":[{"id":"review-1","severity":"warning","description":"design choice","action":"ask-user"}],"summary":"1 issue"}`
	run.Steps[0].FindingsJSON = &fj
	m := NewModel(sock, client, run)
	m.yoloMode = true

	cmd := m.maybeAutoApproveCmd()
	if cmd == nil {
		t.Fatal("expected a yolo command for an awaiting step with actionable findings")
	}
	if msg := cmd(); msg != nil {
		t.Fatalf("expected nil msg, got %#v", msg)
	}

	calls := snapshot()
	if len(calls) != 1 {
		t.Fatalf("expected 1 respond call, got %d", len(calls))
	}
	t.Logf("yolo response action=%s finding_ids=%v", calls[0].Action, calls[0].FindingIDs)
	if calls[0].Action != types.ActionFix {
		t.Fatalf("action = %s, want %s", calls[0].Action, types.ActionFix)
	}
	if len(calls[0].FindingIDs) != 1 || calls[0].FindingIDs[0] != "review-1" {
		t.Fatalf("FindingIDs = %v, want [review-1]", calls[0].FindingIDs)
	}
}

func TestModel_Yolo_ResolutionMatchesAXISelectableIDContract(t *testing.T) {
	tests := []struct {
		name       string
		findings   string
		wantAction types.ApprovalAction
		wantIDs    []string
	}{
		{
			name:       "id-less auto-fix finding is approved",
			findings:   `{"findings":[{"severity":"warning","description":"fixable but not selectable","action":"auto-fix"}],"summary":"1 issue"}`,
			wantAction: types.ActionApprove,
		},
		{
			name:       "id-less ask-user finding is approved",
			findings:   `{"findings":[{"severity":"warning","description":"needs consent but is not selectable","action":"ask-user"}],"summary":"1 issue"}`,
			wantAction: types.ActionApprove,
		},
		{
			name:       "no-op ID does not make an ID-less actionable finding selectable",
			findings:   `{"findings":[{"id":"review-1","severity":"info","description":"selectable context","action":"no-op"},{"severity":"warning","description":"actionable but not selectable","action":"auto-fix"}],"summary":"2 findings"}`,
			wantAction: types.ActionApprove,
		},
		{
			name:       "mixed actionable and no-op findings fix only actionable IDs",
			findings:   `{"findings":[{"id":"review-1","severity":"warning","description":"fixable issue","action":"auto-fix"},{"id":"review-2","severity":"info","description":"informational","action":"no-op"}],"summary":"2 findings"}`,
			wantAction: types.ActionFix,
			wantIDs:    []string{"review-1"},
		},
		{
			name:       "no-op-only findings are approved",
			findings:   `{"findings":[{"id":"review-1","severity":"info","description":"informational","action":"no-op"}],"summary":"1 note"}`,
			wantAction: types.ActionApprove,
		},
		{
			name:       "normal selected IDs are fixed",
			findings:   `{"findings":[{"id":"review-1","severity":"warning","description":"first issue","action":"auto-fix"},{"id":"review-2","severity":"warning","description":"second issue","action":"ask-user"}],"summary":"2 issues"}`,
			wantAction: types.ActionFix,
			wantIDs:    []string{"review-1", "review-2"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sock, client, snapshot := captureRespond(t)
			run := testRun()
			run.Steps[0].Status = types.StepStatusAwaitingApproval
			run.Steps[0].FindingsJSON = &tt.findings
			m := NewModel(sock, client, run)
			m.yoloMode = true

			cmd := m.maybeAutoApproveCmd()
			if cmd == nil {
				t.Fatal("expected a yolo resolution command")
			}
			if msg := cmd(); msg != nil {
				t.Fatalf("expected nil msg, got %#v", msg)
			}

			calls := snapshot()
			if len(calls) != 1 {
				t.Fatalf("respond calls = %d, want 1", len(calls))
			}
			if calls[0].Action != tt.wantAction {
				t.Fatalf("action = %s, want %s", calls[0].Action, tt.wantAction)
			}
			if !slices.Equal(calls[0].FindingIDs, tt.wantIDs) {
				t.Fatalf("FindingIDs = %v, want %v", calls[0].FindingIDs, tt.wantIDs)
			}
		})
	}
}

func TestModel_Yolo_FixPayloadCarriesOnlyActionableSelections(t *testing.T) {
	tests := []struct {
		name       string
		findings   string
		added      []types.Finding
		wantAction types.ApprovalAction
		wantIDs    []string
		wantAdded  []types.Finding
	}{
		{
			name:     "user-only actionable gate fixes through added findings",
			findings: `{"findings":[],"summary":"no agent findings"}`,
			added: []types.Finding{
				{ID: "user-1", Severity: "warning", Description: "user-requested repair", Action: types.ActionAutoFix, Source: types.FindingSourceUser},
			},
			wantAction: types.ActionFix,
			wantAdded: []types.Finding{
				{ID: "user-1", Severity: "warning", Description: "user-requested repair", Action: types.ActionAutoFix, Source: types.FindingSourceUser},
			},
		},
		{
			name:     "mixed agent and user gate preserves both payload classes",
			findings: `{"findings":[{"id":"review-1","severity":"error","description":"agent repair","action":"auto-fix"},{"id":"review-2","severity":"info","description":"agent context","action":"no-op"}],"summary":"mixed"}`,
			added: []types.Finding{
				{ID: "user-1", Severity: "warning", Description: "user repair", Action: types.ActionAskUser, Source: types.FindingSourceUser},
				{ID: "user-2", Severity: "info", Description: "user context", Action: types.ActionNoOp, Source: types.FindingSourceUser},
			},
			wantAction: types.ActionFix,
			wantIDs:    []string{"review-1"},
			wantAdded: []types.Finding{
				{ID: "user-1", Severity: "warning", Description: "user repair", Action: types.ActionAskUser, Source: types.FindingSourceUser},
			},
		},
		{
			name:     "all no-op gate approves without inert fix payload",
			findings: `{"findings":[{"id":"review-1","severity":"info","description":"agent context","action":"no-op"}],"summary":"context"}`,
			added: []types.Finding{
				{ID: "user-1", Severity: "info", Description: "user context", Action: types.ActionNoOp, Source: types.FindingSourceUser},
			},
			wantAction: types.ActionApprove,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sock, client, snapshot := captureRespond(t)
			run := testRun()
			run.Steps[0].Status = types.StepStatusAwaitingApproval
			run.Steps[0].FindingsJSON = &tt.findings
			m := NewModel(sock, client, run)
			m.addedFindings[types.StepReview] = append([]types.Finding(nil), tt.added...)
			m.yoloMode = true

			cmd := m.maybeAutoApproveCmd()
			if cmd == nil {
				t.Fatal("expected a yolo resolution command")
			}
			if msg := cmd(); msg != nil {
				t.Fatalf("expected nil msg, got %#v", msg)
			}

			calls := snapshot()
			if len(calls) != 1 {
				t.Fatalf("respond calls = %d, want 1", len(calls))
			}
			if calls[0].Action != tt.wantAction {
				t.Fatalf("action = %s, want %s", calls[0].Action, tt.wantAction)
			}
			if !slices.Equal(calls[0].FindingIDs, tt.wantIDs) {
				t.Fatalf("FindingIDs = %v, want %v", calls[0].FindingIDs, tt.wantIDs)
			}
			if !slices.Equal(calls[0].AddedFindings, tt.wantAdded) {
				t.Fatalf("AddedFindings = %v, want %v", calls[0].AddedFindings, tt.wantAdded)
			}
		})
	}
}

func TestModel_Yolo_FixesAllActionableFindingsDespiteManualDeselection(t *testing.T) {
	sock, client, snapshot := captureRespond(t)

	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval
	fj := `{"findings":[{"id":"review-1","severity":"warning","description":"first","action":"ask-user"},{"id":"review-2","severity":"warning","description":"second","action":"ask-user"}],"summary":"2 issues"}`
	run.Steps[0].FindingsJSON = &fj
	m := NewModel(sock, client, run)
	m.yoloMode = true
	m.findingSelections[types.StepReview] = map[string]bool{"review-1": true}

	cmd := m.maybeAutoApproveCmd()
	if cmd == nil {
		t.Fatal("expected a yolo command for an awaiting step with actionable findings")
	}
	if msg := cmd(); msg != nil {
		t.Fatalf("expected nil msg, got %#v", msg)
	}

	calls := snapshot()
	if len(calls) != 1 {
		t.Fatalf("expected 1 respond call, got %d", len(calls))
	}
	t.Logf("yolo response action=%s finding_ids=%v", calls[0].Action, calls[0].FindingIDs)
	if calls[0].Action != types.ActionFix {
		t.Fatalf("action = %s, want %s", calls[0].Action, types.ActionFix)
	}
	if got, want := calls[0].FindingIDs, []string{"review-1", "review-2"}; !slices.Equal(got, want) {
		t.Fatalf("FindingIDs = %v, want %v", got, want)
	}
}

func TestModel_ManualFixPreservesActionableSelectionAndDropsNoOp(t *testing.T) {
	sock, client, snapshot := captureRespond(t)

	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval
	findings := `{"findings":[{"id":"review-1","severity":"warning","description":"leave deselected","action":"auto-fix"},{"id":"review-2","severity":"warning","description":"selected fix","action":"ask-user"},{"id":"review-3","severity":"info","description":"selected note","action":"no-op"}],"summary":"3 findings"}`
	run.Steps[0].FindingsJSON = &findings
	m := NewModel(sock, client, run)
	m.addedFindings[types.StepReview] = []types.Finding{
		{ID: "user-1", Severity: "warning", Description: "selected user fix", Action: types.ActionAutoFix, Source: types.FindingSourceUser},
		{ID: "user-2", Severity: "warning", Description: "deselected user fix", Action: types.ActionAskUser, Source: types.FindingSourceUser},
		{ID: "user-3", Severity: "info", Description: "selected user note", Action: types.ActionNoOp, Source: types.FindingSourceUser},
	}
	m.findingSelections[types.StepReview] = map[string]bool{
		"review-2": true,
		"review-3": true,
		"user-1":   true,
		"user-3":   true,
	}

	cmd := m.respondCmd(types.ActionFix)
	if cmd == nil {
		t.Fatal("expected a manual fix command for the selected actionable finding")
	}
	if msg := cmd(); msg != nil {
		t.Fatalf("expected nil msg, got %#v", msg)
	}

	calls := snapshot()
	if len(calls) != 1 {
		t.Fatalf("respond calls = %d, want 1", len(calls))
	}
	if got, want := calls[0].FindingIDs, []string{"review-2"}; !slices.Equal(got, want) {
		t.Fatalf("FindingIDs = %v, want manually selected actionable IDs %v", got, want)
	}
	if got, want := calls[0].AddedFindings, []types.Finding{m.addedFindings[types.StepReview][0]}; !slices.Equal(got, want) {
		t.Fatalf("AddedFindings = %v, want manually selected actionable findings %v", got, want)
	}
}

func TestModel_Yolo_ApprovesNonActionableFindings(t *testing.T) {
	sock, client, snapshot := captureRespond(t)

	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval
	fj := `{"findings":[{"id":"review-1","severity":"info","description":"fyi","action":"no-op"}],"summary":"1 note"}`
	run.Steps[0].FindingsJSON = &fj
	m := NewModel(sock, client, run)
	m.yoloMode = true

	cmd := m.maybeAutoApproveCmd()
	if cmd == nil {
		t.Fatal("expected a yolo command for an awaiting step")
	}
	if msg := cmd(); msg != nil {
		t.Fatalf("expected nil msg, got %#v", msg)
	}

	calls := snapshot()
	if len(calls) != 1 {
		t.Fatalf("expected 1 respond call, got %d", len(calls))
	}
	t.Logf("yolo response action=%s finding_ids=%v", calls[0].Action, calls[0].FindingIDs)
	if calls[0].Action != types.ActionApprove {
		t.Fatalf("action = %s, want %s (non-actionable findings should be approved)", calls[0].Action, types.ActionApprove)
	}
}

func TestModel_Yolo_ApprovesFixReviewAfterFixingOnce(t *testing.T) {
	sock, client, snapshot := captureRespond(t)

	run := testRun()
	run.Steps[0].Status = types.StepStatusAwaitingApproval
	fj := `{"findings":[{"id":"review-1","severity":"warning","description":"design choice","action":"ask-user"}],"summary":"1 issue"}`
	run.Steps[0].FindingsJSON = &fj
	m := NewModel(sock, client, run)
	m.yoloMode = true

	// First gate: actionable findings -> fix.
	if cmd := m.maybeAutoApproveCmd(); cmd != nil {
		cmd()
	} else {
		t.Fatal("expected fix command on first gate")
	}

	// The fix re-runs the step, which re-enters the gate as a fix_review. Yolo
	// must not fix again (that risks an unbounded loop); it accepts the result.
	m.steps[0].Status = types.StepStatusFixReview
	if cmd := m.maybeAutoApproveCmd(); cmd != nil {
		cmd()
	} else {
		t.Fatal("expected approve command on fix_review gate")
	}

	calls := snapshot()
	if len(calls) != 2 {
		t.Fatalf("expected 2 respond calls, got %d", len(calls))
	}
	t.Logf("yolo responses first_action=%s first_finding_ids=%v second_action=%s second_finding_ids=%v", calls[0].Action, calls[0].FindingIDs, calls[1].Action, calls[1].FindingIDs)
	if calls[0].Action != types.ActionFix {
		t.Fatalf("first action = %s, want %s", calls[0].Action, types.ActionFix)
	}
	if calls[1].Action != types.ActionApprove {
		t.Fatalf("second action = %s, want %s", calls[1].Action, types.ActionApprove)
	}
}

func TestModel_Yolo_ApprovesExistingFixReviewWithoutPriorFix(t *testing.T) {
	sock, client, snapshot := captureRespond(t)

	run := testRun()
	run.Steps[0].Status = types.StepStatusFixReview
	fj := `{"findings":[{"id":"review-1","severity":"warning","description":"still here","action":"ask-user"}],"summary":"1 issue"}`
	run.Steps[0].FindingsJSON = &fj
	m := NewModel(sock, client, run)
	m.yoloMode = true

	cmd := m.maybeAutoApproveCmd()
	if cmd == nil {
		t.Fatal("expected yolo to approve an existing fix_review gate")
	}
	if msg := cmd(); msg != nil {
		t.Fatalf("expected nil msg, got %#v", msg)
	}

	calls := snapshot()
	if len(calls) != 1 {
		t.Fatalf("expected 1 respond call, got %d", len(calls))
	}
	t.Logf("yolo response action=%s finding_ids=%v", calls[0].Action, calls[0].FindingIDs)
	if calls[0].Action != types.ActionApprove {
		t.Fatalf("action = %s, want %s", calls[0].Action, types.ActionApprove)
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

package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/kunchenguid/no-mistakes/internal/cimonitor"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func ciRunView(ciStatus types.StepStatus) runView {
	return runView{
		ID:     "run-1",
		Branch: "feature/x",
		Status: string(types.RunRunning),
		Steps: []stepView{
			{Name: string(types.StepPR), Status: string(types.StepStatusCompleted)},
			{Name: string(types.StepCI), Status: string(ciStatus)},
		},
	}
}

func TestCIReadyToMerge(t *testing.T) {
	passedLogs := []string{
		"monitoring CI for PR #42 (timeout: 4h)...",
		cimonitor.ChecksPassedMsg,
	}
	runningLogs := []string{"monitoring CI for PR #42 (timeout: 4h)..."}

	tests := []struct {
		name     string
		rv       runView
		ciLogs   []string
		wantStop bool
	}{
		{
			name:     "ci running and checks passed",
			rv:       ciRunView(types.StepStatusRunning),
			ciLogs:   passedLogs,
			wantStop: true,
		},
		{
			name:     "ci running but checks not passed yet",
			rv:       ciRunView(types.StepStatusRunning),
			ciLogs:   runningLogs,
			wantStop: false,
		},
		{
			name:     "checks passed but ci step already completed",
			rv:       ciRunView(types.StepStatusCompleted),
			ciLogs:   passedLogs,
			wantStop: false,
		},
		{
			name:     "no ci step in run",
			rv:       runView{Status: string(types.RunRunning), Steps: []stepView{{Name: string(types.StepPR), Status: string(types.StepStatusCompleted)}}},
			ciLogs:   passedLogs,
			wantStop: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ciReadyToMerge(tt.rv, tt.ciLogs); got != tt.wantStop {
				t.Errorf("ciReadyToMerge() = %v, want %v", got, tt.wantStop)
			}
		})
	}
}

func TestGateResolution(t *testing.T) {
	tests := []struct {
		name         string
		gate         stepView
		alreadyFixed bool
		wantAction   types.ApprovalAction
		wantIDs      []string
	}{
		{
			name: "actionable findings are fixed with every finding selected",
			gate: stepView{
				Name:         "review",
				Status:       string(types.StepStatusAwaitingApproval),
				FindingsJSON: `{"findings":[{"id":"review-1","severity":"warning","description":"design choice","action":"ask-user"},{"id":"review-2","severity":"info","description":"fyi","action":"no-op"}],"summary":"2"}`,
			},
			wantAction: types.ActionFix,
			wantIDs:    []string{"review-1", "review-2"},
		},
		{
			name: "only non-actionable findings are approved",
			gate: stepView{
				Name:         "test",
				Status:       string(types.StepStatusAwaitingApproval),
				FindingsJSON: `{"findings":[{"id":"test-1","severity":"info","description":"fyi","action":"no-op"}],"summary":"1"}`,
			},
			wantAction: types.ActionApprove,
		},
		{
			name: "no findings are approved",
			gate: stepView{
				Name:         "push",
				Status:       string(types.StepStatusAwaitingApproval),
				FindingsJSON: ``,
			},
			wantAction: types.ActionApprove,
		},
		{
			name: "already fixed step is approved (no fix loop)",
			gate: stepView{
				Name:         "review",
				Status:       string(types.StepStatusFixReview),
				FindingsJSON: `{"findings":[{"id":"review-1","severity":"warning","description":"still here","action":"ask-user"}],"summary":"1"}`,
			},
			alreadyFixed: true,
			wantAction:   types.ActionApprove,
		},
		{
			name: "reattached fix review is approved without in-memory fix state",
			gate: stepView{
				Name:         "review",
				Status:       string(types.StepStatusFixReview),
				FindingsJSON: `{"findings":[{"id":"review-1","severity":"warning","description":"still here","action":"ask-user"}],"summary":"1"}`,
			},
			wantAction: types.ActionApprove,
		},
		{
			name: "actionable findings without ids are approved rather than fixing nothing",
			gate: stepView{
				Name:         "review",
				Status:       string(types.StepStatusAwaitingApproval),
				FindingsJSON: `{"findings":[{"severity":"warning","description":"no id","action":"ask-user"}],"summary":"1"}`,
			},
			wantAction: types.ActionApprove,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			action, ids := gateResolution(tt.gate, tt.alreadyFixed)
			t.Logf("auto-resolution action=%s finding_ids=%v", action, ids)
			if action != tt.wantAction {
				t.Fatalf("action = %s, want %s", action, tt.wantAction)
			}
			if len(ids) != len(tt.wantIDs) {
				t.Fatalf("ids = %v, want %v", ids, tt.wantIDs)
			}
			for i := range ids {
				if ids[i] != tt.wantIDs[i] {
					t.Fatalf("ids = %v, want %v", ids, tt.wantIDs)
				}
			}
		})
	}
}

func TestRenderDriveResult_ChecksPassed(t *testing.T) {
	run := &ipc.RunInfo{
		ID:      "run-1",
		Branch:  "feature/x",
		Status:  types.RunRunning, // not terminal: daemon keeps monitoring until merge
		HeadSHA: "abcdef1234567890",
		PRURL:   strptr("https://github.com/user/repo/pull/42"),
		Steps: []ipc.StepResultInfo{
			{StepName: types.StepCI, Status: types.StepStatusRunning},
		},
	}

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)

	if err := renderDriveResult(cmd, run, true); err != nil {
		t.Fatalf("checks-passed must exit 0, got error: %v", err)
	}

	got := out.String()
	for _, want := range []string{
		"outcome: checks-passed",
		"https://github.com/user/repo/pull/42",
		"merge",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("checks-passed output missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "outcome: passed\n") {
		t.Errorf("checks-passed must not report a terminal passed outcome:\n%s", got)
	}
}

func TestRenderDriveResult_TerminalPassedUnaffected(t *testing.T) {
	run := &ipc.RunInfo{
		ID:     "run-1",
		Branch: "feature/x",
		Status: types.RunCompleted,
		Steps:  []ipc.StepResultInfo{{StepName: types.StepCI, Status: types.StepStatusCompleted}},
	}
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)

	if err := renderDriveResult(cmd, run, false); err != nil {
		t.Fatalf("terminal passed must exit 0, got error: %v", err)
	}
	if got := out.String(); !strings.Contains(got, "outcome: passed") {
		t.Errorf("expected terminal passed outcome, got:\n%s", got)
	}
}

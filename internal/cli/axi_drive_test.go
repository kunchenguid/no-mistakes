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
		"Summarize this pipeline run for the user",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("checks-passed output missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "outcome: passed\n") {
		t.Errorf("checks-passed must not report a terminal passed outcome:\n%s", got)
	}
	// No fixes were applied, so neither the fixes table nor the
	// acknowledge-your-misses instruction should appear.
	for _, reject := range []string{"fixes[", "acknowledge"} {
		if strings.Contains(got, reject) {
			t.Errorf("checks-passed output without fixes must not contain %q:\n%s", reject, got)
		}
	}
}

func TestRenderDriveResult_ChecksPassedWithFixes(t *testing.T) {
	run := &ipc.RunInfo{
		ID:      "run-1",
		Branch:  "feature/x",
		Status:  types.RunRunning,
		HeadSHA: "abcdef1234567890",
		PRURL:   strptr("https://github.com/user/repo/pull/42"),
		Steps: []ipc.StepResultInfo{
			{StepName: types.StepReview, Status: types.StepStatusCompleted, FixSummaries: []string{"handle nil pointer in executor"}},
			{StepName: types.StepTest, Status: types.StepStatusCompleted, FixSummaries: []string{""}},
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
		"fixes[2]{step,summary}:",
		"review,handle nil pointer in executor",
		"test,fix applied (no summary recorded)",
		"Summarize this pipeline run for the user",
		"acknowledge the misses and list each fix so the user can review them",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("checks-passed output missing %q in:\n%s", want, got)
		}
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
	got := out.String()
	if !strings.Contains(got, "outcome: passed") {
		t.Errorf("expected terminal passed outcome, got:\n%s", got)
	}
	if !strings.Contains(got, "Summarize this pipeline run for the user") {
		t.Errorf("terminal passed output missing the summarize instruction:\n%s", got)
	}
}

func TestRenderDriveResult_TerminalPassedWithFixes(t *testing.T) {
	run := &ipc.RunInfo{
		ID:     "run-1",
		Branch: "feature/x",
		Status: types.RunCompleted,
		Steps: []ipc.StepResultInfo{
			{StepName: types.StepLint, Status: types.StepStatusCompleted, FixSummaries: []string{"remove unused import"}},
			{StepName: types.StepCI, Status: types.StepStatusCompleted},
		},
	}
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)

	if err := renderDriveResult(cmd, run, false); err != nil {
		t.Fatalf("terminal passed must exit 0, got error: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"outcome: passed",
		"fixes[1]{step,summary}:",
		"lint,remove unused import",
		"acknowledge the misses and list each fix so the user can review them",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("terminal passed output missing %q in:\n%s", want, got)
		}
	}
}

func TestRenderDriveResult_FailedHasNoSummarizeInstruction(t *testing.T) {
	run := &ipc.RunInfo{
		ID:     "run-1",
		Branch: "feature/x",
		Status: types.RunFailed,
		Steps: []ipc.StepResultInfo{
			{StepName: types.StepTest, Status: types.StepStatusFailed, FixSummaries: []string{"partial fix"}},
		},
	}
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)

	err := renderDriveResult(cmd, run, false)
	if err == nil {
		t.Fatal("failed outcome must exit non-zero")
	}
	got := out.String()
	if strings.Contains(got, "Summarize this pipeline run for the user") {
		t.Errorf("failed outcome must not carry the success summary instruction:\n%s", got)
	}
}

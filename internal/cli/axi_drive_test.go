package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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
		name          string
		gate          stepView
		fixRoundsUsed int
		wantAction    types.ApprovalAction
		wantIDs       []string
		wantResolved  bool
	}{
		{
			name: "actionable findings are fixed with every finding selected",
			gate: stepView{
				Name:         "review",
				Status:       string(types.StepStatusAwaitingApproval),
				FindingsJSON: `{"findings":[{"id":"review-1","severity":"warning","description":"design choice","action":"ask-user"},{"id":"review-2","severity":"info","description":"fyi","action":"no-op"}],"summary":"2"}`,
			},
			wantAction:   types.ActionFix,
			wantIDs:      []string{"review-1", "review-2"},
			wantResolved: true,
		},
		{
			name: "only non-actionable findings are approved",
			gate: stepView{
				Name:         "test",
				Status:       string(types.StepStatusAwaitingApproval),
				FindingsJSON: `{"findings":[{"id":"test-1","severity":"info","description":"fyi","action":"no-op"}],"summary":"1"}`,
			},
			wantAction:   types.ActionApprove,
			wantResolved: true,
		},
		{
			name: "no findings are approved",
			gate: stepView{
				Name:         "push",
				Status:       string(types.StepStatusAwaitingApproval),
				FindingsJSON: ``,
			},
			wantAction:   types.ActionApprove,
			wantResolved: true,
		},
		{
			name: "fix_review with cleared findings is approved",
			gate: stepView{
				Name:         "review",
				Status:       string(types.StepStatusFixReview),
				FindingsJSON: `{"findings":[],"summary":"clean"}`,
			},
			fixRoundsUsed: 1,
			wantAction:    types.ActionApprove,
			wantResolved:  true,
		},
		{
			name: "fix_review with residual actionable findings is fixed again while budget remains",
			gate: stepView{
				Name:         "review",
				Status:       string(types.StepStatusFixReview),
				FindingsJSON: `{"findings":[{"id":"review-1","severity":"error","description":"still here","action":"ask-user"},{"id":"review-2","severity":"warning","description":"new issue","action":"auto-fix"}],"summary":"2"}`,
			},
			fixRoundsUsed: 1,
			wantAction:    types.ActionFix,
			wantIDs:       []string{"review-1", "review-2"},
			wantResolved:  true,
		},
		{
			name: "actionable findings after exhausted budget are handed back, not approved",
			gate: stepView{
				Name:         "review",
				Status:       string(types.StepStatusFixReview),
				FindingsJSON: `{"findings":[{"id":"review-1","severity":"error","description":"still here","action":"ask-user"}],"summary":"1"}`,
			},
			fixRoundsUsed: maxYesFixRoundsPerStep,
			wantResolved:  false,
		},
		{
			name: "actionable findings without ids are handed back rather than fixing nothing or approving them away",
			gate: stepView{
				Name:         "review",
				Status:       string(types.StepStatusAwaitingApproval),
				FindingsJSON: `{"findings":[{"severity":"warning","description":"no id","action":"ask-user"}],"summary":"1"}`,
			},
			wantResolved: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			action, ids, resolved := gateResolution(tt.gate, tt.fixRoundsUsed)
			t.Logf("auto-resolution action=%s finding_ids=%v resolved=%v", action, ids, resolved)
			if resolved != tt.wantResolved {
				t.Fatalf("resolved = %v, want %v", resolved, tt.wantResolved)
			}
			if !tt.wantResolved {
				return
			}
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

// startDriveTestServer serves a scripted daemon over a real IPC socket so
// driveRun/waitStepLeavesGate run against the same transport they use in
// production.
func startDriveTestServer(t *testing.T, srv *ipc.Server) *ipc.Client {
	t.Helper()
	dir, err := os.MkdirTemp("", "nmd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s.sock")
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(sock) }()
	t.Cleanup(func() { srv.Close(); <-errCh })

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c, err := ipc.Dial(sock)
		if err == nil {
			c.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	client, err := ipc.Dial(sock)
	if err != nil {
		t.Fatalf("dial drive test server: %v", err)
	}
	t.Cleanup(func() { client.Close() })
	return client
}

// TestDriveRun_ConsecutiveFixReviewParksAdvanceByRoundCount reproduces the
// --yes wedge: a fix round that completes faster than one poll interval
// re-parks the step as fix_review, so consecutive parks are indistinguishable
// by status alone. The drive loop must treat an advanced fix_round_count as
// progress, fund rounds up to the budget, and hand the gate back parked - not
// spin forever waiting for a status change it already missed.
func TestDriveRun_ConsecutiveFixReviewParksAdvanceByRoundCount(t *testing.T) {
	findings := `{"findings":[{"id":"f-1","severity":"warning","file":"feature.txt","line":1,"description":"potential nil deref","action":"ask-user"}],"summary":"found 1 issue"}`

	var mu sync.Mutex
	fixRounds := 1
	responds := 0

	srv := ipc.NewServer()
	srv.Handle(ipc.MethodGetRun, func(_ context.Context, _ json.RawMessage) (interface{}, error) {
		mu.Lock()
		defer mu.Unlock()
		fj := findings
		return &ipc.GetRunResult{Run: &ipc.RunInfo{
			ID:     "run-1",
			Branch: "feature/x",
			Status: types.RunRunning,
			Steps: []ipc.StepResultInfo{{
				ID:            "sr-1",
				RunID:         "run-1",
				StepName:      types.StepReview,
				Status:        types.StepStatusFixReview,
				FindingsJSON:  &fj,
				FixRoundCount: fixRounds,
			}},
		}}, nil
	})
	srv.Handle(ipc.MethodRespond, func(_ context.Context, raw json.RawMessage) (interface{}, error) {
		var p ipc.RespondParams
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		if p.Action != types.ActionFix {
			return nil, fmt.Errorf("unexpected action %q for a gate with actionable findings", p.Action)
		}
		mu.Lock()
		defer mu.Unlock()
		responds++
		// The fix round completes before the drive loop's next poll and the
		// step re-parks with the same status and findings; only the persisted
		// round count advances.
		fixRounds++
		return &ipc.RespondResult{OK: true}, nil
	})
	client := startDriveTestServer(t, srv)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var progress bytes.Buffer
	run, ciReady, err := driveRun(ctx, &progress, client, "run-1", true, nil)
	if err != nil {
		t.Fatalf("driveRun must hand the exhausted gate back, got error: %v\nprogress:\n%s", err, progress.String())
	}
	if ciReady {
		t.Error("ciReady = true, want false for a parked review gate")
	}
	if run == nil || run.Status != types.RunRunning {
		t.Fatalf("run = %+v, want the still-running parked run handed back", run)
	}

	mu.Lock()
	gotResponds, gotRounds := responds, fixRounds
	mu.Unlock()
	// Round 1 was already persisted; --yes funds rounds 2 and 3, then the
	// budget (maxYesFixRoundsPerStep) is exhausted and the gate is handed back.
	if want := maxYesFixRoundsPerStep - 1; gotResponds != want {
		t.Errorf("fix responds = %d, want %d (budget minus the persisted round)", gotResponds, want)
	}
	if gotRounds != maxYesFixRoundsPerStep {
		t.Errorf("persisted fix rounds = %d, want %d", gotRounds, maxYesFixRoundsPerStep)
	}
	if !strings.Contains(progress.String(), "leaving the run parked for explicit adjudication") {
		t.Errorf("progress missing the adjudication hand-back message:\n%s", progress.String())
	}
}

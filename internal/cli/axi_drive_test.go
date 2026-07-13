package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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

func TestDriveRun_HealthyWaitStaysWithinRequestBudget(t *testing.T) {
	root := makeSocketSafeTempDir(t)
	socketPath := filepath.Join(root, "axi-drive.sock")
	srv := ipc.NewServer()
	var getRunCalls atomic.Int32
	var subscribeCalls atomic.Int32
	srv.Handle(ipc.MethodGetRun, func(_ context.Context, _ json.RawMessage) (interface{}, error) {
		getRunCalls.Add(1)
		return &ipc.GetRunResult{Run: &ipc.RunInfo{
			ID:     "run-1",
			Status: types.RunRunning,
		}}, nil
	})
	srv.HandleStream(ipc.MethodSubscribe, func(ctx context.Context, _ json.RawMessage) (ipc.StreamFunc, error) {
		subscribeCalls.Add(1)
		return func(func(interface{}) error) error {
			<-ctx.Done()
			return nil
		}, nil
	})
	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(socketPath) }()
	t.Cleanup(func() {
		srv.Close()
		select {
		case <-errCh:
		case <-time.After(time.Second):
			t.Error("IPC server did not stop")
		}
	})

	var client *ipc.Client
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		var err error
		client, err = ipc.Dial(socketPath)
		if err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if client == nil {
		t.Fatal("IPC server did not become ready")
	}
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 900*time.Millisecond)
	defer cancel()
	_, _, err := driveRun(ctx, io.Discard, client, socketPath, "run-1", false)
	if err == nil || err != context.DeadlineExceeded {
		t.Fatalf("driveRun error = %v, want context deadline", err)
	}
	if got := getRunCalls.Load(); got != 1 {
		t.Fatalf("healthy 900ms wait made %d get_run requests, want exactly 1 initial reconciliation", got)
	}
	if got := subscribeCalls.Load(); got != 1 {
		t.Fatalf("healthy 900ms wait made %d subscriptions, want 1", got)
	}
}

func TestRunReconciler_SubscribeFirstAndCoalescesDuplicateDelayedEvents(t *testing.T) {
	events := make(chan ipc.Event, 4)
	source := &scriptedRunStateSource{
		subscriptions: []scriptedSubscription{{events: events}},
		runs: []*ipc.RunInfo{
			{ID: "run-1", Status: types.RunRunning},
			{ID: "run-1", Status: types.RunCompleted},
		},
	}
	reconciler := newRunReconciler(source, "run-1")
	defer reconciler.Close()

	first, err := reconciler.Next(context.Background())
	if err != nil || first.Status != types.RunRunning {
		t.Fatalf("initial Next = %#v, %v", first, err)
	}
	events <- ipc.Event{Type: ipc.EventRunUpdated, RunID: "run-1"}
	events <- ipc.Event{Type: ipc.EventRunUpdated, RunID: "run-1"}    // duplicate
	events <- ipc.Event{Type: ipc.EventStepCompleted, RunID: "run-1"} // delayed old transition
	terminal, err := reconciler.Next(context.Background())
	if err != nil || terminal.Status != types.RunCompleted {
		t.Fatalf("event Next = %#v, %v", terminal, err)
	}

	source.mu.Lock()
	defer source.mu.Unlock()
	if got := strings.Join(source.operations, ","); got != "subscribe,reconcile,reconcile" {
		t.Fatalf("operations = %s, want subscribe-first and one coalesced event reconciliation", got)
	}
}

func TestDriveRunDetectsTerminalStateAfterReconnect(t *testing.T) {
	firstEvents := make(chan ipc.Event)
	close(firstEvents)
	source := &scriptedRunStateSource{
		subscriptions: []scriptedSubscription{{events: firstEvents}, {events: make(chan ipc.Event)}},
		runs: []*ipc.RunInfo{
			{ID: "run-1", Status: types.RunRunning},
			{ID: "run-1", Status: types.RunCompleted},
		},
	}
	reconciler := newRunReconciler(source, "run-1")
	defer reconciler.Close()

	run, ciReady, err := driveRunWithReconciler(context.Background(), io.Discard, nil, reconciler, "run-1", false)
	if err != nil {
		t.Fatal(err)
	}
	if ciReady || run == nil || run.Status != types.RunCompleted {
		t.Fatalf("drive result = %#v, ciReady=%v; want completed terminal run", run, ciReady)
	}
}

func TestRunReconciler_ReconnectsBeforeReconcilingDisconnectedTransition(t *testing.T) {
	firstEvents := make(chan ipc.Event)
	secondEvents := make(chan ipc.Event)
	source := &scriptedRunStateSource{
		subscriptions: []scriptedSubscription{{events: firstEvents}, {events: secondEvents}},
		runs: []*ipc.RunInfo{
			{ID: "run-1", Status: types.RunRunning},
			{ID: "run-1", Status: types.RunFailed},
		},
	}
	reconciler := newRunReconciler(source, "run-1")
	defer reconciler.Close()
	if _, err := reconciler.Next(context.Background()); err != nil {
		t.Fatal(err)
	}
	close(firstEvents)

	run, err := reconciler.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != types.RunFailed {
		t.Fatalf("status after reconnect = %s, want failed", run.Status)
	}
	source.mu.Lock()
	defer source.mu.Unlock()
	if got := strings.Join(source.operations, ","); got != "subscribe,reconcile,subscribe,reconcile" {
		t.Fatalf("operations = %s, want reconnect before reconciliation", got)
	}
}

func TestRunReconciler_LogWakeupDoesNotSpendDatabaseRequest(t *testing.T) {
	events := make(chan ipc.Event, 1)
	events <- ipc.Event{Type: ipc.EventLogChunk, RunID: "run-1"}
	source := &scriptedRunStateSource{
		subscriptions: []scriptedSubscription{{events: events}},
		runs:          []*ipc.RunInfo{{ID: "run-1", Status: types.RunRunning}},
	}
	reconciler := newRunReconciler(source, "run-1")
	defer reconciler.Close()
	if _, err := reconciler.Next(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Next(context.Background()); err != nil {
		t.Fatal(err)
	}

	source.mu.Lock()
	defer source.mu.Unlock()
	if got := strings.Join(source.operations, ","); got != "subscribe,reconcile" {
		t.Fatalf("log wakeup operations = %s, want no second database reconciliation", got)
	}
}

func TestRunReconciler_HeartbeatRecoversMissedTerminalEvent(t *testing.T) {
	events := make(chan ipc.Event)
	source := &scriptedRunStateSource{
		subscriptions: []scriptedSubscription{{events: events}},
		runs: []*ipc.RunInfo{
			{ID: "run-1", Status: types.RunRunning},
			{ID: "run-1", Status: types.RunCompleted},
		},
	}
	reconciler := newRunReconciler(source, "run-1")
	reconciler.heartbeatInterval = 10 * time.Millisecond
	defer reconciler.Close()
	if _, err := reconciler.Next(context.Background()); err != nil {
		t.Fatal(err)
	}

	started := time.Now()
	run, err := reconciler.Next(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != types.RunCompleted {
		t.Fatalf("heartbeat status = %s, want completed", run.Status)
	}
	if elapsed := time.Since(started); elapsed < reconciler.heartbeatInterval {
		t.Fatalf("heartbeat reconciled too early after %v", elapsed)
	}
}

func TestRunReconciler_ReconnectAndReconcileFailuresStayVisible(t *testing.T) {
	t.Run("reconnect failure", func(t *testing.T) {
		events := make(chan ipc.Event)
		source := &scriptedRunStateSource{
			subscriptions: []scriptedSubscription{
				{events: events},
				{err: errors.New("socket unavailable")},
				{err: errors.New("socket unavailable")},
			},
			runs: []*ipc.RunInfo{{ID: "run-1", Status: types.RunRunning}},
		}
		reconciler := newRunReconciler(source, "run-1")
		reconciler.reconnectInterval = time.Millisecond
		reconciler.reconnectTimeout = 3 * time.Millisecond
		defer reconciler.Close()
		if _, err := reconciler.Next(context.Background()); err != nil {
			t.Fatal(err)
		}
		close(events)
		_, err := reconciler.Next(context.Background())
		if err == nil || !strings.Contains(err.Error(), "socket unavailable") {
			t.Fatalf("reconnect error = %v, want actionable socket failure", err)
		}
	})

	t.Run("reconcile failure", func(t *testing.T) {
		source := &scriptedRunStateSource{
			subscriptions: []scriptedSubscription{{events: make(chan ipc.Event)}},
			reconcileErr:  errors.New("database unavailable"),
		}
		reconciler := newRunReconciler(source, "run-1")
		defer reconciler.Close()
		_, err := reconciler.Next(context.Background())
		if err == nil || !strings.Contains(err.Error(), "database unavailable") {
			t.Fatalf("reconcile error = %v, want actionable database failure", err)
		}
	})
}

type scriptedSubscription struct {
	events <-chan ipc.Event
	err    error
}

type scriptedRunStateSource struct {
	mu            sync.Mutex
	operations    []string
	subscriptions []scriptedSubscription
	runs          []*ipc.RunInfo
	reconcileErr  error
}

func (s *scriptedRunStateSource) Subscribe(string) (<-chan ipc.Event, func(), error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.operations = append(s.operations, "subscribe")
	if len(s.subscriptions) == 0 {
		return nil, nil, errors.New("no scripted subscription")
	}
	next := s.subscriptions[0]
	if len(s.subscriptions) > 1 {
		s.subscriptions = s.subscriptions[1:]
	}
	if next.err != nil {
		return nil, nil, next.err
	}
	return next.events, func() {}, nil
}

func (s *scriptedRunStateSource) Reconcile(context.Context, string) (*ipc.RunInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.operations = append(s.operations, "reconcile")
	if s.reconcileErr != nil {
		return nil, s.reconcileErr
	}
	if len(s.runs) == 0 {
		return nil, nil
	}
	next := s.runs[0]
	if len(s.runs) > 1 {
		s.runs = s.runs[1:]
	}
	return next, nil
}

func TestCIReadyToMerge(t *testing.T) {
	tests := []struct {
		name     string
		rv       runView
		ciReady  bool
		wantStop bool
	}{
		{
			name:     "ci running and checks passed",
			rv:       ciRunView(types.StepStatusRunning),
			ciReady:  true,
			wantStop: true,
		},
		{
			name:     "ci running but checks not passed yet",
			rv:       ciRunView(types.StepStatusRunning),
			wantStop: false,
		},
		{
			name:     "checks passed but ci step already completed",
			rv:       ciRunView(types.StepStatusCompleted),
			wantStop: false,
		},
		{
			name:     "no ci step in run",
			rv:       runView{Status: string(types.RunRunning), Steps: []stepView{{Name: string(types.StepPR), Status: string(types.StepStatusCompleted)}}},
			wantStop: false,
		},
		{
			name:     "declared no_ci with zero checks is ready",
			rv:       ciRunView(types.StepStatusRunning),
			ciReady:  true,
			wantStop: true,
		},
		{
			name:     "ready-looking agent output is ignored without persisted readiness",
			rv:       ciRunView(types.StepStatusRunning),
			wantStop: false,
		},
		{
			name:     "PR 607 generic empty checks never ready",
			rv:       ciRunView(types.StepStatusRunning),
			wantStop: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.rv.CIReady = tt.ciReady
			if got := ciReadyToMerge(tt.rv); got != tt.wantStop {
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
		"CI checks passed",
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
	if strings.Contains(got, "declares no CI") {
		t.Errorf("all-green path must not claim no_ci declaration:\n%s", got)
	}
	// No fixes were applied, so neither the fixes table nor the
	// acknowledge-your-misses instruction should appear.
	for _, reject := range []string{"fixes[", "acknowledge"} {
		if strings.Contains(got, reject) {
			t.Errorf("checks-passed output without fixes must not contain %q:\n%s", reject, got)
		}
	}
}

func TestRenderDriveResult_DeclaredNoCIChecksPassed(t *testing.T) {
	run := &ipc.RunInfo{
		ID:          "run-1",
		Branch:      "feature/x",
		Status:      types.RunRunning,
		HeadSHA:     "abcdef1234567890",
		PRURL:       strptr("https://github.com/user/repo/pull/42"),
		CIReadyNoCI: true,
		Steps: []ipc.StepResultInfo{
			{StepName: types.StepCI, Status: types.StepStatusRunning},
		},
	}

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)

	// PR 607-shaped empty generic marker is not an authoritative readiness state.
	pr607Logs := []string{
		"monitoring CI for PR #607 (timeout: 4h0m0s)...",
		"mergeable state still pending: PENDING",
		"mergeable state still pending: PENDING",
		"no CI checks reported - still monitoring until merged or closed",
	}
	if cimonitor.ChecksPassed(pr607Logs) || ciReadyToMerge(runViewFromIPC(run)) {
		t.Fatal("PR 607 empty-checks sequence must not be agent-facing ready")
	}

	run.CIReady = true
	if err := renderDriveResult(cmd, run, true); err != nil {
		t.Fatalf("declared no_ci checks-passed must exit 0, got error: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"outcome: checks-passed",
		"declares no CI",
		"no_ci: true",
		"https://github.com/user/repo/pull/42",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("declared no_ci output missing %q in:\n%s", want, got)
		}
	}
	if strings.Contains(got, "CI checks passed - the PR is ready") {
		t.Errorf("declared no_ci path must not silently equate empty results with green:\n%s", got)
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

// A stream gap is state-bearing for the reconciler: it forces exactly one
// authoritative read, which is how a transition the daemon had to coalesce
// away reaches AXI. Subscribe-first behaviour and the single-read budget are
// unchanged.
func TestRunReconciler_StreamGapForcesOneAuthoritativeRead(t *testing.T) {
	events := make(chan ipc.Event, 4)
	source := &scriptedRunStateSource{
		subscriptions: []scriptedSubscription{{events: events}},
		runs: []*ipc.RunInfo{
			{ID: "run-1", Status: types.RunRunning, StateRev: 3},
			{ID: "run-1", Status: types.RunCompleted, StateRev: 71},
		},
	}
	reconciler := newRunReconciler(source, "run-1")
	defer reconciler.Close()
	// Disable the slow lost-event backstop so only the gap can wake the
	// reconciler; otherwise the heartbeat would mask a missing gap route.
	reconciler.heartbeatInterval = time.Hour

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	first, err := reconciler.Next(ctx)
	if err != nil || first.Status != types.RunRunning {
		t.Fatalf("initial Next = %#v, %v", first, err)
	}
	events <- ipc.Event{Type: ipc.EventStreamGap, RunID: "run-1", StateRev: 71}
	after, err := reconciler.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != types.RunCompleted {
		t.Fatalf("post-gap status = %q, want the authoritative terminal state", after.Status)
	}
	source.mu.Lock()
	defer source.mu.Unlock()
	if got := strings.Join(source.operations, ","); got != "subscribe,reconcile,reconcile" {
		t.Fatalf("operations = %s, want exactly one extra reconciliation for the gap", got)
	}
}

// An event type this build does not recognise must be treated as state-bearing
// rather than ignored, so a future producer cannot strand a consumer.
func TestRunReconciler_UnknownEventTypeIsTreatedAsStateBearing(t *testing.T) {
	events := make(chan ipc.Event, 2)
	source := &scriptedRunStateSource{
		subscriptions: []scriptedSubscription{{events: events}},
		runs: []*ipc.RunInfo{
			{ID: "run-1", Status: types.RunRunning},
			{ID: "run-1", Status: types.RunCompleted},
		},
	}
	reconciler := newRunReconciler(source, "run-1")
	defer reconciler.Close()
	reconciler.heartbeatInterval = time.Hour

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := reconciler.Next(ctx); err != nil {
		t.Fatal(err)
	}
	events <- ipc.Event{Type: ipc.EventType("some_future_event"), RunID: "run-1"}
	after, err := reconciler.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != types.RunCompleted {
		t.Fatalf("status after unknown event = %q, want a reconciliation", after.Status)
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
	responds := 0
	events := make(chan ipc.Event, 4)

	srv := ipc.NewServer()
	runAtRound := func(round int) *ipc.RunInfo {
		fj := findings
		return &ipc.RunInfo{
			ID:     "run-1",
			Branch: "feature/x",
			Status: types.RunRunning,
			Steps: []ipc.StepResultInfo{{
				ID:            "sr-1",
				RunID:         "run-1",
				StepName:      types.StepReview,
				Status:        types.StepStatusFixReview,
				FindingsJSON:  &fj,
				FixRoundCount: round,
			}},
		}
	}
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
		events <- ipc.Event{Type: ipc.EventRunUpdated, RunID: "run-1"}
		return &ipc.RespondResult{OK: true}, nil
	})
	client := startDriveTestServer(t, srv)
	source := &scriptedRunStateSource{
		subscriptions: []scriptedSubscription{{events: events}},
		runs: []*ipc.RunInfo{
			runAtRound(1),
			runAtRound(2),
			runAtRound(3),
		},
	}
	reconciler := newRunReconciler(source, "run-1")
	defer reconciler.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var progress bytes.Buffer
	run, ciReady, err := driveRunWithReconciler(ctx, &progress, client, reconciler, "run-1", true)
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
	gotResponds := responds
	mu.Unlock()
	// Round 1 was already persisted; --yes funds rounds 2 and 3, then the
	// budget (maxYesFixRoundsPerStep) is exhausted and the gate is handed back.
	if want := maxYesFixRoundsPerStep - 1; gotResponds != want {
		t.Errorf("fix responds = %d, want %d (budget minus the persisted round)", gotResponds, want)
	}
	if !strings.Contains(progress.String(), "leaving the run parked for explicit adjudication") {
		t.Errorf("progress missing the adjudication hand-back message:\n%s", progress.String())
	}
}

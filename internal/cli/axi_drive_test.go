package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
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
	_, _, err := driveRun(ctx, io.Discard, client, socketPath, "run-1", false, nil)
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

	run, ciReady, err := driveRunWithReconciler(context.Background(), io.Discard, nil, reconciler, "run-1", false, nil)
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

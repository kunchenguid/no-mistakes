package daemon

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func newHandoffFixture(t *testing.T, ready bool) (*db.DB, *db.Run, *RunManager, context.Context, context.CancelCauseFunc, chan struct{}) {
	return newHandoffFixtureWithPRState(t, ready, false)
}

func newHandoffFixtureWithPRState(t *testing.T, ready, nullPRState bool) (*db.DB, *db.Run, *RunManager, context.Context, context.CancelCauseFunc, chan struct{}) {
	t.Helper()
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	repo, err := database.InsertRepo(t.TempDir(), "https://example.test/o/r", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(database.UpdateRunStatus(run.ID, types.RunRunning))
	must(database.UpdateRunPRURL(run.ID, "https://example.test/o/r/pull/1"))
	must(database.UpdateRunPRState(run.ID, "open"))
	if nullPRState {
		sqlDB, err := sql.Open("sqlite", p.DB())
		must(err)
		defer sqlDB.Close()
		_, err = sqlDB.Exec(`UPDATE runs SET pr_state = NULL WHERE id = ?`, run.ID)
		must(err)
	}
	must(database.SetRunCIReady(run.ID, ready))
	step, err := database.InsertStepResult(run.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	must(database.UpdateStepStatus(step.ID, types.StepStatusRunning))

	m := NewRunManager(database, p, nil)
	monitorCtx, cancel := context.WithCancelCause(context.Background())
	wake := make(chan struct{}, 1)
	m.cancels[run.ID] = cancel
	m.ciMonitorActive[run.ID] = true
	m.ciWakes[run.ID] = wake
	return database, run, m, monitorCtx, cancel, wake
}

func requestHandoff(t *testing.T, m *RunManager, runID string, wake <-chan struct{}) <-chan error {
	return requestHandoffContext(t, m, runID, wake, context.Background())
}

func requestHandoffContext(t *testing.T, m *RunManager, runID string, wake <-chan struct{}, ctx context.Context) <-chan error {
	t.Helper()
	result := make(chan error, 1)
	go func() { result <- m.HandleHandoff(ctx, runID) }()
	<-wake
	return result
}

func awaitHandoffResult(t *testing.T, result <-chan error) error {
	t.Helper()
	return <-result
}

func TestHandleHandoffAfterMonitorExitRefusesImmediately(t *testing.T) {
	_, run, m, _, cancel, _ := newHandoffFixture(t, true)
	defer cancel(context.Canceled)

	m.transitionCIMonitorLifecycle(run.ID, false)
	if err := m.HandleHandoff(context.Background(), run.ID); err == nil || !strings.Contains(err.Error(), "actively monitoring") {
		t.Fatalf("handoff error = %v, want immediate monitor refusal", err)
	}
}

func TestReleaseExecutionSlotClearsHandoffState(t *testing.T) {
	_, run, m, _, cancel, wake := newHandoffFixture(t, true)
	defer cancel(context.Canceled)
	m.executors[run.ID] = nil
	m.dones[run.ID] = make(chan struct{})
	result := requestHandoff(t, m, run.ID, wake)

	m.releaseExecutionSlot(run.ID)
	select {
	case err := <-result:
		if err == nil || err.Error() != "run ended before handoff" {
			t.Fatalf("handoff result = %v, want executor-exit refusal", err)
		}
	case <-time.After(time.Second):
		t.Fatal("executor exit left the handoff request waiting")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	_, executor := m.executors[run.ID]
	_, cancelling := m.cancels[run.ID]
	_, done := m.dones[run.ID]
	_, pending := m.handoffRequests[run.ID]
	_, waking := m.ciWakes[run.ID]
	_, monitoring := m.ciMonitorActive[run.ID]
	if executor || cancelling || done || pending || waking || monitoring {
		t.Fatal("executor exit left run or handoff tracking behind")
	}
}

func TestHandleHandoffUnknownPRStateRefuses(t *testing.T) {
	_, run, m, _, cancel, _ := newHandoffFixtureWithPRState(t, true, true)
	defer cancel(context.Canceled)

	if err := m.HandleHandoff(context.Background(), run.ID); err == nil || !strings.Contains(err.Error(), "open PR") {
		t.Fatalf("handoff error = %v, want unknown PR-state refusal", err)
	}
}

func TestHandleHandoffMonitorCommitsGreenOpenPR(t *testing.T) {
	database, run, m, monitorCtx, cancel, wake := newHandoffFixture(t, true)
	defer cancel(context.Canceled)

	result := requestHandoff(t, m, run.ID, wake)
	if err := m.transitionCIObservation(run.ID, false, true, "", func() error {
		return database.SetRunCIReady(run.ID, true)
	}); !errors.Is(err, pipeline.ErrHandoff) {
		t.Fatalf("green observation error = %v, want ErrHandoff", err)
	}
	if err := awaitHandoffResult(t, result); err != nil {
		t.Fatalf("handoff result = %v, want success", err)
	}
	if monitorCtx.Err() != nil {
		t.Fatal("handoff stopped the monitor context")
	}
}

func TestHandleHandoffReadinessWithdrawalRefusesAndContinues(t *testing.T) {
	database, run, m, monitorCtx, cancel, wake := newHandoffFixture(t, true)
	defer cancel(context.Canceled)

	result := requestHandoff(t, m, run.ID, wake)
	if err := m.transitionCIObservation(run.ID, true, false, "CI checks are not green", func() error {
		return database.SetRunCIReady(run.ID, false)
	}); err != nil {
		t.Fatal(err)
	}
	if err := awaitHandoffResult(t, result); err == nil || !strings.Contains(err.Error(), "not green") {
		t.Fatalf("handoff result = %v, want readiness refusal", err)
	}
	if monitorCtx.Err() != nil {
		t.Fatal("refused handoff stopped the monitor")
	}
	got, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.CIReadyAt != nil {
		t.Fatal("readiness withdrawal was not persisted")
	}
}

func TestHandleHandoffReadinessWriteFailureRefusesRequest(t *testing.T) {
	for _, ready := range []bool{false, true} {
		name := "clear"
		if ready {
			name = "persist"
		}
		t.Run(name, func(t *testing.T) {
			database, run, m, monitorCtx, cancel, wake := newHandoffFixture(t, true)
			defer cancel(context.Canceled)
			result := requestHandoff(t, m, run.ID, wake)
			if err := database.Close(); err != nil {
				t.Fatal(err)
			}
			writeErr := m.transitionCIObservation(run.ID, !ready, ready, "", func() error {
				return database.SetRunCIReady(run.ID, ready)
			})
			if writeErr == nil || errors.Is(writeErr, pipeline.ErrHandoff) {
				t.Fatalf("observation error = %v, want readiness write failure", writeErr)
			}
			if err := awaitHandoffResult(t, result); !errors.Is(err, writeErr) {
				t.Fatalf("handoff result = %v, want %v", err, writeErr)
			}
			if monitorCtx.Err() != nil {
				t.Fatal("refused handoff stopped the monitor")
			}
			m.mu.Lock()
			_, pending := m.handoffRequests[run.ID]
			m.mu.Unlock()
			if pending {
				t.Fatal("failed write left the handoff pending")
			}
		})
	}
}

func TestHandleHandoffPRCloseRefusesAndContinues(t *testing.T) {
	database, run, m, monitorCtx, cancel, wake := newHandoffFixture(t, true)
	defer cancel(context.Canceled)

	result := requestHandoff(t, m, run.ID, wake)
	if err := m.transitionCIObservation(run.ID, true, false, "PR is closed", func() error {
		return database.UpdateRunPRState(run.ID, "closed")
	}); err != nil {
		t.Fatal(err)
	}
	if err := awaitHandoffResult(t, result); err == nil || !strings.Contains(err.Error(), "PR is closed") {
		t.Fatalf("handoff result = %v, want PR refusal", err)
	}
	if monitorCtx.Err() != nil {
		t.Fatal("refused handoff stopped the monitor")
	}
	got, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != types.RunCompleted || got.PRState == nil || *got.PRState != "closed" {
		t.Fatalf("run = status %s, PR state %v; want completed/closed", got.Status, got.PRState)
	}
}

func TestHandleHandoffTimeoutClearsRequestAndAllowsRetry(t *testing.T) {
	oldTimeout, oldMargin := handoffRequestTimeout, handoffRequestSafetyMargin
	handoffRequestTimeout = 0
	handoffRequestSafetyMargin = 0
	defer func() {
		handoffRequestTimeout, handoffRequestSafetyMargin = oldTimeout, oldMargin
	}()

	database, run, m, monitorCtx, cancel, wake := newHandoffFixture(t, true)
	defer cancel(context.Canceled)

	result := requestHandoffContext(t, m, run.ID, wake, context.Background())
	if err := awaitHandoffResult(t, result); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("handoff result = %v, want clear timeout", err)
	}

	m.mu.Lock()
	_, pending := m.handoffRequests[run.ID]
	m.mu.Unlock()
	if pending {
		t.Fatal("timeout left a handoff request pending")
	}

	handoffRequestTimeout, handoffRequestSafetyMargin = oldTimeout, oldMargin
	retry := requestHandoff(t, m, run.ID, wake)
	if err := m.transitionCIObservation(run.ID, false, true, "", func() error {
		return database.SetRunCIReady(run.ID, true)
	}); !errors.Is(err, pipeline.ErrHandoff) {
		t.Fatalf("retry observation error = %v, want ErrHandoff", err)
	}
	if err := awaitHandoffResult(t, retry); err != nil {
		t.Fatalf("retry handoff result = %v, want success", err)
	}
	if monitorCtx.Err() != nil {
		t.Fatal("timed out handoff stopped the monitor")
	}
}

func TestHandleHandoffCancellationClearsPendingRequest(t *testing.T) {
	_, run, m, monitorCtx, cancel, wake := newHandoffFixture(t, true)
	defer cancel(context.Canceled)

	result := requestHandoff(t, m, run.ID, wake)
	if err := m.HandleCancel(run.ID); err != nil {
		t.Fatal(err)
	}
	if err := awaitHandoffResult(t, result); err == nil || !strings.Contains(err.Error(), types.RunCancelReasonAbortedByUser) {
		t.Fatalf("handoff result = %v, want cancellation", err)
	}
	if monitorCtx.Err() == nil || context.Cause(monitorCtx) == nil || context.Cause(monitorCtx).Error() != types.RunCancelReasonAbortedByUser {
		t.Fatalf("monitor cause = %v, want user cancellation", context.Cause(monitorCtx))
	}
	m.mu.Lock()
	_, pending := m.handoffRequests[run.ID]
	m.mu.Unlock()
	if pending {
		t.Fatal("cancellation left a handoff request pending")
	}
}

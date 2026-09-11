package pipeline

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestExecutor_ShutdownPreservesParkedGate(t *testing.T) {
	for _, status := range []types.StepStatus{types.StepStatusAwaitingApproval, types.StepStatusFixReview} {
		t.Run(string(status), func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			workDir := t.TempDir()
			var step Step = &reconcilingApprovalStep{name: types.StepCI}
			if status == types.StepStatusAwaitingApproval {
				step = &adaptiveCallStep{name: types.StepCI, fn: func(*StepContext) (*StepOutcome, error) {
					return &StepOutcome{NeedsApproval: true, Findings: `{"findings":[{"id":"ci-1","severity":"warning","action":"ask-user","description":"waiting"}]}`}, nil
				}}
			}
			var events []ipc.Event
			exec := NewExecutor(database, p, nil, nil, []Step{step}, func(event ipc.Event) { events = append(events, event) })
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			done := make(chan error, 1)
			go func() { done <- exec.Execute(ctx, run, repo, workDir) }()
			waitForStepStatus(t, database, run.ID, types.StepCI, types.StepStatusAwaitingApproval)
			if status == types.StepStatusFixReview {
				if err := exec.Respond(types.StepCI, types.ActionFix, []string{"ci-1"}); err != nil {
					t.Fatal(err)
				}
				waitForStepStatus(t, database, run.ID, types.StepCI, status)
			}
			before, err := database.GetStepsByRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			cancel(ErrDaemonShutdown)
			assertSuspended := func() {
				t.Helper()
				select {
				case err := <-done:
					if !errors.Is(err, ErrRunSuspended) {
						t.Fatalf("result = %v, want suspension", err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("shutdown did not drain")
				}
				stored, err := database.GetRun(run.ID)
				if err != nil {
					t.Fatal(err)
				}
				if stored.Status != types.RunRunning || stored.AwaitingAgentSince == nil || stored.Error != nil {
					t.Fatalf("lost parked run: %+v", stored)
				}
				after, err := database.GetStepsByRun(run.ID)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(before, after) {
					t.Fatalf("shutdown changed gate: before=%+v after=%+v", before[0], after[0])
				}
				for _, event := range events {
					if event.Type == ipc.EventRunCompleted {
						t.Fatal("suspension emitted terminal event")
					}
				}
				run = stored
			}
			assertSuspended()
			// A second shutdown must preserve the Resume entrypoint too.
			exec = NewExecutor(database, p, nil, nil, []Step{step}, nil)
			ctx, cancel = context.WithCancelCause(context.Background())
			defer cancel(nil)
			cancel(ErrDaemonShutdown)
			go func() { done <- exec.Resume(ctx, run, repo, workDir) }()
			assertSuspended()
		})
	}
}

func TestExecutor_ShutdownCancellationBoundaries(t *testing.T) {
	for _, active := range []bool{false, true} {
		for _, cause := range []error{ErrDaemonShutdown, context.Canceled, errors.New("daemon shutting down"), errors.New(types.RunCancelReasonAbortedByUser), errors.New(types.RunCancelReasonSuperseded)} {
			if !active && cause == ErrDaemonShutdown {
				continue
			}
			name := "parked/" + cause.Error()
			if active {
				name = "active/" + cause.Error()
			}
			t.Run(name, func(t *testing.T) {
				database, p, run, repo := setupTest(t)
				started := make(chan struct{})
				var step Step = &reconcilingApprovalStep{name: types.StepCI}
				if active {
					step = &adaptiveCallStep{name: types.StepCI, fn: func(sctx *StepContext) (*StepOutcome, error) {
						close(started)
						<-sctx.Ctx.Done()
						// A late successful outcome must not turn interrupted
						// active work into a recoverable parked gate.
						return &StepOutcome{NeedsApproval: true}, nil
					}}
				}
				exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
				ctx, cancel := context.WithCancelCause(context.Background())
				defer cancel(nil)
				workDir := t.TempDir()
				done := make(chan error, 1)
				go func() { done <- exec.Execute(ctx, run, repo, workDir) }()
				if active {
					select {
					case <-started:
					case <-time.After(5 * time.Second):
						t.Fatal("step did not start")
					}
				} else {
					waitForStepStatus(t, database, run.ID, types.StepCI, types.StepStatusAwaitingApproval)
				}
				cancel(cause)
				select {
				case err := <-done:
					if err == nil || errors.Is(err, ErrRunSuspended) {
						t.Fatalf("cancellation result = %v", err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("cancel did not drain")
				}
				stored, err := database.GetRun(run.ID)
				if err != nil {
					t.Fatal(err)
				}
				want := types.RunFailed
				if cause.Error() == types.RunCancelReasonAbortedByUser || cause.Error() == types.RunCancelReasonSuperseded {
					want = types.RunCancelled
				}
				if stored.Status != want || stored.AwaitingAgentSince != nil {
					t.Fatalf("cancelled run = %+v, want %s", stored, want)
				}
			})
		}
	}
}

func TestExecutor_AcceptedAbortWinsShutdown(t *testing.T) {
	database, p, run, repo := setupTest(t)
	step := &reconcilingApprovalStep{name: types.StepCI, started: make(chan struct{}), release: make(chan struct{})}
	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	workDir := t.TempDir()
	done := make(chan error, 1)
	go func() { done <- exec.Execute(ctx, run, repo, workDir) }()
	select {
	case <-step.started:
	case <-time.After(5 * time.Second):
		t.Fatal("reconciliation did not start")
	}
	if err := exec.Respond(types.StepCI, types.ActionAbort, nil); err != nil {
		t.Fatal(err)
	}
	cancel(ErrDaemonShutdown)
	close(step.release)
	select {
	case err := <-done:
		if err == nil || errors.Is(err, ErrRunSuspended) {
			t.Fatalf("accepted abort lost to shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("abort did not drain")
	}
	stored, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !stored.Status.Terminal() || stored.AwaitingAgentSince != nil {
		t.Fatalf("aborted run was preserved: %+v", stored)
	}
}

package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestDLOCK31GracefulShutdownRetainsParkedRun proves a normal daemon shutdown
// keeps a persisted approval gate recoverable. The recovery fixture uses the
// real manager, SQLite state, and a local Git worktree; it is deliberately
// already parked so active work still follows the ordinary cancellation path.
func TestDLOCK31GracefulShutdownRetainsParkedRun(t *testing.T) {
	f := newDLOCK31Recovery(t)
	plan, err := f.m.prepareRecoveredRun(context.Background(), f.run)
	if err != nil {
		t.Fatal(err)
	}

	f.m.resumeRecoveredRun(*plan)
	f.m.Shutdown()

	got, err := f.d.GetRun(f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != types.RunRunning || got.AwaitingAgentSince == nil {
		t.Fatalf("shutdown terminalized parked run: status=%s awaiting=%v", got.Status, got.AwaitingAgentSince)
	}

	recovered, err := f.m.prepareRecoveredRun(context.Background(), got)
	if err != nil {
		t.Fatalf("parked run was not recoverable after shutdown: %v", err)
	}
	if err := recovered.agent.Close(); err != nil {
		t.Fatalf("close recovered agent: %v", err)
	}
}

func TestDLOCK31ShutdownStillCancelsActiveRun(t *testing.T) {
	f := newDLOCK31Recovery(t)
	run, err := f.d.InsertRun(f.run.RepoID, "active", f.published, f.published)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	executor := pipeline.NewExecutor(f.d, f.p, nil, nil, []pipeline.Step{&mockSlowStep{name: types.StepReview, started: started}}, nil)
	ctx, cancel := context.WithCancelCause(context.Background())
	done := make(chan error, 1)
	go func() { done <- executor.Execute(ctx, run, &db.Repo{ID: f.run.RepoID}, f.work) }()
	<-started
	cancel(pipeline.ErrDaemonShutdown)
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("active run completed after shutdown cancellation")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("active run did not stop after shutdown cancellation")
	}
	got, err := f.d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != types.RunFailed || got.AwaitingAgentSince != nil {
		t.Fatalf("active shutdown cancellation was retained: status=%s awaiting=%v", got.Status, got.AwaitingAgentSince)
	}
}

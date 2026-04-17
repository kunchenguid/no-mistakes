package pipeline

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestExecutor_ContextCancellation(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	// Step that blocks until context is cancelled
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			<-sctx.Ctx.Done()
			return nil, sctx.Ctx.Err()
		},
	}

	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(ctx, run, repo, workDir)
	}()

	// Give executor time to start
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error from cancellation, got nil")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor timed out")
	}

	updated, _ := database.GetRun(run.ID)
	if updated.Status != types.RunFailed {
		t.Errorf("expected run status %q, got %q", types.RunFailed, updated.Status)
	}
}

func TestExecutor_ContextCancelCause(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	// Two steps: first passes, second blocks until context is cancelled.
	// This tests that the cause propagates even when detected between steps.
	step1 := newPassStep(types.StepReview)
	step2 := &adaptiveCallStep{
		name: types.StepTest,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			<-sctx.Ctx.Done()
			return nil, context.Cause(sctx.Ctx)
		},
	}

	exec := NewExecutor(database, p, nil, nil, []Step{step1, step2}, nil)

	cancelReason := fmt.Errorf("cancelled: superseded by new push")
	ctx, cancel := context.WithCancelCause(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(ctx, run, repo, workDir)
	}()

	// Give executor time to start step2
	time.Sleep(50 * time.Millisecond)
	cancel(cancelReason)

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error from cancellation, got nil")
		}
		if !strings.Contains(err.Error(), "superseded by new push") {
			t.Errorf("expected error to contain 'superseded by new push', got %q", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor timed out")
	}

	updated, _ := database.GetRun(run.ID)
	if updated.Status != types.RunCancelled {
		t.Errorf("expected run status %q, got %q", types.RunCancelled, updated.Status)
	}
	if updated.Error == nil || !strings.Contains(*updated.Error, "superseded by new push") {
		var got string
		if updated.Error != nil {
			got = *updated.Error
		}
		t.Errorf("expected run error to contain 'superseded by new push', got %q", got)
	}
}

func TestExecutor_ContextCancelCauseBetweenSteps(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	// First step passes and signals, cancel fires before second step starts.
	started := make(chan struct{})
	step1 := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			close(started)
			// Small delay so cancel can fire before step2 starts
			time.Sleep(50 * time.Millisecond)
			return &StepOutcome{ExitCode: 0}, nil
		},
	}
	step2 := newPassStep(types.StepTest)

	exec := NewExecutor(database, p, nil, nil, []Step{step1, step2}, nil)

	cancelReason := fmt.Errorf("cancelled: superseded by new push")
	ctx, cancel := context.WithCancelCause(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(ctx, run, repo, workDir)
	}()

	<-started
	cancel(cancelReason)

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected error from cancellation, got nil")
		}
		if !strings.Contains(err.Error(), "superseded by new push") {
			t.Errorf("expected error to contain 'superseded by new push', got %q", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor timed out")
	}

	updated, _ := database.GetRun(run.ID)
	if updated.Status != types.RunCancelled {
		t.Errorf("expected run status %q, got %q", types.RunCancelled, updated.Status)
	}
	if updated.Error == nil || !strings.Contains(*updated.Error, "superseded by new push") {
		var got string
		if updated.Error != nil {
			got = *updated.Error
		}
		t.Errorf("expected run error to contain 'superseded by new push', got %q", got)
	}
}

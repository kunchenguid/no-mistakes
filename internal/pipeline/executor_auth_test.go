package pipeline

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type authorizationAgent struct{}

func (authorizationAgent) Name() string { return "codex" }
func (authorizationAgent) Close() error { return nil }
func (authorizationAgent) Run(context.Context, agent.RunOpts) (*agent.Result, error) {
	return nil, &agent.AuthorizationRequiredError{Agent: "codex", Detail: "account rotation"}
}

type countingAuthorizationAgent struct {
	attempts atomic.Int32
}

func (a *countingAuthorizationAgent) Name() string { return "codex" }
func (a *countingAuthorizationAgent) Close() error { return nil }
func (a *countingAuthorizationAgent) Run(context.Context, agent.RunOpts) (*agent.Result, error) {
	a.attempts.Add(1)
	return nil, &agent.AuthorizationRequiredError{Agent: "codex", Detail: "account rotation"}
}

func TestExecutorParksAuthorizationRequiredWithoutTerminalizingRun(t *testing.T) {
	database, p, run, repo := setupTest(t)
	step := &adaptiveCallStep{name: types.StepReview, fn: func(sctx *StepContext) (*StepOutcome, error) {
		_, err := sctx.Agent.Run(sctx.Ctx, agent.RunOpts{Prompt: "review", Purpose: "review"})
		return nil, err
	}}
	exec := NewExecutor(database, p, nil, authorizationAgent{}, []Step{step}, nil)
	ctx, cancel := context.WithCancelCause(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- exec.Execute(ctx, run, repo, t.TempDir()) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		got, getErr := database.GetRun(run.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if got.Status == types.RunAwaitingAuth {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("run did not park for auth: %+v", got)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel(fmt.Errorf("daemon shutting down"))
	err := <-errCh
	if err != nil {
		t.Fatalf("parked execution = %v", err)
	}
	got, getErr := database.GetRun(run.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if got.Status != types.RunAwaitingAuth || got.BlockedReason == nil {
		t.Fatalf("run projection = %+v", got)
	}
	if got.AwaitingAgentSince == nil {
		t.Fatal("authorization park did not persist awaiting-agent marker")
	}
	events, err := database.LifecycleEvents(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if event.EventType == "authorization_required" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing authorization_required event: %+v", events)
	}
}

func TestExecutorRequiresExplicitApprovalForEachAuthorizationRetry(t *testing.T) {
	database, p, run, repo := setupTest(t)
	step := &adaptiveCallStep{name: types.StepReview, fn: func(sctx *StepContext) (*StepOutcome, error) {
		_, err := sctx.Agent.Run(sctx.Ctx, agent.RunOpts{Prompt: "review", Purpose: "review"})
		return nil, err
	}}
	ag := &countingAuthorizationAgent{}
	exec := NewExecutor(database, p, nil, ag, []Step{step}, nil)
	ctx, cancel := context.WithCancelCause(context.Background())
	t.Cleanup(func() { cancel(fmt.Errorf("test cleanup")) })
	errCh := make(chan error, 1)
	go func() { errCh <- exec.Execute(ctx, run, repo, t.TempDir()) }()

	waitForAuthorizationGate(t, database, exec, run.ID, types.StepReview, 1)
	if err := exec.Respond(types.StepReview, types.ActionApprove, nil); err != nil {
		t.Fatalf("approve auth retry: %v", err)
	}
	waitForAuthorizationGate(t, database, exec, run.ID, types.StepReview, 2)
	if err := exec.Respond(types.StepReview, types.ActionApprove, nil); err != nil {
		t.Fatalf("approve bounded auth park: %v", err)
	}
	err := <-errCh
	if err != nil {
		t.Fatalf("bounded auth park = %v", err)
	}
	if got := ag.attempts.Load(); got != 2 {
		t.Fatalf("agent attempts = %d, want initial attempt plus one explicit retry", got)
	}
	got, getErr := database.GetRun(run.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	if got.Status != types.RunAwaitingAuth || got.BlockedReason == nil {
		t.Fatalf("run status = %s blocked=%v, want awaiting_auth", got.Status, got.BlockedReason)
	}
	if got.AwaitingAgentSince == nil {
		t.Fatal("bounded authorization park did not retain awaiting-agent marker")
	}
	events, eventErr := database.LifecycleEvents(run.ID)
	if eventErr != nil {
		t.Fatal(eventErr)
	}
	authEvents := 0
	for _, event := range events {
		if event.EventType == "authorization_required" {
			authEvents++
		}
	}
	if authEvents != 2 {
		t.Fatalf("authorization events = %d, want initial and bounded recovery parks", authEvents)
	}
}

func waitForAuthorizationEvents(t *testing.T, database *db.DB, runID string, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if authorizationEventCount(t, database, runID) >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("authorization events = %d, want at least %d", authorizationEventCount(t, database, runID), want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForAuthorizationGate(t *testing.T, database *db.DB, exec *Executor, runID string, step types.StepName, wantEvents int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		exec.mu.Lock()
		waiting := exec.waiting && exec.waitingStep == step
		exec.mu.Unlock()
		if waiting && authorizationEventCount(t, database, runID) >= wantEvents {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("authorization gate not ready: waiting=%v events=%d wantEvents=%d", waiting, authorizationEventCount(t, database, runID), wantEvents)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func authorizationEventCount(t *testing.T, database *db.DB, runID string) int {
	t.Helper()
	events, err := database.LifecycleEvents(runID)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, event := range events {
		if event.EventType == "authorization_required" {
			count++
		}
	}
	return count
}

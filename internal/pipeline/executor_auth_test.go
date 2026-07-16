package pipeline

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type authorizationAgent struct{}

func (authorizationAgent) Name() string { return "codex" }
func (authorizationAgent) Close() error { return nil }
func (authorizationAgent) Run(context.Context, agent.RunOpts) (*agent.Result, error) {
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

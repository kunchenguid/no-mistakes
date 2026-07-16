package pipeline

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/routing"
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
	if got.AwaitingAgentSince != nil {
		t.Fatalf("authorization park set gate timer: %v", *got.AwaitingAgentSince)
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

func TestRecoveredAuthorizationParkSurvivesDaemonShutdown(t *testing.T) {
	database, p, run, repo := setupTest(t)
	step := &adaptiveCallStep{name: types.StepReview, fn: func(sctx *StepContext) (*StepOutcome, error) {
		_, err := sctx.Agent.Run(sctx.Ctx, agent.RunOpts{Prompt: "review", Purpose: "review"})
		return nil, err
	}}

	ctx, cancel := context.WithCancelCause(context.Background())
	first := NewExecutor(database, p, nil, authorizationAgent{}, []Step{step}, nil)
	done := make(chan error, 1)
	go func() { done <- first.Execute(ctx, run, repo, t.TempDir()) }()
	waitForAuthorizationGate(t, database, first, run.ID, types.StepReview, 1)
	cancel(fmt.Errorf("daemon shutting down"))
	if err := <-done; err != nil {
		t.Fatalf("initial parked execution = %v", err)
	}

	recovered, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	recoveryCtx, recoveryCancel := context.WithCancelCause(context.Background())
	second := NewExecutor(database, p, nil, authorizationAgent{}, []Step{step}, nil)
	recoveredDone := make(chan error, 1)
	go func() { recoveredDone <- second.Resume(recoveryCtx, recovered, repo, t.TempDir()) }()
	waitForAuthorizationGate(t, database, second, run.ID, types.StepReview, 1)
	recoveryCancel(fmt.Errorf("daemon shutting down"))
	if err := <-recoveredDone; err != nil {
		t.Fatalf("recovered parked execution = %v", err)
	}

	got, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != types.RunAwaitingAuth || got.BlockedReason == nil {
		t.Fatalf("recovered authorization projection = %+v", got)
	}
	if got.Error != nil && strings.Contains(strings.ToLower(*got.Error), "daemon shutting down") {
		t.Fatalf("shutdown cause overwrote authorization evidence: %q", *got.Error)
	}
	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 || steps[0].Status != types.StepStatusRunning {
		t.Fatalf("recovered authorization step = %+v", steps)
	}
}

func TestRecoveredAuthorizationApprovalKeepsDurableRetryBound(t *testing.T) {
	database, p, run, repo := setupTest(t)
	step := &adaptiveCallStep{name: types.StepReview, fn: func(sctx *StepContext) (*StepOutcome, error) {
		_, err := sctx.Agent.Run(sctx.Ctx, agent.RunOpts{Prompt: "review", Purpose: "review"})
		return nil, err
	}}
	ag := &countingAuthorizationAgent{}
	ctx, cancel := context.WithCancelCause(context.Background())
	first := NewExecutor(database, p, nil, ag, []Step{step}, nil)
	firstDone := make(chan error, 1)
	go func() { firstDone <- first.Execute(ctx, run, repo, t.TempDir()) }()
	waitForAuthorizationGate(t, database, first, run.ID, types.StepReview, 1)
	cancel(fmt.Errorf("daemon shutting down"))
	if err := <-firstDone; err != nil {
		t.Fatalf("first parked execution = %v", err)
	}

	recovered, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	recoveryCtx, recoveryCancel := context.WithCancelCause(context.Background())
	defer recoveryCancel(fmt.Errorf("test cleanup"))
	second := NewExecutor(database, p, nil, ag, []Step{step}, nil)
	secondDone := make(chan error, 1)
	go func() { secondDone <- second.Resume(recoveryCtx, recovered, repo, t.TempDir()) }()
	waitForAuthorizationGate(t, database, second, run.ID, types.StepReview, 1)
	if err := second.Respond(types.StepReview, types.ActionApprove, nil); err != nil {
		t.Fatal(err)
	}
	waitForAuthorizationGate(t, database, second, run.ID, types.StepReview, 2)
	if err := second.Respond(types.StepReview, types.ActionApprove, nil); err != nil {
		t.Fatal(err)
	}
	if err := <-secondDone; err != nil {
		t.Fatalf("bounded recovered execution = %v", err)
	}
	if got := ag.attempts.Load(); got != 2 {
		t.Fatalf("agent attempts after restart approvals = %d, want initial plus one retry", got)
	}
}

func TestRecoveredUnknownFixerCompletionCannotBeApprovedIntoReplay(t *testing.T) {
	database, p, run, repo := setupTest(t)
	step := &adaptiveCallStep{name: types.StepReview, fn: func(sctx *StepContext) (*StepOutcome, error) {
		if !sctx.Fixing {
			return &StepOutcome{NeedsApproval: true, Findings: `{"summary":"needs a fix","risk_level":"high","findings":[]}`}, nil
		}
		_, err := sctx.Agent.Run(sctx.Ctx, agent.RunOpts{Prompt: "fix", Purpose: "review-fix"})
		return nil, err
	}}
	ag := &countingAuthorizationAgent{}
	ctx, cancel := context.WithCancelCause(context.Background())
	first := NewExecutor(database, p, nil, ag, []Step{step}, nil)
	done := make(chan error, 1)
	go func() { done <- first.Execute(ctx, run, repo, t.TempDir()) }()
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	if err := first.Respond(types.StepReview, types.ActionFix, nil); err != nil {
		t.Fatal(err)
	}
	waitForAuthorizationGate(t, database, first, run.ID, types.StepReview, 1)
	cancel(fmt.Errorf("daemon shutting down"))
	if err := <-done; err != nil {
		t.Fatalf("first fixer park = %v", err)
	}

	recovered, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	ctx2, cancel2 := context.WithCancelCause(context.Background())
	defer cancel2(fmt.Errorf("test cleanup"))
	second := NewExecutor(database, p, nil, ag, []Step{step}, nil)
	done2 := make(chan error, 1)
	go func() { done2 <- second.Resume(ctx2, recovered, repo, t.TempDir()) }()
	waitForAuthorizationGate(t, database, second, run.ID, types.StepReview, 1)
	if err := second.Respond(types.StepReview, types.ActionApprove, nil); err != nil {
		t.Fatal(err)
	}
	if err := <-done2; !errors.Is(err, errAuthorizationParked) {
		t.Fatalf("recovered fixer park = %v, want recoverable operator park", err)
	}
	if got := ag.attempts.Load(); got != 1 {
		t.Fatalf("fixer attempts after approval = %d, want no replay", got)
	}
	got, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != types.RunAwaitingAuth || got.BlockedReason == nil || !strings.Contains(*got.BlockedReason, "completion is unknown") {
		t.Fatalf("recovered fixer operator state = %+v", got)
	}
}

func TestRestoreRouteStateUsesLatestDurableClassification(t *testing.T) {
	database, p, run, _ := setupTest(t)
	for _, decision := range []db.RouteDecision{
		{RunID: run.ID, RequestedHarness: "codex", EffectiveHarness: "codex", Risk: string(routing.RiskHigh), Phase: "review", PromptTransport: "stdin", CreatedAt: 10},
		{RunID: run.ID, RequestedHarness: "codex", EffectiveHarness: "codex", Risk: string(routing.RiskMedium), Phase: "test", PromptTransport: "stdin", CreatedAt: 20},
		{RunID: run.ID, RequestedHarness: "codex", EffectiveHarness: "codex", Risk: string(routing.RiskLow), Phase: "test", PromptTransport: "stdin", CreatedAt: 30},
	} {
		if err := database.InsertRouteDecision(decision); err != nil {
			t.Fatal(err)
		}
	}
	exec := NewExecutor(database, p, nil, nil, nil, nil)
	exec.restoreRouteState(run.ID)
	if exec.routeRisk != routing.RiskLow {
		t.Fatalf("restored risk = %q, want latest low classification", exec.routeRisk)
	}
}

func TestRestoreRouteStateUsesPostResultReviewClassification(t *testing.T) {
	for _, tc := range []struct {
		name        string
		risk        routing.Risk
		phase       string
		wantConfirm bool
	}{
		{"recovered medium", routing.RiskMedium, "review", false},
		{"recovered downgrade", routing.RiskLow, "review-fix", false},
		{"recovered high confirmation", routing.RiskHigh, "review-fix", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database, p, run, _ := setupTest(t)
			if err := database.InsertRouteDecision(db.RouteDecision{
				RunID: run.ID, RequestedHarness: "codex", EffectiveHarness: "codex",
				Risk: string(routing.RiskUnknown), Phase: "review", CreatedAt: 10,
			}); err != nil {
				t.Fatal(err)
			}
			if err := database.InsertRouteResult(db.RouteResult{
				RunID: run.ID, StepName: string(types.StepReview), Round: 1,
				Phase: tc.phase, Risk: string(tc.risk), CreatedAt: 20,
			}); err != nil {
				t.Fatal(err)
			}
			exec := NewExecutor(database, p, nil, nil, nil, nil)
			exec.restoreRouteState(run.ID)
			if exec.routeRisk != tc.risk || exec.routeReviewConfirmed != tc.wantConfirm {
				t.Fatalf("restored route = %q/%t, want %q/%t", exec.routeRisk, exec.routeReviewConfirmed, tc.risk, tc.wantConfirm)
			}
		})
	}
}

func TestAuthorizationParkNamesUnknownFixerCompletion(t *testing.T) {
	database, p, run, repo := setupTest(t)
	step := &adaptiveCallStep{name: types.StepReview, fn: func(sctx *StepContext) (*StepOutcome, error) {
		if !sctx.Fixing {
			return &StepOutcome{NeedsApproval: true, Findings: `{"summary":"needs a fix","risk_level":"high","findings":[]}`}, nil
		}
		_, err := sctx.Agent.Run(sctx.Ctx, agent.RunOpts{Prompt: "fix", Purpose: "review-fix"})
		return nil, err
	}}
	exec := NewExecutor(database, p, nil, authorizationAgent{}, []Step{step}, nil)
	ctx, cancel := context.WithCancelCause(context.Background())
	t.Cleanup(func() { cancel(fmt.Errorf("test cleanup")) })
	done := make(chan error, 1)
	go func() { done <- exec.Execute(ctx, run, repo, t.TempDir()) }()
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	if err := exec.Respond(types.StepReview, types.ActionFix, nil); err != nil {
		t.Fatal(err)
	}
	waitForAuthorizationGate(t, database, exec, run.ID, types.StepReview, 1)
	got, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.BlockedReason == nil || !strings.Contains(*got.BlockedReason, "completion is unknown") {
		t.Fatalf("fixer authorization reason = %v", got.BlockedReason)
	}
	cancel(fmt.Errorf("daemon shutting down"))
	if err := <-done; err != nil {
		t.Fatalf("parked fixer execution = %v", err)
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

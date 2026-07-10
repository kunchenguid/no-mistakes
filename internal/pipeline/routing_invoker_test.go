package pipeline

import (
	"context"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type recordingRoutedAgent struct {
	calls  int
	opts   agent.RunOpts
	result *agent.Result
	err    error
}

func (a *recordingRoutedAgent) Name() string { return "recording" }
func (a *recordingRoutedAgent) Close() error { return nil }
func (a *recordingRoutedAgent) Run(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
	a.calls++
	a.opts = opts
	if a.result == nil && a.err == nil {
		return &agent.Result{}, nil
	}
	return a.result, a.err
}

type recordingLegacyInvoker struct {
	calls int
	last  agent.InvocationRequest
}

func (l *recordingLegacyInvoker) Invoke(_ context.Context, req agent.InvocationRequest) (*agent.Result, error) {
	l.calls++
	l.last = req
	return &agent.Result{}, nil
}

func reservedReviewScope(t *testing.T, database *db.DB, run *db.Run) types.InvocationScope {
	t.Helper()
	step, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatalf("insert review step: %v", err)
	}
	round, err := database.ReserveStepRound(step.ID, 1, "initial")
	if err != nil {
		t.Fatalf("reserve round: %v", err)
	}
	return types.InvocationScope{Kind: types.InvocationScopePipeline, RunID: run.ID, StepResultID: step.ID, StepRoundID: round.ID}
}

func TestRoutingInvokerLaunchesReviewStrongCandidate(t *testing.T) {
	database, _, run, _ := setupTest(t)
	scope := reservedReviewScope(t, database, run)

	native := &recordingRoutedAgent{result: &agent.Result{Usage: agent.TokenUsage{InputTokens: 100, OutputTokens: 20}}}
	legacy := &recordingLegacyInvoker{}
	var factoryName types.AgentName
	var factoryExe string
	ri := newRoutingInvoker(legacy, config.DefaultRoutingConfig(), database)
	ri.newAgent = func(name types.AgentName, executable string) (agent.Agent, error) {
		factoryName, factoryExe = name, executable
		return native, nil
	}

	_, err := ri.Invoke(context.Background(), agent.InvocationRequest{
		Purpose: types.PurposeInitialReview,
		Scope:   scope,
		Payload: agent.RunOpts{Prompt: "review the diff"},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if legacy.calls != 0 {
		t.Fatalf("legacy invoker called %d times for a routed purpose", legacy.calls)
	}
	if native.calls != 1 {
		t.Fatalf("native agent calls = %d, want 1", native.calls)
	}
	if factoryName != types.AgentCodex || factoryExe != "codex" {
		t.Fatalf("factory built (%q,%q), want (codex,codex)", factoryName, factoryExe)
	}
	if native.opts.Model != "gpt-5.6-sol" || native.opts.Effort != types.EffortHigh {
		t.Fatalf("native run opts model/effort = (%q,%q), want (gpt-5.6-sol,high)", native.opts.Model, native.opts.Effort)
	}
	if native.opts.Prompt != "review the diff" {
		t.Fatalf("native prompt = %q, want the review prompt", native.opts.Prompt)
	}

	attempts, err := database.GetInvocationAttemptsByStepResult(scope.StepResultID)
	if err != nil || len(attempts) != 1 {
		t.Fatalf("attempts = %+v, err = %v; want one", attempts, err)
	}
	got := attempts[0]
	wantCandidate := types.InvocationCandidate{Profile: "review_strong", Tier: 0, CandidateIndex: 0, Runner: types.RunnerCodex, Model: "gpt-5.6-sol", Effort: types.EffortHigh}
	if got.Start.Candidate != wantCandidate {
		t.Fatalf("recorded candidate = %+v, want %+v", got.Start.Candidate, wantCandidate)
	}
	if got.Start.Purpose != types.PurposeInitialReview || got.Start.Role != types.InvocationRoleVerifier {
		t.Fatalf("recorded purpose/role = %q/%q", got.Start.Purpose, got.Start.Role)
	}
	if got.Terminal == nil || got.Terminal.Outcome != types.InvocationOutcomeSucceeded {
		t.Fatalf("terminal = %+v, want succeeded", got.Terminal)
	}
	if got.Terminal.InputTokens != 100 || got.Terminal.OutputTokens != 20 {
		t.Fatalf("terminal tokens = %+v, want in=100 out=20", got.Terminal)
	}
}

func TestRoutingInvokerDelegatesUnmigratedPurpose(t *testing.T) {
	database, _, run, _ := setupTest(t)
	scope := reservedReviewScope(t, database, run)
	legacy := &recordingLegacyInvoker{}
	factoryCalled := false
	ri := newRoutingInvoker(legacy, config.DefaultRoutingConfig(), database)
	ri.newAgent = func(types.AgentName, string) (agent.Agent, error) {
		factoryCalled = true
		return &recordingRoutedAgent{}, nil
	}

	if _, err := ri.Invoke(context.Background(), agent.InvocationRequest{
		Purpose: types.PurposeLintInspection,
		Scope:   scope,
		Payload: agent.RunOpts{Prompt: "lint"},
	}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if legacy.calls != 1 {
		t.Fatalf("legacy calls = %d, want 1 for an unmigrated purpose", legacy.calls)
	}
	if factoryCalled {
		t.Fatal("routed native agent must not be built for an unmigrated purpose")
	}
}

func TestRoutingInvokerZeroRoutingDelegates(t *testing.T) {
	database, _, run, _ := setupTest(t)
	scope := reservedReviewScope(t, database, run)
	legacy := &recordingLegacyInvoker{}
	ri := newRoutingInvoker(legacy, config.RoutingConfig{}, database)
	ri.newAgent = func(types.AgentName, string) (agent.Agent, error) {
		t.Fatal("factory must not run when routing is unconfigured")
		return nil, nil
	}
	if _, err := ri.Invoke(context.Background(), agent.InvocationRequest{
		Purpose: types.PurposeInitialReview,
		Scope:   scope,
		Payload: agent.RunOpts{Prompt: "review"},
	}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if legacy.calls != 1 {
		t.Fatalf("legacy calls = %d, want 1 when routing is unconfigured", legacy.calls)
	}
}

func TestRoutingInvokerFailsBeforeLaunchOnMissingRoute(t *testing.T) {
	database, _, run, _ := setupTest(t)
	scope := reservedReviewScope(t, database, run)
	routing := config.DefaultRoutingConfig()
	delete(routing.Routes, types.PurposeInitialReview)
	legacy := &recordingLegacyInvoker{}
	ri := newRoutingInvoker(legacy, routing, database)
	ri.newAgent = func(types.AgentName, string) (agent.Agent, error) {
		t.Fatal("no model may launch when routing data is missing")
		return nil, nil
	}
	if _, err := ri.Invoke(context.Background(), agent.InvocationRequest{
		Purpose: types.PurposeInitialReview,
		Scope:   scope,
		Payload: agent.RunOpts{Prompt: "review"},
	}); err == nil {
		t.Fatal("expected a fail-before-launch error for a missing route")
	}
	attempts, err := database.GetInvocationAttemptsByStepResult(scope.StepResultID)
	if err != nil {
		t.Fatalf("get attempts: %v", err)
	}
	if len(attempts) != 0 {
		t.Fatalf("recorded %d attempts, want 0 when nothing launched", len(attempts))
	}
}

func TestRoutingInvokerRecordsOperationalFailureDomain(t *testing.T) {
	database, _, run, _ := setupTest(t)
	scope := reservedReviewScope(t, database, run)
	native := &recordingRoutedAgent{err: &agent.OperationalError{Kind: agent.OpFailureOverload, Err: errStub("overloaded")}}
	ri := newRoutingInvoker(&recordingLegacyInvoker{}, config.DefaultRoutingConfig(), database)
	ri.newAgent = func(types.AgentName, string) (agent.Agent, error) { return native, nil }

	if _, err := ri.Invoke(context.Background(), agent.InvocationRequest{
		Purpose: types.PurposeInitialReview,
		Scope:   scope,
		Payload: agent.RunOpts{Prompt: "review"},
	}); err == nil {
		t.Fatal("expected the operational failure to surface")
	}
	attempts, _ := database.GetInvocationAttemptsByStepResult(scope.StepResultID)
	if len(attempts) != 1 || attempts[0].Terminal == nil {
		t.Fatalf("attempts = %+v, want one finalized attempt", attempts)
	}
	if attempts[0].Terminal.Outcome != types.InvocationOutcomeFailed {
		t.Fatalf("outcome = %q, want failed", attempts[0].Terminal.Outcome)
	}
	if attempts[0].Terminal.FailureDomain != types.FailureDomainOpenAI {
		t.Fatalf("failure domain = %q, want openai", attempts[0].Terminal.FailureDomain)
	}
}

type errStub string

func (e errStub) Error() string { return string(e) }

func recordRoutedReviewAttempt(t *testing.T, database *db.DB, run *db.Run) (stepID, roundID, attemptID string) {
	t.Helper()
	step, _ := database.InsertStepResult(run.ID, types.StepReview)
	round, _ := database.ReserveStepRound(step.ID, 1, "initial")
	id, err := database.StartInvocationAttempt(types.InvocationAttemptStart{
		Purpose:      types.PurposeInitialReview,
		Role:         types.InvocationRoleVerifier,
		Scope:        types.InvocationScope{Kind: types.InvocationScopePipeline, RunID: run.ID, StepResultID: step.ID, StepRoundID: round.ID},
		CandidateKey: "review_strong:0:codex",
		Candidate:    types.InvocationCandidate{Profile: "review_strong", Runner: types.RunnerCodex, Model: "gpt-5.6-sol", Effort: types.EffortHigh},
	})
	if err != nil {
		t.Fatalf("start routed review attempt: %v", err)
	}
	_ = database.FinishInvocationAttempt(id, types.InvocationAttemptTerminal{Outcome: types.InvocationOutcomeSucceeded})
	return step.ID, round.ID, id
}

func TestExecutorRecordsReviewLineagesForRoutedReview(t *testing.T) {
	database, p, run, _ := setupTest(t)
	_, roundID, attemptID := recordRoutedReviewAttempt(t, database, run)

	exec := NewExecutor(database, p, nil, nil, nil, nil)
	findingsJSON := `{"findings":[{"id":"review-1","severity":"error","description":"bug","action":"auto-fix"},{"id":"review-2","severity":"info","description":"note","action":"no-op"}],"summary":"2"}`
	exec.recordReviewLineages(run, types.StepReview, roundID, findingsJSON)

	lineages, err := database.GetFindingLineagesByAttempt(attemptID)
	if err != nil {
		t.Fatalf("get lineages: %v", err)
	}
	if len(lineages) != 2 {
		t.Fatalf("lineages = %d, want 2", len(lineages))
	}
	if lineages[0].DisplayID != "review-1" || lineages[0].ID == "review-1" {
		t.Fatalf("lineage identity must be independent of the display id: %+v", lineages[0])
	}
}

func TestExecutorSkipsLineagesForLegacyReview(t *testing.T) {
	database, p, run, _ := setupTest(t)
	step, _ := database.InsertStepResult(run.ID, types.StepReview)
	round, _ := database.ReserveStepRound(step.ID, 1, "initial")
	_, err := database.StartInvocationAttempt(types.InvocationAttemptStart{
		Purpose:      types.PurposeInitialReview,
		Role:         types.InvocationRoleVerifier,
		Scope:        types.InvocationScope{Kind: types.InvocationScopePipeline, RunID: run.ID, StepResultID: step.ID, StepRoundID: round.ID},
		CandidateKey: types.LegacyCandidateKey,
	})
	if err != nil {
		t.Fatalf("start legacy review attempt: %v", err)
	}

	exec := NewExecutor(database, p, nil, nil, nil, nil)
	findingsJSON := `{"findings":[{"id":"review-1","severity":"error","description":"bug","action":"auto-fix"}],"summary":"1"}`
	exec.recordReviewLineages(run, types.StepReview, round.ID, findingsJSON)

	byRun, err := database.GetFindingLineagesByRun(run.ID)
	if err != nil {
		t.Fatalf("get lineages: %v", err)
	}
	if len(byRun) != 0 {
		t.Fatalf("legacy review created %d lineages, want 0", len(byRun))
	}
}

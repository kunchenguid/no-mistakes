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
	ri := newRoutingInvoker(legacy, config.DefaultRoutingConfig(), database, newProviderCircuits())
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
	ri := newRoutingInvoker(legacy, config.DefaultRoutingConfig(), database, newProviderCircuits())
	ri.newAgent = func(types.AgentName, string) (agent.Agent, error) {
		factoryCalled = true
		return &recordingRoutedAgent{}, nil
	}

	if _, err := ri.Invoke(context.Background(), agent.InvocationRequest{
		Purpose: types.Purpose("legacy_only_purpose"),
		Scope:   scope,
		Payload: agent.RunOpts{Prompt: "legacy path"},
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
	ri := newRoutingInvoker(legacy, config.RoutingConfig{}, database, newProviderCircuits())
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
	ri := newRoutingInvoker(legacy, routing, database, newProviderCircuits())
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

// perRunner dispatches the factory to a per-runner recording agent so a cascade
// test can give codex and claude distinct behaviors.
func perRunner(codex, claude agent.Agent) agentFactory {
	return func(name types.AgentName, _ string) (agent.Agent, error) {
		if name == types.AgentClaude {
			return claude, nil
		}
		return codex, nil
	}
}

func opError() error {
	return &agent.OperationalError{Kind: agent.OpFailureOverload, Err: errStub("overloaded")}
}

func attemptForRunner(attempts []*db.InvocationAttempt, runner types.Runner) *db.InvocationAttempt {
	for _, a := range attempts {
		if a.Start.Candidate.Runner == runner {
			return a
		}
	}
	return nil
}

func TestRoutingInvokerFailsOverToBackupOnOperationalFailure(t *testing.T) {
	database, _, run, _ := setupTest(t)
	scope := reservedReviewScope(t, database, run)
	codex := &recordingRoutedAgent{err: opError()}
	claude := &recordingRoutedAgent{result: &agent.Result{Output: []byte(`{}`), Usage: agent.TokenUsage{InputTokens: 7}}}
	circuits := newProviderCircuits()
	ri := newRoutingInvoker(&recordingLegacyInvoker{}, config.DefaultRoutingConfig(), database, circuits)
	ri.newAgent = perRunner(codex, claude)

	result, err := ri.Invoke(context.Background(), agent.InvocationRequest{
		Purpose: types.PurposeInitialReview, Scope: scope, Payload: agent.RunOpts{Prompt: "review"},
	})
	if err != nil {
		t.Fatalf("failover should surface the backup success, got %v", err)
	}
	if result == nil {
		t.Fatal("expected the backup Candidate's result")
	}
	if codex.calls != 1 || claude.calls != 1 {
		t.Fatalf("calls codex=%d claude=%d, want 1 and 1", codex.calls, claude.calls)
	}
	if !circuits.isOpen(types.FailureDomainOpenAI) || circuits.isOpen(types.FailureDomainAnthropic) {
		t.Fatal("expected only the OpenAI circuit open after codex's operational failure")
	}
	attempts, _ := database.GetInvocationAttemptsByStepResult(scope.StepResultID)
	if len(attempts) != 2 {
		t.Fatalf("attempts = %d, want 2 (failed codex, succeeded claude)", len(attempts))
	}
	codexAttempt := attemptForRunner(attempts, types.RunnerCodex)
	if codexAttempt.Terminal.Outcome != types.InvocationOutcomeFailed || codexAttempt.Terminal.FailureDomain != types.FailureDomainOpenAI {
		t.Fatalf("codex attempt = %+v, want failed/openai", codexAttempt.Terminal)
	}
	claudeAttempt := attemptForRunner(attempts, types.RunnerClaude)
	if claudeAttempt.Terminal.Outcome != types.InvocationOutcomeSucceeded {
		t.Fatalf("claude attempt = %+v, want succeeded", claudeAttempt.Terminal)
	}
}

func TestRoutingInvokerOpensBothCircuitsAndFailsClosed(t *testing.T) {
	database, _, run, _ := setupTest(t)
	scope := reservedReviewScope(t, database, run)
	codex := &recordingRoutedAgent{err: opError()}
	claude := &recordingRoutedAgent{err: opError()}
	circuits := newProviderCircuits()
	ri := newRoutingInvoker(&recordingLegacyInvoker{}, config.DefaultRoutingConfig(), database, circuits)
	ri.newAgent = perRunner(codex, claude)

	if _, err := ri.Invoke(context.Background(), agent.InvocationRequest{
		Purpose: types.PurposeInitialReview, Scope: scope, Payload: agent.RunOpts{Prompt: "review"},
	}); err == nil {
		t.Fatal("expected fail-closed after both candidates fail operationally")
	}
	if !circuits.isOpen(types.FailureDomainOpenAI) || !circuits.isOpen(types.FailureDomainAnthropic) {
		t.Fatal("expected both provider circuits open")
	}
	attempts, _ := database.GetInvocationAttemptsByStepResult(scope.StepResultID)
	if len(attempts) != 2 {
		t.Fatalf("attempts = %d, want 2 failed", len(attempts))
	}
	if attemptForRunner(attempts, types.RunnerCodex).Terminal.FailureDomain != types.FailureDomainOpenAI {
		t.Fatal("codex attempt should carry the openai failure domain")
	}
	if attemptForRunner(attempts, types.RunnerClaude).Terminal.FailureDomain != types.FailureDomainAnthropic {
		t.Fatal("claude attempt should carry the anthropic failure domain")
	}
}

func TestRoutingInvokerNonOperationalFailureDoesNotFailOver(t *testing.T) {
	database, _, run, _ := setupTest(t)
	scope := reservedReviewScope(t, database, run)
	codex := &recordingRoutedAgent{err: errStub("strong review output malformed")}
	claude := &recordingRoutedAgent{}
	circuits := newProviderCircuits()
	ri := newRoutingInvoker(&recordingLegacyInvoker{}, config.DefaultRoutingConfig(), database, circuits)
	ri.newAgent = perRunner(codex, claude)

	if _, err := ri.Invoke(context.Background(), agent.InvocationRequest{
		Purpose: types.PurposeInitialReview, Scope: scope, Payload: agent.RunOpts{Prompt: "review"},
	}); err == nil {
		t.Fatal("expected the non-operational failure to surface")
	}
	if claude.calls != 0 {
		t.Fatalf("claude launched %d times; a non-operational failure must not fail over", claude.calls)
	}
	if circuits.isOpen(types.FailureDomainOpenAI) {
		t.Fatal("a non-operational failure must never open a provider circuit")
	}
	attempts, _ := database.GetInvocationAttemptsByStepResult(scope.StepResultID)
	if len(attempts) != 1 {
		t.Fatalf("attempts = %d, want 1 (no failover)", len(attempts))
	}
	if attempts[0].Terminal.Outcome != types.InvocationOutcomeFailed || attempts[0].Terminal.FailureDomain != "" {
		t.Fatalf("attempt = %+v, want failed with no failure domain", attempts[0].Terminal)
	}
}

func TestRoutingInvokerSkipsOpenCircuitDomain(t *testing.T) {
	database, _, run, _ := setupTest(t)
	scope := reservedReviewScope(t, database, run)
	codex := &recordingRoutedAgent{}
	claude := &recordingRoutedAgent{result: &agent.Result{Output: []byte(`{}`)}}
	circuits := newProviderCircuits()
	circuits.markOpen(types.FailureDomainOpenAI) // a prior routed invocation opened OpenAI
	ri := newRoutingInvoker(&recordingLegacyInvoker{}, config.DefaultRoutingConfig(), database, circuits)
	ri.newAgent = perRunner(codex, claude)

	result, err := ri.Invoke(context.Background(), agent.InvocationRequest{
		Purpose: types.PurposeInitialReview, Scope: scope, Payload: agent.RunOpts{Prompt: "review"},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if result == nil {
		t.Fatal("expected the backup Candidate's result")
	}
	if codex.calls != 0 {
		t.Fatalf("codex launched %d times; its provider circuit was open", codex.calls)
	}
	if claude.calls != 1 {
		t.Fatalf("claude calls = %d, want 1", claude.calls)
	}
	attempts, _ := database.GetInvocationAttemptsByStepResult(scope.StepResultID)
	if len(attempts) != 2 {
		t.Fatalf("attempts = %d, want 2 (skipped codex, succeeded claude)", len(attempts))
	}
	codexAttempt := attemptForRunner(attempts, types.RunnerCodex)
	if codexAttempt.Terminal.Outcome != types.InvocationOutcomeSkipped || codexAttempt.Terminal.FailureDomain != types.FailureDomainOpenAI {
		t.Fatalf("codex attempt = %+v, want skipped/openai", codexAttempt.Terminal)
	}
	if attemptForRunner(attempts, types.RunnerClaude).Terminal.Outcome != types.InvocationOutcomeSucceeded {
		t.Fatal("claude attempt should have succeeded")
	}
}

func TestRoutingInvokerFailsClosedWhenAllCircuitsOpen(t *testing.T) {
	database, _, run, _ := setupTest(t)
	scope := reservedReviewScope(t, database, run)
	circuits := newProviderCircuits()
	circuits.markOpen(types.FailureDomainOpenAI)
	circuits.markOpen(types.FailureDomainAnthropic)
	ri := newRoutingInvoker(&recordingLegacyInvoker{}, config.DefaultRoutingConfig(), database, circuits)
	ri.newAgent = func(types.AgentName, string) (agent.Agent, error) {
		t.Fatal("no candidate may launch when every provider circuit is open")
		return nil, nil
	}
	if _, err := ri.Invoke(context.Background(), agent.InvocationRequest{
		Purpose: types.PurposeInitialReview, Scope: scope, Payload: agent.RunOpts{Prompt: "review"},
	}); err == nil {
		t.Fatal("expected fail-closed when all provider circuits are open")
	}
	attempts, _ := database.GetInvocationAttemptsByStepResult(scope.StepResultID)
	if len(attempts) != 2 {
		t.Fatalf("attempts = %d, want 2 skipped", len(attempts))
	}
	for _, a := range attempts {
		if a.Terminal == nil || a.Terminal.Outcome != types.InvocationOutcomeSkipped {
			t.Fatalf("attempt terminal = %+v, want skipped", a.Terminal)
		}
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

func TestRoutingInvokerLaunchesRequestedTier(t *testing.T) {
	database, _, run, _ := setupTest(t)
	scope := reservedReviewScope(t, database, run)
	native := &recordingRoutedAgent{result: &agent.Result{Output: []byte(`{"summary":"x"}`)}}
	ri := newRoutingInvoker(&recordingLegacyInvoker{}, config.DefaultRoutingConfig(), database, newProviderCircuits())
	ri.newAgent = func(types.AgentName, string) (agent.Agent, error) { return native, nil }

	if _, err := ri.Invoke(context.Background(), agent.InvocationRequest{
		Purpose: types.PurposeStructuredFindingRepair, Tier: 1, Scope: scope, Payload: agent.RunOpts{Prompt: "fix"},
	}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if native.opts.Model != "gpt-5.6-terra" || native.opts.Effort != types.EffortMedium {
		t.Fatalf("tier 1 model/effort = %q/%q, want gpt-5.6-terra/medium", native.opts.Model, native.opts.Effort)
	}
	attempts, _ := database.GetInvocationAttemptsByStepResult(scope.StepResultID)
	if len(attempts) != 1 || attempts[0].Start.Candidate.Profile != "fix_balanced" || attempts[0].Start.Candidate.Tier != 1 {
		t.Fatalf("attempt candidate = %+v, want fix_balanced tier 1", attempts[0].Start.Candidate)
	}
}

func TestRoutingInvokerRejectsOutOfRangeTier(t *testing.T) {
	database, _, run, _ := setupTest(t)
	scope := reservedReviewScope(t, database, run)
	ri := newRoutingInvoker(&recordingLegacyInvoker{}, config.DefaultRoutingConfig(), database, newProviderCircuits())
	ri.newAgent = func(types.AgentName, string) (agent.Agent, error) {
		t.Fatal("no candidate may launch for an out-of-range tier")
		return nil, nil
	}
	if _, err := ri.Invoke(context.Background(), agent.InvocationRequest{
		Purpose: types.PurposeStructuredFindingRepair, Tier: 9, Scope: scope, Payload: agent.RunOpts{Prompt: "fix"},
	}); err == nil {
		t.Fatal("expected an out-of-range tier to fail closed")
	}
}

// TestRoutingInvokerLaunchesRoutineCandidates proves the four gate-scoped
// routine Purposes route to their declared Profile/model/effort through a fresh
// native Candidate — never the legacy invoker — and record ownership.
func TestRoutingInvokerLaunchesRoutineCandidates(t *testing.T) {
	cases := []struct {
		name    string
		purpose types.Purpose
		profile string
		model   string
		effort  types.Effort
		role    types.InvocationRole
	}{
		{"intent summary", types.PurposeIntentSummarization, "prose_fast", "gpt-5.6-luna", types.EffortLow, types.InvocationRoleVerifier},
		{"pr composition", types.PurposePRComposition, "prose_fast", "gpt-5.6-luna", types.EffortLow, types.InvocationRoleFixer},
		{"intent disambiguation", types.PurposeIntentDisambiguation, "tools_balanced", "gpt-5.6-terra", types.EffortHigh, types.InvocationRoleVerifier},
		{"test evidence", types.PurposeTestEvidence, "tools_balanced", "gpt-5.6-terra", types.EffortHigh, types.InvocationRoleFixer},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			database, _, run, _ := setupTest(t)
			scope := reservedReviewScope(t, database, run)
			native := &recordingRoutedAgent{result: &agent.Result{}}
			legacy := &recordingLegacyInvoker{}
			var factoryName types.AgentName
			ri := newRoutingInvoker(legacy, config.DefaultRoutingConfig(), database, newProviderCircuits())
			ri.newAgent = func(name types.AgentName, _ string) (agent.Agent, error) {
				factoryName = name
				return native, nil
			}
			if _, err := ri.Invoke(context.Background(), agent.InvocationRequest{
				Purpose: tc.purpose, Scope: scope, Payload: agent.RunOpts{Prompt: "go"},
			}); err != nil {
				t.Fatalf("Invoke: %v", err)
			}
			if legacy.calls != 0 {
				t.Fatalf("legacy invoker called %d times; a routed routine purpose must not use legacy", legacy.calls)
			}
			if native.calls != 1 || factoryName != types.AgentCodex {
				t.Fatalf("native calls=%d factory=%q; want 1 launch via codex", native.calls, factoryName)
			}
			if native.opts.Model != tc.model || native.opts.Effort != tc.effort {
				t.Fatalf("native model/effort=(%q,%q), want (%q,%q)", native.opts.Model, native.opts.Effort, tc.model, tc.effort)
			}
			attempts, err := database.GetInvocationAttemptsByStepResult(scope.StepResultID)
			if err != nil || len(attempts) != 1 {
				t.Fatalf("attempts=%+v err=%v; want exactly one recorded", attempts, err)
			}
			got := attempts[0].Start
			if got.Purpose != tc.purpose || got.Role != tc.role {
				t.Fatalf("recorded purpose/role=%q/%q, want %q/%q", got.Purpose, got.Role, tc.purpose, tc.role)
			}
			if got.Candidate.Profile != tc.profile || got.Candidate.Model != tc.model || got.Candidate.Effort != tc.effort {
				t.Fatalf("recorded candidate=%+v, want profile=%q model=%q effort=%q", got.Candidate, tc.profile, tc.model, tc.effort)
			}
		})
	}
}

// TestRoutinegatePurposesAreRouted guards against a routine Purpose silently
// dropping back to the legacy path.
func TestRoutineGatePurposesAreRouted(t *testing.T) {
	for _, p := range []types.Purpose{
		types.PurposeIntentSummarization,
		types.PurposePRComposition,
		types.PurposeIntentDisambiguation,
		types.PurposeTestEvidence,
	} {
		if !routedPurposes[p] {
			t.Errorf("purpose %q is not routed; gate-scoped routine work would use the legacy adapter", p)
		}
	}
}

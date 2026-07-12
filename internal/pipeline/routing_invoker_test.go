package pipeline

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
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
	runFn  func(agent.RunOpts) (*agent.Result, error)
}

func (a *recordingRoutedAgent) Name() string { return "recording" }
func (a *recordingRoutedAgent) Close() error { return nil }
func (a *recordingRoutedAgent) Run(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
	a.calls++
	a.opts = opts
	if a.runFn != nil {
		return a.runFn(opts)
	}
	if a.result == nil && a.err == nil {
		return &agent.Result{}, nil
	}
	return a.result, a.err
}

type failingFinishJournal struct {
	agent.InvocationJournal
	err error
}

func (j failingFinishJournal) FinishInvocationAttempt(string, types.InvocationAttemptTerminal) error {
	return j.err
}

type failingAttemptIsolation struct {
	err   error
	calls int
}

func (i *failingAttemptIsolation) RestoreFailedAttempt() error {
	i.calls++
	return i.err
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
	var factoryName types.AgentName
	var factoryExe string
	ri := newRoutingInvoker(config.DefaultRoutingConfig(), database, newProviderCircuits())
	ri.newAgent = func(name types.AgentName, executable string) (agent.Agent, error) {
		factoryName, factoryExe = name, executable
		return native, nil
	}

	_, err := ri.Invoke(context.Background(), agent.InvocationRequest{
		Purpose: types.PurposeInitialReview,
		Scope:   scope,
		Payload: agent.RunOpts{Prompt: "review the diff", Purpose: "legacy-review-label"},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
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
	if native.opts.Role != types.InvocationRoleVerifier {
		t.Fatalf("native run role = %q, want registry-derived verifier", native.opts.Role)
	}
	if native.opts.Prompt != "review the diff" {
		t.Fatalf("native prompt = %q, want the review prompt", native.opts.Prompt)
	}
	if native.opts.Purpose != string(types.PurposeInitialReview) {
		t.Fatalf("native purpose = %q, want semantic routed purpose %q", native.opts.Purpose, types.PurposeInitialReview)
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

func TestRoutingInvokerFailsClosedOnUnroutablePurpose(t *testing.T) {
	database, _, run, _ := setupTest(t)
	scope := reservedReviewScope(t, database, run)
	factoryCalled := false
	ri := newRoutingInvoker(config.DefaultRoutingConfig(), database, newProviderCircuits())
	ri.newAgent = func(types.AgentName, string) (agent.Agent, error) {
		factoryCalled = true
		return &recordingRoutedAgent{}, nil
	}

	if _, err := ri.Invoke(context.Background(), agent.InvocationRequest{
		Purpose: types.Purpose("unroutable_purpose"),
		Scope:   scope,
		Payload: agent.RunOpts{Prompt: "no route"},
	}); err == nil {
		t.Fatal("expected an unroutable purpose to fail closed with no legacy fallback")
	}
	if factoryCalled {
		t.Fatal("no native agent may be built for an unroutable purpose")
	}
}

func TestRoutingInvokerFailsClosedWhenRoutingUnconfigured(t *testing.T) {
	database, _, run, _ := setupTest(t)
	scope := reservedReviewScope(t, database, run)
	ri := newRoutingInvoker(config.RoutingConfig{}, database, newProviderCircuits())
	ri.newAgent = func(types.AgentName, string) (agent.Agent, error) {
		t.Fatal("factory must not run when routing is unconfigured")
		return nil, nil
	}
	if _, err := ri.Invoke(context.Background(), agent.InvocationRequest{
		Purpose: types.PurposeInitialReview,
		Scope:   scope,
		Payload: agent.RunOpts{Prompt: "review"},
	}); err == nil {
		t.Fatal("expected fail-closed error when routing is unconfigured, not a legacy fallback")
	}
}

func TestRoutingInvokerFailsBeforeLaunchOnMissingRoute(t *testing.T) {
	database, _, run, _ := setupTest(t)
	scope := reservedReviewScope(t, database, run)
	routing := config.DefaultRoutingConfig()
	delete(routing.Routes, types.PurposeInitialReview)
	ri := newRoutingInvoker(routing, database, newProviderCircuits())
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
	ri := newRoutingInvoker(config.DefaultRoutingConfig(), database, circuits)
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

func TestRoutingInvokerStopsCascadeWhenOperationalTerminalCannotBePersisted(t *testing.T) {
	database, _, run, _ := setupTest(t)
	scope := reservedReviewScope(t, database, run)
	codex := &recordingRoutedAgent{err: opError()}
	claude := &recordingRoutedAgent{}
	circuits := newProviderCircuits()
	journalErr := errStub("terminal journal unavailable")
	ri := newRoutingInvoker(config.DefaultRoutingConfig(), failingFinishJournal{
		InvocationJournal: database,
		err:               journalErr,
	}, circuits)
	ri.newAgent = perRunner(codex, claude)

	_, err := ri.Invoke(context.Background(), agent.InvocationRequest{
		Purpose: types.PurposeInitialReview, Scope: scope, Payload: agent.RunOpts{Prompt: "review"},
	})
	if err == nil {
		t.Fatal("expected terminal persistence failure")
	}
	var operational *agent.OperationalError
	if errors.As(err, &operational) {
		t.Fatalf("terminal persistence failure unwraps as operational provider error: %v", err)
	}
	if !errors.Is(err, journalErr) {
		t.Fatalf("error = %v, want terminal journal cause", err)
	}
	if codex.calls != 1 || claude.calls != 0 {
		t.Fatalf("calls codex=%d claude=%d, want 1 and 0", codex.calls, claude.calls)
	}
	if circuits.isOpen(types.FailureDomainOpenAI) || circuits.isOpen(types.FailureDomainAnthropic) {
		t.Fatal("no circuit may open before its terminal fact is durable")
	}
	attempts, getErr := database.GetInvocationAttemptsByStepResult(scope.StepResultID)
	if getErr != nil {
		t.Fatalf("get attempts: %v", getErr)
	}
	if len(attempts) != 1 {
		t.Fatalf("attempts = %d, want one durably active attempt", len(attempts))
	}
	if attempts[0].Terminal != nil {
		t.Fatalf("terminal = %+v, want active attempt after terminal persistence failure", attempts[0].Terminal)
	}
}

func TestRoutingInvokerOpensBothCircuitsAndFailsClosed(t *testing.T) {
	database, _, run, _ := setupTest(t)
	scope := reservedReviewScope(t, database, run)
	codex := &recordingRoutedAgent{err: opError()}
	claude := &recordingRoutedAgent{err: opError()}
	circuits := newProviderCircuits()
	ri := newRoutingInvoker(config.DefaultRoutingConfig(), database, circuits)
	ri.newAgent = perRunner(codex, claude)

	_, err := ri.Invoke(context.Background(), agent.InvocationRequest{
		Purpose: types.PurposeInitialReview, Scope: scope, Payload: agent.RunOpts{Prompt: "review"},
	})
	if err == nil {
		t.Fatal("expected fail-closed after both candidates fail operationally")
	}
	var unavailable *agent.ProfileUnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("error = %T %v, want identifiable ProfileUnavailableError", err, err)
	}
	if unavailable.Profile != "review_strong" || unavailable.Cause == nil {
		t.Fatalf("profile unavailable error = %+v, want review_strong with operational cause", unavailable)
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
	ri := newRoutingInvoker(config.DefaultRoutingConfig(), database, circuits)
	ri.newAgent = perRunner(codex, claude)

	_, err := ri.Invoke(context.Background(), agent.InvocationRequest{
		Purpose: types.PurposeInitialReview, Scope: scope, Payload: agent.RunOpts{Prompt: "review"},
	})
	if err == nil {
		t.Fatal("expected the non-operational failure to surface")
	}
	var unavailable *agent.ProfileUnavailableError
	if errors.As(err, &unavailable) {
		t.Fatalf("executed candidate's bad result was misclassified as profile unavailable: %v", err)
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
	ri := newRoutingInvoker(config.DefaultRoutingConfig(), database, circuits)
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
	if attempts[0].Start.Candidate.Runner != types.RunnerCodex ||
		attempts[0].Terminal.Outcome != types.InvocationOutcomeSkipped ||
		attempts[0].Terminal.FailureDomain != types.FailureDomainOpenAI {
		t.Fatalf("first attempt = %+v, want immutable codex skipped/openai fact", attempts[0])
	}
	if attempts[1].Start.Candidate.Runner != types.RunnerClaude ||
		attempts[1].Terminal.Outcome != types.InvocationOutcomeSucceeded {
		t.Fatalf("second attempt = %+v, want claude success after skipped codex", attempts[1])
	}
}

func TestRoutingInvokerFailsClosedWhenAllCircuitsOpen(t *testing.T) {
	database, _, run, _ := setupTest(t)
	scope := reservedReviewScope(t, database, run)
	circuits := newProviderCircuits()
	circuits.markOpen(types.FailureDomainOpenAI)
	circuits.markOpen(types.FailureDomainAnthropic)
	ri := newRoutingInvoker(config.DefaultRoutingConfig(), database, circuits)
	ri.newAgent = func(types.AgentName, string) (agent.Agent, error) {
		t.Fatal("no candidate may launch when every provider circuit is open")
		return nil, nil
	}
	_, err := ri.Invoke(context.Background(), agent.InvocationRequest{
		Purpose: types.PurposeInitialReview, Scope: scope, Payload: agent.RunOpts{Prompt: "review"},
	})
	if err == nil {
		t.Fatal("expected fail-closed when all provider circuits are open")
	}
	var unavailable *agent.ProfileUnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("error = %T %v, want identifiable ProfileUnavailableError", err, err)
	}
	if unavailable.Profile != "review_strong" || unavailable.Cause != nil {
		t.Fatalf("profile unavailable error = %+v, want review_strong with no executed-candidate cause", unavailable)
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
	ri := newRoutingInvoker(config.DefaultRoutingConfig(), database, newProviderCircuits())
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
	ri := newRoutingInvoker(config.DefaultRoutingConfig(), database, newProviderCircuits())
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
			var factoryName types.AgentName
			ri := newRoutingInvoker(config.DefaultRoutingConfig(), database, newProviderCircuits())
			ri.newAgent = func(name types.AgentName, _ string) (agent.Agent, error) {
				factoryName = name
				return native, nil
			}
			if _, err := ri.Invoke(context.Background(), agent.InvocationRequest{
				Purpose: tc.purpose, Scope: scope, Payload: agent.RunOpts{Prompt: "go"},
			}); err != nil {
				t.Fatalf("Invoke: %v", err)
			}
			if native.calls != 1 || factoryName != types.AgentCodex {
				t.Fatalf("native calls=%d factory=%q; want 1 launch via codex", native.calls, factoryName)
			}
			if native.opts.Model != tc.model || native.opts.Effort != tc.effort {
				t.Fatalf("native model/effort=(%q,%q), want (%q,%q)", native.opts.Model, native.opts.Effort, tc.model, tc.effort)
			}
			if native.opts.Role != tc.role {
				t.Fatalf("native role=%q, want registry-derived %q", native.opts.Role, tc.role)
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
	routing := config.DefaultRoutingConfig()
	for _, p := range []types.Purpose{
		types.PurposeIntentSummarization,
		types.PurposePRComposition,
		types.PurposeIntentDisambiguation,
		types.PurposeTestEvidence,
	} {
		if _, err := routing.ResolveRoute(p); err != nil {
			t.Errorf("purpose %q has no route: %v", p, err)
		}
	}
}

func TestRoutingInvokerPropagatesEveryRegisteredPurposeRoleToNativeLaunch(t *testing.T) {
	for _, definition := range types.AllPurposeDefinitions() {
		t.Run(string(definition.Purpose), func(t *testing.T) {
			database, _, run, _ := setupTest(t)
			scope := reservedReviewScope(t, database, run)
			native := &recordingRoutedAgent{result: &agent.Result{}}
			ri := newRoutingInvoker(config.DefaultRoutingConfig(), database, newProviderCircuits())
			ri.newAgent = func(types.AgentName, string) (agent.Agent, error) {
				return native, nil
			}

			_, err := ri.Invoke(context.Background(), agent.InvocationRequest{
				Purpose: definition.Purpose,
				Scope:   scope,
				Payload: agent.RunOpts{Prompt: "exercise role propagation"},
			})
			if err != nil {
				t.Fatalf("Invoke: %v", err)
			}
			if native.calls != 1 {
				t.Fatalf("native agent calls = %d, want 1", native.calls)
			}
			if native.opts.Role != definition.Role {
				t.Fatalf("native role = %q, want registry-derived %q", native.opts.Role, definition.Role)
			}
		})
	}
}

type routedCandidateState struct {
	head                 string
	headRef              string
	indexTree            string
	refs                 string
	status               string
	trackedContent       string
	untrackedContent     string
	untrackedMode        os.FileMode
	untrackedLink        string
	symlinkParentTarget  string
	ignoredContent       string
	ignoredMode          os.FileMode
	directoryModes       map[string]os.FileMode
	directoryFileContent map[string]string
}

func seedRoutedCandidateState(t *testing.T) (string, routedCandidateState) {
	t.Helper()
	dir := t.TempDir()
	initGitRepo(t, dir)
	if err := os.Chmod(dir, 0o751); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, dir, ".gitignore", "*.ignored\n")
	if err := os.Mkdir(filepath.Join(dir, "tracked-dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, dir, "tracked-dir/value.txt", "tracked directory\n")
	execGit(t, dir, "add", ".gitignore", "tracked-dir/value.txt")
	execGit(t, dir, "commit", "-m", "seed candidate topology")
	execGit(t, dir, "branch", "auxiliary")
	execGit(t, dir, "tag", "candidate-baseline")
	execGit(t, dir, "symbolic-ref", "refs/no-mistakes/test-symbolic", "refs/heads/auxiliary")
	if err := os.Chmod(filepath.Join(dir, "tracked-dir"), 0o711); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, dir, "README.md", "legitimate staged\n")
	execGit(t, dir, "add", "README.md")
	writeTestFile(t, dir, "README.md", "legitimate unstaged\n")

	untracked := filepath.Join(dir, "legitimate-untracked.sh")
	if err := os.WriteFile(untracked, []byte("#!/bin/sh\necho legitimate\n"), 0o751); err != nil {
		t.Fatal(err)
	}
	untrackedTree := filepath.Join(dir, "legitimate-tree")
	untrackedNested := filepath.Join(untrackedTree, "nested")
	untrackedEmpty := filepath.Join(untrackedNested, "empty")
	if err := os.MkdirAll(untrackedEmpty, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, dir, "legitimate-tree/nested/value.txt", "legitimate untracked tree\n")
	for path, mode := range map[string]os.FileMode{
		untrackedTree:   0o751,
		untrackedNested: 0o711,
		untrackedEmpty:  0o701,
	} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}

	ignored := filepath.Join(dir, "legitimate.ignored")
	if err := os.WriteFile(ignored, []byte("legitimate ignored\n"), 0o741); err != nil {
		t.Fatal(err)
	}
	ignoredTree := filepath.Join(dir, "legitimate-cache.ignored")
	ignoredNested := filepath.Join(ignoredTree, "nested")
	ignoredEmpty := filepath.Join(ignoredNested, "empty")
	if err := os.MkdirAll(ignoredEmpty, 0o755); err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, dir, "legitimate-cache.ignored/nested/value.txt", "legitimate ignored tree\n")
	for path, mode := range map[string]os.FileMode{
		ignoredTree:   0o750,
		ignoredNested: 0o710,
		ignoredEmpty:  0o700,
	} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}

	linkTarget := ""
	symlinkParentTarget := ""
	if runtime.GOOS != "windows" {
		linkTarget = "legitimate-untracked.sh"
		if err := os.Symlink(linkTarget, filepath.Join(dir, "legitimate-link")); err != nil {
			t.Fatal(err)
		}
		symlinkParentTarget = "legitimate-tree/nested"
		if err := os.Symlink(symlinkParentTarget, filepath.Join(dir, "legitimate-parent-link")); err != nil {
			t.Fatal(err)
		}
	}
	info, err := os.Stat(untracked)
	if err != nil {
		t.Fatal(err)
	}
	ignoredInfo, err := os.Stat(ignored)
	if err != nil {
		t.Fatal(err)
	}
	return dir, routedCandidateState{
		head:                gitOut(t, dir, "rev-parse", "HEAD"),
		headRef:             gitOut(t, dir, "rev-parse", "--symbolic-full-name", "HEAD"),
		indexTree:           gitOut(t, dir, "write-tree"),
		status:              gitOut(t, dir, "status", "--porcelain=v1", "--untracked-files=all"),
		refs:                visibleRepairRefs(t, dir),
		trackedContent:      "legitimate unstaged\n",
		untrackedContent:    "#!/bin/sh\necho legitimate\n",
		untrackedMode:       info.Mode().Perm(),
		untrackedLink:       linkTarget,
		symlinkParentTarget: symlinkParentTarget,
		ignoredContent:      "legitimate ignored\n",
		ignoredMode:         ignoredInfo.Mode().Perm(),
		directoryModes: map[string]os.FileMode{
			".":                                     0o751,
			"tracked-dir":                           0o711,
			"legitimate-tree":                       0o751,
			"legitimate-tree/nested":                0o711,
			"legitimate-tree/nested/empty":          0o701,
			"legitimate-cache.ignored":              0o750,
			"legitimate-cache.ignored/nested":       0o710,
			"legitimate-cache.ignored/nested/empty": 0o700,
		},
		directoryFileContent: map[string]string{
			"legitimate-tree/nested/value.txt":          "legitimate untracked tree\n",
			"legitimate-cache.ignored/nested/value.txt": "legitimate ignored tree\n",
		},
	}
}

func assertRoutedCandidateState(t *testing.T, dir string, want routedCandidateState) {
	t.Helper()
	assertRoutedCandidateBase(t, dir, want)
	if got := gitOut(t, dir, "status", "--porcelain=v1", "--untracked-files=all"); got != want.status {
		t.Fatalf("status after failed attempt:\n%s\nwant:\n%s", got, want.status)
	}
}

func assertRoutedCandidateBase(t *testing.T, dir string, want routedCandidateState) {
	t.Helper()
	if got := gitOut(t, dir, "rev-parse", "HEAD"); got != want.head {
		t.Fatalf("HEAD = %s, want %s", got, want.head)
	}
	if got := gitOut(t, dir, "rev-parse", "--symbolic-full-name", "HEAD"); got != want.headRef {
		t.Fatalf("HEAD ref = %q, want %q", got, want.headRef)
	}
	if got := visibleRepairRefs(t, dir); got != want.refs {
		t.Fatalf("refs after attempt:\n%s\nwant:\n%s", got, want.refs)
	}
	if got := gitOut(t, dir, "write-tree"); got != want.indexTree {
		t.Fatalf("index tree = %s, want %s", got, want.indexTree)
	}
	tracked, err := os.ReadFile(filepath.Join(dir, "README.md"))
	if err != nil || string(tracked) != want.trackedContent {
		t.Fatalf("tracked content = %q, err = %v, want %q", tracked, err, want.trackedContent)
	}
	untrackedPath := filepath.Join(dir, "legitimate-untracked.sh")
	untracked, err := os.ReadFile(untrackedPath)
	if err != nil || string(untracked) != want.untrackedContent {
		t.Fatalf("untracked content = %q, err = %v, want %q", untracked, err, want.untrackedContent)
	}
	info, err := os.Stat(untrackedPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want.untrackedMode {
		t.Fatalf("untracked mode = %o, want %o", got, want.untrackedMode)
	}
	if want.untrackedLink != "" {
		target, err := os.Readlink(filepath.Join(dir, "legitimate-link"))
		if err != nil || target != want.untrackedLink {
			t.Fatalf("untracked symlink target = %q, err = %v, want %q", target, err, want.untrackedLink)
		}
	}
	if want.symlinkParentTarget != "" {
		target, err := os.Readlink(filepath.Join(dir, "legitimate-parent-link"))
		if err != nil || target != want.symlinkParentTarget {
			t.Fatalf("untracked symlink parent target = %q, err = %v, want %q", target, err, want.symlinkParentTarget)
		}
		throughParent, err := os.ReadFile(filepath.Join(dir, "legitimate-parent-link", "value.txt"))
		if err != nil || string(throughParent) != want.directoryFileContent["legitimate-tree/nested/value.txt"] {
			t.Fatalf("content through restored symlink parent = %q, err = %v", throughParent, err)
		}
	}
	ignoredPath := filepath.Join(dir, "legitimate.ignored")
	ignored, err := os.ReadFile(ignoredPath)
	if err != nil || string(ignored) != want.ignoredContent {
		t.Fatalf("ignored content = %q, err = %v, want %q", ignored, err, want.ignoredContent)
	}
	ignoredInfo, err := os.Stat(ignoredPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := ignoredInfo.Mode().Perm(); got != want.ignoredMode {
		t.Fatalf("ignored mode = %o, want %o", got, want.ignoredMode)
	}
	for path, wantMode := range want.directoryModes {
		info, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(path)))
		if err != nil {
			t.Fatalf("restored directory %q: %v", path, err)
		}
		if !info.IsDir() {
			t.Fatalf("restored path %q has type %s, want directory", path, info.Mode())
		}
		if got := info.Mode().Perm(); got != wantMode {
			t.Fatalf("restored directory %q mode = %o, want %o", path, got, wantMode)
		}
	}
	for path, wantContent := range want.directoryFileContent {
		content, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(path)))
		if err != nil || string(content) != wantContent {
			t.Fatalf("restored tree content %q = %q, err = %v, want %q", path, content, err, wantContent)
		}
	}
}

func mutateFailedRoutedCandidate(t *testing.T, dir, label string) {
	t.Helper()
	writeTestFile(t, dir, "README.md", label+" tracked mutation\n")
	writeTestFile(t, dir, label+"-staged.txt", label+" staged\n")
	writeTestFile(t, dir, label+"-untracked.txt", label+" untracked\n")
	if err := os.WriteFile(filepath.Join(dir, "legitimate-untracked.sh"), []byte(label+" corrupted\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, "legitimate-untracked.sh"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "legitimate.ignored"), []byte(label+" ignored mutation\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, "legitimate.ignored"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, "tracked-dir"), 0o777); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"legitimate-tree", "legitimate-cache.ignored"} {
		if err := os.RemoveAll(filepath.Join(dir, path)); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(dir, path), 0o777); err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, dir, path+"/replacement.txt", label+" replacement tree\n")
		if err := os.Chmod(filepath.Join(dir, path), 0o777); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{label + "-tree/deep", label + "-tree.ignored/deep"} {
		if err := os.MkdirAll(filepath.Join(dir, path), 0o755); err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, dir, path+"/file.txt", label+" attempt-created tree\n")
	}
	writeTestFile(t, dir, label+".ignored", label+" ignored mutation\n")
	if runtime.GOOS != "windows" {
		if err := os.Remove(filepath.Join(dir, "legitimate-link")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(label+"-untracked.txt", filepath.Join(dir, "legitimate-link")); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(dir, "legitimate-parent-link")); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(dir, "legitimate-parent-link"), 0o755); err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, dir, "legitimate-parent-link/value.txt", label+" replaced symlink parent\n")
	}
	execGit(t, dir, "add", "README.md", label+"-staged.txt", "legitimate-untracked.sh")
	execGit(t, dir, "commit", "-m", label+" failed attempt")
	execGit(t, dir, "checkout", "--detach", "HEAD")
	execGit(t, dir, "update-ref", "refs/heads/auxiliary", "HEAD")
	execGit(t, dir, "update-ref", "-d", "refs/tags/candidate-baseline")
	execGit(t, dir, "update-ref", "refs/heads/"+label+"-created", "HEAD")
	execGit(t, dir, "symbolic-ref", "refs/no-mistakes/test-symbolic", "refs/heads/"+label+"-created")
	writeTestFile(t, dir, label+"-after-commit.txt", label+" after commit\n")
}

func visibleRepairRefs(t *testing.T, dir string) string {
	t.Helper()
	lines := strings.Split(gitOut(t, dir, "for-each-ref", "--format=%(refname)%09%(objectname)%09%(symref)"), "\n")
	visible := lines[:0]
	for _, line := range lines {
		if !strings.HasPrefix(line, "refs/no-mistakes/repair-attempt-snapshots/") {
			visible = append(visible, line)
		}
	}
	return strings.Join(visible, "\n")
}

func conflictedRebaseWorktree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	initGitRepo(t, dir)
	baseBranch := gitOut(t, dir, "branch", "--show-current")
	execGit(t, dir, "checkout", "-b", "feature")
	writeTestFile(t, dir, "README.md", "feature change\n")
	execGit(t, dir, "add", "README.md")
	execGit(t, dir, "commit", "-m", "feature change")
	execGit(t, dir, "checkout", baseBranch)
	writeTestFile(t, dir, "README.md", "base change\n")
	execGit(t, dir, "add", "README.md")
	execGit(t, dir, "commit", "-m", "base change")
	execGit(t, dir, "checkout", "feature")
	rebase := exec.Command("git", "rebase", baseBranch)
	rebase.Dir = dir
	if out, err := rebase.CombinedOutput(); err == nil {
		t.Fatalf("rebase unexpectedly completed without a conflict:\n%s", out)
	}
	if got := gitOut(t, dir, "diff", "--name-only", "--diff-filter=U"); got != "README.md" {
		t.Fatalf("conflicted paths = %q, want README.md", got)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "info", "exclude"), []byte("rebase-cache/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for path, mode := range map[string]os.FileMode{
		"rebase-tree":               0o751,
		"rebase-tree/nested":        0o711,
		"rebase-tree/nested/empty":  0o701,
		"rebase-cache":              0o750,
		"rebase-cache/nested":       0o710,
		"rebase-cache/nested/empty": 0o700,
	} {
		if err := os.MkdirAll(filepath.Join(dir, filepath.FromSlash(path)), mode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(dir, filepath.FromSlash(path)), mode); err != nil {
			t.Fatal(err)
		}
	}
	writeTestFile(t, dir, "rebase-tree/nested/value.txt", "untracked rebase candidate\n")
	writeTestFile(t, dir, "rebase-cache/nested/value.txt", "ignored rebase candidate\n")
	return dir
}

func TestRoutingInvokerRestoresConflictedRebaseBeforeProviderFailover(t *testing.T) {
	database, _, run, _ := setupTest(t)
	scope := reservedReviewScope(t, database, run)
	dir := conflictedRebaseWorktree(t)

	codex := &recordingRoutedAgent{runFn: func(opts agent.RunOpts) (*agent.Result, error) {
		writeTestFile(t, opts.CWD, "README.md", "premature conflict resolution\n")
		execGit(t, opts.CWD, "add", "README.md")
		continueRebase := exec.Command("git", "rebase", "--continue")
		continueRebase.Dir = opts.CWD
		continueRebase.Env = append(os.Environ(), "GIT_EDITOR=true")
		if out, err := continueRebase.CombinedOutput(); err != nil {
			t.Fatalf("complete failed provider rebase: %v\n%s", err, out)
		}
		for _, path := range []string{"rebase-tree", "rebase-cache"} {
			if err := os.RemoveAll(filepath.Join(opts.CWD, path)); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.MkdirAll(filepath.Join(opts.CWD, "failed-rebase-tree.ignored", "deep"), 0o755); err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, opts.CWD, "failed-rebase-tree.ignored/deep/value.txt", "failed provider\n")
		return nil, opError()
	}}
	claude := &recordingRoutedAgent{runFn: func(opts agent.RunOpts) (*agent.Result, error) {
		content, err := os.ReadFile(filepath.Join(opts.CWD, "README.md"))
		if err != nil || !strings.Contains(string(content), "<<<<<<<") {
			t.Fatalf("backup provider received a mutated conflict: %q, err=%v", content, err)
		}
		if got := gitOut(t, opts.CWD, "diff", "--name-only", "--diff-filter=U"); got != "README.md" {
			t.Fatalf("backup provider conflict paths = %q, want README.md", got)
		}
		for path, wantMode := range map[string]os.FileMode{
			"rebase-tree":               0o751,
			"rebase-tree/nested":        0o711,
			"rebase-tree/nested/empty":  0o701,
			"rebase-cache":              0o750,
			"rebase-cache/nested":       0o710,
			"rebase-cache/nested/empty": 0o700,
		} {
			info, err := os.Lstat(filepath.Join(opts.CWD, filepath.FromSlash(path)))
			if err != nil || !info.IsDir() || info.Mode().Perm() != wantMode {
				t.Fatalf("restored conflicted directory %q = mode %v, err=%v; want directory %o", path, info, err, wantMode)
			}
		}
		for path, want := range map[string]string{
			"rebase-tree/nested/value.txt":  "untracked rebase candidate\n",
			"rebase-cache/nested/value.txt": "ignored rebase candidate\n",
		} {
			content, err := os.ReadFile(filepath.Join(opts.CWD, filepath.FromSlash(path)))
			if err != nil || string(content) != want {
				t.Fatalf("restored conflicted tree content %q = %q, err=%v; want %q", path, content, err, want)
			}
		}
		if _, err := os.Lstat(filepath.Join(opts.CWD, "failed-rebase-tree.ignored")); !os.IsNotExist(err) {
			t.Fatalf("attempt-created conflicted tree survived provider failover: %v", err)
		}
		return &agent.Result{Output: []byte(`{"summary":"retry from conflict"}`)}, nil
	}}
	ri := newRoutingInvoker(config.DefaultRoutingConfig(), database, newProviderCircuits())
	ri.newAgent = perRunner(codex, claude)

	if _, err := ri.Invoke(context.Background(), agent.InvocationRequest{
		Purpose: types.PurposeUnstructuredConflictRepair,
		Scope:   scope,
		Payload: agent.RunOpts{Prompt: "resolve conflict", CWD: dir},
	}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if codex.calls != 1 || claude.calls != 1 {
		t.Fatalf("provider calls = codex %d, claude %d; want one failed attempt and one clean failover", codex.calls, claude.calls)
	}
}

func TestRoutingInvokerRestoresFailedProviderBeforeBackupAndCommitsOnlySuccessfulMutation(t *testing.T) {
	database, _, run, _ := setupTest(t)
	scope := reservedReviewScope(t, database, run)
	dir, before := seedRoutedCandidateState(t)

	codex := &recordingRoutedAgent{runFn: func(opts agent.RunOpts) (*agent.Result, error) {
		mutateFailedRoutedCandidate(t, opts.CWD, "failed-codex")
		return nil, opError()
	}}
	claude := &recordingRoutedAgent{runFn: func(opts agent.RunOpts) (*agent.Result, error) {
		assertRoutedCandidateState(t, opts.CWD, before)
		writeTestFile(t, opts.CWD, "successful-claude.txt", "successful backup\n")
		return &agent.Result{Output: []byte(`{"summary":"successful backup"}`)}, nil
	}}
	ri := newRoutingInvoker(config.DefaultRoutingConfig(), database, newProviderCircuits())
	ri.newAgent = perRunner(codex, claude)

	if _, err := ri.Invoke(context.Background(), agent.InvocationRequest{
		Purpose: types.PurposeStructuredFindingRepair,
		Scope:   scope,
		Payload: agent.RunOpts{Prompt: "repair", CWD: dir},
	}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	assertRoutedCandidateBase(t, dir, before)
	if _, err := os.Stat(filepath.Join(dir, "successful-claude.txt")); err != nil {
		t.Fatalf("successful backup mutation missing: %v", err)
	}
	for _, failed := range []string{
		"failed-codex-staged.txt", "failed-codex-untracked.txt", "failed-codex-after-commit.txt",
		"failed-codex-tree", "failed-codex-tree.ignored",
	} {
		if _, err := os.Lstat(filepath.Join(dir, failed)); !os.IsNotExist(err) {
			t.Fatalf("failed provider mutation %q survived: %v", failed, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(dir, "failed-codex.ignored")); !os.IsNotExist(err) {
		t.Fatalf("failed provider ignored mutation survived: %v", err)
	}
	execGit(t, dir, "add", "-A")
	execGit(t, dir, "commit", "-m", "accepted repair")
	names := gitOut(t, dir, "show", "--format=", "--name-only", "HEAD")
	if !strings.Contains(names, "successful-claude.txt") || strings.Contains(names, "failed-codex") {
		t.Fatalf("accepted repair commit attribution = %q", names)
	}
}

func TestRoutingInvokerRestoresExactCandidateWhenEveryProviderFails(t *testing.T) {
	database, _, run, _ := setupTest(t)
	scope := reservedReviewScope(t, database, run)
	dir, before := seedRoutedCandidateState(t)

	codex := &recordingRoutedAgent{runFn: func(opts agent.RunOpts) (*agent.Result, error) {
		mutateFailedRoutedCandidate(t, opts.CWD, "failed-codex")
		return nil, opError()
	}}
	claude := &recordingRoutedAgent{runFn: func(opts agent.RunOpts) (*agent.Result, error) {
		assertRoutedCandidateState(t, opts.CWD, before)
		mutateFailedRoutedCandidate(t, opts.CWD, "failed-claude")
		return nil, opError()
	}}
	ri := newRoutingInvoker(config.DefaultRoutingConfig(), database, newProviderCircuits())
	ri.newAgent = perRunner(codex, claude)

	_, err := ri.Invoke(context.Background(), agent.InvocationRequest{
		Purpose: types.PurposeStructuredFindingRepair,
		Scope:   scope,
		Payload: agent.RunOpts{Prompt: "repair", CWD: dir},
	})
	var unavailable *agent.ProfileUnavailableError
	if !errors.As(err, &unavailable) {
		t.Fatalf("error = %v, want ProfileUnavailableError", err)
	}
	assertRoutedCandidateState(t, dir, before)
	for _, failed := range []string{
		"failed-codex-staged.txt", "failed-codex-untracked.txt", "failed-codex-after-commit.txt",
		"failed-codex.ignored", "failed-codex-tree", "failed-codex-tree.ignored",
		"failed-claude-staged.txt", "failed-claude-untracked.txt", "failed-claude-after-commit.txt",
		"failed-claude.ignored", "failed-claude-tree", "failed-claude-tree.ignored",
	} {
		if _, err := os.Lstat(filepath.Join(dir, failed)); !os.IsNotExist(err) {
			t.Fatalf("failed provider mutation %q survived final error: %v", failed, err)
		}
	}
}

func TestRoutingInvokerRestoresClaudeStructuredOutputRetryBeforeSuccess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX fake Claude executable")
	}
	database, _, run, _ := setupTest(t)
	scope := reservedReviewScope(t, database, run)
	dir, before := seedRoutedCandidateState(t)
	countPath := filepath.Join(t.TempDir(), "attempt-count")
	t.Setenv("NM_ATTEMPT_COUNT", countPath)
	script := filepath.Join(t.TempDir(), "claude")
	contents := `#!/bin/sh
count=0
if [ -f "$NM_ATTEMPT_COUNT" ]; then count=$(cat "$NM_ATTEMPT_COUNT"); fi
count=$((count + 1))
printf '%s' "$count" > "$NM_ATTEMPT_COUNT"
if [ "$count" -eq 1 ]; then
  printf '%s\n' 'failed structured retry' > "$PWD/failed-structured.txt"
  printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"session_id":"failed-session"}'
  exit 0
fi
if [ -e "$PWD/failed-structured.txt" ]; then
  printf '%s\n' '{"type":"result","subtype":"error","is_error":true,"result":"failed mutation survived"}'
  exit 0
fi
printf '%s\n' 'successful structured retry' > "$PWD/successful-structured.txt"
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"session_id":"successful-session","structured_output":{"summary":"successful retry"}}'
`
	if err := os.WriteFile(script, []byte(contents), 0o755); err != nil {
		t.Fatal(err)
	}
	routing := config.DefaultRoutingConfig()
	profile := routing.Profiles[config.ProfileFixFast]
	profile.Candidates = []config.Candidate{{Runner: types.RunnerClaude, Model: "claude-sonnet-5", Effort: types.EffortMedium}}
	routing.Profiles[config.ProfileFixFast] = profile
	runner := routing.Runners[types.RunnerClaude]
	runner.Executable = script
	routing.Runners[types.RunnerClaude] = runner
	ri := newRoutingInvoker(routing, database, newProviderCircuits())

	result, err := ri.Invoke(context.Background(), agent.InvocationRequest{
		Purpose: types.PurposeStructuredFindingRepair,
		Scope:   scope,
		Payload: agent.RunOpts{Prompt: "repair", CWD: dir, JSONSchema: commitSummarySchemaJSON},
	})
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if result == nil || string(result.Output) != `{"summary":"successful retry"}` {
		t.Fatalf("result = %+v", result)
	}
	assertRoutedCandidateBase(t, dir, before)
	if _, err := os.Lstat(filepath.Join(dir, "failed-structured.txt")); !os.IsNotExist(err) {
		t.Fatalf("failed structured-output mutation survived retry: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(dir, "successful-structured.txt")); err != nil || string(got) != "successful structured retry\n" {
		t.Fatalf("successful structured-output mutation = %q, err = %v", got, err)
	}
}

func TestRoutingInvokerRestoresFailedFixerAfterCallerCancellation(t *testing.T) {
	database, _, run, _ := setupTest(t)
	scope := reservedReviewScope(t, database, run)
	dir, before := seedRoutedCandidateState(t)
	ctx, cancel := context.WithCancel(context.Background())
	codex := &recordingRoutedAgent{runFn: func(opts agent.RunOpts) (*agent.Result, error) {
		mutateFailedRoutedCandidate(t, opts.CWD, "cancelled-codex")
		cancel()
		return nil, context.Canceled
	}}
	ri := newRoutingInvoker(config.DefaultRoutingConfig(), database, newProviderCircuits())
	ri.newAgent = perRunner(codex, &recordingRoutedAgent{})

	_, err := ri.Invoke(ctx, agent.InvocationRequest{
		Purpose: types.PurposeStructuredFindingRepair,
		Scope:   scope,
		Payload: agent.RunOpts{Prompt: "repair", CWD: dir},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context cancellation", err)
	}
	assertRoutedCandidateState(t, dir, before)
	for _, failed := range []string{"cancelled-codex-staged.txt", "cancelled-codex-untracked.txt", "cancelled-codex-after-commit.txt"} {
		if _, err := os.Lstat(filepath.Join(dir, failed)); !os.IsNotExist(err) {
			t.Fatalf("cancelled fixer mutation %q survived: %v", failed, err)
		}
	}
}

func TestRoutingInvokerCancellationBeatsSuccessfulFixerResult(t *testing.T) {
	database, _, run, _ := setupTest(t)
	scope := reservedReviewScope(t, database, run)
	dir, before := seedRoutedCandidateState(t)
	ctx, cancel := context.WithCancel(context.Background())
	codex := &recordingRoutedAgent{runFn: func(opts agent.RunOpts) (*agent.Result, error) {
		mutateFailedRoutedCandidate(t, opts.CWD, "cancelled-success")
		cancel()
		return &agent.Result{Output: []byte(`{"summary":"must not publish"}`)}, nil
	}}
	claude := &recordingRoutedAgent{}
	circuits := newProviderCircuits()
	ri := newRoutingInvoker(config.DefaultRoutingConfig(), database, circuits)
	ri.newAgent = perRunner(codex, claude)

	_, err := ri.Invoke(ctx, agent.InvocationRequest{
		Purpose: types.PurposeStructuredFindingRepair,
		Scope:   scope,
		Payload: agent.RunOpts{Prompt: "repair", CWD: dir},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context cancellation", err)
	}
	if codex.calls != 1 || claude.calls != 0 {
		t.Fatalf("provider calls = codex %d, claude %d; cancellation must not fail over", codex.calls, claude.calls)
	}
	assertRoutedCandidateState(t, dir, before)
	attempts, getErr := database.GetInvocationAttemptsByStepResult(scope.StepResultID)
	if getErr != nil || len(attempts) != 1 {
		t.Fatalf("attempts = %+v, err = %v; want one", attempts, getErr)
	}
	if terminal := attempts[0].Terminal; terminal == nil || terminal.Outcome != types.InvocationOutcomeCancelled || terminal.FailureDomain != "" {
		t.Fatalf("terminal = %+v, want cancelled without failure domain", terminal)
	}
	if circuits.isOpen(types.FailureDomainOpenAI) || circuits.isOpen(types.FailureDomainAnthropic) {
		t.Fatal("cancellation must not open a provider circuit")
	}
}

func TestRoutingInvokerCancellationBeatsOperationalFailure(t *testing.T) {
	database, _, run, _ := setupTest(t)
	scope := reservedReviewScope(t, database, run)
	ctx, cancel := context.WithCancel(context.Background())
	codex := &recordingRoutedAgent{runFn: func(agent.RunOpts) (*agent.Result, error) {
		cancel()
		return nil, opError()
	}}
	claude := &recordingRoutedAgent{}
	circuits := newProviderCircuits()
	ri := newRoutingInvoker(config.DefaultRoutingConfig(), database, circuits)
	ri.newAgent = perRunner(codex, claude)

	_, err := ri.Invoke(ctx, agent.InvocationRequest{
		Purpose: types.PurposeInitialReview,
		Scope:   scope,
		Payload: agent.RunOpts{Prompt: "review"},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context cancellation", err)
	}
	if codex.calls != 1 || claude.calls != 0 {
		t.Fatalf("provider calls = codex %d, claude %d; cancellation must not fail over", codex.calls, claude.calls)
	}
	attempts, getErr := database.GetInvocationAttemptsByStepResult(scope.StepResultID)
	if getErr != nil || len(attempts) != 1 {
		t.Fatalf("attempts = %+v, err = %v; want one", attempts, getErr)
	}
	if terminal := attempts[0].Terminal; terminal == nil || terminal.Outcome != types.InvocationOutcomeCancelled || terminal.FailureDomain != "" {
		t.Fatalf("terminal = %+v, want cancelled without failure domain", terminal)
	}
	if circuits.isOpen(types.FailureDomainOpenAI) || circuits.isOpen(types.FailureDomainAnthropic) {
		t.Fatal("cancellation must not open a provider circuit")
	}
}

func TestRoutingInvokerRestoreFailureDoesNotAuthorizeCircuitAfterRestart(t *testing.T) {
	database, _, run, _ := setupTest(t)
	scope := reservedReviewScope(t, database, run)
	isolation := &failingAttemptIsolation{err: errors.New("restore topology")}
	codex := &recordingRoutedAgent{err: opError()}
	claude := &recordingRoutedAgent{}
	circuits := newProviderCircuits()
	ri := newRoutingInvoker(config.DefaultRoutingConfig(), database, circuits)
	ri.newAgent = perRunner(codex, claude)

	_, err := ri.Invoke(context.Background(), agent.InvocationRequest{
		Purpose: types.PurposeStructuredFindingRepair,
		Scope:   scope,
		Payload: agent.RunOpts{Prompt: "repair", CWD: t.TempDir(), AttemptIsolation: isolation},
	})
	if err == nil || isolation.calls == 0 {
		t.Fatalf("error = %v, restore calls = %d; want fatal restore failure", err, isolation.calls)
	}
	if codex.calls != 1 || claude.calls != 0 {
		t.Fatalf("provider calls = codex %d, claude %d; failed restore must stop failover", codex.calls, claude.calls)
	}
	attempts, getErr := database.GetInvocationAttemptsByStepResult(scope.StepResultID)
	if getErr != nil || len(attempts) != 1 {
		t.Fatalf("attempts = %+v, err = %v; want one", attempts, getErr)
	}
	if terminal := attempts[0].Terminal; terminal == nil || terminal.Outcome != types.InvocationOutcomeFailed || terminal.FailureDomain != "" {
		t.Fatalf("terminal = %+v, want failed without circuit-authorizing domain", terminal)
	}
	restarted := providerCircuitsFromAttempts(attempts)
	if circuits.isOpen(types.FailureDomainOpenAI) || restarted.isOpen(types.FailureDomainOpenAI) {
		t.Fatal("failed restore must keep both live and reconstructed circuits closed")
	}
}

func TestRoutingInvokerRejectsSuccessfulFixerRefMutationAndRestores(t *testing.T) {
	database, _, run, _ := setupTest(t)
	scope := reservedReviewScope(t, database, run)
	dir, before := seedRoutedCandidateState(t)
	codex := &recordingRoutedAgent{runFn: func(opts agent.RunOpts) (*agent.Result, error) {
		writeTestFile(t, opts.CWD, "successful-but-invalid.txt", "invalid topology\n")
		execGit(t, opts.CWD, "add", "successful-but-invalid.txt")
		execGit(t, opts.CWD, "commit", "-m", "invalid fixer commit")
		execGit(t, opts.CWD, "update-ref", "refs/heads/auxiliary", "HEAD")
		execGit(t, opts.CWD, "update-ref", "refs/heads/created-by-fixer", "HEAD")
		return &agent.Result{Output: []byte(`{"summary":"invalid topology"}`)}, nil
	}}
	claude := &recordingRoutedAgent{}
	ri := newRoutingInvoker(config.DefaultRoutingConfig(), database, newProviderCircuits())
	ri.newAgent = perRunner(codex, claude)

	_, err := ri.Invoke(context.Background(), agent.InvocationRequest{
		Purpose: types.PurposeStructuredFindingRepair,
		Scope:   scope,
		Payload: agent.RunOpts{Prompt: "repair", CWD: dir},
	})
	if err == nil || !strings.Contains(err.Error(), "protected HEAD or ref topology") {
		t.Fatalf("error = %v, want protected topology rejection", err)
	}
	if codex.calls != 1 || claude.calls != 0 {
		t.Fatalf("provider calls = codex %d, claude %d; integrity failure must not fail over", codex.calls, claude.calls)
	}
	assertRoutedCandidateState(t, dir, before)
	if _, statErr := os.Lstat(filepath.Join(dir, "successful-but-invalid.txt")); !os.IsNotExist(statErr) {
		t.Fatalf("invalid successful mutation survived: %v", statErr)
	}
	attempts, getErr := database.GetInvocationAttemptsByStepResult(scope.StepResultID)
	if getErr != nil || len(attempts) != 1 {
		t.Fatalf("attempts = %+v, err = %v; want one", attempts, getErr)
	}
	if terminal := attempts[0].Terminal; terminal == nil || terminal.Outcome != types.InvocationOutcomeFailed || terminal.FailureDomain != "" {
		t.Fatalf("terminal = %+v, want non-operational integrity failure", terminal)
	}
}

func TestRoutingInvokerRestoresDetachedConflictedRebaseBeforeFailover(t *testing.T) {
	database, _, run, _ := setupTest(t)
	scope := reservedReviewScope(t, database, run)
	dir := t.TempDir()
	initGitRepo(t, dir)
	baseBranch := gitOut(t, dir, "branch", "--show-current")
	execGit(t, dir, "checkout", "-b", "detached-feature")
	writeTestFile(t, dir, "README.md", "detached feature\n")
	execGit(t, dir, "add", "README.md")
	execGit(t, dir, "commit", "-m", "detached feature")
	featureSHA := gitOut(t, dir, "rev-parse", "HEAD")
	execGit(t, dir, "checkout", baseBranch)
	writeTestFile(t, dir, "README.md", "new base\n")
	execGit(t, dir, "add", "README.md")
	execGit(t, dir, "commit", "-m", "new base")
	execGit(t, dir, "checkout", "--detach", featureSHA)
	rebase := exec.Command("git", "rebase", baseBranch)
	rebase.Dir = dir
	if out, err := rebase.CombinedOutput(); err == nil {
		t.Fatalf("detached rebase unexpectedly completed:\n%s", out)
	}

	codex := &recordingRoutedAgent{runFn: func(opts agent.RunOpts) (*agent.Result, error) {
		writeTestFile(t, opts.CWD, "README.md", "premature detached resolution\n")
		execGit(t, opts.CWD, "add", "README.md")
		continueRebase := exec.Command("git", "rebase", "--continue")
		continueRebase.Dir = opts.CWD
		continueRebase.Env = append(os.Environ(), "GIT_EDITOR=true")
		if out, err := continueRebase.CombinedOutput(); err != nil {
			t.Fatalf("complete failed detached rebase: %v\n%s", err, out)
		}
		return nil, opError()
	}}
	claude := &recordingRoutedAgent{runFn: func(opts agent.RunOpts) (*agent.Result, error) {
		if got := gitOut(t, opts.CWD, "rev-parse", "--symbolic-full-name", "HEAD"); got != "HEAD" {
			t.Fatalf("restored detached conflict HEAD ref = %q, want HEAD", got)
		}
		content, err := os.ReadFile(filepath.Join(opts.CWD, "README.md"))
		if err != nil || !strings.Contains(string(content), "<<<<<<<") {
			t.Fatalf("restored detached conflict content = %q, err = %v", content, err)
		}
		if got := gitOut(t, opts.CWD, "diff", "--name-only", "--diff-filter=U"); got != "README.md" {
			t.Fatalf("restored detached conflicts = %q, want README.md", got)
		}
		return &agent.Result{Output: []byte(`{"summary":"retry detached conflict"}`)}, nil
	}}
	ri := newRoutingInvoker(config.DefaultRoutingConfig(), database, newProviderCircuits())
	ri.newAgent = perRunner(codex, claude)
	if _, err := ri.Invoke(context.Background(), agent.InvocationRequest{
		Purpose: types.PurposeUnstructuredConflictRepair,
		Scope:   scope,
		Payload: agent.RunOpts{Prompt: "resolve detached conflict", CWD: dir},
	}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if codex.calls != 1 || claude.calls != 1 {
		t.Fatalf("provider calls = codex %d, claude %d; want detached failover", codex.calls, claude.calls)
	}
}

func TestRunSessionsDoesNotColdRetryFatalRoutedTransaction(t *testing.T) {
	database, _, run, _ := setupTest(t)
	scope := reservedReviewScope(t, database, run)
	dir, before := seedRoutedCandidateState(t)
	journalErr := errors.New("terminal unavailable")
	codex := &recordingRoutedAgent{runFn: func(opts agent.RunOpts) (*agent.Result, error) {
		mutateFailedRoutedCandidate(t, opts.CWD, "fatal-session")
		return nil, opError()
	}}
	ri := newRoutingInvoker(config.DefaultRoutingConfig(), failingFinishJournal{
		InvocationJournal: database,
		err:               journalErr,
	}, newProviderCircuits())
	ri.newAgent = perRunner(codex, &recordingRoutedAgent{})
	sessions := NewRunSessions(database, run.ID, nil, true)
	sessions.remember(SessionRoleFixer, "stored-session", string(types.AgentCodex))

	_, err := sessions.InvokeRequest(context.Background(), ri, SessionRoleFixer, agent.InvocationRequest{
		Purpose: types.PurposeStructuredFindingRepair,
		Scope:   scope,
		Payload: agent.RunOpts{Prompt: "repair", CWD: dir},
	}, nil)
	if !agent.IsFatalInvocationError(err) || !errors.Is(err, journalErr) {
		t.Fatalf("error = %v, want fatal terminal journal error", err)
	}
	if codex.calls != 1 {
		t.Fatalf("native calls = %d, want one without cold-session retry", codex.calls)
	}
	if stored := sessions.id(SessionRoleFixer); stored.ID != "stored-session" {
		t.Fatalf("stored session = %+v, want original identity retained", stored)
	}
	assertRoutedCandidateState(t, dir, before)
}

func TestRoutingInvokerRejectsSuccessfulFixerTrackedDirectoryModeMutation(t *testing.T) {
	database, _, run, _ := setupTest(t)
	scope := reservedReviewScope(t, database, run)
	dir, before := seedRoutedCandidateState(t)
	codex := &recordingRoutedAgent{runFn: func(opts agent.RunOpts) (*agent.Result, error) {
		if err := os.Chmod(filepath.Join(opts.CWD, "tracked-dir"), 0o777); err != nil {
			t.Fatal(err)
		}
		return &agent.Result{Output: []byte(`{"summary":"invalid directory mode"}`)}, nil
	}}
	claude := &recordingRoutedAgent{}
	ri := newRoutingInvoker(config.DefaultRoutingConfig(), database, newProviderCircuits())
	ri.newAgent = perRunner(codex, claude)

	_, err := ri.Invoke(context.Background(), agent.InvocationRequest{
		Purpose: types.PurposeStructuredFindingRepair,
		Scope:   scope,
		Payload: agent.RunOpts{Prompt: "repair", CWD: dir},
	})
	if err == nil || !strings.Contains(err.Error(), "protected directory mode") {
		t.Fatalf("error = %v, want protected directory mode rejection", err)
	}
	if codex.calls != 1 || claude.calls != 0 {
		t.Fatalf("provider calls = codex %d, claude %d; integrity failure must not fail over", codex.calls, claude.calls)
	}
	assertRoutedCandidateState(t, dir, before)
}

package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/routing"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// usageAgent is a minimal agent that reports token usage and echoes session
// starts, for perf-recording tests.
type usageAgent struct{ resumable bool }

func (u *usageAgent) Name() string                { return "usage-agent" }
func (u *usageAgent) Close() error                { return nil }
func (u *usageAgent) SupportsSessionResume() bool { return u.resumable }

func (u *usageAgent) Run(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
	result := &agent.Result{
		Output: json.RawMessage(`{}`),
		Model:  "test-model-1",
		Usage:  agent.TokenUsage{InputTokens: 100, OutputTokens: 20, CacheReadTokens: 60},
	}
	if opts.Session != nil {
		if opts.Session.ID != "" {
			result.SessionID = opts.Session.ID
			result.Resumed = true
		} else {
			result.SessionID = "sess-new"
		}
	}
	return result, nil
}

type fallbackUsageAgent struct {
	name   string
	result *agent.Result
	err    error
}

type routingCaptureAgent struct {
	seen []agent.RunOpts
}

func (a *routingCaptureAgent) Name() string { return "codex" }
func (a *routingCaptureAgent) Close() error { return nil }
func (a *routingCaptureAgent) Run(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
	a.seen = append(a.seen, opts)
	return &agent.Result{Text: "ok"}, nil
}

func TestPerfRecordingAgentRoutesSolOnlyForConfirmedReview(t *testing.T) {
	database, _, run, _ := setupTest(t)
	capture := &routingCaptureAgent{}
	wrapped := &perfRecordingAgent{
		inner: capture, db: database, runID: run.ID, stepName: types.StepReview,
		repository:          "https://github.com/RaFoyer/no-mistakes.git",
		sourceConfiguration: "cfg-fingerprint", configurationGeneration: "generation-1",
		risk:               func() routing.Risk { return routing.RiskHigh },
		reviewConfirmation: func() bool { return false },
		round:              func() int { return 1 },
	}
	if _, err := wrapped.Run(context.Background(), agent.RunOpts{Prompt: "review", Purpose: "review", RouteReviewConfirmation: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := wrapped.Run(context.Background(), agent.RunOpts{Prompt: "test", Purpose: "test", RouteReviewConfirmation: true}); err != nil {
		t.Fatal(err)
	}
	if len(capture.seen) != 2 {
		t.Fatalf("captured %d invocations", len(capture.seen))
	}
	if got := capture.seen[0].Routing.EffectiveModel; got != routing.ModelSol || capture.seen[0].Routing.EffectiveEffort != routing.EffortHigh {
		t.Fatalf("review route = %+v, want Sol/high", capture.seen[0].Routing)
	}
	if got := capture.seen[1].Routing.EffectiveModel; got != routing.ModelTerra || capture.seen[1].Routing.EffectiveEffort != routing.EffortHigh {
		t.Fatalf("test route = %+v, want Terra/high", capture.seen[1].Routing)
	}
	decisions, err := database.RouteDecisions(run.ID)
	if err != nil || len(decisions) != 2 {
		t.Fatalf("route decisions = %+v, err = %v", decisions, err)
	}
	if decisions[0].SourceConfiguration != "cfg-fingerprint" || decisions[0].ConfigurationGeneration != "generation-1" || decisions[0].PromptBytes == 0 {
		t.Fatalf("route evidence = %+v", decisions[0])
	}
}

func TestExecutorReviewRiskDowngradeRevokesLiveSolConfirmation(t *testing.T) {
	database, p, run, repo := setupTest(t)
	capture := &routingCaptureAgent{}
	callCount := 0
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			callCount++
			priorRisk := routeRiskFromFindings(sctx.PreviousFindings)
			if _, err := sctx.Agent.Run(sctx.Ctx, agent.RunOpts{
				Prompt:                  "review",
				Purpose:                 "review",
				RouteRisk:               priorRisk,
				RouteReviewConfirmation: sctx.Fixing && priorRisk == routing.RiskHigh,
			}); err != nil {
				return nil, err
			}

			risk := "high"
			if callCount >= 3 {
				risk = "low"
			}
			needsApproval := callCount < 4
			findings := `{"findings":[],"risk_level":"` + risk + `","risk_rationale":"route test","risk_scope":"source-or-external"}`
			if needsApproval {
				findings = `{"findings":[{"id":"route-finding","severity":"warning","description":"continue route test","action":"ask-user"}],"risk_level":"` + risk + `","risk_rationale":"route test","risk_scope":"source-or-external"}`
			}
			return &StepOutcome{NeedsApproval: needsApproval, Findings: findings}, nil
		},
	}
	exec := NewExecutor(database, p, &config.Config{}, capture, []Step{step}, nil)
	done := make(chan error, 1)
	go func() { done <- exec.Execute(context.Background(), run, repo, t.TempDir()) }()

	for wantResults := 1; wantResults <= 3; wantResults++ {
		deadline := time.Now().Add(5 * time.Second)
		for {
			results, err := database.RouteResults(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(results) >= wantResults {
				if err := exec.Respond(types.StepReview, types.ActionFix, nil); err == nil {
					break
				}
			}
			if time.Now().After(deadline) {
				t.Fatalf("review round %d never reached its approval gate", wantResults)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor timed out")
	}
	if len(capture.seen) != 4 {
		t.Fatalf("review routes = %d, want four rounds", len(capture.seen))
	}
	if got := capture.seen[3].Routing; got.Phase != "review" || got.EffectiveModel != routing.ModelLuna || got.EffectiveEffort != routing.EffortXHigh {
		t.Fatalf("post-downgrade review route = %+v, want ordinary Luna/xhigh review", got)
	}
}

func (a *fallbackUsageAgent) Name() string { return a.name }

func (a *fallbackUsageAgent) Run(context.Context, agent.RunOpts) (*agent.Result, error) {
	return a.result, a.err
}

func (a *fallbackUsageAgent) Close() error { return nil }

// TestExecutor_RecordsAgentInvocationsLocally proves every agent invocation
// produces one local agent_invocations row carrying run/step identity,
// purpose, round, session mode, model, timing, and token usage - and that
// the raw session id never lands in the telemetry row.
func TestExecutor_RecordsAgentInvocationsLocally(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			if _, err := sctx.RunAgentSession(SessionRoleReviewer, agent.RunOpts{Prompt: "review", Purpose: "review"}); err != nil {
				return nil, err
			}
			if _, err := sctx.Agent.Run(sctx.Ctx, agent.RunOpts{Prompt: "evidence"}); err != nil {
				return nil, err
			}
			return &StepOutcome{}, nil
		},
	}

	cfg := &config.Config{Agent: types.AgentClaude, SessionReuse: true}
	exec := NewExecutor(database, p, cfg, &usageAgent{resumable: true}, []Step{step}, nil)
	if err := exec.Execute(context.Background(), run, repo, workDir); err != nil {
		t.Fatalf("execute: %v", err)
	}

	invocations, err := database.GetAgentInvocationsByRun(run.ID)
	if err != nil {
		t.Fatalf("get invocations: %v", err)
	}
	if len(invocations) != 2 {
		t.Fatalf("got %d invocation rows, want 2", len(invocations))
	}

	review := invocations[0]
	if review.Purpose != "review" || review.StepName != "review" || review.Round != 1 {
		t.Fatalf("review row = %+v", review)
	}
	if review.SessionMode != db.InvocationModeStarted {
		t.Fatalf("review session mode = %q, want started", review.SessionMode)
	}
	if review.SessionKey == "" || review.SessionKey == "sess-new" {
		t.Fatalf("session key must be a fingerprint, not empty or the raw id: %q", review.SessionKey)
	}
	if review.Agent != "usage-agent" || review.Model != "test-model-1" {
		t.Fatalf("agent/model = %q/%q", review.Agent, review.Model)
	}
	if review.InputTokens != 100 || review.OutputTokens != 20 || review.CacheReadTokens != 60 {
		t.Fatalf("token usage not recorded: %+v", review)
	}
	if review.ExitStatus != "ok" || review.StartedAt == 0 || review.CompletedAt == 0 {
		t.Fatalf("timing/exit not recorded: %+v", review)
	}

	// The second invocation ran outside any session and defaults its purpose
	// to the step name.
	evidence := invocations[1]
	if evidence.SessionMode != db.InvocationModeCold || evidence.Purpose != "review" {
		t.Fatalf("evidence row = %+v", evidence)
	}
}

func TestPerfRecordingAgent_RecordsFallbackAttemptsSeparately(t *testing.T) {
	database, _, run, _ := setupTest(t)
	wrapped := &perfRecordingAgent{
		inner: agent.NewFallback([]agent.Agent{
			&fallbackUsageAgent{name: "codex", err: errors.New("codex start: executable not found")},
			&fallbackUsageAgent{name: "claude", result: &agent.Result{Model: "test-model-2"}},
		}),
		db:       database,
		runID:    run.ID,
		stepName: types.StepReview,
		round:    func() int { return 1 },
	}

	if _, err := wrapped.Run(context.Background(), agent.RunOpts{Purpose: "review"}); err == nil {
		t.Fatal("Codex policy must fail closed instead of falling back to Claude")
	}
	invocations, err := database.GetAgentInvocationsByRun(run.ID)
	if err != nil {
		t.Fatalf("get invocations: %v", err)
	}
	if len(invocations) != 1 {
		t.Fatalf("got %d invocation rows, want one attempted Codex route", len(invocations))
	}
	byAgent := map[string]db.AgentInvocation{}
	for _, invocation := range invocations {
		byAgent[invocation.Agent] = invocation
	}
	if got := byAgent["codex"]; got.ExitStatus != "error" || got.FailureCategory != "spawn" {
		t.Fatalf("codex invocation = %+v", got)
	}
}

func TestPerfRecordingAgent_MixedFallbackRecordsActualProviderCold(t *testing.T) {
	database, _, run, _ := setupTest(t)
	wrapped := &perfRecordingAgent{
		inner: agent.NewFallback([]agent.Agent{
			&fallbackUsageAgent{name: "pi", result: &agent.Result{Model: "pi-model"}},
			&usageAgent{resumable: true},
		}),
		db:       database,
		runID:    run.ID,
		stepName: types.StepReview,
		round:    func() int { return 1 },
	}

	sessions := NewRunSessions(database, run.ID, wrapped, true)
	if _, err := sessions.Run(context.Background(), wrapped, SessionRoleReviewer, agent.RunOpts{Purpose: "review"}, nil); err != nil {
		t.Fatalf("run session: %v", err)
	}

	invocations, err := database.GetAgentInvocationsByRun(run.ID)
	if err != nil {
		t.Fatalf("get invocations: %v", err)
	}
	if len(invocations) != 1 {
		t.Fatalf("got %d invocation rows, want 1", len(invocations))
	}
	if got := invocations[0]; got.Agent != "pi" || got.SessionMode != db.InvocationModeCold {
		t.Fatalf("invocation = %+v, want pi cold", got)
	}
}

func TestPerfRecordingAgent_MixedFallbackAttributesEveryConcreteAdapter(t *testing.T) {
	database, _, run, _ := setupTest(t)
	wrapped := &perfRecordingAgent{
		inner: agent.NewFallback([]agent.Agent{
			&fallbackUsageAgent{name: "pi", err: errors.New("pi start: executable not found")},
			&fallbackUsageAgent{name: "claude", result: &agent.Result{Model: "claude-sonnet"}},
		}),
		db: database, runID: run.ID, stepName: types.StepReview,
		round: func() int { return 1 },
	}
	if _, err := wrapped.Run(context.Background(), agent.RunOpts{Purpose: "review"}); err != nil {
		t.Fatalf("fallback run: %v", err)
	}
	routes, err := database.RouteDecisions(run.ID)
	if err != nil || len(routes) != 2 {
		t.Fatalf("routes = %+v, err = %v", routes, err)
	}
	if routes[0].EffectiveHarness != "pi" || routes[1].EffectiveHarness != "claude" {
		t.Fatalf("route providers = %q/%q", routes[0].EffectiveHarness, routes[1].EffectiveHarness)
	}
	invs, err := database.GetAgentInvocationsByRun(run.ID)
	if err != nil || len(invs) != 2 {
		t.Fatalf("invocations = %+v, err = %v", invs, err)
	}
	if invs[0].Agent != "pi" || invs[0].EffectiveHarness != "pi" || invs[0].ExitStatus != "error" {
		t.Fatalf("failed Pi evidence = %+v", invs[0])
	}
	if invs[1].Agent != "claude" || invs[1].EffectiveHarness != "claude" || invs[1].EffectiveModel != "" {
		t.Fatalf("successful Claude evidence = %+v", invs[1])
	}
}

func TestPerfRecordingAgentDirectNonCodexRiskEvidenceIsTruthful(t *testing.T) {
	adapters := []string{"claude", "pi", "grok", "opencode", "acp:copilot"}
	for _, adapter := range adapters {
		adapter := adapter
		t.Run(adapter, func(t *testing.T) {
			for _, tc := range []struct {
				name      string
				step      types.StepName
				risk      routing.Risk
				confirmed bool
				purpose   string
			}{
				{"medium", types.StepTest, routing.RiskMedium, false, "test"},
				{"high", types.StepTest, routing.RiskHigh, false, "test"},
				{"confirmation", types.StepReview, routing.RiskHigh, true, "review"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					database, _, run, _ := setupTest(t)
					wrapped := &perfRecordingAgent{
						inner: &fallbackUsageAgent{name: adapter, result: &agent.Result{Model: "provider-model"}},
						db:    database, runID: run.ID, stepName: tc.step,
						risk:               func() routing.Risk { return tc.risk },
						reviewConfirmation: func() bool { return tc.confirmed },
						round:              func() int { return 1 },
					}
					if _, err := wrapped.Run(context.Background(), agent.RunOpts{Purpose: tc.purpose}); err != nil {
						t.Fatal(err)
					}
					invs, err := database.GetAgentInvocationsByRun(run.ID)
					if err != nil || len(invs) != 1 {
						t.Fatalf("invs = %+v, err = %v", invs, err)
					}
					if invs[0].EffectiveHarness != adapter || invs[0].EffectiveModel != "" || invs[0].EffectiveEffort != "" {
						t.Fatalf("invocation evidence = %+v", invs[0])
					}
				})
			}
		})
	}
}

func TestPerfRecordingAgentClearsUnenforceableGPTEvidence(t *testing.T) {
	database, _, run, _ := setupTest(t)
	wrapped := &perfRecordingAgent{
		inner:    &fallbackUsageAgent{name: "claude", result: &agent.Result{Model: "claude-sonnet"}},
		db:       database,
		runID:    run.ID,
		stepName: types.StepReview,
		round:    func() int { return 1 },
	}
	_, err := wrapped.Run(context.Background(), agent.RunOpts{
		Purpose: "review",
		Routing: routing.Decision{RequestedHarness: "claude", EffectiveHarness: "codex", EffectiveModel: routing.ModelSol, EffectiveEffort: routing.EffortHigh, PolicyVersion: routing.PolicyVersion},
	})
	if err != nil {
		t.Fatal(err)
	}
	invs, err := database.GetAgentInvocationsByRun(run.ID)
	if err != nil || len(invs) != 1 {
		t.Fatalf("invocations = %+v, err=%v", invs, err)
	}
	if invs[0].EffectiveHarness != "claude" || invs[0].EffectiveModel != "" || invs[0].EffectiveEffort != "" {
		t.Fatalf("unenforceable route evidence = %+v", invs[0])
	}
}

func TestPerfRecordingAgentFallbackRefusesUnenforceableGPTRoute(t *testing.T) {
	database, _, run, _ := setupTest(t)
	wrapped := &perfRecordingAgent{
		inner: agent.NewFallback([]agent.Agent{
			&fallbackUsageAgent{name: "pi", err: errors.New("pi start: executable not found")},
			&fallbackUsageAgent{name: "claude", result: &agent.Result{Model: "claude-sonnet"}},
		}),
		db: database, runID: run.ID, stepName: types.StepReview,
		round: func() int { return 1 },
	}
	_, err := wrapped.Run(context.Background(), agent.RunOpts{
		Purpose: "review",
		Routing: routing.Decision{
			RequestedHarness: "codex", EffectiveHarness: "codex",
			EffectiveModel: routing.ModelSol, EffectiveEffort: routing.EffortHigh,
			PolicyVersion: routing.PolicyVersion, Phase: "review-confirmation", Risk: routing.RiskHigh,
		},
	})
	if err == nil || !strings.Contains(err.Error(), "cannot enforce") {
		t.Fatalf("fallback error = %v, want fail-closed Codex policy error", err)
	}
	invs, err := database.GetAgentInvocationsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(invs) != 0 {
		t.Fatalf("unenforceable fallback launched attempts: %+v", invs)
	}
}

// TestExecutor_AccumulatesParkedDuration proves a gate wait lands in the
// run's persisted parked total once the wait ends.
func TestExecutor_AccumulatesParkedDuration(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()

	step := newApprovalStep(types.StepReview, `{"findings":[{"severity":"warning","description":"x","action":"ask-user"}],"summary":"1"}`)
	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)

	done := make(chan error, 1)
	go func() {
		done <- exec.Execute(context.Background(), run, repo, workDir)
	}()

	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	time.Sleep(50 * time.Millisecond)
	if err := exec.Respond(types.StepReview, types.ActionApprove, nil); err != nil {
		t.Fatalf("respond: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("execute: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor timed out")
	}

	got, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.ParkedMS < 50 {
		t.Fatalf("ParkedMS = %d, want >= 50 (the gate wait)", got.ParkedMS)
	}
	if got.AwaitingAgentSince != nil {
		t.Fatal("awaiting marker must be clear after resume")
	}
}

package pipeline

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// cumulativeSessionAgent models codex: a stable durable session whose reported
// token usage is cumulative across resumed rounds, with bounded activity
// metrics and no cache-creation reporting.
type cumulativeSessionAgent struct {
	round     int
	cumInput  int
	cumOutput int
	cumCache  int
}

func (a *cumulativeSessionAgent) Name() string                { return "codex" }
func (a *cumulativeSessionAgent) Close() error                { return nil }
func (a *cumulativeSessionAgent) SupportsSessionResume() bool { return true }

func (a *cumulativeSessionAgent) Run(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
	a.round++
	// Round 1 adds 1000/100/600; round 2 adds 1500/150/1200 - so the cumulative
	// counters grow and the per-round deltas are the additions.
	switch a.round {
	case 1:
		a.cumInput, a.cumOutput, a.cumCache = 1000, 100, 600
	default:
		a.cumInput, a.cumOutput, a.cumCache = 2500, 250, 1800
	}
	return &agent.Result{
		Output:        json.RawMessage(`{"findings":[{"severity":"warning","description":"x","action":"auto-fix"},{"severity":"error","description":"y","action":"ask-user"}],"summary":"s"}`),
		SessionID:     "sess-abc",
		Resumed:       opts.Session != nil && opts.Session.ID != "",
		Model:         "gpt-5.6-sol",
		ModelProvider: "openai",
		Usage: agent.TokenUsage{
			InputTokens:       a.cumInput,
			OutputTokens:      a.cumOutput,
			CacheReadTokens:   a.cumCache,
			ReasoningTokens:   5 * a.round,
			ReasoningReported: true,
		},
		UsageReported: true,
		Metrics: &agent.InvocationMetrics{
			ModelRoundtrips:  4,
			ToolCalls:        3,
			ToolCategories:   agent.ToolCategoryCounts{TestLint: 1, Edit: 1, Read: 1},
			SubprocessWaitMS: 1200,
		},
		SessionUsageCumulative: true,
		CacheCreationReported:  false,
	}, nil
}

// usageGapSessionAgent models a codex thread whose middle round dies before
// any usage event, so that round's row stores unknown token counts.
type usageGapSessionAgent struct{ round int }

func (a *usageGapSessionAgent) Name() string                { return "codex" }
func (a *usageGapSessionAgent) Close() error                { return nil }
func (a *usageGapSessionAgent) SupportsSessionResume() bool { return true }

func (a *usageGapSessionAgent) Run(_ context.Context, _ agent.RunOpts) (*agent.Result, error) {
	a.round++
	if a.round == 2 {
		return nil, errors.New("codex exited: status 1")
	}
	cumulative := 1000
	if a.round == 3 {
		cumulative = 4000
	}
	return &agent.Result{
		Output:                 json.RawMessage(`{}`),
		SessionID:              "sess-gap",
		Resumed:                true,
		Usage:                  agent.TokenUsage{InputTokens: cumulative, Reported: true},
		UsageReported:          true,
		SessionUsageCumulative: true,
	}, nil
}

// TestPerfRecording_UsagelessRoundDoesNotResetTheSessionPrior proves the prior
// cumulative lookup skips rows whose usage is unknown. A failed round now
// writes such a row; treating it as the prior would report "no prior", and the
// next round's whole cumulative counter would be recorded as one round's
// delta - here 4000 instead of 3000.
func TestPerfRecording_UsagelessRoundDoesNotResetTheSessionPrior(t *testing.T) {
	database, _, run, _ := setupTest(t)

	roundNum := 0
	wrapped := &perfRecordingAgent{
		inner:    &usageGapSessionAgent{},
		db:       database,
		runID:    run.ID,
		stepName: types.StepReview,
		round:    func() int { return roundNum },
	}
	for r := 1; r <= 3; r++ {
		roundNum = r
		_, _ = wrapped.Run(context.Background(), agent.RunOpts{
			Purpose: "review",
			Session: &agent.SessionRef{ID: "sess-gap"},
		})
	}

	invs, err := database.GetAgentInvocationsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(invs) != 3 {
		t.Fatalf("got %d rows, want 3", len(invs))
	}
	if invs[0].SessionKey == "" || invs[0].SessionKey != invs[1].SessionKey || invs[1].SessionKey != invs[2].SessionKey {
		t.Fatalf("all three rounds must share one session key: %q/%q/%q", invs[0].SessionKey, invs[1].SessionKey, invs[2].SessionKey)
	}
	assertPtr(t, "round 1 delta input", invs[0].DeltaInputTokens, 1000)
	if invs[1].InputTokens != nil || invs[1].DeltaInputTokens != nil {
		t.Fatalf("the failed round must record unknown usage, got raw=%v delta=%v", invs[1].InputTokens, invs[1].DeltaInputTokens)
	}
	assertPtr(t, "round 3 delta input", invs[2].DeltaInputTokens, 3000)
}

// TestPerfRecording_ResumedSessionRecordsPerRoundDeltas proves a resumed
// session's cumulative token counters are stored per round as correct deltas,
// with fresh input, reasoning, model identity, activity metrics, workload, and
// finding counts all populated, and cache creation left unknown.
func TestPerfRecording_ResumedSessionRecordsPerRoundDeltas(t *testing.T) {
	database, _, run, _ := setupTest(t)

	roundNum := 0
	base := &cumulativeSessionAgent{}
	wrapped := &perfRecordingAgent{
		inner:    base,
		db:       database,
		runID:    run.ID,
		stepName: types.StepReview,
		round:    func() int { return roundNum },
	}
	sessions := NewRunSessions(database, run.ID, wrapped, true)

	for r := 1; r <= 2; r++ {
		roundNum = r
		opts := agent.RunOpts{
			Purpose:  "review",
			Workload: &agent.InvocationWorkload{Files: 4, Lines: 120},
		}
		if _, err := sessions.Run(context.Background(), wrapped, SessionRoleReviewer, opts, nil); err != nil {
			t.Fatalf("round %d: %v", r, err)
		}
	}

	invs, err := database.GetAgentInvocationsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(invs) != 2 {
		t.Fatalf("got %d rows, want 2", len(invs))
	}
	r1, r2 := invs[0], invs[1]

	if r1.SessionMode != db.InvocationModeStarted || r2.SessionMode != db.InvocationModeResumed {
		t.Fatalf("session modes = %q/%q", r1.SessionMode, r2.SessionMode)
	}
	if r1.SessionKey == "" || r1.SessionKey != r2.SessionKey {
		t.Fatalf("session keys must match across rounds: %q/%q", r1.SessionKey, r2.SessionKey)
	}

	// Raw counters are cumulative.
	assertPtr(t, "r1 raw input", r1.InputTokens, 1000)
	assertPtr(t, "r2 raw input", r2.InputTokens, 2500)
	// Deltas are the per-round additions.
	assertPtr(t, "r1 delta input", r1.DeltaInputTokens, 1000)
	assertPtr(t, "r2 delta input", r2.DeltaInputTokens, 1500)
	assertPtr(t, "r2 delta output", r2.DeltaOutputTokens, 150)
	assertPtr(t, "r2 delta cache", r2.DeltaCacheReadTokens, 1200)
	// Fresh input = input - cache read (cumulative per row).
	assertPtr(t, "r1 fresh", r1.FreshInputTokens, 400)
	assertPtr(t, "r2 fresh", r2.FreshInputTokens, 700)
	// Reasoning + activity metrics.
	assertPtr(t, "r2 reasoning", r2.ReasoningTokens, 10)
	assertPtr(t, "r2 roundtrips", r2.ModelRoundtrips, 4)
	assertPtr(t, "r2 tool calls", r2.ToolCalls, 3)
	assertPtr(t, "r2 test/lint", r2.ToolTestLintCalls, 1)
	assertPtr64(t, "r2 subprocess wait", r2.SubprocessWaitMS, 1200)
	// Workload + findings.
	assertPtr(t, "r2 workload files", r2.WorkloadFiles, 4)
	assertPtr(t, "r2 workload lines", r2.WorkloadLines, 120)
	assertPtr(t, "r2 finding count", r2.FindingCount, 2)
	// Model identity.
	if r2.Model != "gpt-5.6-sol" || r2.ModelProvider == nil || *r2.ModelProvider != "openai" {
		t.Fatalf("model/provider = %q/%v", r2.Model, r2.ModelProvider)
	}
	// Cache creation is unknown (codex does not report it), not a fabricated 0.
	if r2.CacheCreationTokens != nil {
		t.Fatalf("cache creation must be unknown, got %v", *r2.CacheCreationTokens)
	}
}

func TestPerfRecording_ReasoningDoesNotRequireActivityMetrics(t *testing.T) {
	database, _, run, _ := setupTest(t)
	recorder := &perfRecordingAgent{db: database, runID: run.ID, stepName: types.StepReview}
	inv := &db.AgentInvocation{}
	recorder.recordResult(inv, "", &agent.Result{
		Usage: agent.TokenUsage{
			InputTokens:       21,
			OutputTokens:      8,
			ReasoningTokens:   3,
			ReasoningReported: true,
		},
		UsageReported: true,
	})
	assertPtr(t, "reasoning without activity metrics", inv.ReasoningTokens, 3)
	if inv.ModelRoundtrips != nil || inv.ToolCalls != nil || inv.SubprocessWaitMS != nil {
		t.Fatalf("missing activity metrics must stay unknown: %+v", inv)
	}
}

// resumeFailingAgent starts a session cold, then fails any resume with an
// exit-shaped error, then succeeds on the fresh fallback session.
type resumeFailingAgent struct{ calls int }

func (a *resumeFailingAgent) Name() string                { return "codex" }
func (a *resumeFailingAgent) Close() error                { return nil }
func (a *resumeFailingAgent) SupportsSessionResume() bool { return true }

func (a *resumeFailingAgent) Run(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
	a.calls++
	if opts.Session != nil && opts.Session.ID != "" && !opts.SessionFallback {
		return nil, errors.New("codex exited: status 1")
	}
	return &agent.Result{Output: json.RawMessage(`{}`), SessionID: "sess-xyz"}, nil
}

// TestPerfRecording_FallbackRecordsReason proves a failed resume records both
// the failed resume row and a fallback row carrying a classified reason.
func TestPerfRecording_FallbackRecordsReason(t *testing.T) {
	database, _, run, _ := setupTest(t)

	roundNum := 0
	wrapped := &perfRecordingAgent{
		inner:    &resumeFailingAgent{},
		db:       database,
		runID:    run.ID,
		stepName: types.StepReview,
		round:    func() int { return roundNum },
	}
	sessions := NewRunSessions(database, run.ID, wrapped, true)

	for r := 1; r <= 2; r++ {
		roundNum = r
		if _, err := sessions.Run(context.Background(), wrapped, SessionRoleFixer, agent.RunOpts{Purpose: "review-fix"}, nil); err != nil {
			t.Fatalf("round %d: %v", r, err)
		}
	}

	invs, err := database.GetAgentInvocationsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var fallback, failedResume *db.AgentInvocation
	for i := range invs {
		switch invs[i].SessionMode {
		case db.InvocationModeFallback:
			fallback = &invs[i]
		case db.InvocationModeResumed:
			if invs[i].ExitStatus == "error" {
				failedResume = &invs[i]
			}
		}
	}
	if failedResume == nil {
		t.Fatal("expected a failed resumed invocation row")
	}
	if fallback == nil {
		t.Fatal("expected a fallback invocation row")
	}
	if fallback.FallbackReason == nil || *fallback.FallbackReason != db.FallbackReasonExit {
		t.Fatalf("fallback reason = %v, want %q", fallback.FallbackReason, db.FallbackReasonExit)
	}
}

// TestPerfRecording_MissingProviderUsageIsUnknown proves an adapter that reports
// no usage or activity metrics records unknown (NULL) fields, not zeros.
func TestPerfRecording_MissingProviderUsageIsUnknown(t *testing.T) {
	database, _, run, _ := setupTest(t)

	wrapped := &perfRecordingAgent{
		inner:    &noUsageAgent{},
		db:       database,
		runID:    run.ID,
		stepName: types.StepTest,
		round:    func() int { return 1 },
	}
	if _, err := wrapped.Run(context.Background(), agent.RunOpts{Purpose: "test-evidence"}); err != nil {
		t.Fatalf("run: %v", err)
	}
	invs, err := database.GetAgentInvocationsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(invs) != 1 {
		t.Fatalf("got %d rows, want 1", len(invs))
	}
	inv := invs[0]
	for name, p := range map[string]*int{
		"input_tokens":     inv.InputTokens,
		"output_tokens":    inv.OutputTokens,
		"cache_read":       inv.CacheReadTokens,
		"model_roundtrips": inv.ModelRoundtrips,
		"tool_calls":       inv.ToolCalls,
		"cache_creation":   inv.CacheCreationTokens,
		"fresh_input":      inv.FreshInputTokens,
		"delta_input":      inv.DeltaInputTokens,
		"delta_output":     inv.DeltaOutputTokens,
		"delta_cache_read": inv.DeltaCacheReadTokens,
		"reasoning":        inv.ReasoningTokens,
		"finding_count":    inv.FindingCount,
		"workload_files":   inv.WorkloadFiles,
	} {
		if p != nil {
			t.Fatalf("%s must be unknown (nil) for a no-usage invocation, got %d", name, *p)
		}
	}
	if inv.SubprocessWaitMS != nil {
		t.Fatalf("subprocess wait must be unknown, got %d", *inv.SubprocessWaitMS)
	}
}

type noUsageAgent struct{}

func (noUsageAgent) Name() string { return "noop-agent" }
func (noUsageAgent) Close() error { return nil }
func (noUsageAgent) Run(context.Context, agent.RunOpts) (*agent.Result, error) {
	return &agent.Result{}, nil
}

type schemaRejectedUsageAgent struct{}

func (schemaRejectedUsageAgent) Name() string { return "pi" }
func (schemaRejectedUsageAgent) Close() error { return nil }
func (schemaRejectedUsageAgent) Run(context.Context, agent.RunOpts) (*agent.Result, error) {
	return &agent.Result{
		Usage: agent.TokenUsage{
			InputTokens:     553_000,
			OutputTokens:    12_000,
			CacheReadTokens: 400_000,
			Reported:        true,
		},
		UsageReported: true,
	}, errors.New("pi output parse: JSON output must be object")
}

type failedNoUsageAgent struct{ err error }

func (failedNoUsageAgent) Name() string { return "pi" }
func (failedNoUsageAgent) Close() error { return nil }
func (a failedNoUsageAgent) Run(context.Context, agent.RunOpts) (*agent.Result, error) {
	return nil, a.err
}

func TestPerfRecording_SchemaRejectedInvocationRecordsReportedUsage(t *testing.T) {
	inv := recordOneInvocation(t, &schemaRejectedUsageAgent{}, context.Background())
	if inv.ExitStatus != "error" || inv.FailureCategory != "parse" {
		t.Fatalf("exit = %s/%s, want error/parse", inv.ExitStatus, inv.FailureCategory)
	}
	assertPtr(t, "input", inv.InputTokens, 553_000)
	assertPtr(t, "output", inv.OutputTokens, 12_000)
	assertPtr(t, "cache read", inv.CacheReadTokens, 400_000)
	assertPtr(t, "fresh input", inv.FreshInputTokens, 153_000)
}

// TestPerfRecording_FailedResumedInvocationStaysResumed proves a resumed turn
// that reports usage and then fails is still recorded as a resume. Adapters set
// Resumed only after finalizing, so a failed result's Resumed=false is no
// evidence the session was silently replaced - recording it as a fallback would
// invent a fallback with no reason and skew the resume-health counts.
func TestPerfRecording_FailedResumedInvocationStaysResumed(t *testing.T) {
	database, _, run, _ := setupTest(t)
	wrapped := &perfRecordingAgent{
		inner:    &schemaRejectedUsageAgent{},
		db:       database,
		runID:    run.ID,
		stepName: types.StepReview,
		round:    func() int { return 2 },
	}
	_, _ = wrapped.Run(context.Background(), agent.RunOpts{
		Purpose: "review-fix",
		Session: &agent.SessionRef{ID: "sess-xyz"},
	})
	invs, err := database.GetAgentInvocationsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(invs) != 1 {
		t.Fatalf("got %d rows, want 1", len(invs))
	}
	if invs[0].SessionMode != db.InvocationModeResumed {
		t.Fatalf("session mode = %q, want %q", invs[0].SessionMode, db.InvocationModeResumed)
	}
	if invs[0].FallbackReason != nil {
		t.Fatalf("failed resume must not invent a fallback reason: %v", *invs[0].FallbackReason)
	}
}

type replacedSessionAgent struct{}

func (replacedSessionAgent) Name() string                { return "antigravity" }
func (replacedSessionAgent) Close() error                { return nil }
func (replacedSessionAgent) SupportsSessionResume() bool { return true }
func (replacedSessionAgent) Run(context.Context, agent.RunOpts) (*agent.Result, error) {
	return &agent.Result{
		SessionID:     "conversation-B",
		Usage:         agent.TokenUsage{InputTokens: 900, Reported: true},
		UsageReported: true,
	}, errors.New("antigravity output parse: JSON output must be object")
}

// TestPerfRecording_FailedTurnInADifferentSessionRecordsFallback proves the
// silent-replacement signal survives a failed turn. The adapter named a
// session other than the one requested, which is evidence the resume never
// happened rather than an unset field - recording it as a resume would hide
// the stale-session path the fallback bucket exists to expose.
func TestPerfRecording_FailedTurnInADifferentSessionRecordsFallback(t *testing.T) {
	database, _, run, _ := setupTest(t)
	wrapped := &perfRecordingAgent{
		inner:    replacedSessionAgent{},
		db:       database,
		runID:    run.ID,
		stepName: types.StepReview,
		round:    func() int { return 2 },
	}
	_, _ = wrapped.Run(context.Background(), agent.RunOpts{
		Purpose: "review",
		Session: &agent.SessionRef{ID: "conversation-A"},
	})

	invs, err := database.GetAgentInvocationsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(invs) != 1 {
		t.Fatalf("got %d rows, want 1", len(invs))
	}
	if invs[0].SessionMode != db.InvocationModeFallback {
		t.Fatalf("session mode = %q, want %q", invs[0].SessionMode, db.InvocationModeFallback)
	}
	assertPtr(t, "input", invs[0].InputTokens, 900)
}

func TestPerfRecording_FailedInvocationWithoutUsageIsUnknown(t *testing.T) {
	inv := recordOneInvocation(t, failedNoUsageAgent{err: errors.New("pi exited: status 1")}, context.Background())
	if inv.ExitStatus != "error" {
		t.Fatalf("exit = %s, want error", inv.ExitStatus)
	}
	assertUnknownRawTokens(t, inv)
}

func TestPerfRecording_CancelledInvocationWithoutUsageIsUnknown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	inv := recordOneInvocation(t, failedNoUsageAgent{err: context.Canceled}, ctx)
	if inv.ExitStatus != "cancelled" {
		t.Fatalf("exit = %s, want cancelled", inv.ExitStatus)
	}
	assertUnknownRawTokens(t, inv)
}

func TestPerfRecording_ReportedZeroTokensAreZeroNotUnknown(t *testing.T) {
	inv := recordOneInvocation(t, &zeroUsageAgent{}, context.Background())
	if inv.ExitStatus != "ok" {
		t.Fatalf("exit = %s, want ok", inv.ExitStatus)
	}
	assertPtr(t, "input", inv.InputTokens, 0)
	assertPtr(t, "output", inv.OutputTokens, 0)
	assertPtr(t, "cache read", inv.CacheReadTokens, 0)
}

type zeroUsageAgent struct{}

func (zeroUsageAgent) Name() string { return "pi" }
func (zeroUsageAgent) Close() error { return nil }
func (zeroUsageAgent) Run(context.Context, agent.RunOpts) (*agent.Result, error) {
	return &agent.Result{
		Usage:         agent.TokenUsage{Reported: true},
		UsageReported: true,
	}, nil
}

func recordOneInvocation(t *testing.T, inner agent.Agent, ctx context.Context) db.AgentInvocation {
	t.Helper()
	database, _, run, _ := setupTest(t)
	wrapped := &perfRecordingAgent{
		inner:    inner,
		db:       database,
		runID:    run.ID,
		stepName: types.StepReview,
		round:    func() int { return 1 },
	}
	_, _ = wrapped.Run(ctx, agent.RunOpts{Purpose: "review"})
	invs, err := database.GetAgentInvocationsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(invs) != 1 {
		t.Fatalf("got %d rows, want 1", len(invs))
	}
	return invs[0]
}

func assertUnknownRawTokens(t *testing.T, inv db.AgentInvocation) {
	t.Helper()
	if inv.InputTokens != nil || inv.OutputTokens != nil || inv.CacheReadTokens != nil ||
		inv.FreshInputTokens != nil || inv.DeltaInputTokens != nil {
		t.Fatalf("unreported usage must be unknown, got input=%v output=%v cache=%v fresh=%v delta=%v",
			inv.InputTokens, inv.OutputTokens, inv.CacheReadTokens, inv.FreshInputTokens, inv.DeltaInputTokens)
	}
}

func assertPtr(t *testing.T, name string, got *int, want int) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s = nil, want %d", name, want)
	}
	if *got != want {
		t.Fatalf("%s = %d, want %d", name, *got, want)
	}
}

func assertPtr64(t *testing.T, name string, got *int64, want int64) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s = nil, want %d", name, want)
	}
	if *got != want {
		t.Fatalf("%s = %d, want %d", name, *got, want)
	}
}

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
			InputTokens:     a.cumInput,
			OutputTokens:    a.cumOutput,
			CacheReadTokens: a.cumCache,
			ReasoningTokens: 5 * a.round,
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
	if r1.InputTokens != 1000 || r2.InputTokens != 2500 {
		t.Fatalf("raw input = %d/%d, want 1000/2500", r1.InputTokens, r2.InputTokens)
	}
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

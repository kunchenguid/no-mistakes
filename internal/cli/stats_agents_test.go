package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
)

// TestStatsAgentsReportsLocalPerformanceTelemetry proves the read-only
// report surface exposes the locally persisted invocation evidence: per-
// purpose aggregates via --agents and per-run detail (including accumulated
// parked time) via --run.
func TestStatsAgentsReportsLocalPerformanceTelemetry(t *testing.T) {
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)
	p := paths.WithRoot(nmHome)

	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	repo, err := d.InsertRepoWithID("repo-1", "/tmp/repo", "https://github.com/test/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature/x", "abc", "def")
	if err != nil {
		t.Fatal(err)
	}
	seed := []db.AgentInvocation{
		{RunID: run.ID, StepName: "review", Round: 1, Purpose: "review", Agent: "codex", Model: "gpt-5.2", SessionMode: db.InvocationModeStarted, SessionKey: "deadbeef00000000", StartedAt: 1, CompletedAt: 2, DurationMS: 60_000, ExitStatus: "ok", InputTokens: 100, OutputTokens: 10, CacheReadTokens: 40, CacheCreationTokens: statsIntPtr(20)},
		{RunID: run.ID, StepName: "review", Round: 2, Purpose: "review", Agent: "codex", Model: "gpt-5.2", SessionMode: db.InvocationModeResumed, SessionKey: "deadbeef00000000", StartedAt: 3, CompletedAt: 4, DurationMS: 30_000, ExitStatus: "ok", InputTokens: 50, OutputTokens: 5, CacheReadTokens: 45, CacheCreationTokens: statsIntPtr(25)},
		{RunID: run.ID, StepName: "review", Round: 2, Purpose: "review-fix", Agent: "codex", Model: "gpt-5.2", SessionMode: db.InvocationModeStarted, SessionKey: "feedface00000000", StartedAt: 5, CompletedAt: 6, DurationMS: 45_000, ExitStatus: "ok"},
	}
	for _, inv := range seed {
		if _, err := d.InsertAgentInvocation(inv); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.AddRunParkedDuration(run.ID, 90_000); err != nil {
		t.Fatal(err)
	}
	d.Close()

	out, err := executeCmd("stats", "--agents")
	if err != nil {
		t.Fatalf("stats --agents: %v\n%s", err, out)
	}
	for _, want := range []string{"PURPOSE", "review", "review-fix", "RESUMED", "CACHE WRITE TOK", "45"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stats --agents missing %q in:\n%s", want, out)
		}
	}

	out, err = executeCmd("stats", "--run", run.ID)
	if err != nil {
		t.Fatalf("stats --run: %v\n%s", err, out)
	}
	for _, want := range []string{run.ID, "parked at gates 1m30s total", "resumed", "deadbeef00000000", "gpt-5.2", "CACHE WR", "20"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stats --run missing %q in:\n%s", want, out)
		}
	}
	// The seeded rows carry no activity metrics, so those fields render as the
	// unknown marker, distinct from a recorded zero.
	if !strings.Contains(out, "-") {
		t.Fatalf("stats --run should render unknown metric fields as \"-\":\n%s", out)
	}
}

func statsIntPtr(v int) *int       { return &v }
func statsInt64Ptr(v int64) *int64 { return &v }
func statsBoolPtr(v bool) *bool    { return &v }

// TestStatsRendersPopulatedFidelityMetrics proves the report surfaces the new
// activity histogram, subprocess/model time split, and per-round token deltas
// when they are recorded.
func TestStatsRendersPopulatedFidelityMetrics(t *testing.T) {
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)
	p := paths.WithRoot(nmHome)

	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	repo, err := d.InsertRepoWithID("repo-1", "/tmp/repo", "https://github.com/test/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature/x", "abc", "def")
	if err != nil {
		t.Fatal(err)
	}
	inv := db.AgentInvocation{
		RunID: run.ID, StepName: "review", Round: 2, Purpose: "review-fix", Agent: "codex",
		Model: "gpt-5.6-sol", ModelProvider: strPtrCLI("openai"),
		SessionMode: db.InvocationModeResumed, SessionKey: "deadbeef00000000",
		StartedAt: 1, CompletedAt: 2, DurationMS: 10_000, SubprocessWaitMS: statsInt64Ptr(2_000),
		ExitStatus: "ok", InputTokens: 2500, OutputTokens: 250, CacheReadTokens: 1800,
		FreshInputTokens: statsIntPtr(700), ReasoningTokens: statsIntPtr(9),
		DeltaInputTokens: statsIntPtr(1500), DeltaOutputTokens: statsIntPtr(150), DeltaCacheReadTokens: statsIntPtr(1200),
		ModelRoundtrips: statsIntPtr(24), ToolCalls: statsIntPtr(7),
		ToolWaitCalls: statsIntPtr(0), ToolTestLintCalls: statsIntPtr(2), ToolEditCalls: statsIntPtr(3),
		ToolReadCalls: statsIntPtr(1), ToolGitCalls: statsIntPtr(1), ToolOtherCalls: statsIntPtr(0),
		ReviewPacketBytes: statsIntPtr(2048), ReviewHistoryBytes: statsIntPtr(512), ReviewPacketOmittedParts: statsIntPtr(1), ReviewPacketOversize: statsBoolPtr(true),
		ReviewHistoryOmittedRounds: statsIntPtr(2), ReviewHistoryOmittedFindings: statsIntPtr(5),
		WorkloadFiles: statsIntPtr(12), WorkloadLines: statsIntPtr(1060), FindingCount: statsIntPtr(3),
	}
	if _, err := d.InsertAgentInvocation(inv); err != nil {
		t.Fatal(err)
	}
	d.Close()

	out, err := executeCmd("stats", "--agents")
	if err != nil {
		t.Fatalf("stats --agents: %v\n%s", err, out)
	}
	for _, want := range []string{"ROUNDTRIPS", "TEST/LINT", "SUBPROC", "24", "METRICS", "1/1", "PACKET", "2048", "OVERSIZE"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stats --agents missing %q in:\n%s", want, out)
		}
	}

	out, err = executeCmd("stats", "--run", run.ID)
	if err != nil {
		t.Fatalf("stats --run: %v\n%s", err, out)
	}
	// Per-round delta (1500) is shown distinctly from the raw cumulative (2500),
	// the tool histogram and the workload render, and the model-time split appears.
	for _, want := range []string{"Δ IN (round)", "1500", "2500", "7 0/2/3/1/1/0", "12/1060", "MODEL", "PACKET B", "2048", "1/2/5", "true"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stats --run missing %q in:\n%s", want, out)
		}
	}
}

func TestStatsAgentsSinceGroupsPurposeAgentModelAndSession(t *testing.T) {
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)
	p := paths.WithRoot(nmHome)

	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	repo, err := d.InsertRepoWithID("repo-window", "/tmp/repo-window", "https://github.com/test/repo-window", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature/window", "abc", "def")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	seed := []db.AgentInvocation{
		{RunID: run.ID, StepName: "review", Round: 1, Purpose: "review", Agent: "codex", Model: "old-model", SessionMode: db.InvocationModeCold, StartedAt: now - 7200, CompletedAt: now - 7199, DurationMS: 10, ExitStatus: "ok", CacheReadTokens: 99},
		{RunID: run.ID, StepName: "review", Round: 1, Purpose: "review", Agent: "claude", Model: "opus", SessionMode: db.InvocationModeCold, StartedAt: now - 120, CompletedAt: now - 119, DurationMS: 20, ExitStatus: "ok", InputTokens: 1, OutputTokens: 2, CacheReadTokens: 3, CacheCreationTokens: statsIntPtr(4)},
		{RunID: run.ID, StepName: "review", Round: 2, Purpose: "review-fix", Agent: "claude", Model: "sonnet", SessionMode: db.InvocationModeResumed, StartedAt: now - 60, CompletedAt: now - 59, DurationMS: 30, ExitStatus: "ok", InputTokens: 5, OutputTokens: 6, CacheReadTokens: 7, CacheCreationTokens: statsIntPtr(8)},
	}
	for _, inv := range seed {
		if _, err := d.InsertAgentInvocation(inv); err != nil {
			t.Fatal(err)
		}
	}
	d.Close()

	out, err := executeCmd("stats", "--agents", "--since", "1h")
	if err != nil {
		t.Fatalf("stats --agents --since: %v\n%s", err, out)
	}
	for _, want := range []string{"PURPOSE", "AGENT", "MODEL", "SESSION", "review", "review-fix", "claude", "opus", "sonnet", "cold", "resumed"} {
		if !strings.Contains(out, want) {
			t.Fatalf("windowed route report missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "old-model") {
		t.Fatalf("windowed route report included old invocation:\n%s", out)
	}

	if _, err := executeCmd("stats", "--since", "1h"); err == nil || !strings.Contains(err.Error(), "requires --agents") {
		t.Fatalf("stats --since without --agents error = %v, want requires --agents", err)
	}
	if _, err := executeCmd("stats", "--agents", "--since", "0s"); err == nil || !strings.Contains(err.Error(), "must be positive") {
		t.Fatalf("stats --agents --since 0s error = %v, want positive duration", err)
	}
}

func strPtrCLI(s string) *string { return &s }

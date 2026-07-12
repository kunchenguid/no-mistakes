package db

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestAgentInvocations_InsertAndReadBack(t *testing.T) {
	d, _, run := openSessionTestDB(t)

	inv := AgentInvocation{
		RunID:               run.ID,
		StepName:            "review",
		Round:               2,
		Purpose:             "review-fix",
		Agent:               "codex",
		Model:               "gpt-5.2-codex",
		SessionMode:         InvocationModeResumed,
		SessionKey:          "abcd1234abcd1234",
		StartedAt:           1_700_000_000,
		CompletedAt:         1_700_000_090,
		DurationMS:          90_000,
		ExitStatus:          "ok",
		InputTokens:         1000,
		OutputTokens:        200,
		CacheReadTokens:     800,
		CacheCreationTokens: intPtr(50),
	}
	if _, err := d.InsertAgentInvocation(inv); err != nil {
		t.Fatalf("insert: %v", err)
	}

	got, err := d.GetAgentInvocationsByRun(run.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d invocations, want 1", len(got))
	}
	back := got[0]
	if back.Purpose != "review-fix" || back.Round != 2 || back.SessionMode != InvocationModeResumed ||
		back.DurationMS != 90_000 || back.InputTokens != 1000 || back.CacheReadTokens != 800 || back.Model != "gpt-5.2-codex" {
		t.Fatalf("readback mismatch: %+v", back)
	}
	if back.CacheCreationTokens == nil || *back.CacheCreationTokens != 50 {
		t.Fatalf("cache creation readback = %v, want 50", back.CacheCreationTokens)
	}
}

func intPtr(v int) *int { return &v }

// TestAgentInvocations_NullableFidelityFieldsRoundTrip proves the session-
// fidelity columns survive an insert/read cycle both when populated and when
// left unknown (NULL), so missing data reads back as nil rather than zero.
func TestAgentInvocations_NullableFidelityFieldsRoundTrip(t *testing.T) {
	d, _, run := openSessionTestDB(t)

	// Fully populated row.
	full := AgentInvocation{
		RunID: run.ID, StepName: "review", Round: 2, Purpose: "review", Agent: "codex",
		Model: "gpt-5.6-sol", ModelProvider: strPtr("openai"),
		SessionMode: InvocationModeFallback, SessionKey: "key1", FallbackReason: strPtr(FallbackReasonExit),
		StartedAt: 1, CompletedAt: 2, DurationMS: 5000, SubprocessWaitMS: int64Ptr(1200),
		ExitStatus: "ok", InputTokens: 2500, OutputTokens: 250, CacheReadTokens: 1800,
		CacheCreationTokens: intPtr(0), FreshInputTokens: intPtr(700), ReasoningTokens: intPtr(9),
		DeltaInputTokens: intPtr(1500), DeltaOutputTokens: intPtr(150), DeltaCacheReadTokens: intPtr(1200),
		ModelRoundtrips: intPtr(4), ToolCalls: intPtr(3),
		ToolWaitCalls: intPtr(0), ToolTestLintCalls: intPtr(1), ToolEditCalls: intPtr(1),
		ToolReadCalls: intPtr(1), ToolGitCalls: intPtr(0), ToolOtherCalls: intPtr(0),
		WorkloadFiles: intPtr(4), WorkloadLines: intPtr(120), FindingCount: intPtr(2),
	}
	if _, err := d.InsertAgentInvocation(full); err != nil {
		t.Fatalf("insert full: %v", err)
	}
	// Minimal row: every nullable field unknown.
	minimal := AgentInvocation{
		RunID: run.ID, StepName: "test", Round: 1, Purpose: "test-evidence", Agent: "codex",
		SessionMode: InvocationModeCold, StartedAt: 3, CompletedAt: 4, DurationMS: 10, ExitStatus: "ok",
	}
	if _, err := d.InsertAgentInvocation(minimal); err != nil {
		t.Fatalf("insert minimal: %v", err)
	}

	got, err := d.GetAgentInvocationsByRun(run.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d rows, want 2", len(got))
	}
	f := got[0]
	if f.ModelProvider == nil || *f.ModelProvider != "openai" ||
		f.FallbackReason == nil || *f.FallbackReason != FallbackReasonExit ||
		f.SubprocessWaitMS == nil || *f.SubprocessWaitMS != 1200 ||
		f.CacheCreationTokens == nil || *f.CacheCreationTokens != 0 ||
		f.DeltaInputTokens == nil || *f.DeltaInputTokens != 1500 ||
		f.ToolTestLintCalls == nil || *f.ToolTestLintCalls != 1 ||
		f.FindingCount == nil || *f.FindingCount != 2 {
		t.Fatalf("full row lost a fidelity field: %+v", f)
	}
	m := got[1]
	for name, isNil := range map[string]bool{
		"ModelProvider":    m.ModelProvider == nil,
		"FallbackReason":   m.FallbackReason == nil,
		"SubprocessWaitMS": m.SubprocessWaitMS == nil,
		"CacheCreation":    m.CacheCreationTokens == nil,
		"FreshInput":       m.FreshInputTokens == nil,
		"DeltaInput":       m.DeltaInputTokens == nil,
		"ModelRoundtrips":  m.ModelRoundtrips == nil,
		"ToolCalls":        m.ToolCalls == nil,
		"WorkloadFiles":    m.WorkloadFiles == nil,
		"FindingCount":     m.FindingCount == nil,
	} {
		if !isNil {
			t.Fatalf("minimal row %s should read back as unknown (nil)", name)
		}
	}
}

func strPtr(s string) *string { return &s }
func int64Ptr(v int64) *int64 { return &v }

// TestAgentInvocations_PrivacySafeShape guards the privacy boundary: the
// table has no column that could hold prompts, outputs, or diffs, and the
// session identity is stored only as a fingerprint column.
func TestAgentInvocations_PrivacySafeShape(t *testing.T) {
	d, _, _ := openSessionTestDB(t)

	rows, err := d.sql.Query(`SELECT name FROM pragma_table_info('agent_invocations')`)
	if err != nil {
		t.Fatalf("table info: %v", err)
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		columns = append(columns, name)
	}
	for _, col := range columns {
		lower := strings.ToLower(col)
		if strings.HasSuffix(lower, "_tokens") {
			continue // token counts are numeric usage data, not content
		}
		for _, forbidden := range []string{"prompt", "output", "diff", "transcript", "secret", "credential", "text", "content"} {
			if strings.Contains(lower, forbidden) {
				t.Fatalf("agent_invocations column %q could hold %s content", col, forbidden)
			}
		}
	}
}

func TestAgentInvocations_HasRunTimelineIndex(t *testing.T) {
	d, _, _ := openSessionTestDB(t)

	rows, err := d.sql.Query(`SELECT name FROM pragma_index_list('agent_invocations')`)
	if err != nil {
		t.Fatalf("index list: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		if name == "idx_agent_invocations_run_started_id" {
			return
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	t.Fatal("agent invocations must have a run timeline index")
}

func TestAgentInvocationAggregatesAndRunSummary(t *testing.T) {
	d, _, run := openSessionTestDB(t)

	seed := []AgentInvocation{
		{RunID: run.ID, StepName: "review", Round: 1, Purpose: "review", Agent: "codex", SessionMode: InvocationModeStarted, StartedAt: 1, CompletedAt: 2, DurationMS: 100, ExitStatus: "ok", InputTokens: 10, OutputTokens: 5},
		{RunID: run.ID, StepName: "review", Round: 2, Purpose: "review", Agent: "codex", SessionMode: InvocationModeResumed, StartedAt: 3, CompletedAt: 4, DurationMS: 50, ExitStatus: "ok", InputTokens: 10, OutputTokens: 5},
		{RunID: run.ID, StepName: "review", Round: 2, Purpose: "review-fix", Agent: "codex", SessionMode: InvocationModeFallback, StartedAt: 5, CompletedAt: 6, DurationMS: 70, ExitStatus: "error", FailureCategory: "exit"},
	}
	for _, inv := range seed {
		if _, err := d.InsertAgentInvocation(inv); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	aggregates, err := d.AgentInvocationAggregates()
	if err != nil {
		t.Fatalf("aggregates: %v", err)
	}
	byPurpose := map[string]AgentInvocationAggregate{}
	for _, a := range aggregates {
		byPurpose[a.Purpose] = a
	}
	review := byPurpose["review"]
	if review.Count != 2 || review.TotalDurationMS != 150 || review.AvgDurationMS != 75 ||
		review.Started != 1 || review.Resumed != 1 || review.InputTokens != 20 {
		t.Fatalf("review aggregate = %+v", review)
	}
	fix := byPurpose["review-fix"]
	if fix.Count != 1 || fix.Fallback != 1 || fix.Errors != 1 {
		t.Fatalf("review-fix aggregate = %+v", fix)
	}

	summary, err := d.AgentInvocationSummaryForRun(run.ID)
	if err != nil {
		t.Fatalf("run summary: %v", err)
	}
	if summary.Count != 3 || summary.Resumed != 1 || summary.Fallback != 1 || summary.TotalDurationMS != 220 {
		t.Fatalf("run summary = %+v", summary)
	}
}

func TestAgentInvocationAggregatesPreserveUnknownMetrics(t *testing.T) {
	d, _, run := openSessionTestDB(t)
	inv := AgentInvocation{
		RunID: run.ID, StepName: "review", Round: 1, Purpose: "review", Agent: "codex",
		SessionMode: InvocationModeCold, StartedAt: 1, CompletedAt: 2, DurationMS: 10, ExitStatus: "ok",
	}
	if _, err := d.InsertAgentInvocation(inv); err != nil {
		t.Fatal(err)
	}
	aggregates, err := d.AgentInvocationAggregates()
	if err != nil {
		t.Fatal(err)
	}
	if len(aggregates) != 1 {
		t.Fatalf("got %d aggregates, want 1", len(aggregates))
	}
	a := aggregates[0]
	if a.SubprocessWaitMS != nil || a.CacheCreationTokens != nil ||
		a.FreshInputTokens != nil || a.ReasoningTokens != nil ||
		a.ModelRoundtrips != nil || a.ToolCalls != nil {
		t.Fatalf("unknown aggregate metrics became recorded values: %+v", a)
	}
}

func TestAgentInvocationAggregatesHidePartialMetrics(t *testing.T) {
	d, _, run := openSessionTestDB(t)
	for _, inv := range []AgentInvocation{
		{RunID: run.ID, StepName: "review", Round: 1, Purpose: "review", Agent: "codex", SessionMode: InvocationModeCold, StartedAt: 1, CompletedAt: 2, DurationMS: 10, ExitStatus: "ok", FreshInputTokens: intPtr(3)},
		{RunID: run.ID, StepName: "review", Round: 2, Purpose: "review", Agent: "codex", SessionMode: InvocationModeCold, StartedAt: 3, CompletedAt: 4, DurationMS: 10, ExitStatus: "ok"},
	} {
		if _, err := d.InsertAgentInvocation(inv); err != nil {
			t.Fatal(err)
		}
	}
	aggregates, err := d.AgentInvocationAggregates()
	if err != nil {
		t.Fatal(err)
	}
	if aggregates[0].FreshInputTokens != nil {
		t.Fatalf("partial fresh input = %v, want nil", *aggregates[0].FreshInputTokens)
	}
}

func TestAddRunParkedDurationAccumulates(t *testing.T) {
	d, _, run := openSessionTestDB(t)

	if err := d.AddRunParkedDuration(run.ID, 1500); err != nil {
		t.Fatalf("add: %v", err)
	}
	if err := d.AddRunParkedDuration(run.ID, 500); err != nil {
		t.Fatalf("add again: %v", err)
	}
	if err := d.AddRunParkedDuration(run.ID, 0); err != nil {
		t.Fatalf("zero add: %v", err)
	}

	got, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.ParkedMS != 2000 {
		t.Fatalf("ParkedMS = %d, want 2000", got.ParkedMS)
	}
}

func TestCompleteRunAwaitingAgentAccumulatesParkedDuration(t *testing.T) {
	d, _, run := openSessionTestDB(t)

	if err := d.SetRunAwaitingAgent(run.ID); err != nil {
		t.Fatalf("set awaiting: %v", err)
	}
	if err := d.CompleteRunAwaitingAgent(run.ID, 1500); err != nil {
		t.Fatalf("complete awaiting: %v", err)
	}

	got, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.AwaitingAgentSince != nil {
		t.Fatal("AwaitingAgentSince must be cleared after completion")
	}
	if got.ParkedMS != 1500 {
		t.Fatalf("ParkedMS = %d, want 1500", got.ParkedMS)
	}
}

// TestRecoverStaleRunsAccumulatesParkedTime proves a crash while parked does
// not lose the parked evidence: recovery folds the live awaiting marker into
// the run's parked total.
func TestRecoverStaleRunsAccumulatesParkedTime(t *testing.T) {
	d, _, run := openSessionTestDB(t)

	if err := d.UpdateRunStatus(run.ID, "running"); err != nil {
		t.Fatalf("mark running: %v", err)
	}
	// Park the run 60 seconds in the past.
	past := now() - 60
	if _, err := d.sql.Exec(`UPDATE runs SET awaiting_agent_since = ? WHERE id = ?`, past, run.ID); err != nil {
		t.Fatalf("seed awaiting: %v", err)
	}

	if _, err := d.RecoverStaleRuns("daemon crashed during execution"); err != nil {
		t.Fatalf("recover: %v", err)
	}

	got, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.AwaitingAgentSince != nil {
		t.Fatal("awaiting marker must be cleared by recovery")
	}
	if got.ParkedMS < 59_000 || got.ParkedMS > 62_000 {
		t.Fatalf("ParkedMS = %d, want ~60000 accumulated from the crashed park", got.ParkedMS)
	}
}

// TestOpenMigratesAgentInvocationsAndParkedMS proves databases created before
// the performance-telemetry schema gain the table and column on reopen.
func TestOpenMigratesAgentInvocationsAndParkedMS(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	d, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := d.sql.Exec(`DROP TABLE agent_invocations`); err != nil {
		t.Fatalf("drop table: %v", err)
	}
	// Simulate a pre-parked_ms runs table by rebuilding it without the column.
	if _, err := d.sql.Exec(`ALTER TABLE runs DROP COLUMN parked_ms`); err != nil {
		t.Fatalf("drop column: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	d, err = Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer d.Close()

	repo, err := d.InsertRepo("/tmp/repo", "https://github.com/test/repo", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	run, err := d.InsertRun(repo.ID, "b", "h", "b")
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	if err := d.AddRunParkedDuration(run.ID, 10); err != nil {
		t.Fatalf("parked_ms column missing after migration: %v", err)
	}
	if _, err := d.InsertAgentInvocation(AgentInvocation{RunID: run.ID, StepName: "review", Round: 1, Purpose: "review", Agent: "codex", SessionMode: InvocationModeCold, StartedAt: 1, CompletedAt: 2, DurationMS: 1, ExitStatus: "ok"}); err != nil {
		t.Fatalf("agent_invocations table missing after migration: %v", err)
	}
}

// TestOpenMigratesSessionFidelityColumns proves a database whose
// agent_invocations table predates the session-fidelity columns gains them on
// reopen, and that pre-existing rows read those columns back as unknown (nil)
// rather than a fabricated zero.
func TestOpenMigratesSessionFidelityColumns(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	d, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	repo, err := d.InsertRepo("/tmp/repo", "https://github.com/test/repo", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	run, err := d.InsertRun(repo.ID, "b", "h", "b")
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	// Simulate a pre-fidelity table by dropping the new columns, then insert a
	// legacy row that has no fidelity data.
	for _, col := range []string{"model_provider", "fallback_reason", "subprocess_wait_ms",
		"fresh_input_tokens", "reasoning_tokens", "model_roundtrips", "tool_calls", "finding_count"} {
		if _, err := d.sql.Exec(`ALTER TABLE agent_invocations DROP COLUMN ` + col); err != nil {
			t.Fatalf("drop %s: %v", col, err)
		}
	}
	if _, err := d.sql.Exec(`INSERT INTO agent_invocations
		(id, run_id, step_name, round, purpose, agent, model, session_mode, session_key, exit_status, failure_category, started_at, completed_at, duration_ms, input_tokens, output_tokens, cache_read_tokens)
		VALUES ('legacy1', ?, 'review', 1, 'review', 'codex', '', 'started', '', 'ok', '', 1, 2, 100, 500, 20, 300)`, run.ID); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	d, err = Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer d.Close()

	got, err := d.GetAgentInvocationsByRun(run.ID)
	if err != nil {
		t.Fatalf("get after migration: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d rows, want 1", len(got))
	}
	legacy := got[0]
	if legacy.InputTokens != 500 {
		t.Fatalf("legacy input tokens = %d, want 500", legacy.InputTokens)
	}
	if legacy.ModelProvider != nil || legacy.SubprocessWaitMS != nil ||
		legacy.ModelRoundtrips != nil || legacy.ToolCalls != nil || legacy.FindingCount != nil {
		t.Fatalf("legacy row must read new columns as unknown, got %+v", legacy)
	}
	// The migrated table now accepts the new fields.
	if _, err := d.InsertAgentInvocation(AgentInvocation{
		RunID: run.ID, StepName: "review", Round: 2, Purpose: "review", Agent: "codex",
		SessionMode: InvocationModeResumed, StartedAt: 3, CompletedAt: 4, DurationMS: 1, ExitStatus: "ok",
		ModelRoundtrips: intPtr(3), ToolCalls: intPtr(2), SubprocessWaitMS: int64Ptr(500),
	}); err != nil {
		t.Fatalf("insert after migration: %v", err)
	}
}

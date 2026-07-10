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
		CacheCreationTokens: 50,
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
}

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

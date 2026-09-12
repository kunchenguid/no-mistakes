package cli

import (
	"regexp"
	"strings"
	"testing"

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
		{RunID: run.ID, StepName: "review", Round: 1, Purpose: "review", Agent: "codex", Model: "gpt-5.2", SessionMode: db.InvocationModeStarted, SessionKey: "deadbeef00000000", StartedAt: 1, CompletedAt: 2, DurationMS: 60_000, ExitStatus: "ok", InputTokens: statsIntPtr(100), OutputTokens: statsIntPtr(10), CacheReadTokens: statsIntPtr(40), CacheCreationTokens: statsIntPtr(20)},
		{RunID: run.ID, StepName: "review", Round: 2, Purpose: "review", Agent: "codex", Model: "gpt-5.2", SessionMode: db.InvocationModeResumed, SessionKey: "deadbeef00000000", StartedAt: 3, CompletedAt: 4, DurationMS: 30_000, ExitStatus: "ok", InputTokens: statsIntPtr(50), OutputTokens: statsIntPtr(5), CacheReadTokens: statsIntPtr(45), CacheCreationTokens: statsIntPtr(25)},
		{RunID: run.ID, StepName: "review", Round: 2, Purpose: "review-fix", Agent: "codex", Model: "gpt-5.2", SessionMode: db.InvocationModeStarted, SessionKey: "feedface00000000", StartedAt: 5, CompletedAt: 6, DurationMS: 45_000, ExitStatus: "ok"},
		{RunID: run.ID, StepName: "document", Round: 1, Purpose: "housekeeping", Agent: "codex", Model: "gpt-5.2", SessionMode: db.InvocationModeCold, StartedAt: 7, CompletedAt: 8, DurationMS: 172_000, ExitStatus: "ok"},
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
	for _, want := range []string{"PURPOSE", "review", "review-fix", "housekeeping (document+lint)", "RESUMED", "CACHE WRITE TOK", "45"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stats --agents missing %q in:\n%s", want, out)
		}
	}

	out, err = executeCmd("stats", "--run", run.ID)
	if err != nil {
		t.Fatalf("stats --run: %v\n%s", err, out)
	}
	for _, want := range []string{run.ID, "parked at gates 1m30s total", "resumed", "deadbeef00000000", "gpt-5.2", "document+lint", "housekeeping (document+lint)", "CACHE WR", "20"} {
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
		ExitStatus: "ok", InputTokens: statsIntPtr(2500), OutputTokens: statsIntPtr(250), CacheReadTokens: statsIntPtr(1800),
		FreshInputTokens: statsIntPtr(700), ReasoningTokens: statsIntPtr(9),
		DeltaInputTokens: statsIntPtr(1500), DeltaOutputTokens: statsIntPtr(150), DeltaCacheReadTokens: statsIntPtr(1200),
		ModelRoundtrips: statsIntPtr(24), ToolCalls: statsIntPtr(7),
		ToolWaitCalls: statsIntPtr(0), ToolTestLintCalls: statsIntPtr(2), ToolEditCalls: statsIntPtr(3),
		ToolReadCalls: statsIntPtr(1), ToolGitCalls: statsIntPtr(1), ToolOtherCalls: statsIntPtr(0),
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
	for _, want := range []string{"ROUNDTRIPS", "TEST/LINT", "SUBPROC", "24", "METRICS", "1/1"} {
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
	for _, want := range []string{"Δ IN (round)", "1500", "2500", "7 0/2/3/1/1/0", "12/1060", "MODEL"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stats --run missing %q in:\n%s", want, out)
		}
	}
}

// TestStatsDistinguishesUnreportedTokensFromReportedZero proves the rendered
// report tells a round whose usage was never recorded apart from a round that
// genuinely used no tokens: the first shows "-" in the token cells, the second
// shows "0". Both rows are seeded in one run so a renderer that collapses the
// two cases fails whichever way it collapses them.
func TestStatsDistinguishesUnreportedTokensFromReportedZero(t *testing.T) {
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
	// Neither purpose may contain the "-" being asserted, or a stray label
	// match would stand in for the cell under test.
	for _, inv := range []db.AgentInvocation{{
		RunID: run.ID, StepName: "test", Round: 1, Purpose: "unreported", Agent: "pi", Model: "pi-1",
		SessionMode: db.InvocationModeCold, StartedAt: 1, CompletedAt: 2, DurationMS: 45 * 60_000,
		ExitStatus: "error", FailureCategory: "parse",
	}, {
		RunID: run.ID, StepName: "review", Round: 1, Purpose: "zero", Agent: "pi", Model: "pi-1",
		SessionMode: db.InvocationModeCold, StartedAt: 3, CompletedAt: 4, DurationMS: 1_000,
		ExitStatus: "ok", InputTokens: statsIntPtr(0), OutputTokens: statsIntPtr(0), CacheReadTokens: statsIntPtr(0),
	}} {
		if _, err := d.InsertAgentInvocation(inv); err != nil {
			t.Fatal(err)
		}
	}
	d.Close()

	perRun, err := executeCmd("stats", "--run", run.ID)
	if err != nil {
		t.Fatalf("stats --run: %v\n%s", err, perRun)
	}
	runRows := statsTableRows(t, perRun, "IN (raw)")
	assertStatsCells(t, runRows, "unreported", map[string]string{
		"IN (raw)": "-", "OUT (raw)": "-", "CACHE RD (raw)": "-",
	})
	assertStatsCells(t, runRows, "zero", map[string]string{
		"IN (raw)": "0", "OUT (raw)": "0", "CACHE RD (raw)": "0",
	})

	aggregates, err := executeCmd("stats", "--agents")
	if err != nil {
		t.Fatalf("stats --agents: %v\n%s", err, aggregates)
	}
	aggRows := statsTableRows(t, aggregates, "IN TOK")
	assertStatsCells(t, aggRows, "unreported", map[string]string{
		"IN TOK": "-", "OUT TOK": "-", "CACHE READ TOK": "-", "USAGE": "0/1",
	})
	assertStatsCells(t, aggRows, "zero", map[string]string{
		"IN TOK": "0", "OUT TOK": "0", "CACHE READ TOK": "0", "USAGE": "1/1",
	})
}

// statsTableRows parses one rendered table keyed by column title. The table is
// the one whose header carries headerCell; the tabwriter pads every cell by at
// least two spaces, so a run of two or more spaces separates cells while the
// single spaces inside a title like "IN (raw)" survive.
func statsTableRows(t *testing.T, out, headerCell string) []map[string]string {
	t.Helper()
	gap := regexp.MustCompile(`\s{2,}`)
	lines := strings.Split(out, "\n")
	header := -1
	for i, line := range lines {
		if strings.Contains(line, headerCell) {
			header = i
			break
		}
	}
	if header < 0 {
		t.Fatalf("no table with column %q in:\n%s", headerCell, out)
	}
	titles := gap.Split(strings.TrimSpace(lines[header]), -1)
	var rows []map[string]string
	for _, line := range lines[header+1:] {
		if strings.TrimSpace(line) == "" {
			break
		}
		cells := gap.Split(strings.TrimSpace(line), -1)
		if len(cells) != len(titles) {
			t.Fatalf("row %q split into %d cells, want %d for %v", line, len(cells), len(titles), titles)
		}
		row := make(map[string]string, len(titles))
		for i, title := range titles {
			row[title] = cells[i]
		}
		rows = append(rows, row)
	}
	return rows
}

func assertStatsCells(t *testing.T, rows []map[string]string, purpose string, want map[string]string) {
	t.Helper()
	for _, row := range rows {
		if row["PURPOSE"] != purpose {
			continue
		}
		for column, value := range want {
			got, ok := row[column]
			if !ok {
				t.Fatalf("purpose %q has no column %q (row %v)", purpose, column, row)
			}
			if got != value {
				t.Errorf("purpose %q column %q = %q, want %q", purpose, column, got, value)
			}
		}
		return
	}
	t.Fatalf("no row for purpose %q in %v", purpose, rows)
}

func strPtrCLI(s string) *string { return &s }

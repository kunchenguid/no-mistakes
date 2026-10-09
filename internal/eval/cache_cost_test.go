package eval

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
)

// TestPersistEvaluationRecordsCacheTokens pins the registry's cache token
// columns: cache reads dominate review cost (the P0 smoke measured 23.4M
// cache-read against 556 fresh-input tokens across claude's eight replays),
// so a registry row that drops them understates real cost by orders of
// magnitude and forces cost analysis back into the per-eval JSON files.
func TestPersistEvaluationRecordsCacheTokens(t *testing.T) {
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	caseDir := store.caseDir("cache-cost")
	if err := os.MkdirAll(caseDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeJSON(filepath.Join(caseDir, "labels.json"), Labels{Version: labelsVersion}); err != nil {
		t.Fatal(err)
	}
	c := Case{Manifest: Manifest{ID: "cache-cost", SourceRunID: "run", SourceRoundID: "round"}, Dir: caseDir}
	if err := store.registerCase(c); err != nil {
		t.Fatal(err)
	}
	if err := store.persistEvaluation(c, Evaluation{
		ID:               "evaluation",
		SessionID:        "session",
		CaseID:           c.ID,
		Candidate:        "claude+test",
		Repeat:           1,
		Status:           "completed",
		TokensReported:   true,
		InputTokens:      2_934_600,
		OutputTokens:     1475,
		CacheReadTokens:  2_921_704,
		CacheWriteTokens: 12_340,
		FreshInputTokens: 556,
	}); err != nil {
		t.Fatal(err)
	}

	var cacheRead, cacheWrite int64
	if err := store.db.QueryRow(
		`SELECT cache_read_tokens, cache_write_tokens FROM evaluations WHERE id = 'evaluation'`,
	).Scan(&cacheRead, &cacheWrite); err != nil {
		t.Fatalf("read cache token columns: %v", err)
	}
	if cacheRead != 2_921_704 || cacheWrite != 12_340 {
		t.Fatalf("registry cache tokens = read %d write %d, want read 2921704 write 12340", cacheRead, cacheWrite)
	}
}

func openLegacyEvaluationRegistry(t *testing.T, root string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(root, "registry.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(`
CREATE TABLE cases (
    id TEXT PRIMARY KEY,
    source_run_id TEXT NOT NULL,
    source_round_id TEXT NOT NULL UNIQUE,
    captured_at INTEGER NOT NULL,
    repo_fingerprint TEXT NOT NULL,
    branch TEXT NOT NULL,
    language TEXT NOT NULL,
    size_bucket TEXT NOT NULL,
    severity TEXT NOT NULL,
    gold_count INTEGER NOT NULL,
    path TEXT NOT NULL UNIQUE
);
CREATE TABLE evaluations (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL,
    case_id TEXT NOT NULL REFERENCES cases(id) ON DELETE CASCADE,
    candidate TEXT NOT NULL,
    repeat_number INTEGER NOT NULL,
    started_at INTEGER NOT NULL,
    completed_at INTEGER NOT NULL,
    status TEXT NOT NULL,
    gold_count INTEGER NOT NULL,
    true_positive INTEGER NOT NULL,
    false_negative INTEGER NOT NULL,
    false_positive INTEGER NOT NULL,
    pending INTEGER NOT NULL,
    tokens_reported INTEGER NOT NULL,
    input_tokens INTEGER NOT NULL,
    output_tokens INTEGER NOT NULL,
    fresh_input_tokens INTEGER NOT NULL,
    duration_ms INTEGER NOT NULL,
    path TEXT NOT NULL UNIQUE
);
INSERT INTO cases (id, source_run_id, source_round_id, captured_at, repo_fingerprint, branch, language, size_bucket, severity, gold_count, path)
VALUES ('case-old', 'run', 'round', 1, 'fp', 'main', 'go', 's', 'sev', 0, 'p');
INSERT INTO evaluations (id, session_id, case_id, candidate, repeat_number, started_at, completed_at, status, gold_count, true_positive, false_negative, false_positive, pending, tokens_reported, input_tokens, output_tokens, fresh_input_tokens, duration_ms, path)
VALUES ('old-eval', 'session', 'case-old', 'claude+test', 1, 1, 2, 'completed', 0, 0, 0, 0, 0, 1, 10, 20, 30, 40, 'p');
`)
	if err != nil {
		t.Fatalf("seed legacy registry: %v", err)
	}
	return db
}

// TestStoreMigrationAddsCacheTokenColumnsForward opens a registry created by
// an older binary (evaluations without cache columns and one row already in
// it) and proves the migration adds the columns without losing the row,
// so existing histories stay readable and new rows persist cleanly.
func TestStoreMigrationAddsCacheTokenColumnsForward(t *testing.T) {
	root := t.TempDir()
	db := openLegacyEvaluationRegistry(t, root)
	legacyPath := filepath.Join(root, "legacy-evaluation.json")
	if err := os.WriteFile(legacyPath, []byte(`{"cache_read_tokens":2921704}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE evaluations SET path = ? WHERE id = 'old-eval'`, legacyPath); err != nil {
		t.Fatal(err)
	}
	withWritesPath := filepath.Join(root, "evaluation-with-writes.json")
	if err := writeJSON(withWritesPath, Evaluation{CacheReadTokens: 200, CacheWriteTokens: 300}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO evaluations SELECT 'with-writes', session_id, case_id, candidate, repeat_number, started_at, completed_at, status, gold_count, true_positive, false_negative, false_positive, pending, tokens_reported, input_tokens, output_tokens, fresh_input_tokens, duration_ms, ? FROM evaluations WHERE id = 'old-eval'`, withWritesPath); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO evaluations SELECT 'missing-payload', session_id, case_id, candidate, repeat_number, started_at, completed_at, status, gold_count, true_positive, false_negative, false_positive, pending, tokens_reported, input_tokens, output_tokens, fresh_input_tokens, duration_ms, ? FROM evaluations WHERE id = 'old-eval'`, filepath.Join(root, "missing.json")); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	for _, column := range []string{"cache_read_tokens", "cache_write_tokens"} {
		var found int
		if err := store.db.QueryRow(
			`SELECT count(*) FROM pragma_table_info('evaluations') WHERE name = ?`, column,
		).Scan(&found); err != nil {
			t.Fatal(err)
		}
		if found != 1 {
			t.Fatalf("migrated evaluations table lacks the %s column", column)
		}
	}
	for attempt := 0; attempt < 2; attempt++ {
		for _, tc := range []struct {
			id                 string
			input, read, write int64
			unknown            bool
		}{
			{"old-eval", 2_921_714, 2_921_704, 0, false},
			{"with-writes", 510, 200, 300, false},
			{"missing-payload", 10, 0, 0, true},
		} {
			var input int64
			var cacheRead, cacheWrite sql.NullInt64
			if err := store.db.QueryRow(
				`SELECT input_tokens, cache_read_tokens, cache_write_tokens FROM evaluations WHERE id = ?`, tc.id,
			).Scan(&input, &cacheRead, &cacheWrite); err != nil {
				t.Fatalf("legacy row unreadable after migration: %v", err)
			}
			if input != tc.input || cacheRead != (sql.NullInt64{Int64: tc.read, Valid: !tc.unknown}) || cacheWrite != (sql.NullInt64{Int64: tc.write, Valid: !tc.unknown}) {
				t.Fatalf("%s: input %d cache-read %v cache-write %v, want %d/%d/%d (unknown: %t)", tc.id, input, cacheRead, cacheWrite, tc.input, tc.read, tc.write, tc.unknown)
			}
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		if attempt == 0 {
			store, err = Open(root)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestStoreMigrationSkipsDamagedCachePayloads(t *testing.T) {
	for _, damage := range []string{"malformed", "unreadable", "missing"} {
		t.Run(damage, func(t *testing.T) {
			root := t.TempDir()
			database := openLegacyEvaluationRegistry(t, root)
			goodPath := filepath.Join(root, "good.json")
			if err := writeJSON(goodPath, Evaluation{CacheReadTokens: 200, CacheWriteTokens: 300}); err != nil {
				t.Fatal(err)
			}
			if _, err := database.Exec(`UPDATE evaluations SET path = ? WHERE id = 'old-eval'`, goodPath); err != nil {
				t.Fatal(err)
			}
			badPath := filepath.Join(root, "bad.json")
			const malformed = `{"cache_read_tokens":99,"cache_write_tokens":`
			switch damage {
			case "malformed":
				if err := os.WriteFile(badPath, []byte(malformed), 0o600); err != nil {
					t.Fatal(err)
				}
			case "unreadable":
				// Reading a directory fails on every platform, including as root.
				if err := os.Mkdir(badPath, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := database.Exec(`INSERT INTO evaluations SELECT 'bad-eval', session_id, case_id, candidate, repeat_number, started_at, completed_at, status, gold_count, true_positive, false_negative, false_positive, pending, tokens_reported, input_tokens, output_tokens, fresh_input_tokens, duration_ms, ? FROM evaluations WHERE id = 'old-eval'`, badPath); err != nil {
				t.Fatal(err)
			}
			if err := database.Close(); err != nil {
				t.Fatal(err)
			}
			warnings, err := os.Create(filepath.Join(root, "warnings.log"))
			if err != nil {
				t.Fatal(err)
			}
			originalStderr, originalLogWriter := os.Stderr, log.Writer()
			os.Stderr = warnings
			// The CLI discards logs when its optional log directory is absent.
			log.SetOutput(io.Discard)
			t.Cleanup(func() {
				os.Stderr = originalStderr
				log.SetOutput(originalLogWriter)
				_ = warnings.Close()
			})
			for attempt := 0; attempt < 2; attempt++ {
				store, err := Open(root)
				if err != nil {
					t.Fatalf("open registry with %s payload: %v", damage, err)
				}
				t.Cleanup(func() { _ = store.Close() })
				var read, write int64
				if err := store.db.QueryRow(`SELECT cache_read_tokens, cache_write_tokens FROM evaluations WHERE id = 'old-eval'`).Scan(&read, &write); err != nil {
					t.Fatal(err)
				}
				if read != 200 || write != 300 {
					t.Fatalf("good row cache tokens = %d/%d, want 200/300", read, write)
				}
				var input, fresh int64
				var badRead, badWrite sql.NullInt64
				var path string
				if err := store.db.QueryRow(`SELECT input_tokens, fresh_input_tokens, cache_read_tokens, cache_write_tokens, path FROM evaluations WHERE id = 'bad-eval'`).Scan(&input, &fresh, &badRead, &badWrite, &path); err != nil {
					t.Fatal(err)
				}
				if input != 10 || fresh != 30 || badRead.Valid || badWrite.Valid || path != badPath {
					t.Fatalf("damaged row changed: input %d fresh %d cache %v/%v path %q", input, fresh, badRead, badWrite, path)
				}
				switch damage {
				case "malformed":
					data, err := os.ReadFile(badPath)
					if err != nil || string(data) != malformed {
						t.Fatalf("damaged payload changed: %q, %v", data, err)
					}
				case "unreadable":
					info, err := os.Stat(badPath)
					if err != nil || !info.IsDir() {
						t.Fatalf("unreadable payload changed: %v", err)
					}
				case "missing":
					if _, err := os.Stat(badPath); !os.IsNotExist(err) {
						t.Fatalf("missing payload changed: %v", err)
					}
				}
				if err := store.Close(); err != nil {
					t.Fatal(err)
				}
			}
			// The skip must be visible: the warning names the skipped record's
			// id so the operator can repair or retire it, and never quotes the
			// damaged payload's contents.
			warningBytes, err := os.ReadFile(warnings.Name())
			if err != nil {
				t.Fatal(err)
			}
			warningText := string(warningBytes)
			if !strings.Contains(warningText, "bad-eval") {
				t.Fatalf("migration warnings must name the skipped record id, got: %q", warningText)
			}
			if damage == "malformed" && strings.Contains(warningText, malformed) {
				t.Fatalf("migration warning must not quote the damaged payload: %q", warningText)
			}
		})
	}
}

func TestReportKeepsUnmigratedCacheCostsUnknown(t *testing.T) {
	for _, column := range []string{"cache_read_tokens", "cache_write_tokens"} {
		t.Run(column, func(t *testing.T) {
			store, err := Open(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			c := Case{Manifest: Manifest{ID: "unknown-cache", SourceRunID: "run", SourceRoundID: "round"}, Dir: store.caseDir("unknown-cache")}
			if err := store.registerCase(c); err != nil {
				t.Fatal(err)
			}
			if err := store.persistEvaluation(c, Evaluation{ID: "unknown-cache", SessionID: "session", CaseID: c.ID, Candidate: "claude,model=test", Repeat: 1,
				Status: "completed", HasFindingGold: true, GoldCount: 1, TruePositive: 1, TokensReported: true,
				InputTokens: 510, FreshInputTokens: 10, CacheReadTokens: 200, CacheWriteTokens: 300, OutputTokens: 20}); err != nil {
				t.Fatal(err)
			}
			if _, err := store.db.Exec(`UPDATE evaluations SET ` + column + ` = NULL`); err != nil {
				t.Fatal(err)
			}
			reports, err := Report(store)
			if err != nil {
				t.Fatal(err)
			}
			if len(reports) != 1 || reports[0].Summary.TruePositive != 1 || reports[0].AverageTokens != nil || reports[0].AverageCacheReadTokens != nil || reports[0].AverageCacheWriteTokens != nil || reports[0].OnFrontier {
				t.Fatalf("unknown cache cost must preserve scores without claiming cost or frontier: %#v", reports)
			}
		})
	}
}

func TestHistoricalEvaluationCostsUseMigratedTokenCounters(t *testing.T) {
	root := t.TempDir()
	database := openLegacyEvaluationRegistry(t, root)
	if _, err := database.Exec(`DELETE FROM evaluations`); err != nil {
		t.Fatal(err)
	}
	type legacyCase struct {
		candidate                       string
		input, fresh, reads, writes     int64
		reported                        bool
		wantInput, wantFresh, wantTotal int64
	}
	var cases []legacyCase
	for _, name := range []string{"claude", "pi", "opencode"} {
		for _, spelling := range []string{name + "+legacy", name + ",model=legacy"} {
			cases = append(cases, legacyCase{spelling, 10_000, 0, 20_000, 0, true, 30_000, 10_000, 30_500})
		}
	}
	cases = append(cases,
		legacyCase{"claude,model=large-fresh", 40_000, 20_000, 20_000, 0, true, 60_000, 40_000, 60_500},
		legacyCase{"opencode,model=reported-writes", 10_000, 0, 20_000, 1_000, true, 31_000, 10_000, 31_500},
		legacyCase{"grok,model=legacy", 30_000, 10_000, 20_000, 0, true, 30_000, 10_000, 30_500},
		legacyCase{"grok,model=reported-writes", 31_000, 11_000, 20_000, 1_000, true, 31_000, 10_000, 31_500},
		legacyCase{"codex,model=legacy", 30_000, 10_000, 20_000, 0, true, 30_000, 10_000, 30_500},
		legacyCase{"claude,model=unknown", 0, 0, 0, 0, false, 0, 0, 0},
	)
	for i, tc := range cases {
		evaluation := Evaluation{ID: fmt.Sprintf("legacy-%d", i), SessionID: "session", CaseID: "case-old", Candidate: tc.candidate,
			Repeat: 1, Status: "completed", HasFindingGold: true, GoldCount: 2, TruePositive: 2,
			TokensReported: tc.reported, InputTokens: tc.input, FreshInputTokens: tc.fresh,
			CacheReadTokens: tc.reads, CacheWriteTokens: tc.writes}
		if tc.reported {
			evaluation.OutputTokens = 500
		}
		path := filepath.Join(root, evaluation.ID+".json")
		if err := writeJSON(path, evaluation); err != nil {
			t.Fatal(err)
		}
		if _, err := database.Exec(`INSERT INTO evaluations
(id, session_id, case_id, candidate, repeat_number, started_at, completed_at, status, gold_count, true_positive, false_negative, false_positive, pending, tokens_reported, input_tokens, output_tokens, fresh_input_tokens, duration_ms, path)
VALUES (?, 'session', 'case-old', ?, 1, 1, 2, 'completed', 2, 2, 0, 0, 0, ?, ?, ?, ?, 40, ?)`,
			evaluation.ID, tc.candidate, tc.reported, tc.input, evaluation.OutputTokens, tc.fresh, path); err != nil {
			t.Fatal(err)
		}
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	c := Case{Manifest: Manifest{ID: "case-old"}, Dir: filepath.Join(root, "cases", "case-old")}
	for _, evaluation := range []Evaluation{
		{ID: "normalized", SessionID: "new", CaseID: c.ID, Candidate: "claude,model=normalized", Repeat: 1,
			Status: "completed", HasFindingGold: true, GoldCount: 2, TruePositive: 2, TokensReported: true,
			InputTokens: 31_000, FreshInputTokens: 10_000, CacheReadTokens: 20_000, CacheWriteTokens: 1_000, OutputTokens: 500},
		{ID: "fresh-only", SessionID: "new", CaseID: c.ID, Candidate: "codex,model=fresh-only", Repeat: 1,
			Status: "completed", HasFindingGold: true, GoldCount: 2, TruePositive: 2, TokensReported: true,
			InputTokens: 29_000, FreshInputTokens: 29_000, OutputTokens: 500},
	} {
		if err := store.persistEvaluation(c, evaluation); err != nil {
			t.Fatal(err)
		}
		cases = append(cases, legacyCase{candidate: evaluation.Candidate, reported: true,
			wantInput: evaluation.InputTokens, wantFresh: evaluation.FreshInputTokens,
			reads: evaluation.CacheReadTokens, writes: evaluation.CacheWriteTokens,
			wantTotal: evaluation.InputTokens + evaluation.OutputTokens})
	}
	for attempt := 0; attempt < 2; attempt++ {
		reports, err := Report(store)
		if err != nil {
			t.Fatal(err)
		}
		byCandidate := make(map[string]CandidateReport)
		for _, report := range reports {
			byCandidate[report.Summary.Candidate] = report
		}
		for _, tc := range cases {
			var input, fresh, reads, writes int64
			if err := store.db.QueryRow(`SELECT input_tokens, fresh_input_tokens, cache_read_tokens, cache_write_tokens FROM evaluations WHERE candidate = ?`, tc.candidate).
				Scan(&input, &fresh, &reads, &writes); err != nil {
				t.Fatal(err)
			}
			if input != tc.wantInput || fresh != tc.wantFresh || reads != tc.reads || writes != tc.writes {
				t.Fatalf("%s registry tokens = %d/%d/%d/%d, want %d/%d/%d/%d", tc.candidate,
					input, fresh, reads, writes, tc.wantInput, tc.wantFresh, tc.reads, tc.writes)
			}
			report, exists := byCandidate[tc.candidate]
			if !exists {
				t.Fatalf("no report for %s", tc.candidate)
			}
			if !tc.reported {
				if report.AverageTokens != nil {
					t.Fatalf("%s cost = %v, want unknown", tc.candidate, *report.AverageTokens)
				}
				continue
			}
			if report.AverageTokens == nil || *report.AverageTokens != float64(tc.wantTotal) {
				t.Fatalf("%s report = %#v, want cost %d", tc.candidate, report, tc.wantTotal)
			}
			if tc.writes == 0 && strings.Contains(RenderReport([]CandidateReport{report}), "cache-write") {
				t.Fatalf("%s renders unreported cache-write tokens", tc.candidate)
			}
			wantFrontier := tc.candidate == "codex,model=fresh-only"
			if report.OnFrontier != wantFrontier {
				t.Fatalf("%s frontier = %v, want %v", tc.candidate, report.OnFrontier, wantFrontier)
			}
		}
		if err := store.Close(); err != nil {
			t.Fatal(err)
		}
		if attempt == 0 {
			store, err = Open(root)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
}

// TestObservedAgentSumsCacheWriteTokens extends the honest-cost rule to
// cache-write tokens: an adapter that reports cache creation has it summed
// across attempts, and an attempt without the field leaves the sum alone
// rather than fabricating a write count.
func TestObservedAgentSumsCacheWriteTokens(t *testing.T) {
	withWrite := func(input, output, cacheRead, cacheWrite int) *agent.Result {
		result := reportedUsage(input, output, cacheRead)
		result.Usage.CacheCreationTokens = cacheWrite
		result.Usage.CacheCreationReported = true
		return result
	}
	observed := &observedAgent{inner: &retryingAgent{
		attempts: []*agent.Result{withWrite(50_000, 100, 5_000, 2_000), withWrite(50_000, 100, 5_000, 3_000)},
	}}
	if _, err := observed.Run(context.Background(), agent.RunOpts{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if observed.usage.CacheCreationTokens != 5_000 {
		t.Fatalf("cache-write sum = %d, want 5000 across both attempts", observed.usage.CacheCreationTokens)
	}

	unreported := &observedAgent{inner: &retryingAgent{
		attempts: []*agent.Result{reportedUsage(50_000, 100, 5_000), withWrite(50_000, 100, 5_000, 2_000)},
	}}
	if _, err := unreported.Run(context.Background(), agent.RunOpts{}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if unreported.usage.CacheCreationTokens != 2_000 {
		t.Fatalf("cache-write sum = %d, want only the attempt that reported cache creation", unreported.usage.CacheCreationTokens)
	}
}

func TestObservedAgentCountsEachTokenBucketOnce(t *testing.T) {
	usage := &agent.Result{
		Usage: agent.TokenUsage{InputTokens: 31_000, OutputTokens: 500, CacheReadTokens: 20_000,
			CacheCreationTokens: 1_000, Reported: true, CacheCreationReported: true},
		UsageReported: true,
	}
	for _, tc := range []struct {
		name     string
		inner    agent.Agent
		attempts int64
	}{
		{"returned usage", &fixedResultAgent{result: usage}, 1},
		{"retried usage", &retryingAgent{attempts: []*agent.Result{usage, usage}}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			observed := &observedAgent{inner: tc.inner}
			if _, err := observed.Run(context.Background(), agent.RunOpts{}); err != nil {
				t.Fatal(err)
			}
			rows := []Evaluation{{TokensReported: !observed.usageMissing,
				FreshInputTokens: int64(observed.freshInputTokens), OutputTokens: int64(observed.usage.OutputTokens),
				CacheReadTokens: int64(observed.usage.CacheReadTokens), CacheWriteTokens: int64(observed.usage.CacheCreationTokens)}}
			total, reads, writes, ok := averageTokens(rows)
			if !ok || total != float64(31_500*tc.attempts) || reads != float64(20_000*tc.attempts) || writes != float64(1_000*tc.attempts) || rows[0].FreshInputTokens != 10_000*tc.attempts {
				t.Fatalf("cost = %v/%v/%v fresh %d reported %v", total, reads, writes, rows[0].FreshInputTokens, ok)
			}
		})
	}
}

// reportsForEvaluations drives the real report path: every evaluation is
// persisted through the store (JSON payload plus registry row) exactly as a
// replay persists one, then rendered.
func reportsForEvaluations(t *testing.T, evaluations []Evaluation) []CandidateReport {
	t.Helper()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, evaluation := range evaluations {
		caseID := evaluation.Candidate + "-case"
		caseDir := store.caseDir(caseID)
		if err := os.MkdirAll(caseDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := writeJSON(filepath.Join(caseDir, "labels.json"), Labels{Version: labelsVersion}); err != nil {
			t.Fatal(err)
		}
		c := Case{Manifest: Manifest{ID: caseID, SourceRunID: "run-" + evaluation.Candidate, SourceRoundID: "round-" + evaluation.Candidate}, Dir: caseDir}
		if err := store.registerCase(c); err != nil {
			t.Fatal(err)
		}
		evaluation.ID = "evaluation-" + evaluation.Candidate
		evaluation.SessionID = "session"
		evaluation.CaseID = c.ID
		evaluation.Repeat = 1
		if err := store.persistEvaluation(c, evaluation); err != nil {
			t.Fatal(err)
		}
	}
	reports, err := Report(store)
	if err != nil {
		t.Fatal(err)
	}
	return reports
}

// TestRenderReportIncludesCacheTokensInTokenCost pins the report's token
// line to the honest total: cache reads dominate review cost, so a line that
// prints only fresh-input and output understates cost by orders of magnitude
// and ranks the recall-vs-cost frontier by the wrong number.
func TestRenderReportIncludesCacheTokensInTokenCost(t *testing.T) {
	evaluations := []Evaluation{{
		Candidate: "claude+test", Status: "completed", TokensReported: true,
		InputTokens: 2_934_114, OutputTokens: 184, CacheReadTokens: 2_921_704, CacheWriteTokens: 12_340,
		FreshInputTokens: 70, DurationMS: 1000,
	}}
	output := RenderReport(reportsForEvaluations(t, evaluations))
	want := "token cost: 254 fresh-input + output + 2921704 cache-read + 12340 cache-write = 2934298 tokens per reported replay"
	if !strings.Contains(output, want) {
		t.Fatalf("report = %q, want %q", output, want)
	}

	// A candidate whose agent never reports cache keeps the historical line
	// instead of printing fabricated zeros.
	legacy := []Evaluation{{
		Candidate: "pi+test", Status: "completed", TokensReported: true,
		InputTokens: 60, OutputTokens: 40, FreshInputTokens: 60, DurationMS: 1000,
	}}
	legacyOutput := RenderReport(reportsForEvaluations(t, legacy))
	if strings.Contains(legacyOutput, "cache-read") {
		t.Fatalf("report = %q, want no cache segment when no replay reported cache tokens", legacyOutput)
	}
	if !strings.Contains(legacyOutput, "fresh-input + output tokens per reported replay") {
		t.Fatalf("report = %q, want the historical fresh-input + output line", legacyOutput)
	}
}

func TestRenderReportCacheSegmentsAddToTotal(t *testing.T) {
	for _, tc := range []struct {
		name          string
		reads, writes int64
		want          string
	}{
		{"read only", 20, 0, "12 fresh-input + output + 20 cache-read = 32 tokens per reported replay"},
		{"write only", 0, 30, "12 fresh-input + output + 30 cache-write = 42 tokens per reported replay"},
		{"both", 20, 30, "12 fresh-input + output + 20 cache-read + 30 cache-write = 62 tokens per reported replay"},
		{"neither", 0, 0, "12 fresh-input + output tokens per reported replay"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reports := reportsForEvaluations(t, []Evaluation{{Candidate: "test", Status: "completed", TokensReported: true,
				InputTokens: 10 + tc.reads + tc.writes, FreshInputTokens: 10, OutputTokens: 2, CacheReadTokens: tc.reads, CacheWriteTokens: tc.writes}})
			output := RenderReport(reports)
			if !strings.Contains(output, tc.want) {
				t.Fatalf("report = %q, want %q", output, tc.want)
			}
		})
	}
	total, writes := 42.0, 30.0
	output := RenderReport([]CandidateReport{{AverageTokens: &total, AverageCacheWriteTokens: &writes}})
	if !strings.Contains(output, "12 fresh-input + output + 30 cache-write = 42 tokens per reported replay") {
		t.Fatalf("write-only report = %q", output)
	}
}

// TestRenderReportFrontierRanksOnTrueCost proves the frontier compares the
// cache-inclusive total: an arm burning millions of cache-read tokens must
// not read as cheaper than one that spent them honestly.
func TestRenderReportFrontierRanksOnTrueCost(t *testing.T) {
	cacheHeavy := Evaluation{Candidate: "cache-heavy", Status: "completed", TokensReported: true,
		InputTokens: 2_921_774, OutputTokens: 184, CacheReadTokens: 2_921_704, FreshInputTokens: 70, DurationMS: 1000, HasFindingGold: true, GoldCount: 2, TruePositive: 2}
	freshHeavy := Evaluation{Candidate: "fresh-heavy", Status: "completed", TokensReported: true,
		InputTokens: 1_500_000, OutputTokens: 40_000, FreshInputTokens: 1_500_000, DurationMS: 1000, HasFindingGold: true, GoldCount: 2, TruePositive: 2}
	reports := reportsForEvaluations(t, []Evaluation{cacheHeavy, freshHeavy})
	if len(reports) != 2 {
		t.Fatalf("reports = %#v, want both candidates", reports)
	}
	if reports[0].Summary.Candidate != "cache-heavy" || reports[0].OnFrontier {
		t.Fatalf("cache-heavy report = %#v, want outside frontier", reports[0])
	}
	if reports[1].Summary.Candidate != "fresh-heavy" || !reports[1].OnFrontier {
		t.Fatalf("fresh-heavy report = %#v, want on frontier", reports[1])
	}
}

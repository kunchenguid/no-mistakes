package db

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func canaryFindingsJSON(n int) string {
	items := make([]string, n)
	for i := range items {
		items[i] = fmt.Sprintf(`{"id":"f%d","severity":"warning","description":"d","action":"auto-fix"}`, i)
	}
	return `{"findings":[` + strings.Join(items, ",") + `],"summary":"s"}`
}

// seedCompletedCanaryRun creates a completed run with one agent-bearing review
// round of the given execution duration, plus escalation/failover/finding facts.
func seedCompletedCanaryRun(t *testing.T, d *DB, repoID string, execMS int64, tier, candIndex, findings int) string {
	t.Helper()
	run, err := d.InsertRun(repoID, "feature", "head-"+newID(), "base-"+newID())
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	step, err := d.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatalf("insert step: %v", err)
	}
	var initialFindings *string
	if findings > 0 {
		value := canaryFindingsJSON(findings)
		initialFindings = &value
		if err := d.SetStepFindings(step.ID, canaryFindingsJSON(findings)); err != nil {
			t.Fatalf("set findings: %v", err)
		}
	}
	round, err := d.ReserveStepRound(step.ID, 1, "initial")
	if err != nil {
		t.Fatalf("reserve round: %v", err)
	}
	attemptID, err := d.StartInvocationAttempt(types.InvocationAttemptStart{
		Purpose:      types.PurposeInitialReview,
		Role:         types.InvocationRoleVerifier,
		Scope:        types.InvocationScope{Kind: types.InvocationScopePipeline, RunID: run.ID, StepResultID: step.ID, StepRoundID: round.ID},
		CandidateKey: "codex:review_strong:0",
		Candidate:    types.InvocationCandidate{Profile: "review_strong", Tier: tier, CandidateIndex: candIndex, Runner: types.RunnerCodex, Model: "gpt-5.6-sol", Effort: types.EffortHigh},
	})
	if err != nil {
		t.Fatalf("start attempt: %v", err)
	}
	if err := d.FinishInvocationAttempt(attemptID, types.InvocationAttemptTerminal{Outcome: types.InvocationOutcomeSucceeded, DurationMS: execMS}); err != nil {
		t.Fatalf("finish attempt: %v", err)
	}
	if err := d.CompleteReservedStepRound(round.ID, initialFindings, nil, execMS); err != nil {
		t.Fatalf("complete round: %v", err)
	}
	if err := d.UpdateRunStatus(run.ID, types.RunCompleted); err != nil {
		t.Fatalf("complete run: %v", err)
	}
	return run.ID
}

func TestComputeCanaryRunFactsRetainsInitialReviewFindings(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepoWithID("repo-1", "/tmp/repo", "origin", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	step, err := d.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	initial := canaryFindingsJSON(2)
	if err := d.SetStepFindings(step.ID, initial); err != nil {
		t.Fatal(err)
	}
	round, err := d.ReserveStepRound(step.ID, 1, "initial")
	if err != nil {
		t.Fatal(err)
	}
	if err := d.CompleteReservedStepRound(round.ID, &initial, nil, 0); err != nil {
		t.Fatal(err)
	}
	if err := d.ClearStepFindings(step.ID); err != nil {
		t.Fatal(err)
	}
	facts, err := computeCanaryRunFacts(d.sql, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if facts.InitialFindings != 2 {
		t.Fatalf("initial findings = %d, want 2 after repair clears current findings", facts.InitialFindings)
	}
}

func setRunUpdatedAt(t *testing.T, d *DB, runID string, ts int64) {
	t.Helper()
	if _, err := d.sql.Exec(`UPDATE runs SET updated_at = ? WHERE id = ?`, ts, runID); err != nil {
		t.Fatalf("set run updated_at: %v", err)
	}
}

func canaryActivatedAt(t *testing.T, d *DB) int64 {
	t.Helper()
	var ts int64
	if err := d.sql.QueryRow(`SELECT activated_at FROM canary_activation WHERE id = 1`).Scan(&ts); err != nil {
		t.Fatalf("read activated_at: %v", err)
	}
	return ts
}

func TestActivateCanaryFreezesTenMostRecentIdempotently(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepoWithID("repo-1", "/tmp/repo", "origin", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	// Twelve completed runs; control updated_at so recency ordering is exact.
	var ids []string
	for i := 0; i < 12; i++ {
		id := seedCompletedCanaryRun(t, d, repo.ID, int64((i+1)*1000), 0, 0, 0)
		setRunUpdatedAt(t, d, id, int64(1000+i)) // later i == more recent
		ids = append(ids, id)
	}

	activated, err := d.ActivateCanary("fp1", nil)
	if err != nil {
		t.Fatalf("activate: %v", err)
	}
	if !activated {
		t.Fatal("first activation should report performed")
	}

	report, err := d.GetCanaryReport()
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if !report.Activated {
		t.Fatal("report should be activated")
	}
	if len(report.Baseline.Runs) != 10 || !report.Baseline.Complete {
		t.Fatalf("baseline = %d runs (complete=%v), want 10 complete", len(report.Baseline.Runs), report.Baseline.Complete)
	}
	// Kept runs are i=2..11 (exec 3000..12000); even-set median = (7000+8000)/2.
	if report.Baseline.MedianExecMS != 7500 {
		t.Fatalf("baseline median = %d, want 7500", report.Baseline.MedianExecMS)
	}
	for _, r := range report.Baseline.Runs {
		if r.RunID == ids[0] || r.RunID == ids[1] {
			t.Fatalf("baseline included an excluded oldest run %s", r.RunID)
		}
	}

	// Idempotent: a second activation is a no-op and never re-freezes.
	activated2, err := d.ActivateCanary("fp2", nil)
	if err != nil {
		t.Fatalf("re-activate: %v", err)
	}
	if activated2 {
		t.Fatal("second activation should be a no-op")
	}
	report2, err := d.GetCanaryReport()
	if err != nil {
		t.Fatalf("report2: %v", err)
	}
	if report2.Fingerprint != "fp1" || len(report2.Baseline.Runs) != 10 {
		t.Fatalf("baseline changed after re-activation: fp=%q runs=%d", report2.Fingerprint, len(report2.Baseline.Runs))
	}
}

func TestRecordRoutedRunEntersCohortOnceWithExclusions(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepoWithID("repo-1", "/tmp/repo", "origin", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	// Pre-activation completed run: excluded by the durable completion fence.
	pre := seedCompletedCanaryRun(t, d, repo.ID, 1000, 0, 0, 0)
	if _, err := d.ActivateCanary("fp", nil); err != nil {
		t.Fatalf("activate: %v", err)
	}
	at := canaryActivatedAt(t, d)

	if added, err := d.RecordRoutedRunInCanary(pre, -1, -1); err != nil || added {
		t.Fatalf("pre-activation run added=%v err=%v, want not added", added, err)
	}

	// Failed run after activation: excluded.
	failedRun, err := d.InsertRun(repo.ID, "feature", "h", "b")
	if err != nil {
		t.Fatalf("insert failed run: %v", err)
	}
	if err := d.UpdateRunErrorStatus(failedRun.ID, "boom", types.RunFailed); err != nil {
		t.Fatalf("fail run: %v", err)
	}
	setRunUpdatedAt(t, d, failedRun.ID, at+10)
	if added, err := d.RecordRoutedRunInCanary(failedRun.ID, -1, -1); err != nil || added {
		t.Fatalf("failed run added=%v err=%v, want not added", added, err)
	}

	// Successful run after activation: added once, duplicate ignored.
	ok := seedCompletedCanaryRun(t, d, repo.ID, 2000, 0, 0, 0)
	setRunUpdatedAt(t, d, ok, at+20)
	if added, err := d.RecordRoutedRunInCanary(ok, 3, 40); err != nil || !added {
		t.Fatalf("successful run added=%v err=%v, want added", added, err)
	}
	if added, _ := d.RecordRoutedRunInCanary(ok, 3, 40); added {
		t.Fatal("duplicate run must not be added again")
	}

	report, err := d.GetCanaryReport()
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if len(report.Routed.Runs) != 1 {
		t.Fatalf("routed cohort = %d, want 1", len(report.Routed.Runs))
	}
	if report.Routed.Runs[0].ChangedFiles != 3 || report.Routed.Runs[0].ChangedLines != 40 {
		t.Fatalf("changed stats not recorded: %+v", report.Routed.Runs[0])
	}
}

func TestRecordRoutedRunUsesCompletionOrderFenceAtEqualTimestamps(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepoWithID("repo-1", "/tmp/repo", "origin", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}

	preFence := seedCompletedCanaryRun(t, d, repo.ID, 1000, 0, 0, 0)
	if _, err := d.ActivateCanary("fp", nil); err != nil {
		t.Fatalf("activate: %v", err)
	}
	at := canaryActivatedAt(t, d)
	setRunUpdatedAt(t, d, preFence, at)

	postFence := seedCompletedCanaryRun(t, d, repo.ID, 2000, 0, 0, 0)
	setRunUpdatedAt(t, d, postFence, at)

	if added, err := d.RecordRoutedRunInCanary(preFence, -1, -1); err != nil || added {
		t.Fatalf("equal-timestamp pre-fence run added=%v err=%v, want excluded", added, err)
	}
	if added, err := d.RecordRoutedRunInCanary(postFence, -1, -1); err != nil || !added {
		t.Fatalf("equal-timestamp post-fence run added=%v err=%v, want admitted", added, err)
	}

	report, err := d.GetCanaryReport()
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if len(report.Routed.Runs) != 1 || report.Routed.Runs[0].RunID != postFence {
		t.Fatalf("routed cohort = %+v, want only post-fence run %s", report.Routed.Runs, postFence)
	}
}

func TestRecordRoutedRunSelectsFirstTenConcurrentEqualTimestampCompletions(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepoWithID("repo-1", "/tmp/repo", "origin", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	if _, err := d.ActivateCanary("fp", nil); err != nil {
		t.Fatalf("activate: %v", err)
	}
	at := canaryActivatedAt(t, d)

	runIDs := make([]string, 12)
	for i := range runIDs {
		runIDs[i] = seedCompletedCanaryRun(t, d, repo.ID, int64(1000+i), 0, 0, 0)
		setRunUpdatedAt(t, d, runIDs[i], at)
	}
	start := make(chan struct{})
	added := make([]bool, len(runIDs))
	errs := make([]error, len(runIDs))
	var wg sync.WaitGroup
	for i := range runIDs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			added[i], errs[i] = d.RecordRoutedRunInCanary(runIDs[i], -1, -1)
		}()
	}
	close(start)
	wg.Wait()
	for i := range runIDs {
		if errs[i] != nil {
			t.Fatalf("offer completion %d: %v", i, errs[i])
		}
		wantAdded := i < canaryCohortSize
		if added[i] != wantAdded {
			t.Fatalf("completion %d added=%v, want %v", i, added[i], wantAdded)
		}
	}

	report, err := d.GetCanaryReport()
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if len(report.Routed.Runs) != canaryCohortSize || !report.Routed.Complete {
		t.Fatalf("routed cohort = %d (complete=%v), want 10 complete", len(report.Routed.Runs), report.Routed.Complete)
	}
	for i, fact := range report.Routed.Runs {
		if fact.RunID != runIDs[i] {
			t.Fatalf("routed position %d = %s, want completion-order run %s", i, fact.RunID, runIDs[i])
		}
	}
}

func TestCanaryReportTargetPendingUntilBothCohortsComplete(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepoWithID("repo-1", "/tmp/repo", "origin", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}

	dormant, err := d.GetCanaryReport()
	if err != nil {
		t.Fatalf("dormant report: %v", err)
	}
	if dormant.Activated || dormant.Met != nil {
		t.Fatalf("dormant report activated=%v met=%v", dormant.Activated, dormant.Met)
	}

	for range canaryCohortSize {
		seedCompletedCanaryRun(t, d, repo.ID, 20000, 0, 0, 0)
	}
	if _, err := d.ActivateCanary("fp", nil); err != nil {
		t.Fatalf("activate: %v", err)
	}
	report, err := d.GetCanaryReport()
	if err != nil {
		t.Fatalf("empty routed report: %v", err)
	}
	if !report.Baseline.Complete || report.Routed.Complete || report.Met != nil {
		t.Fatalf("empty routed state: baseline_complete=%v routed_complete=%v met=%v", report.Baseline.Complete, report.Routed.Complete, report.Met)
	}

	for i := range canaryCohortSize - 1 {
		execMS := int64(12000)
		if i >= 5 {
			execMS = 16000
		}
		id := seedCompletedCanaryRun(t, d, repo.ID, execMS, 0, 0, 0)
		if added, err := d.RecordRoutedRunInCanary(id, -1, -1); err != nil || !added {
			t.Fatalf("routed run %d added=%v err=%v", i, added, err)
		}
	}
	preliminary, err := d.GetCanaryReport()
	if err != nil {
		t.Fatalf("preliminary report: %v", err)
	}
	if preliminary.Routed.Complete || preliminary.Met != nil {
		t.Fatalf("nine-run report complete=%v met=%v, want incomplete and pending", preliminary.Routed.Complete, preliminary.Met)
	}

	tenth := seedCompletedCanaryRun(t, d, repo.ID, 16000, 0, 0, 0)
	if added, err := d.RecordRoutedRunInCanary(tenth, -1, -1); err != nil || !added {
		t.Fatalf("tenth routed run added=%v err=%v", added, err)
	}
	complete, err := d.GetCanaryReport()
	if err != nil {
		t.Fatalf("complete report: %v", err)
	}
	if !complete.Routed.Complete || complete.Routed.MedianExecMS != 14000 {
		t.Fatalf("complete routed state: complete=%v median=%d, want true/14000", complete.Routed.Complete, complete.Routed.MedianExecMS)
	}
	if complete.Met == nil || !*complete.Met {
		t.Fatalf("30%% advisory target should be met at exact threshold 14000 <= 70%% of 20000; met=%v", complete.Met)
	}
}

func TestCanaryReportCompleteCohortCanMissAdvisoryTarget(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepoWithID("repo-1", "/tmp/repo", "origin", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	for range canaryCohortSize {
		seedCompletedCanaryRun(t, d, repo.ID, 10000, 0, 0, 0)
	}
	if _, err := d.ActivateCanary("fp", nil); err != nil {
		t.Fatalf("activate: %v", err)
	}
	for i := range canaryCohortSize {
		id := seedCompletedCanaryRun(t, d, repo.ID, 9000, 0, 0, 0)
		if added, err := d.RecordRoutedRunInCanary(id, -1, -1); err != nil || !added {
			t.Fatalf("routed run %d added=%v err=%v", i, added, err)
		}
	}
	report, err := d.GetCanaryReport()
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if report.Met == nil || *report.Met {
		t.Fatalf("complete target should be missed (routed 9000 > 7000); met=%v", report.Met)
	}
}

func TestCanaryRunFactsCaptureSupplements(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepoWithID("repo-1", "/tmp/repo", "origin", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	if _, err := d.ActivateCanary("fp", nil); err != nil {
		t.Fatalf("activate: %v", err)
	}
	at := canaryActivatedAt(t, d)
	// exec 5000ms, escalated tier (tier>0), backup candidate (index>0), 3 findings.
	id := seedCompletedCanaryRun(t, d, repo.ID, 5000, 2, 1, 3)
	setRunUpdatedAt(t, d, id, at+10)
	if added, err := d.RecordRoutedRunInCanary(id, 7, 88); err != nil || !added {
		t.Fatalf("added=%v err=%v", added, err)
	}
	report, err := d.GetCanaryReport()
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if len(report.Routed.Runs) != 1 {
		t.Fatalf("routed cohort = %d, want 1", len(report.Routed.Runs))
	}
	f := report.Routed.Runs[0]
	if f.ExecutionMS != 5000 {
		t.Errorf("execution metric = %d, want 5000", f.ExecutionMS)
	}
	if f.InvocationMS != 5000 {
		t.Errorf("invocation duration = %d, want 5000", f.InvocationMS)
	}
	if f.Escalations != 1 {
		t.Errorf("escalations = %d, want 1", f.Escalations)
	}
	if f.Failovers != 1 {
		t.Errorf("failovers = %d, want 1", f.Failovers)
	}
	if f.InitialFindings != 3 {
		t.Errorf("initial findings = %d, want 3", f.InitialFindings)
	}
	if f.ChangedFiles != 7 || f.ChangedLines != 88 {
		t.Errorf("changed files/lines = %d/%d, want 7/88", f.ChangedFiles, f.ChangedLines)
	}
}

func TestCanaryCompletionFenceMigratesLegacyActivationDeterministically(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "legacy.sqlite")
	legacy, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(wal)&_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	if _, err := legacy.Exec(`
		CREATE TABLE runs (
			id TEXT PRIMARY KEY,
			repo_id TEXT NOT NULL,
			branch TEXT NOT NULL,
			head_sha TEXT NOT NULL,
			base_sha TEXT NOT NULL,
			status TEXT NOT NULL,
			pr_url TEXT,
			error TEXT,
			awaiting_agent_since INTEGER,
			intent TEXT,
			intent_source TEXT,
			intent_session_id TEXT,
			intent_score REAL,
			created_at INTEGER NOT NULL,
			updated_at INTEGER NOT NULL
		);
		CREATE TABLE canary_activation (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			activated_at INTEGER NOT NULL,
			fingerprint TEXT NOT NULL DEFAULT ''
		);
		INSERT INTO runs (id, repo_id, branch, head_sha, base_sha, status, created_at, updated_at)
		VALUES
			('run-b', 'repo-1', 'feature', 'b-head', 'base', 'completed', 1, 100),
			('run-a', 'repo-1', 'feature', 'a-head', 'base', 'completed', 1, 100),
			('run-c', 'repo-1', 'feature', 'c-head', 'base', 'running', 1, 100);
		INSERT INTO canary_activation (id, activated_at, fingerprint) VALUES (1, 100, 'legacy-fp');
	`); err != nil {
		legacy.Close()
		t.Fatalf("seed legacy canary: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	d, err := Open(dbPath)
	if err != nil {
		t.Fatalf("migrate legacy db: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	var fence, aSequence, bSequence int64
	if err := d.sql.QueryRow(`SELECT completion_fence FROM canary_activation WHERE id = 1`).Scan(&fence); err != nil {
		t.Fatalf("read migrated fence: %v", err)
	}
	if err := d.sql.QueryRow(`SELECT sequence FROM run_completion_order WHERE run_id = 'run-a'`).Scan(&aSequence); err != nil {
		t.Fatalf("read run-a sequence: %v", err)
	}
	if err := d.sql.QueryRow(`SELECT sequence FROM run_completion_order WHERE run_id = 'run-b'`).Scan(&bSequence); err != nil {
		t.Fatalf("read run-b sequence: %v", err)
	}
	if aSequence != 1 || bSequence != 2 || fence != 2 {
		t.Fatalf("legacy completion order a=%d b=%d fence=%d, want 1/2/2", aSequence, bSequence, fence)
	}
	if _, err := d.sql.Exec(`
		INSERT INTO canary_cohort_runs
			(cohort, position, run_id, completed_at, execution_ms, invocation_ms, escalations, failovers, changed_files, changed_lines, initial_findings, created_at)
		VALUES ('routed', 0, 'run-a', 100, 0, 0, 0, 0, -1, -1, 0, 100)`); err != nil {
		t.Fatalf("restore legacy routed cohort: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("close migrated db: %v", err)
	}
	d, err = Open(dbPath)
	if err != nil {
		t.Fatalf("reopen migrated db: %v", err)
	}

	if err := d.UpdateRunStatus("run-c", types.RunCompleted); err != nil {
		t.Fatalf("complete post-migration run: %v", err)
	}
	var cSequence int64
	if err := d.sql.QueryRow(`SELECT sequence FROM run_completion_order WHERE run_id = 'run-c'`).Scan(&cSequence); err != nil {
		t.Fatalf("read run-c sequence: %v", err)
	}
	if cSequence != 3 {
		t.Fatalf("post-migration completion sequence = %d, want 3", cSequence)
	}
	if added, err := d.RecordRoutedRunInCanary("run-c", -1, -1); err != nil || !added {
		t.Fatalf("post-migration routed run added=%v err=%v, want appended after legacy member", added, err)
	}
	var position int
	if err := d.sql.QueryRow(`SELECT position FROM canary_cohort_runs WHERE cohort = 'routed' AND run_id = 'run-c'`).Scan(&position); err != nil {
		t.Fatalf("read post-migration routed position: %v", err)
	}
	if position != 1 {
		t.Fatalf("post-migration routed position = %d, want 1 after legacy member", position)
	}
}

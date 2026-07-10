package db

import (
	"fmt"
	"strings"
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
	if findings > 0 {
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
	if err := d.CompleteReservedStepRound(round.ID, nil, nil, execMS); err != nil {
		t.Fatalf("complete round: %v", err)
	}
	if err := d.UpdateRunStatus(run.ID, types.RunCompleted); err != nil {
		t.Fatalf("complete run: %v", err)
	}
	return run.ID
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
	if _, err := d.ActivateCanary("fp", nil); err != nil {
		t.Fatalf("activate: %v", err)
	}
	at := canaryActivatedAt(t, d)

	// Pre-activation completed run: excluded.
	pre := seedCompletedCanaryRun(t, d, repo.ID, 1000, 0, 0, 0)
	setRunUpdatedAt(t, d, pre, at-10)
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

func TestRecordRoutedRunCapsCohortAtTen(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepoWithID("repo-1", "/tmp/repo", "origin", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	if _, err := d.ActivateCanary("fp", nil); err != nil {
		t.Fatalf("activate: %v", err)
	}
	at := canaryActivatedAt(t, d)
	for i := 0; i < 10; i++ {
		id := seedCompletedCanaryRun(t, d, repo.ID, 1000, 0, 0, 0)
		setRunUpdatedAt(t, d, id, at+int64(1+i))
		if added, err := d.RecordRoutedRunInCanary(id, -1, -1); err != nil || !added {
			t.Fatalf("routed run %d added=%v err=%v, want added", i, added, err)
		}
	}
	overflow := seedCompletedCanaryRun(t, d, repo.ID, 1000, 0, 0, 0)
	setRunUpdatedAt(t, d, overflow, at+100)
	if added, err := d.RecordRoutedRunInCanary(overflow, -1, -1); err != nil || added {
		t.Fatalf("eleventh routed run added=%v err=%v, want not added (cohort full)", added, err)
	}
	report, err := d.GetCanaryReport()
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if len(report.Routed.Runs) != 10 || !report.Routed.Complete {
		t.Fatalf("routed cohort = %d (complete=%v), want 10 complete", len(report.Routed.Runs), report.Routed.Complete)
	}
}

func TestCanaryReportTargetAndDormancy(t *testing.T) {
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

	// Baseline of three: exec 10000/20000/30000 -> median 20000 (odd set).
	for i, ms := range []int64{10000, 20000, 30000} {
		id := seedCompletedCanaryRun(t, d, repo.ID, ms, 0, 0, 0)
		setRunUpdatedAt(t, d, id, int64(100+i))
	}
	if _, err := d.ActivateCanary("fp", nil); err != nil {
		t.Fatalf("activate: %v", err)
	}
	at := canaryActivatedAt(t, d)

	r0, err := d.GetCanaryReport()
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if r0.Met != nil {
		t.Fatal("Met must be nil with an empty routed cohort")
	}
	if r0.Baseline.Complete {
		t.Fatal("a baseline of three must report incomplete")
	}
	if r0.Baseline.MedianExecMS != 20000 {
		t.Fatalf("baseline median = %d, want 20000", r0.Baseline.MedianExecMS)
	}

	// Routed of three: exec 10000/12000/14000 -> median 12000 <= 70% of 20000.
	for i, ms := range []int64{10000, 12000, 14000} {
		id := seedCompletedCanaryRun(t, d, repo.ID, ms, 0, 0, 0)
		setRunUpdatedAt(t, d, id, at+int64(10+i))
		if added, err := d.RecordRoutedRunInCanary(id, -1, -1); err != nil || !added {
			t.Fatalf("routed run %d added=%v err=%v", i, added, err)
		}
	}
	rm, err := d.GetCanaryReport()
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if rm.Routed.MedianExecMS != 12000 {
		t.Fatalf("routed median = %d, want 12000", rm.Routed.MedianExecMS)
	}
	if rm.Met == nil || !*rm.Met {
		t.Fatalf("target should be met (baseline 20000, routed 12000 <= 14000); met=%v", rm.Met)
	}
}

func TestCanaryReportTargetMissed(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepoWithID("repo-1", "/tmp/repo", "origin", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	// Baseline median 10000; routed median 9000 > 7000 (70%): target missed.
	base := seedCompletedCanaryRun(t, d, repo.ID, 10000, 0, 0, 0)
	setRunUpdatedAt(t, d, base, 100)
	if _, err := d.ActivateCanary("fp", nil); err != nil {
		t.Fatalf("activate: %v", err)
	}
	at := canaryActivatedAt(t, d)
	routed := seedCompletedCanaryRun(t, d, repo.ID, 9000, 0, 0, 0)
	setRunUpdatedAt(t, d, routed, at+10)
	if added, err := d.RecordRoutedRunInCanary(routed, -1, -1); err != nil || !added {
		t.Fatalf("routed run added=%v err=%v", added, err)
	}
	report, err := d.GetCanaryReport()
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if report.Met == nil || *report.Met {
		t.Fatalf("target should be missed (routed 9000 > 7000); met=%v", report.Met)
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

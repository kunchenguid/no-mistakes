package pipeline

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestExecutorRejectsCompletionWhenCanaryActivationCannotPersist(t *testing.T) {
	database, p, firstRun, repo := setupTest(t)
	workDir := gitInitTestDir(t)
	cfg := &config.Config{Routing: config.DefaultRoutingConfig()}
	steps := []Step{
		newPassStep(types.StepReview),
		newPassStep(types.StepTest),
		newPassStep(types.StepLint),
	}

	faultDB, err := sql.Open("sqlite", p.DB())
	if err != nil {
		t.Fatalf("open fault-injection connection: %v", err)
	}
	t.Cleanup(func() { faultDB.Close() })
	if _, err := faultDB.Exec(`
		CREATE TRIGGER reject_canary_activation
		BEFORE INSERT ON canary_activation
		BEGIN
			SELECT RAISE(FAIL, 'injected canary activation failure');
		END;
	`); err != nil {
		t.Fatalf("install activation failure: %v", err)
	}

	exec := NewExecutor(database, p, cfg, nil, steps, nil)
	err = exec.Execute(context.Background(), firstRun, repo, workDir)
	if err == nil || !strings.Contains(err.Error(), "canary activation") {
		t.Fatalf("Execute() error = %v, want canary activation failure", err)
	}
	persistedFirst, err := database.GetRun(firstRun.ID)
	if err != nil {
		t.Fatalf("get rejected run: %v", err)
	}
	if persistedFirst.Status == types.RunCompleted {
		t.Fatal("run completed even though activation and fingerprint persistence failed")
	}
	activated, err := database.IsCanaryActivated()
	if err != nil {
		t.Fatalf("check activation after failure: %v", err)
	}
	if activated {
		t.Fatal("failed activation transaction persisted a partial activation")
	}

	if _, err := faultDB.Exec(`DROP TRIGGER reject_canary_activation`); err != nil {
		t.Fatalf("remove activation failure: %v", err)
	}
	secondRun, err := database.InsertRun(repo.ID, "feature", "second-head", "second-base")
	if err != nil {
		t.Fatalf("insert second run: %v", err)
	}
	if err := exec.Execute(context.Background(), secondRun, repo, workDir); err != nil {
		t.Fatalf("second Execute() error = %v", err)
	}
	report, err := database.GetCanaryReport()
	if err != nil {
		t.Fatalf("canary report: %v", err)
	}
	for _, fact := range report.Baseline.Runs {
		if fact.RunID == firstRun.ID {
			t.Fatalf("activation-failed run %s contaminated the pre-routing baseline", firstRun.ID)
		}
	}
	if len(report.Routed.Runs) != 1 || report.Routed.Runs[0].RunID != secondRun.ID {
		t.Fatalf("routed cohort = %+v, want only accepted run %s", report.Routed.Runs, secondRun.ID)
	}
}

func TestExecutorBackfillsTransientCanaryIntakeFailureWithoutFailingRun(t *testing.T) {
	database, p, firstRun, repo := setupTest(t)
	workDir := gitInitTestDir(t)
	cfg := &config.Config{Routing: config.DefaultRoutingConfig()}
	steps := []Step{
		newPassStep(types.StepReview),
		newPassStep(types.StepTest),
		newPassStep(types.StepLint),
	}
	exec := NewExecutor(database, p, cfg, nil, steps, nil)

	faultDB, err := sql.Open("sqlite", p.DB())
	if err != nil {
		t.Fatalf("open fault-injection connection: %v", err)
	}
	t.Cleanup(func() { faultDB.Close() })
	if _, err := faultDB.Exec(`
		CREATE TRIGGER reject_routed_canary_intake
		BEFORE INSERT ON canary_cohort_runs
		WHEN NEW.cohort = 'routed'
		BEGIN
			SELECT RAISE(FAIL, 'injected routed canary intake failure');
		END;
	`); err != nil {
		t.Fatalf("install intake failure: %v", err)
	}

	if err := exec.Execute(context.Background(), firstRun, repo, workDir); err != nil {
		t.Fatalf("first Execute() error = %v, want advisory intake failure ignored", err)
	}
	persistedFirst, err := database.GetRun(firstRun.ID)
	if err != nil {
		t.Fatalf("get first run: %v", err)
	}
	if persistedFirst.Status != types.RunCompleted {
		t.Fatalf("first run status = %q, want completed despite intake failure", persistedFirst.Status)
	}
	var routedCount int
	if err := faultDB.QueryRow(`SELECT count(*) FROM canary_cohort_runs WHERE cohort = 'routed'`).Scan(&routedCount); err != nil {
		t.Fatalf("count failed intake rows: %v", err)
	}
	if routedCount != 0 {
		t.Fatalf("routed cohort after injected failure = %d, want 0", routedCount)
	}
	if _, err := faultDB.Exec(`DROP TRIGGER reject_routed_canary_intake`); err != nil {
		t.Fatalf("remove intake failure: %v", err)
	}

	runIDs := []string{firstRun.ID}
	for i := 1; i < 10; i++ {
		run, err := database.InsertRun(repo.ID, "feature", fmt.Sprintf("head-%d", i), fmt.Sprintf("base-%d", i))
		if err != nil {
			t.Fatalf("insert run %d: %v", i, err)
		}
		if err := exec.Execute(context.Background(), run, repo, workDir); err != nil {
			t.Fatalf("Execute() run %d error = %v", i, err)
		}
		runIDs = append(runIDs, run.ID)
		if i == 1 {
			if err := faultDB.QueryRow(`SELECT count(*) FROM canary_cohort_runs WHERE cohort = 'routed'`).Scan(&routedCount); err != nil {
				t.Fatalf("count completion-boundary backfill rows: %v", err)
			}
			if routedCount != 2 {
				t.Fatalf("routed cohort after next completion = %d, want failed run and current run backfilled", routedCount)
			}
		}
	}

	report, err := database.GetCanaryReport()
	if err != nil {
		t.Fatalf("canary report: %v", err)
	}
	if !report.Routed.Complete || len(report.Routed.Runs) != len(runIDs) {
		t.Fatalf("routed cohort = %d runs (complete=%v), want ten-run report", len(report.Routed.Runs), report.Routed.Complete)
	}
	for i, fact := range report.Routed.Runs {
		if fact.RunID != runIDs[i] {
			t.Fatalf("routed position %d = %s, want completion-order run %s", i, fact.RunID, runIDs[i])
		}
	}
	if added, err := database.RecordRoutedRunInCanary(firstRun.ID, -1, -1); err != nil || added {
		t.Fatalf("idempotent retry added=%v err=%v, want no-op", added, err)
	}
	retried, err := database.GetCanaryReport()
	if err != nil {
		t.Fatalf("canary report after retry: %v", err)
	}
	if len(retried.Routed.Runs) != len(runIDs) {
		t.Fatalf("routed cohort after retry = %d runs, want %d", len(retried.Routed.Runs), len(runIDs))
	}
	for i, fact := range retried.Routed.Runs {
		if fact.RunID != runIDs[i] {
			t.Fatalf("retried routed position %d = %s, want %s", i, fact.RunID, runIDs[i])
		}
	}
}

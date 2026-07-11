package pipeline

import (
	"context"
	"database/sql"
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

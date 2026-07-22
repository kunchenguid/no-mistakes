package pipeline

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
	_ "modernc.org/sqlite"
)

func TestExecutor_TerminalizesRunWhenInitialStatusWriteFails(t *testing.T) {
	database, p, run, repo := setupTest(t)
	installRunStatusFailureTrigger(t, p.DB(), string(types.RunRunning))
	events := &eventCollector{}
	exec := NewExecutor(database, p, nil, nil, []Step{newPassStep(types.StepReview)}, events.handler)

	err := exec.Execute(context.Background(), run, repo, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "update run status") {
		t.Fatalf("Execute() error = %v", err)
	}
	assertDurableFailedRunAndEvent(t, database, run.ID, events)
}

func TestExecutor_TerminalizesRunWhenFinalCompletedWriteFails(t *testing.T) {
	database, p, run, repo := setupTest(t)
	installRunStatusFailureTrigger(t, p.DB(), string(types.RunCompleted))
	events := &eventCollector{}
	exec := NewExecutor(database, p, nil, nil, []Step{newPassStep(types.StepReview)}, events.handler)

	err := exec.Execute(context.Background(), run, repo, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "update run status") {
		t.Fatalf("Execute() error = %v", err)
	}
	assertDurableFailedRunAndEvent(t, database, run.ID, events)
}

func installRunStatusFailureTrigger(t *testing.T, path, status string) {
	t.Helper()
	raw, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	_, err = raw.Exec(`CREATE TRIGGER reject_test_run_status
		BEFORE UPDATE OF status ON runs
		WHEN NEW.status = '` + status + `'
		BEGIN
			SELECT RAISE(FAIL, 'injected status write failure');
		END`)
	if err != nil {
		t.Fatal(err)
	}
}

func assertDurableFailedRunAndEvent(t *testing.T, database *db.DB, runID string, events *eventCollector) {
	t.Helper()
	got, err := database.GetRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != types.RunFailed {
		t.Fatalf("durable run status = %s, want failed", got.Status)
	}
	completed := events.findRunEvent(ipc.EventRunCompleted)
	if completed == nil || completed.Status == nil || *completed.Status != string(types.RunFailed) {
		t.Fatalf("terminal event = %+v, want failed run_completed", completed)
	}
}

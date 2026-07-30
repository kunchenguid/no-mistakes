package db

import (
	"path/filepath"
	"testing"
)

func TestUpdateRunTaskIDPersistsIDAndFormat(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo("/tmp/repo", "https://github.com/test/repo", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	if run.TaskID != nil || run.TaskIDFormat != nil {
		t.Fatalf("fresh run carries a task id: %v / %v", run.TaskID, run.TaskIDFormat)
	}

	if err := d.UpdateRunTaskID(run.ID, RunTaskID{ID: "WA-3093", Format: "prefix"}); err != nil {
		t.Fatalf("update run task id: %v", err)
	}
	got, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.TaskID == nil || *got.TaskID != "WA-3093" {
		t.Fatalf("task id = %v, want WA-3093", got.TaskID)
	}
	if got.TaskIDFormat == nil || *got.TaskIDFormat != "prefix" {
		t.Fatalf("task id format = %v, want prefix", got.TaskIDFormat)
	}
}

func TestLatestRunTaskIDIsScopedToTheBranchAndReturnsTheNewest(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo("/tmp/repo", "https://github.com/test/repo", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	stamp := func(branch, id, format string) {
		t.Helper()
		run, err := d.InsertRun(repo.ID, branch, "head", "base")
		if err != nil {
			t.Fatalf("insert run: %v", err)
		}
		if id == "" {
			return
		}
		if err := d.UpdateRunTaskID(run.ID, RunTaskID{ID: id, Format: format}); err != nil {
			t.Fatalf("update run task id: %v", err)
		}
	}

	stamp("feature", "WA-1", "prefix")
	stamp("feature", "WA-2", "suffix")
	stamp("feature", "", "")
	stamp("other", "WA-9", "prefix")

	got, err := d.LatestRunTaskID(repo.ID, "feature")
	if err != nil {
		t.Fatalf("latest run task id: %v", err)
	}
	if got == nil || got.ID != "WA-2" || got.Format != "suffix" {
		t.Fatalf("latest = %+v, want WA-2 / suffix", got)
	}

	// A branch nobody ever stamped stays unstamped: inheritance is per branch,
	// never per repository.
	none, err := d.LatestRunTaskID(repo.ID, "untouched")
	if err != nil {
		t.Fatalf("latest run task id: %v", err)
	}
	if none != nil {
		t.Fatalf("latest for an unstamped branch = %+v, want nil", none)
	}
}

func TestOpenMigratesTaskIDColumnsAndLeavesLegacyRunsUnbound(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	d, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	repo, err := d.InsertRepo("/tmp/repo", "https://github.com/test/repo", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	// Simulate a database created before the task-id columns existed, then
	// reopen so the additive migration runs against a legacy row.
	for _, col := range []string{"task_id", "task_id_format"} {
		if _, err := d.sql.Exec(`ALTER TABLE runs DROP COLUMN ` + col); err != nil {
			t.Fatalf("drop %s: %v", col, err)
		}
	}
	if _, err := d.sql.Exec(
		`INSERT INTO runs (id, repo_id, branch, head_sha, base_sha, status, pr_state, created_at, updated_at)
		 VALUES ('legacy1', ?, 'feature', 'head', 'base', 'completed', 'none', 1, 1)`, repo.ID); err != nil {
		t.Fatalf("insert legacy run: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { reopened.Close() })

	run, err := reopened.GetRun("legacy1")
	if err != nil {
		t.Fatalf("get legacy run: %v", err)
	}
	if run == nil {
		t.Fatal("legacy run disappeared after migration")
	}
	if run.TaskID != nil || run.TaskIDFormat != nil {
		t.Fatalf("migration invented a task id for a legacy run: %v / %v", run.TaskID, run.TaskIDFormat)
	}
}

package db

import (
	"path/filepath"
	"testing"
)

func TestRunPriorityDefaultsToOrdinaryAndPersists(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")
	run, err := d.InsertRun(repo.ID, "feature", "abc123", "def456")
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	if run.Priority != 0 {
		t.Fatalf("inserted run priority = %d, want 0", run.Priority)
	}
	got, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Priority != 0 {
		t.Fatalf("stored default priority = %d, want 0", got.Priority)
	}

	if err := d.UpdateRunPriority(run.ID, 20); err != nil {
		t.Fatalf("update priority: %v", err)
	}
	got, err = d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Priority != 20 {
		t.Fatalf("stored priority = %d, want 20", got.Priority)
	}
	active, err := d.GetActiveRun(repo.ID, "feature")
	if err != nil {
		t.Fatal(err)
	}
	if active == nil || active.Priority != 20 {
		t.Fatalf("active run priority = %+v, want 20", active)
	}
}

func TestUpdateRunPriorityRefusesAMissingRun(t *testing.T) {
	d := openTestDB(t)
	if err := d.UpdateRunPriority("missing", 10); err == nil {
		t.Fatal("update priority of a missing run succeeded")
	}
}

// Read-only surfaces (axi status, the axi home view, gate context) open the
// database without migrating it, so during a rollout a newer CLI can read a
// database that an older daemon migrated without the priority column. Those
// reads must keep working and report every run as ordinary.
func TestReadOnlyRunReadsToleratePreviousSchemaWithoutPriority(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.sqlite")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	repo, _ := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")
	run, err := d.InsertRun(repo.ID, "feature", "abc123", "def456")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.sql.Exec(`ALTER TABLE runs DROP COLUMN priority`); err != nil {
		t.Fatalf("simulate a database from before the priority column: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}

	ro, err := OpenReadOnly(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ro.Close() })
	got, err := ro.GetRun(run.ID)
	if err != nil {
		t.Fatalf("read-only GetRun on the previous schema: %v", err)
	}
	if got == nil || got.Priority != 0 {
		t.Fatalf("read-only run = %+v, want the run reported as ordinary", got)
	}
	active, err := ro.GetActiveRun(repo.ID, "feature")
	if err != nil || active == nil {
		t.Fatalf("read-only GetActiveRun on the previous schema = %+v, %v", active, err)
	}
	runs, err := ro.GetRunsByRepo(repo.ID)
	if err != nil || len(runs) != 1 {
		t.Fatalf("read-only GetRunsByRepo on the previous schema = %d runs, %v", len(runs), err)
	}
}

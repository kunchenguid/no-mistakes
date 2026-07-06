package db

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func openTestDB(t *testing.T) *DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.sqlite")
	d, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func TestOpenAndClose(t *testing.T) {
	d := openTestDB(t)
	if d == nil {
		t.Fatal("expected non-nil db")
	}
}

func TestOpenCreatesSchema(t *testing.T) {
	d := openTestDB(t)
	// verify tables exist by querying them
	var count int
	if err := d.sql.QueryRow("SELECT count(*) FROM repos").Scan(&count); err != nil {
		t.Fatalf("repos table missing: %v", err)
	}
	if err := d.sql.QueryRow("SELECT count(*) FROM runs").Scan(&count); err != nil {
		t.Fatalf("runs table missing: %v", err)
	}
	if err := d.sql.QueryRow("SELECT count(*) FROM step_results").Scan(&count); err != nil {
		t.Fatalf("step_results table missing: %v", err)
	}
	if !hasColumn(t, d, "repos", "fork_url") {
		t.Fatal("repos.fork_url column missing from fresh schema")
	}
	for _, column := range []string{"last_activity_at", "last_activity", "agent_pid"} {
		if !hasColumn(t, d, "step_results", column) {
			t.Fatalf("step_results.%s column missing from fresh schema", column)
		}
	}
}

func TestOpenCreatesStepRoundsTable(t *testing.T) {
	d := openTestDB(t)
	var count int
	if err := d.sql.QueryRow("SELECT count(*) FROM step_rounds").Scan(&count); err != nil {
		t.Fatalf("step_rounds table missing: %v", err)
	}
}

func TestOpenMigratesExistingStepRoundsColumns(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.sqlite")

	legacyDB, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(wal)&_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	if _, err := legacyDB.Exec(`
		CREATE TABLE step_rounds (
			id TEXT PRIMARY KEY,
			step_result_id TEXT NOT NULL,
			round INTEGER NOT NULL,
			trigger_type TEXT NOT NULL,
			findings_json TEXT,
			duration_ms INTEGER NOT NULL,
			created_at INTEGER NOT NULL
		);
	`); err != nil {
		legacyDB.Close()
		t.Fatalf("create legacy step_rounds table: %v", err)
	}
	if err := legacyDB.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	d, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open migrated db: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	rows, err := d.sql.Query(`PRAGMA table_info(step_rounds)`)
	if err != nil {
		t.Fatalf("pragma table_info(step_rounds): %v", err)
	}
	defer rows.Close()

	columns := map[string]bool{}
	for rows.Next() {
		var cid int
		var name string
		var colType string
		var notNull int
		var dfltValue any
		var pk int
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dfltValue, &pk); err != nil {
			t.Fatalf("scan table_info: %v", err)
		}
		columns[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate table_info: %v", err)
	}

	for _, name := range []string{"selected_finding_ids", "selection_source", "fix_summary"} {
		if !columns[name] {
			t.Fatalf("expected migrated column %q to exist", name)
		}
	}
}

func TestOpenMigratesReposForkURLColumn(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.sqlite")

	legacyDB, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(wal)&_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	if _, err := legacyDB.Exec(`
		CREATE TABLE repos (
			id TEXT PRIMARY KEY,
			working_path TEXT NOT NULL UNIQUE,
			upstream_url TEXT NOT NULL,
			default_branch TEXT NOT NULL DEFAULT 'main',
			created_at INTEGER NOT NULL
		);
		INSERT INTO repos (id, working_path, upstream_url, default_branch, created_at)
		VALUES ('repo-1', '/work/repo', 'git@github.com:parent/repo.git', 'main', 123);
	`); err != nil {
		legacyDB.Close()
		t.Fatalf("create legacy repos table: %v", err)
	}
	if err := legacyDB.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	d, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open migrated db: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	if !hasColumn(t, d, "repos", "fork_url") {
		t.Fatal("expected migrated fork_url column")
	}
	repo, err := d.GetRepo("repo-1")
	if err != nil {
		t.Fatalf("get migrated repo: %v", err)
	}
	if repo == nil {
		t.Fatal("expected migrated repo")
	}
	if repo.ForkURL != "" {
		t.Fatalf("fork url = %q, want empty", repo.ForkURL)
	}
	updated, err := d.UpdateRepoForkURL(repo.ID, "git@github.com:fork/repo.git")
	if err != nil {
		t.Fatalf("update migrated fork URL: %v", err)
	}
	if updated.ForkURL != "git@github.com:fork/repo.git" {
		t.Fatalf("fork url after update = %q, want fork URL", updated.ForkURL)
	}
}

func TestOpenMigratesStepActivityColumns(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.sqlite")

	legacyDB, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(wal)&_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	if _, err := legacyDB.Exec(`
		CREATE TABLE step_results (
			id TEXT PRIMARY KEY,
			run_id TEXT NOT NULL,
			step_name TEXT NOT NULL,
			step_order INTEGER NOT NULL,
			status TEXT NOT NULL DEFAULT 'pending',
			exit_code INTEGER,
			duration_ms INTEGER,
			log_path TEXT,
			findings_json TEXT,
			error TEXT,
			started_at INTEGER,
			completed_at INTEGER
		);
	`); err != nil {
		legacyDB.Close()
		t.Fatalf("create legacy step_results table: %v", err)
	}
	if err := legacyDB.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	d, err := Open(dbPath)
	if err != nil {
		t.Fatalf("open migrated db: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	for _, column := range []string{"last_activity_at", "last_activity", "agent_pid"} {
		if !hasColumn(t, d, "step_results", column) {
			t.Fatalf("expected migrated column %q", column)
		}
	}
}

func hasColumn(t *testing.T, d *DB, table, column string) bool {
	t.Helper()
	rows, err := d.sql.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		t.Fatalf("pragma table_info(%s): %v", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name string
		var colType string
		var notNull int
		var dfltValue any
		var pk int
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dfltValue, &pk); err != nil {
			t.Fatalf("scan table_info: %v", err)
		}
		if name == column {
			return true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate table_info: %v", err)
	}
	return false
}

func TestOpenWaitsForTransientMigrationLock(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.sqlite")
	locker, err := sql.Open("sqlite", dbPath+"?_pragma=journal_mode(wal)&_pragma=foreign_keys(on)")
	if err != nil {
		t.Fatalf("open locker db: %v", err)
	}
	defer locker.Close()
	if _, err := locker.Exec("BEGIN EXCLUSIVE"); err != nil {
		t.Fatalf("begin exclusive lock: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		d, err := Open(dbPath)
		if err == nil {
			err = d.Close()
		}
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Open returned before the migration lock was released")
		}
		t.Fatalf("Open should wait for a transient migration lock, got: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	if _, err := locker.Exec("COMMIT"); err != nil {
		t.Fatalf("commit exclusive lock: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Open after lock release: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Open did not finish after the migration lock was released")
	}
}

package db

import (
	"path/filepath"
	"testing"
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
}

func TestOpenCreatesStepRoundsTable(t *testing.T) {
	d := openTestDB(t)
	var count int
	if err := d.sql.QueryRow("SELECT count(*) FROM step_rounds").Scan(&count); err != nil {
		t.Fatalf("step_rounds table missing: %v", err)
	}
}

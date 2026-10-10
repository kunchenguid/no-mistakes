package db

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpenExistingDoesNotCreateOrMigrateRecoveryState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "existing.sqlite")
	if _, err := OpenExisting(path); err == nil {
		t.Fatal("created missing recovery state")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("missing file was created")
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	d, err := OpenExisting(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var count int
	if err := d.sql.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table'").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("recovery initialization migrated %d tables", count)
	}
	if _, err := d.GetRunsByRepo("unknown"); err == nil {
		t.Fatal("legacy schema did not fail closed")
	}
}

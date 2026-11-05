package stepstest

import (
	"os"
	"path/filepath"
	"sync"

	"github.com/kunchenguid/no-mistakes/internal/db"
)

// Migrate once, then copy the closed, empty database for each test. Rebuilding
// the schema for every StepContext dominates race-enabled tests on macOS.
var databaseTemplate = sync.OnceValues(func() ([]byte, error) {
	dir, err := os.MkdirTemp("", "steps-db-template-*")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(dir) }()
	path := filepath.Join(dir, "template.db")
	database, err := db.Open(path)
	if err != nil {
		return nil, err
	}
	// Close checkpoints the WAL before we copy the database file.
	if err = database.Close(); err != nil {
		return nil, err
	}
	return os.ReadFile(path)
})

// OpenNewDB creates an isolated, migrated test database at a new path. Recovery
// tests must use db.Open to reopen existing state rather than replace it.
func OpenNewDB(path string) (*db.DB, error) {
	data, err := databaseTemplate()
	if err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	_, writeErr := file.Write(data)
	closeErr := file.Close()
	if writeErr != nil {
		return nil, writeErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return db.Open(path)
}

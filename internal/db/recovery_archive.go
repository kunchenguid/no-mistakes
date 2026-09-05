package db

import (
	"database/sql"
	"fmt"
)

// RecoveryArchive is an immutable provenance snapshot for an existing Git
// archive ref. OwnerRunID associates the record with the recovery lookup;
// RunID is repeated evidence and must independently match before use.
type RecoveryArchive struct {
	ID               string
	OwnerRunID       string
	RepoID           string
	RunID            string
	Branch           string
	RequiredHeadSHA  string
	PreservedHeadSHA string
	ArchiveRef       string
	CreatedAt        int64
}

// RecordRecoveryArchive appends one archive binding. Repeating the exact same
// binding is idempotent, while reusing its owner-run/ref identity with changed
// provenance fails closed. Semantic and Git-object verification belong to the
// branch synchronization service, immediately before this method is called.
func (d *DB) RecordRecoveryArchive(record RecoveryArchive) (*RecoveryArchive, error) {
	if record.ID == "" {
		record.ID = newID()
	}
	if record.OwnerRunID == "" {
		record.OwnerRunID = record.RunID
	}
	if record.CreatedAt == 0 {
		record.CreatedAt = now()
	}

	existing, found, err := d.recoveryArchiveByOwnerAndRef(record.OwnerRunID, record.ArchiveRef)
	if err != nil {
		return nil, err
	}
	if found {
		if sameRecoveryArchiveBinding(existing, &record) {
			return existing, nil
		}
		return nil, fmt.Errorf("recovery archive %s for run %s is already bound with different provenance", record.ArchiveRef, record.OwnerRunID)
	}

	_, err = d.sql.Exec(`
		INSERT INTO recovery_archives
			(id, owner_run_id, repo_id, run_id, branch, required_head_sha, preserved_head_sha, archive_ref, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		record.ID, record.OwnerRunID, record.RepoID, record.RunID, record.Branch,
		record.RequiredHeadSHA, record.PreservedHeadSHA, record.ArchiveRef, record.CreatedAt,
	)
	if err != nil {
		// A concurrent exact retry may have won the unique-key race. Re-read it
		// and retain idempotence without ever replacing the winning evidence.
		existing, found, readErr := d.recoveryArchiveByOwnerAndRef(record.OwnerRunID, record.ArchiveRef)
		if readErr == nil && found && sameRecoveryArchiveBinding(existing, &record) {
			return existing, nil
		}
		return nil, fmt.Errorf("record recovery archive: %w", err)
	}
	return &record, nil
}

// GetRecoveryArchivesByRun returns every record associated with a recovery
// run. Callers require exactly one verified record before it can affect branch
// custody classification.
func (d *DB) GetRecoveryArchivesByRun(ownerRunID string) ([]*RecoveryArchive, error) {
	rows, err := d.sql.Query(`
		SELECT id, owner_run_id, repo_id, run_id, branch, required_head_sha, preserved_head_sha, archive_ref, created_at
		FROM recovery_archives
		WHERE owner_run_id = ?
		ORDER BY created_at, id`, ownerRunID)
	if err != nil {
		return nil, fmt.Errorf("get recovery archives: %w", err)
	}
	defer rows.Close()

	var records []*RecoveryArchive
	for rows.Next() {
		record := &RecoveryArchive{}
		if err := scanRecoveryArchive(rows, record); err != nil {
			return nil, fmt.Errorf("scan recovery archive: %w", err)
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate recovery archives: %w", err)
	}
	return records, nil
}

func (d *DB) recoveryArchiveByOwnerAndRef(ownerRunID, archiveRef string) (*RecoveryArchive, bool, error) {
	record := &RecoveryArchive{}
	err := scanRecoveryArchive(d.sql.QueryRow(`
		SELECT id, owner_run_id, repo_id, run_id, branch, required_head_sha, preserved_head_sha, archive_ref, created_at
		FROM recovery_archives
		WHERE owner_run_id = ? AND archive_ref = ?`, ownerRunID, archiveRef), record)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("get recovery archive: %w", err)
	}
	return record, true, nil
}

func scanRecoveryArchive(row interface{ Scan(...any) error }, record *RecoveryArchive) error {
	return row.Scan(
		&record.ID, &record.OwnerRunID, &record.RepoID, &record.RunID, &record.Branch,
		&record.RequiredHeadSHA, &record.PreservedHeadSHA, &record.ArchiveRef, &record.CreatedAt,
	)
}

func sameRecoveryArchiveBinding(a, b *RecoveryArchive) bool {
	return a != nil && b != nil &&
		a.OwnerRunID == b.OwnerRunID &&
		a.RepoID == b.RepoID &&
		a.RunID == b.RunID &&
		a.Branch == b.Branch &&
		a.RequiredHeadSHA == b.RequiredHeadSHA &&
		a.PreservedHeadSHA == b.PreservedHeadSHA &&
		a.ArchiveRef == b.ArchiveRef
}

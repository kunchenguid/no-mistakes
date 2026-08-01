package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

const (
	GateRefLockStatePrepared = "prepared"
	GateRefLockStateStamped  = "stamped"
)

type GateRefLockJournal struct {
	RunID             string
	RepoID            string
	GatePath          string
	Branch            string
	Ref               string
	LockPath          string
	OwnerGeneration   string
	AuthorityEndpoint string
	ExpectedHead      string
	FileIdentity      string
	State             string
}

func (d *DB) PrepareGateRefLock(journal GateRefLockJournal) error {
	journal.RunID = strings.TrimSpace(journal.RunID)
	journal.RepoID = strings.TrimSpace(journal.RepoID)
	journal.GatePath = strings.TrimSpace(journal.GatePath)
	journal.Branch = strings.TrimSpace(journal.Branch)
	journal.Ref = strings.TrimSpace(journal.Ref)
	journal.LockPath = strings.TrimSpace(journal.LockPath)
	journal.OwnerGeneration = strings.TrimSpace(journal.OwnerGeneration)
	journal.AuthorityEndpoint = strings.TrimSpace(journal.AuthorityEndpoint)
	journal.ExpectedHead = strings.TrimSpace(journal.ExpectedHead)
	journal.FileIdentity = strings.TrimSpace(journal.FileIdentity)
	if journal.RunID == "" || journal.RepoID == "" || journal.GatePath == "" || journal.Branch == "" || journal.Ref == "" || journal.LockPath == "" || journal.OwnerGeneration == "" || journal.AuthorityEndpoint == "" || journal.ExpectedHead == "" || journal.FileIdentity == "" {
		return fmt.Errorf("prepare gate ref lock: exact ownership metadata is required")
	}
	stamp := now()
	_, err := d.sql.Exec(`INSERT INTO gate_ref_locks (run_id, repo_id, gate_path, branch, ref, lock_path, owner_generation, authority_endpoint, expected_head, file_identity, state, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(run_id) DO UPDATE SET repo_id = excluded.repo_id, gate_path = excluded.gate_path, branch = excluded.branch, ref = excluded.ref, lock_path = excluded.lock_path, owner_generation = excluded.owner_generation, authority_endpoint = excluded.authority_endpoint, expected_head = excluded.expected_head, file_identity = excluded.file_identity, state = excluded.state, updated_at = excluded.updated_at`,
		journal.RunID, journal.RepoID, journal.GatePath, journal.Branch, journal.Ref, journal.LockPath, journal.OwnerGeneration, journal.AuthorityEndpoint, journal.ExpectedHead, journal.FileIdentity, GateRefLockStatePrepared, stamp, stamp)
	return err
}

func (d *DB) GetGateRefLock(runID string) (*GateRefLockJournal, error) {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return nil, nil
	}
	var journal GateRefLockJournal
	err := d.sql.QueryRow(`SELECT run_id, repo_id, gate_path, branch, ref, lock_path, owner_generation, authority_endpoint, expected_head, file_identity, state FROM gate_ref_locks WHERE run_id = ?`, runID).Scan(
		&journal.RunID, &journal.RepoID, &journal.GatePath, &journal.Branch, &journal.Ref, &journal.LockPath, &journal.OwnerGeneration, &journal.AuthorityEndpoint, &journal.ExpectedHead, &journal.FileIdentity, &journal.State)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get gate ref lock: %w", err)
	}
	return &journal, nil
}

func (d *DB) MarkGateRefLockStamped(runID, ownerGeneration string) error {
	result, err := d.sql.Exec(`UPDATE gate_ref_locks SET state = ?, updated_at = ? WHERE run_id = ? AND owner_generation = ? AND state = ?`, GateRefLockStateStamped, now(), strings.TrimSpace(runID), strings.TrimSpace(ownerGeneration), GateRefLockStatePrepared)
	if err != nil {
		return fmt.Errorf("mark gate ref lock stamped: %w", err)
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return fmt.Errorf("mark gate ref lock stamped: ownership journal is missing or changed")
	}
	return nil
}

func (d *DB) UpdateGateRefLockIdentity(runID, ownerGeneration, fileIdentity string) error {
	result, err := d.sql.Exec(`UPDATE gate_ref_locks SET file_identity = ?, updated_at = ? WHERE run_id = ? AND owner_generation = ? AND state IN (?, ?)`, strings.TrimSpace(fileIdentity), now(), strings.TrimSpace(runID), strings.TrimSpace(ownerGeneration), GateRefLockStatePrepared, GateRefLockStateStamped)
	if err != nil {
		return fmt.Errorf("update gate ref lock identity: %w", err)
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return fmt.Errorf("update gate ref lock identity: ownership journal is missing or changed")
	}
	return nil
}

func (d *DB) ClearGateRefLock(runID, ownerGeneration string) error {
	_, err := d.sql.Exec(`DELETE FROM gate_ref_locks WHERE run_id = ? AND owner_generation = ?`, strings.TrimSpace(runID), strings.TrimSpace(ownerGeneration))
	if err != nil {
		return fmt.Errorf("clear gate ref lock: %w", err)
	}
	return nil
}

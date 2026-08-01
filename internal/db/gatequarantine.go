package db

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

type GateRefQuarantine struct {
	RepoID       string
	GatePath     string
	Ref          string
	ExpectedHead string
	ObservedHead string
	Reason       string
}

func (d *DB) QuarantineGateRef(repoID, gatePath, ref, expectedHead, observedHead, reason string) error {
	repoID = strings.TrimSpace(repoID)
	gatePath = strings.TrimSpace(gatePath)
	ref = strings.TrimSpace(ref)
	expectedHead = strings.TrimSpace(expectedHead)
	observedHead = strings.TrimSpace(observedHead)
	reason = strings.TrimSpace(reason)
	if repoID == "" || gatePath == "" || ref == "" || reason == "" {
		return fmt.Errorf("quarantine gate ref: exact mismatch evidence is required")
	}
	_, err := d.sql.Exec(`INSERT INTO gate_ref_quarantines (repo_id, gate_path, ref, expected_head, observed_head, reason, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(repo_id, gate_path, ref) DO UPDATE SET expected_head = excluded.expected_head, observed_head = excluded.observed_head, reason = excluded.reason, updated_at = excluded.updated_at`, repoID, gatePath, ref, expectedHead, observedHead, reason, now(), now())
	if err != nil {
		return fmt.Errorf("quarantine gate ref: %w", err)
	}
	return nil
}

func (d *DB) GetGateRefQuarantine(repoID, gatePath, ref string) (*GateRefQuarantine, error) {
	var quarantine GateRefQuarantine
	err := d.sql.QueryRow(`SELECT repo_id, gate_path, ref, expected_head, observed_head, reason FROM gate_ref_quarantines WHERE repo_id = ? AND gate_path = ? AND ref = ?`, strings.TrimSpace(repoID), strings.TrimSpace(gatePath), strings.TrimSpace(ref)).Scan(&quarantine.RepoID, &quarantine.GatePath, &quarantine.Ref, &quarantine.ExpectedHead, &quarantine.ObservedHead, &quarantine.Reason)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get gate ref quarantine: %w", err)
	}
	return &quarantine, nil
}

func (d *DB) ClearGateRefQuarantine(repoID, gatePath, ref string) error {
	_, err := d.sql.Exec(`DELETE FROM gate_ref_quarantines WHERE repo_id = ? AND gate_path = ? AND ref = ?`, strings.TrimSpace(repoID), strings.TrimSpace(gatePath), strings.TrimSpace(ref))
	if err != nil {
		return fmt.Errorf("clear gate ref quarantine: %w", err)
	}
	return nil
}

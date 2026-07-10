package db

import (
	"database/sql"
	"fmt"
)

// Seal is an immutable record of a publish candidate: the exact commit SHA
// sealed after the pre-Verify content mutators completed against a clean
// worktree. Seals are append-only; a repaired or reverified candidate creates a
// new Seal rather than rewriting the prior one.
type Seal struct {
	ID       string
	RunID    string
	SHA      string
	Reason   string
	SealedAt int64
}

// CreateSeal appends a new candidate seal for a run.
func (d *DB) CreateSeal(runID, sha, reason string) (*Seal, error) {
	s := &Seal{
		ID:       newID(),
		RunID:    runID,
		SHA:      sha,
		Reason:   reason,
		SealedAt: now(),
	}
	_, err := d.sql.Exec(
		`INSERT INTO run_seals (id, run_id, sha, reason, sealed_at) VALUES (?, ?, ?, ?, ?)`,
		s.ID, s.RunID, s.SHA, s.Reason, s.SealedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("create seal: %w", err)
	}
	return s, nil
}

// LatestSeal returns the most recent seal for a run, or nil when none exists.
func (d *DB) LatestSeal(runID string) (*Seal, error) {
	s := &Seal{}
	err := d.sql.QueryRow(
		`SELECT id, run_id, sha, reason, sealed_at FROM run_seals WHERE run_id = ? ORDER BY sealed_at DESC, id DESC LIMIT 1`,
		runID,
	).Scan(&s.ID, &s.RunID, &s.SHA, &s.Reason, &s.SealedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("latest seal: %w", err)
	}
	return s, nil
}

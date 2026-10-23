package db

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// RunRetry links a run that continues a failed run to that failure: which
// step failed, in which round, with what error, and the preserved head the
// continuation started from. The continuation run's own status is the outcome.
type RunRetry struct {
	RunID             string
	SourceRunID       string
	StepName          types.StepName
	SourceRound       int
	SourceError       string
	CheckpointHeadSHA string
	CreatedAt         int64
}

// InsertRunRetry records the audit link for a continuation run. It fails when
// the source run was already continued.
func (d *DB) InsertRunRetry(r RunRetry) error {
	_, err := d.sql.Exec(
		`INSERT INTO run_retries (run_id, source_run_id, step_name, source_round, source_error, checkpoint_head_sha, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		r.RunID, r.SourceRunID, r.StepName, r.SourceRound, r.SourceError, r.CheckpointHeadSHA, now(),
	)
	if err != nil {
		return fmt.Errorf("record run retry: %w", err)
	}
	return nil
}

// GetRunRetry returns the audit link of a continuation run, or nil when the
// run is not one.
func (d *DB) GetRunRetry(runID string) (*RunRetry, error) {
	return d.getRunRetry(`SELECT run_id, source_run_id, step_name, source_round, source_error, checkpoint_head_sha, created_at FROM run_retries WHERE run_id = ?`, runID)
}

// GetRunRetryBySource returns the audit link of the run that continued
// sourceRunID, or nil when it was never continued.
func (d *DB) GetRunRetryBySource(sourceRunID string) (*RunRetry, error) {
	return d.getRunRetry(`SELECT run_id, source_run_id, step_name, source_round, source_error, checkpoint_head_sha, created_at FROM run_retries WHERE source_run_id = ?`, sourceRunID)
}

func (d *DB) getRunRetry(query, id string) (*RunRetry, error) {
	r := &RunRetry{}
	err := d.sql.QueryRow(query, id).Scan(&r.RunID, &r.SourceRunID, &r.StepName, &r.SourceRound, &r.SourceError, &r.CheckpointHeadSHA, &r.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get run retry: %w", err)
	}
	return r, nil
}

// DeleteRun removes a run record with every row that cascades from it. A
// retry refused at launch uses it so the refused continuation never becomes
// the branch's latest run.
func (d *DB) DeleteRun(id string) error {
	_, err := d.sql.Exec(`DELETE FROM runs WHERE id = ?`, id)
	return err
}

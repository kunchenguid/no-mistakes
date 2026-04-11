package db

import (
	"database/sql"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// StepResult represents the result of a pipeline step execution.
type StepResult struct {
	ID           string
	RunID        string
	StepName     types.StepName
	StepOrder    int
	Status       types.StepStatus
	ExitCode     *int
	DurationMS   *int64
	LogPath      *string
	FindingsJSON *string
	Error        *string
	StartedAt    *int64
	CompletedAt  *int64
}

// InsertStepResult creates a new step result record.
func (d *DB) InsertStepResult(runID string, stepName types.StepName) (*StepResult, error) {
	s := &StepResult{
		ID:        newID(),
		RunID:     runID,
		StepName:  stepName,
		StepOrder: stepName.Order(),
		Status:    types.StepStatusPending,
	}
	_, err := d.sql.Exec(
		`INSERT INTO step_results (id, run_id, step_name, step_order, status) VALUES (?, ?, ?, ?, ?)`,
		s.ID, s.RunID, s.StepName, s.StepOrder, s.Status,
	)
	if err != nil {
		return nil, fmt.Errorf("insert step result: %w", err)
	}
	return s, nil
}

// GetStepResult returns a step result by ID.
func (d *DB) GetStepResult(id string) (*StepResult, error) {
	s := &StepResult{}
	err := d.sql.QueryRow(
		`SELECT id, run_id, step_name, step_order, status, exit_code, duration_ms, log_path, findings_json, error, started_at, completed_at FROM step_results WHERE id = ?`, id,
	).Scan(&s.ID, &s.RunID, &s.StepName, &s.StepOrder, &s.Status, &s.ExitCode, &s.DurationMS, &s.LogPath, &s.FindingsJSON, &s.Error, &s.StartedAt, &s.CompletedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get step result: %w", err)
	}
	return s, nil
}

// GetStepsByRun returns all step results for a run, in execution order.
func (d *DB) GetStepsByRun(runID string) ([]*StepResult, error) {
	rows, err := d.sql.Query(
		`SELECT id, run_id, step_name, step_order, status, exit_code, duration_ms, log_path, findings_json, error, started_at, completed_at FROM step_results WHERE run_id = ? ORDER BY step_order`, runID,
	)
	if err != nil {
		return nil, fmt.Errorf("get steps by run: %w", err)
	}
	defer rows.Close()
	var steps []*StepResult
	for rows.Next() {
		s := &StepResult{}
		if err := rows.Scan(&s.ID, &s.RunID, &s.StepName, &s.StepOrder, &s.Status, &s.ExitCode, &s.DurationMS, &s.LogPath, &s.FindingsJSON, &s.Error, &s.StartedAt, &s.CompletedAt); err != nil {
			return nil, fmt.Errorf("scan step result: %w", err)
		}
		steps = append(steps, s)
	}
	return steps, rows.Err()
}

// UpdateStepStatus updates a step's status.
func (d *DB) UpdateStepStatus(id string, status types.StepStatus) error {
	_, err := d.sql.Exec(`UPDATE step_results SET status = ? WHERE id = ?`, status, id)
	if err != nil {
		return fmt.Errorf("update step status: %w", err)
	}
	return nil
}

// StartStep marks a step as running with a started_at timestamp.
func (d *DB) StartStep(id string) error {
	_, err := d.sql.Exec(`UPDATE step_results SET status = ?, started_at = ? WHERE id = ?`, types.StepStatusRunning, now(), id)
	if err != nil {
		return fmt.Errorf("start step: %w", err)
	}
	return nil
}

// CompleteStep marks a step as completed with timing and result info.
func (d *DB) CompleteStep(id string, exitCode int, durationMS int64, logPath string) error {
	_, err := d.sql.Exec(
		`UPDATE step_results SET status = ?, exit_code = ?, duration_ms = ?, log_path = ?, completed_at = ? WHERE id = ?`,
		types.StepStatusCompleted, exitCode, durationMS, logPath, now(), id,
	)
	if err != nil {
		return fmt.Errorf("complete step: %w", err)
	}
	return nil
}

// FailStep marks a step as failed with an error message.
func (d *DB) FailStep(id string, errMsg string) error {
	_, err := d.sql.Exec(
		`UPDATE step_results SET status = ?, error = ?, completed_at = ? WHERE id = ?`,
		types.StepStatusFailed, errMsg, now(), id,
	)
	if err != nil {
		return fmt.Errorf("fail step: %w", err)
	}
	return nil
}

// SetStepFindings sets the findings JSON on a step result.
func (d *DB) SetStepFindings(id string, findingsJSON string) error {
	_, err := d.sql.Exec(`UPDATE step_results SET findings_json = ? WHERE id = ?`, findingsJSON, id)
	if err != nil {
		return fmt.Errorf("set step findings: %w", err)
	}
	return nil
}

// ClearStepFindings removes any stored findings JSON from a step result.
func (d *DB) ClearStepFindings(id string) error {
	_, err := d.sql.Exec(`UPDATE step_results SET findings_json = NULL WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("clear step findings: %w", err)
	}
	return nil
}

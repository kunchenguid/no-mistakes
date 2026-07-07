package db

import (
	"database/sql"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// StepResult represents the result of a pipeline step execution.
type StepResult struct {
	ID             string
	RunID          string
	StepName       types.StepName
	StepOrder      int
	Status         types.StepStatus
	ExitCode       *int
	DurationMS     *int64
	LogPath        *string
	FindingsJSON   *string
	Error          *string
	StartedAt      *int64
	CompletedAt    *int64
	LastActivityAt *int64
	LastActivity   *string
	AgentPID       *int
	AutoFixLimit   *int
}

const stepResultColumns = `id, run_id, step_name, step_order, status, exit_code, duration_ms, log_path, findings_json, error, started_at, completed_at, last_activity_at, last_activity, agent_pid, auto_fix_limit`

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
		`SELECT `+stepResultColumns+` FROM step_results WHERE id = ?`, id,
	).Scan(&s.ID, &s.RunID, &s.StepName, &s.StepOrder, &s.Status, &s.ExitCode, &s.DurationMS, &s.LogPath, &s.FindingsJSON, &s.Error, &s.StartedAt, &s.CompletedAt, &s.LastActivityAt, &s.LastActivity, &s.AgentPID, &s.AutoFixLimit)
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
		`SELECT `+stepResultColumns+` FROM step_results WHERE run_id = ? ORDER BY step_order`, runID,
	)
	if err != nil {
		return nil, fmt.Errorf("get steps by run: %w", err)
	}
	defer rows.Close()
	var steps []*StepResult
	for rows.Next() {
		s := &StepResult{}
		if err := rows.Scan(&s.ID, &s.RunID, &s.StepName, &s.StepOrder, &s.Status, &s.ExitCode, &s.DurationMS, &s.LogPath, &s.FindingsJSON, &s.Error, &s.StartedAt, &s.CompletedAt, &s.LastActivityAt, &s.LastActivity, &s.AgentPID, &s.AutoFixLimit); err != nil {
			return nil, fmt.Errorf("scan step result: %w", err)
		}
		steps = append(steps, s)
	}
	return steps, rows.Err()
}

// UpdateStepStatus updates a step's status.
func (d *DB) UpdateStepStatus(id string, status types.StepStatus) error {
	_, err := d.sql.Exec(`UPDATE step_results SET status = ?, last_activity_at = ?, last_activity = ? WHERE id = ?`, status, now(), fmt.Sprintf("status: %s", status), id)
	if err != nil {
		return fmt.Errorf("update step status: %w", err)
	}
	return nil
}

// UpdateStepStatusWithDuration updates a step's status and execution duration together.
func (d *DB) UpdateStepStatusWithDuration(id string, status types.StepStatus, durationMS int64) error {
	_, err := d.sql.Exec(`UPDATE step_results SET status = ?, duration_ms = ?, last_activity_at = ?, last_activity = ? WHERE id = ?`, status, durationMS, now(), fmt.Sprintf("status: %s", status), id)
	if err != nil {
		return fmt.Errorf("update step status with duration: %w", err)
	}
	return nil
}

// StartStep marks a step as running with a started_at timestamp.
func (d *DB) StartStep(id string) error {
	return d.StartStepWithAutoFixLimit(id, 0)
}

// StartStepWithAutoFixLimit marks a step as running and records the effective
// auto-fix limit that status surfaces use while the step is active.
func (d *DB) StartStepWithAutoFixLimit(id string, autoFixLimit int) error {
	ts := now()
	_, err := d.sql.Exec(`UPDATE step_results SET status = ?, started_at = ?, last_activity_at = ?, last_activity = ?, agent_pid = NULL, auto_fix_limit = ? WHERE id = ?`, types.StepStatusRunning, ts, ts, "step started", autoFixLimitDBValue(autoFixLimit), id)
	if err != nil {
		return fmt.Errorf("start step: %w", err)
	}
	return nil
}

func (d *DB) SetStepAutoFixLimit(id string, autoFixLimit int) error {
	if _, err := d.sql.Exec(`UPDATE step_results SET auto_fix_limit = ? WHERE id = ?`, autoFixLimitDBValue(autoFixLimit), id); err != nil {
		return fmt.Errorf("set step auto-fix limit: %w", err)
	}
	return nil
}

func autoFixLimitDBValue(autoFixLimit int) any {
	if autoFixLimit <= 0 {
		return nil
	}
	return autoFixLimit
}

// CompleteStep marks a step as completed with timing and result info.
func (d *DB) CompleteStep(id string, exitCode int, durationMS int64, logPath string) error {
	return d.CompleteStepWithStatus(id, types.StepStatusCompleted, exitCode, durationMS, logPath)
}

// CompleteStepWithStatus marks a step as finished with timing and result info.
func (d *DB) CompleteStepWithStatus(id string, status types.StepStatus, exitCode int, durationMS int64, logPath string) error {
	_, err := d.sql.Exec(
		`UPDATE step_results SET status = ?, exit_code = ?, duration_ms = ?, log_path = ?, completed_at = ?, last_activity_at = ?, last_activity = ?, agent_pid = NULL WHERE id = ?`,
		status, exitCode, durationMS, logPath, now(), now(), fmt.Sprintf("status: %s", status), id,
	)
	if err != nil {
		return fmt.Errorf("complete step: %w", err)
	}
	return nil
}

// FailStep marks a step as failed with an error message and duration.
func (d *DB) FailStep(id string, errMsg string, durationMS int64) error {
	_, err := d.sql.Exec(
		`UPDATE step_results SET status = ?, error = ?, duration_ms = ?, completed_at = ?, last_activity_at = ?, last_activity = ?, agent_pid = NULL WHERE id = ?`,
		types.StepStatusFailed, errMsg, durationMS, now(), now(), "step failed: "+errMsg, id,
	)
	if err != nil {
		return fmt.Errorf("fail step: %w", err)
	}
	return nil
}

// TouchStepActivity records the latest meaningful activity for an active step
// without changing its status or current agent pid.
func (d *DB) TouchStepActivity(id string, text string) error {
	_, err := d.sql.Exec(`UPDATE step_results SET last_activity_at = ?, last_activity = ? WHERE id = ?`, now(), text, id)
	if err != nil {
		return fmt.Errorf("touch step activity: %w", err)
	}
	return nil
}

// SetStepAgentActivity records an agent lifecycle activity and replaces the
// active agent pid. Passing nil clears the pid after the process exits.
func (d *DB) SetStepAgentActivity(id string, text string, agentPID *int) error {
	_, err := d.sql.Exec(`UPDATE step_results SET last_activity_at = ?, last_activity = ?, agent_pid = ? WHERE id = ?`, now(), text, agentPID, id)
	if err != nil {
		return fmt.Errorf("set step agent activity: %w", err)
	}
	return nil
}

// SetStepDuration sets the execution-only duration on a step result.
func (d *DB) SetStepDuration(id string, durationMS int64) error {
	_, err := d.sql.Exec(`UPDATE step_results SET duration_ms = ? WHERE id = ?`, durationMS, id)
	if err != nil {
		return fmt.Errorf("set step duration: %w", err)
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

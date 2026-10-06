package db

import (
	"fmt"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// AdmitLateCIFindings atomically invalidates readiness and retains an amendment
// only while this exact published head is actively monitored. It never changes
// publication/custody bindings. Removing review authority forces the existing
// CI repair path to revalidate amended bytes before publishing them.
func (d *DB) AdmitLateCIFindings(runID, stepID, head, findings string) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	ts := now()
	result, err := tx.Exec(`UPDATE runs SET ci_ready_at = NULL, ci_ready_no_ci = 0,
  review_approved_head_sha = NULL, awaiting_agent_since = ?, updated_at = ?
  WHERE id = ? AND status = ? AND head_sha = ? AND last_pushed_sha = ?
  AND pr_state = 'open' AND COALESCE(push_active, 0) = 0`, ts, ts, runID, types.RunRunning, head, head)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("late finding requires the exact active published CI head")
	}
	result, err = tx.Exec(`UPDATE step_results SET status = ?, findings_json = ?,
  duration_ms = COALESCE(duration_ms, 0), agent_pid = NULL,
	  last_activity_at = ?, last_activity = ? WHERE id = ? AND run_id = ?
  AND step_name = ? AND status = ?`, types.StepStatusAwaitingApproval, findings, ts,
		"late CI finding admitted", stepID, runID, types.StepCI, types.StepStatusRunning)
	if err != nil {
		return err
	}
	changed, err = result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("late finding requires a running CI step")
	}
	// Recovery requires a complete round matching the retained gate. Record it
	// in this transaction, before cancellation can interrupt the live executor.
	_, err = tx.Exec(`INSERT INTO step_rounds
	 (id, step_result_id, round, trigger_type, findings_json, duration_ms, created_at)
	 VALUES (?, ?, (SELECT COALESCE(MAX(round), 0) + 1 FROM step_rounds WHERE step_result_id = ?), 'initial', ?, 0, ?)`,
		newID(), stepID, stepID, findings, ts)
	if err != nil {
		return fmt.Errorf("retain late finding round: %w", err)
	}
	return tx.Commit()
}

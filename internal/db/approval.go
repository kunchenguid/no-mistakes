package db

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// ApprovalGate is the durable identity and exact result snapshot for one
// approval point. A step can create multiple gates over its lifetime; its
// approval_gate_id always identifies the current one.
type ApprovalGate struct {
	ID            string
	RunID         string
	StepResultID  string
	SourceRoundID string
	Status        types.StepStatus
	FindingsJSON  string
	DurationMS    int64
	CreatedAt     int64
}

// ParkApprovalGateInput contains the complete step result that becomes visible
// when a run parks at an approval gate.
type ParkApprovalGateInput struct {
	RunID         string
	StepResultID  string
	SourceRoundID string
	Status        types.StepStatus
	FindingsJSON  string
	DurationMS    int64
}

// ApprovalAction is an immutable response journal entry. AppliedAt remains nil
// until the executor has durably applied the requested effect.
type ApprovalAction struct {
	ID                     string
	GateID                 string
	RunID                  string
	StepResultID           string
	StepRoundID            string
	Action                 types.ApprovalAction
	SelectedFindingIDsJSON string
	InstructionsJSON       string
	AddedFindingsJSON      string
	CreatedAt              int64
	AppliedAt              *int64
}

// ApprovalActionInput is the exact response payload acknowledged by the
// approval endpoint. StepRoundID is the completed source round for the gate,
// not an inferred latest round.
type ApprovalActionInput struct {
	GateID                 string
	RunID                  string
	StepResultID           string
	StepRoundID            string
	Action                 types.ApprovalAction
	SelectedFindingIDsJSON string
	InstructionsJSON       string
	AddedFindingsJSON      string
}

const approvalActionColumns = `id, gate_id, run_id, step_result_id, step_round_id, action, selected_finding_ids_json, instructions_json, added_findings_json, created_at, applied_at`

const approvalGateColumns = `id, run_id, step_result_id, source_round_id, status, findings_json, duration_ms, created_at`

func scanApprovalGate(row interface{ Scan(...any) error }, gate *ApprovalGate) error {
	return row.Scan(
		&gate.ID, &gate.RunID, &gate.StepResultID, &gate.SourceRoundID,
		&gate.Status, &gate.FindingsJSON, &gate.DurationMS, &gate.CreatedAt,
	)
}

func scanApprovalAction(row interface{ Scan(...any) error }, action *ApprovalAction) error {
	return row.Scan(
		&action.ID, &action.GateID, &action.RunID, &action.StepResultID,
		&action.StepRoundID, &action.Action, &action.SelectedFindingIDsJSON,
		&action.InstructionsJSON, &action.AddedFindingsJSON,
		&action.CreatedAt, &action.AppliedAt,
	)
}

// GetApprovalGate returns a durable gate by its identity.
func (d *DB) GetApprovalGate(id string) (*ApprovalGate, error) {
	gate := &ApprovalGate{}
	err := scanApprovalGate(d.sql.QueryRow(
		`SELECT `+approvalGateColumns+` FROM approval_gates WHERE id = ?`,
		id,
	), gate)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get approval gate: %w", err)
	}
	return gate, nil
}

// GetCurrentApprovalGate returns the gate referenced by a step's current
// approval_gate_id. It returns nil when the step is unknown or has no gate.
func (d *DB) GetCurrentApprovalGate(stepResultID string) (*ApprovalGate, error) {
	gate := &ApprovalGate{}
	err := scanApprovalGate(d.sql.QueryRow(
		`SELECT g.id, g.run_id, g.step_result_id, g.source_round_id, g.status, g.findings_json, g.duration_ms, g.created_at
		 FROM approval_gates g
		 JOIN step_results s ON s.approval_gate_id = g.id
		 WHERE s.id = ?`,
		stepResultID,
	), gate)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get current approval gate: %w", err)
	}
	return gate, nil
}

// ParkApprovalGate creates a durable gate, publishes its exact result on the
// step, and marks the run parked in one transaction. Any failed validation or
// write leaves all three surfaces unchanged.
func (d *DB) ParkApprovalGate(input ParkApprovalGateInput) (*ApprovalGate, error) {
	if err := validateParkApprovalGateInput(input); err != nil {
		return nil, err
	}

	tx, err := d.sql.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin park approval gate: %w", err)
	}
	defer tx.Rollback()

	var stepRunID string
	var currentStepStatus types.StepStatus
	var currentGateID *string
	var runStatus types.RunStatus
	var awaitingAgentSince *int64
	var roundStepID, roundState string
	var previousGateResolved bool
	err = tx.QueryRow(`
		SELECT s.run_id, s.status, s.approval_gate_id,
		       r.status, r.awaiting_agent_since,
		       sr.step_result_id, sr.state,
		       CASE
		           WHEN s.approval_gate_id IS NULL THEN 1
		           WHEN EXISTS (
		               SELECT 1 FROM approval_actions aa
		               WHERE aa.gate_id = s.approval_gate_id AND aa.applied_at IS NOT NULL
		           ) THEN 1
		           ELSE 0
		       END
		FROM step_results s
		JOIN runs r ON r.id = ?
		JOIN step_rounds sr ON sr.id = ?
		WHERE s.id = ?`, input.RunID, input.SourceRoundID, input.StepResultID,
	).Scan(
		&stepRunID, &currentStepStatus, &currentGateID,
		&runStatus, &awaitingAgentSince,
		&roundStepID, &roundState, &previousGateResolved,
	)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("park approval gate: run, step, or source round not found")
	}
	if err != nil {
		return nil, fmt.Errorf("validate approval gate ownership: %w", err)
	}
	if stepRunID != input.RunID {
		return nil, fmt.Errorf("park approval gate: step %q does not belong to run %q", input.StepResultID, input.RunID)
	}
	if roundStepID != input.StepResultID {
		return nil, fmt.Errorf("park approval gate: source round %q does not belong to step %q", input.SourceRoundID, input.StepResultID)
	}
	if roundState != StepRoundCompleted {
		return nil, fmt.Errorf("park approval gate: source round %q is not completed", input.SourceRoundID)
	}
	if runStatus != types.RunRunning {
		return nil, fmt.Errorf("park approval gate: run %q has status %q, want %q", input.RunID, runStatus, types.RunRunning)
	}
	if awaitingAgentSince != nil {
		return nil, fmt.Errorf("park approval gate: run %q is already parked", input.RunID)
	}
	if !previousGateResolved {
		return nil, fmt.Errorf("park approval gate: step %q has an unresolved current gate", input.StepResultID)
	}
	expectedStepStatus := types.StepStatusRunning
	if input.Status == types.StepStatusFixReview {
		expectedStepStatus = types.StepStatusFixing
	}
	if currentStepStatus != expectedStepStatus {
		return nil, fmt.Errorf("park approval gate: step %q has status %q, want %q", input.StepResultID, currentStepStatus, expectedStepStatus)
	}

	ts := now()
	gate := &ApprovalGate{
		ID:            newID(),
		RunID:         input.RunID,
		StepResultID:  input.StepResultID,
		SourceRoundID: input.SourceRoundID,
		Status:        input.Status,
		FindingsJSON:  input.FindingsJSON,
		DurationMS:    input.DurationMS,
		CreatedAt:     ts,
	}
	if _, err := tx.Exec(`
		INSERT INTO approval_gates
		    (id, run_id, step_result_id, source_round_id, status, findings_json, duration_ms, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		gate.ID, gate.RunID, gate.StepResultID, gate.SourceRoundID,
		gate.Status, gate.FindingsJSON, gate.DurationMS, gate.CreatedAt,
	); err != nil {
		return nil, fmt.Errorf("insert approval gate: %w", err)
	}
	result, err := tx.Exec(`
		UPDATE step_results
		SET status = ?, findings_json = ?, duration_ms = ?, approval_gate_id = ?,
		    last_activity_at = ?, last_activity = ?
		WHERE id = ? AND run_id = ? AND status = ?`,
		gate.Status, gate.FindingsJSON, gate.DurationMS, gate.ID,
		ts, fmt.Sprintf("status: %s", gate.Status),
		gate.StepResultID, gate.RunID, expectedStepStatus,
	)
	if err != nil {
		return nil, fmt.Errorf("park approval gate step: %w", err)
	}
	if err := requireOneRow(result, "park approval gate step"); err != nil {
		return nil, err
	}
	result, err = tx.Exec(`
		UPDATE runs SET awaiting_agent_since = ?, updated_at = ?
		WHERE id = ? AND status = ? AND awaiting_agent_since IS NULL`,
		ts, ts, gate.RunID, types.RunRunning,
	)
	if err != nil {
		return nil, fmt.Errorf("park approval gate run: %w", err)
	}
	if err := requireOneRow(result, "park approval gate run"); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit park approval gate: %w", err)
	}
	return gate, nil
}

func validateParkApprovalGateInput(input ParkApprovalGateInput) error {
	if strings.TrimSpace(input.RunID) == "" || strings.TrimSpace(input.StepResultID) == "" || strings.TrimSpace(input.SourceRoundID) == "" {
		return fmt.Errorf("park approval gate: run, step, and source round IDs are required")
	}
	if input.Status != types.StepStatusAwaitingApproval && input.Status != types.StepStatusFixReview {
		return fmt.Errorf("park approval gate: invalid gate status %q", input.Status)
	}
	if input.DurationMS < 0 {
		return fmt.Errorf("park approval gate: duration must not be negative")
	}
	if !json.Valid([]byte(input.FindingsJSON)) {
		return fmt.Errorf("park approval gate: findings_json is not valid JSON")
	}
	return nil
}

// InsertApprovalAction appends one immutable response for the current parked
// gate. It rejects duplicate, stale, mismatched, and unparked responses.
func (d *DB) InsertApprovalAction(input ApprovalActionInput) (*ApprovalAction, error) {
	if err := validateApprovalActionInput(input); err != nil {
		return nil, err
	}

	tx, err := d.sql.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin insert approval action: %w", err)
	}
	defer tx.Rollback()

	var gateRunID, gateStepID, sourceRoundID string
	var gateStatus types.StepStatus
	var stepRunID string
	var stepStatus types.StepStatus
	var currentGateID *string
	var runStatus types.RunStatus
	var awaitingAgentSince *int64
	var roundStepID, roundState string
	var actionCount int
	err = tx.QueryRow(`
		SELECT g.run_id, g.step_result_id, g.source_round_id, g.status,
		       s.run_id, s.status, s.approval_gate_id,
		       r.status, r.awaiting_agent_since,
		       sr.step_result_id, sr.state,
		       (SELECT count(*) FROM approval_actions aa WHERE aa.gate_id = g.id)
		FROM approval_gates g
		JOIN step_results s ON s.id = g.step_result_id
		JOIN runs r ON r.id = g.run_id
		JOIN step_rounds sr ON sr.id = g.source_round_id
		WHERE g.id = ?`, input.GateID,
	).Scan(
		&gateRunID, &gateStepID, &sourceRoundID, &gateStatus,
		&stepRunID, &stepStatus, &currentGateID,
		&runStatus, &awaitingAgentSince,
		&roundStepID, &roundState, &actionCount,
	)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("insert approval action: gate %q not found", input.GateID)
	}
	if err != nil {
		return nil, fmt.Errorf("validate approval action gate: %w", err)
	}
	if gateRunID != input.RunID || stepRunID != input.RunID {
		return nil, fmt.Errorf("insert approval action: gate does not belong to run %q", input.RunID)
	}
	if gateStepID != input.StepResultID {
		return nil, fmt.Errorf("insert approval action: gate does not belong to step %q", input.StepResultID)
	}
	if sourceRoundID != input.StepRoundID || roundStepID != input.StepResultID || roundState != StepRoundCompleted {
		return nil, fmt.Errorf("insert approval action: source round %q does not belong to the current gate", input.StepRoundID)
	}
	if currentGateID == nil || *currentGateID != input.GateID || stepStatus != gateStatus {
		return nil, fmt.Errorf("insert approval action: gate %q is stale or no longer current", input.GateID)
	}
	if runStatus != types.RunRunning || awaitingAgentSince == nil {
		return nil, fmt.Errorf("insert approval action: gate %q is not parked", input.GateID)
	}
	if actionCount != 0 {
		return nil, fmt.Errorf("insert approval action: gate %q already has a response", input.GateID)
	}

	action := &ApprovalAction{
		ID:                     newID(),
		GateID:                 input.GateID,
		RunID:                  input.RunID,
		StepResultID:           input.StepResultID,
		StepRoundID:            input.StepRoundID,
		Action:                 input.Action,
		SelectedFindingIDsJSON: input.SelectedFindingIDsJSON,
		InstructionsJSON:       input.InstructionsJSON,
		AddedFindingsJSON:      input.AddedFindingsJSON,
		CreatedAt:              now(),
	}
	if _, err := tx.Exec(`
		INSERT INTO approval_actions
		    (id, gate_id, run_id, step_result_id, step_round_id, action,
		     selected_finding_ids_json, instructions_json, added_findings_json, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		action.ID, action.GateID, action.RunID, action.StepResultID,
		action.StepRoundID, action.Action, action.SelectedFindingIDsJSON,
		action.InstructionsJSON, action.AddedFindingsJSON, action.CreatedAt,
	); err != nil {
		return nil, fmt.Errorf("insert approval action: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit approval action: %w", err)
	}
	return action, nil
}

func validateApprovalActionInput(input ApprovalActionInput) error {
	if strings.TrimSpace(input.GateID) == "" || strings.TrimSpace(input.RunID) == "" || strings.TrimSpace(input.StepResultID) == "" || strings.TrimSpace(input.StepRoundID) == "" {
		return fmt.Errorf("insert approval action: gate, run, step, and source round IDs are required")
	}
	switch input.Action {
	case types.ActionApprove, types.ActionFix, types.ActionSkip, types.ActionAbort:
	default:
		return fmt.Errorf("insert approval action: invalid action %q", input.Action)
	}
	var selected []string
	if err := json.Unmarshal([]byte(input.SelectedFindingIDsJSON), &selected); err != nil {
		return fmt.Errorf("insert approval action: selected_finding_ids_json must be a JSON array of strings or null")
	}
	var instructions map[string]string
	if err := json.Unmarshal([]byte(input.InstructionsJSON), &instructions); err != nil {
		return fmt.Errorf("insert approval action: instructions_json must be a JSON object with string values or null")
	}
	var added []json.RawMessage
	if err := json.Unmarshal([]byte(input.AddedFindingsJSON), &added); err != nil {
		return fmt.Errorf("insert approval action: added_findings_json must be a JSON array or null")
	}
	if input.Action == types.ActionFix {
		if len(selected) == 0 && len(added) == 0 {
			return fmt.Errorf("insert approval action: fix requires at least one selected or added finding")
		}
		return nil
	}
	if len(selected) != 0 || len(instructions) != 0 || len(added) != 0 {
		return fmt.Errorf("insert approval action: %q does not accept fix payload", input.Action)
	}
	return nil
}

// GetPendingApprovalAction returns the unapplied response for gateID. A nil
// result means no response is waiting, including when the gate is unknown or
// its response has already been applied.
func (d *DB) GetPendingApprovalAction(gateID string) (*ApprovalAction, error) {
	action := &ApprovalAction{}
	err := scanApprovalAction(d.sql.QueryRow(
		`SELECT `+approvalActionColumns+` FROM approval_actions WHERE gate_id = ? AND applied_at IS NULL`,
		gateID,
	), action)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get pending approval action: %w", err)
	}
	return action, nil
}

// CompleteApprovalAction marks an action applied and resumes its run in one
// transaction. Repeating completion for an already-applied action is a no-op.
func (d *DB) CompleteApprovalAction(actionID string, parkedMS int64) error {
	if strings.TrimSpace(actionID) == "" {
		return fmt.Errorf("complete approval action: action ID is required")
	}
	if parkedMS < 0 {
		return fmt.Errorf("complete approval action: parked duration must not be negative")
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return fmt.Errorf("begin complete approval action: %w", err)
	}
	defer tx.Rollback()

	var appliedAt *int64
	var gateID, runID string
	var gateStatus, stepStatus types.StepStatus
	var currentGateID *string
	var runStatus types.RunStatus
	var awaitingAgentSince *int64
	err = tx.QueryRow(`
		SELECT aa.applied_at, aa.gate_id, aa.run_id, g.status,
		       s.status, s.approval_gate_id, r.status, r.awaiting_agent_since
		FROM approval_actions aa
		JOIN approval_gates g ON g.id = aa.gate_id
		JOIN step_results s ON s.id = g.step_result_id
		JOIN runs r ON r.id = aa.run_id
		WHERE aa.id = ?`, actionID,
	).Scan(
		&appliedAt, &gateID, &runID, &gateStatus,
		&stepStatus, &currentGateID, &runStatus, &awaitingAgentSince,
	)
	if err == sql.ErrNoRows {
		return fmt.Errorf("complete approval action: action %q not found", actionID)
	}
	if err != nil {
		return fmt.Errorf("validate approval action completion: %w", err)
	}
	if appliedAt != nil {
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit repeated approval action completion: %w", err)
		}
		return nil
	}
	if currentGateID == nil || *currentGateID != gateID || stepStatus != gateStatus {
		return fmt.Errorf("complete approval action: gate %q is stale or no longer current", gateID)
	}
	if runStatus != types.RunRunning || awaitingAgentSince == nil {
		return fmt.Errorf("complete approval action: gate %q is not parked", gateID)
	}

	ts := now()
	result, err := tx.Exec(`UPDATE approval_actions SET applied_at = ? WHERE id = ? AND applied_at IS NULL`, ts, actionID)
	if err != nil {
		return fmt.Errorf("mark approval action applied: %w", err)
	}
	if err := requireOneRow(result, "mark approval action applied"); err != nil {
		return err
	}
	result, err = tx.Exec(`
		UPDATE runs
		SET awaiting_agent_since = NULL,
		    parked_ms = COALESCE(parked_ms, 0) + ?,
		    updated_at = ?
		WHERE id = ? AND status = ? AND awaiting_agent_since IS NOT NULL`,
		parkedMS, ts, runID, types.RunRunning,
	)
	if err != nil {
		return fmt.Errorf("complete approval action run marker: %w", err)
	}
	if err := requireOneRow(result, "complete approval action run marker"); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit approval action completion: %w", err)
	}
	return nil
}

func requireOneRow(result sql.Result, operation string) error {
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s rows affected: %w", operation, err)
	}
	if changed != 1 {
		return fmt.Errorf("%s: state changed concurrently", operation)
	}
	return nil
}

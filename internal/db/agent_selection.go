package db

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// AgentSelection is one recorded quota-auto routing decision. It is append-only
// per run: the first row is the engine the run started on, and every later row is
// a mid-run switch, so a reader reconstructs the run's engine history in order.
type AgentSelection struct {
	ID       int64
	RunID    string
	Step     string
	Reason   string
	Agent    string
	Provider string
	// Model is the catalog model row the decision was made on. It is empty when
	// the selected candidate's evidence carried no model id.
	Model string
	// Evidence is the bounded JSON payload of the decision: the chosen standing
	// plus every candidate's verdict. It never contains the raw vendor report.
	Evidence string
	// ReportAt is when the evidence itself was generated, which can predate the
	// decision (quota-axi may answer from its own cache).
	ReportAt  *int64
	CreatedAt int64
}

// RecordAgentSelection appends one routing decision to a run.
func (d *DB) RecordAgentSelection(selection AgentSelection) error {
	if selection.RunID == "" {
		return fmt.Errorf("record agent selection: run id is required")
	}
	createdAt := selection.CreatedAt
	if createdAt == 0 {
		createdAt = time.Now().Unix()
	}
	_, err := d.sql.Exec(
		`INSERT INTO run_agent_selections (run_id, step, reason, agent, provider, model, evidence, report_at, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		selection.RunID, selection.Step, selection.Reason, selection.Agent, selection.Provider,
		selection.Model, selection.Evidence, selection.ReportAt, createdAt,
	)
	if err != nil {
		return fmt.Errorf("record agent selection: %w", err)
	}
	return nil
}

// ListAgentSelections returns a run's recorded routing decisions, oldest first.
func (d *DB) ListAgentSelections(runID string) ([]AgentSelection, error) {
	rows, err := d.sql.Query(
		`SELECT id, run_id, step, reason, agent, provider, model, evidence, report_at, created_at
		   FROM run_agent_selections WHERE run_id = ? ORDER BY id`,
		runID,
	)
	if err != nil {
		return nil, fmt.Errorf("list agent selections: %w", err)
	}
	defer rows.Close()

	var selections []AgentSelection
	for rows.Next() {
		var (
			selection AgentSelection
			reportAt  sql.NullInt64
		)
		if err := rows.Scan(
			&selection.ID, &selection.RunID, &selection.Step, &selection.Reason, &selection.Agent,
			&selection.Provider, &selection.Model, &selection.Evidence, &reportAt, &selection.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan agent selection: %w", err)
		}
		if reportAt.Valid {
			value := reportAt.Int64
			selection.ReportAt = &value
		}
		selections = append(selections, selection)
	}
	if err := rows.Err(); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("list agent selections: %w", err)
	}
	return selections, nil
}

package db

import "fmt"

// StepRound represents one execution round within a pipeline step.
type StepRound struct {
	ID           string
	StepResultID string
	Round        int
	Trigger      string  // "initial", "auto_fix", "user_fix"
	FindingsJSON *string // nullable - findings produced by this round
	DurationMS   int64
	CreatedAt    int64
}

// InsertStepRound creates a new round record for a step result.
func (d *DB) InsertStepRound(stepResultID string, round int, trigger string, findingsJSON *string, durationMS int64) (*StepRound, error) {
	r := &StepRound{
		ID:           newID(),
		StepResultID: stepResultID,
		Round:        round,
		Trigger:      trigger,
		FindingsJSON: findingsJSON,
		DurationMS:   durationMS,
		CreatedAt:    now(),
	}
	_, err := d.sql.Exec(
		`INSERT INTO step_rounds (id, step_result_id, round, trigger_type, findings_json, duration_ms, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.StepResultID, r.Round, r.Trigger, r.FindingsJSON, r.DurationMS, r.CreatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("insert step round: %w", err)
	}
	return r, nil
}

// GetRoundsByStep returns all rounds for a step result, ordered by round number.
func (d *DB) GetRoundsByStep(stepResultID string) ([]*StepRound, error) {
	rows, err := d.sql.Query(
		`SELECT id, step_result_id, round, trigger_type, findings_json, duration_ms, created_at FROM step_rounds WHERE step_result_id = ? ORDER BY round`,
		stepResultID,
	)
	if err != nil {
		return nil, fmt.Errorf("get rounds by step: %w", err)
	}
	defer rows.Close()
	var rounds []*StepRound
	for rows.Next() {
		r := &StepRound{}
		if err := rows.Scan(&r.ID, &r.StepResultID, &r.Round, &r.Trigger, &r.FindingsJSON, &r.DurationMS, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan step round: %w", err)
		}
		rounds = append(rounds, r)
	}
	return rounds, rows.Err()
}

package db

import (
	"database/sql"
	"fmt"
)

const (
	RoundSelectionSourceUser    = "user"
	RoundSelectionSourceAutoFix = "auto_fix"

	StepRoundReserved  = "reserved"
	StepRoundCompleted = "completed"
	StepRoundFailed    = "failed"
	StepRoundCancelled = "cancelled"
)

// StepRound represents one execution round within a pipeline step.
type StepRound struct {
	ID           string
	StepResultID string
	Round        int
	Trigger      string  // "initial", "auto_fix"; legacy "user_fix" is treated as "auto_fix"
	FindingsJSON *string // nullable - findings produced by this round
	// UserFindingsJSON, when non-nil, is the merged finding list that was
	// dispatched to the fix agent after the user edited per-finding
	// instructions or added their own findings. It includes both the
	// selected agent-produced findings (with any attached user
	// instructions) and the user-authored findings.
	UserFindingsJSON *string
	// SelectedFindingIDs, when non-nil, is a JSON array of finding IDs that
	// were chosen (by the user or auto-fix filter) to be fixed AFTER this
	// round. It is populated on the round whose findings triggered the next
	// round, so that later rounds' prompts can tell which findings were
	// deliberately left unselected.
	SelectedFindingIDs *string
	SelectionSource    *string
	// FixSummary, when non-nil, is the agent's one-line commit summary for
	// the fix attempt performed during this round. It is only set when the
	// round itself was a fix round (trigger=="auto_fix").
	FixSummary  *string
	State       string
	StartedAt   *int64
	CompletedAt *int64
	DurationMS  int64
	CreatedAt   int64
}

// StepRoundStats summarizes execution rounds for a step. It lets status
// surfaces show whether a running/fixing step is in an initial pass or a fix
// pass without reloading every round in callers.
type StepRoundStats struct {
	TotalRounds        int
	FixRounds          int
	LatestRound        int
	LatestTrigger      string
	LatestSelection    string
	LatestRoundAt      int64
	LatestFixRound     int
	LatestFixRoundAt   int64
	SelectedForFix     bool
	AutoSelectedForFix bool
	PendingFixSource   string
}

// IsFixRound reports whether this round was a fix attempt. Legacy "user_fix"
// rounds count: they were fix rounds dispatched by an explicit user selection.
func (r *StepRound) IsFixRound() bool {
	return r.Trigger == "auto_fix" || r.Trigger == "user_fix"
}

// StepFixSummaries returns one entry per fix round for a step, in round order:
// the agent's one-line fix summary, or "" when the round recorded none.
func (d *DB) StepFixSummaries(stepResultID string) ([]string, error) {
	rounds, err := d.GetRoundsByStep(stepResultID)
	if err != nil {
		return nil, err
	}
	var summaries []string
	for _, r := range rounds {
		if !r.IsFixRound() {
			continue
		}
		summary := ""
		if r.FixSummary != nil {
			summary = *r.FixSummary
		}
		summaries = append(summaries, summary)
	}
	return summaries, nil
}

// StepRoundStats returns aggregate round information for a step result.
func (d *DB) StepRoundStats(stepResultID string) (StepRoundStats, error) {
	rounds, err := d.GetRoundsByStep(stepResultID)
	if err != nil {
		return StepRoundStats{}, err
	}
	var stats StepRoundStats
	latestSelectedRound := 0
	latestSelectedSource := ""
	for _, r := range rounds {
		stats.TotalRounds++
		stats.LatestRound = r.Round
		stats.LatestTrigger = r.Trigger
		stats.LatestRoundAt = r.CreatedAt
		if r.SelectionSource != nil {
			stats.LatestSelection = *r.SelectionSource
		}
		if r.SelectedFindingIDs != nil && *r.SelectedFindingIDs != "" {
			stats.SelectedForFix = true
			stats.AutoSelectedForFix = r.SelectionSource != nil && *r.SelectionSource == RoundSelectionSourceAutoFix
			latestSelectedRound = r.Round
			latestSelectedSource = stats.LatestSelection
		}
		if r.IsFixRound() {
			stats.FixRounds++
			stats.LatestFixRound = stats.FixRounds
			stats.LatestFixRoundAt = r.CreatedAt
		}
	}
	if latestSelectedRound == stats.LatestRound {
		stats.PendingFixSource = latestSelectedSource
	}
	return stats, nil
}

const stepRoundColumns = `id, step_result_id, round, trigger_type, findings_json, user_findings_json, selected_finding_ids, selection_source, fix_summary, state, started_at, completed_at, duration_ms, created_at`

func scanStepRound(row interface{ Scan(...any) error }, round *StepRound) error {
	return row.Scan(
		&round.ID, &round.StepResultID, &round.Round, &round.Trigger,
		&round.FindingsJSON, &round.UserFindingsJSON, &round.SelectedFindingIDs,
		&round.SelectionSource, &round.FixSummary, &round.State,
		&round.StartedAt, &round.CompletedAt, &round.DurationMS, &round.CreatedAt,
	)
}

// ReserveStepRound creates a stable round identity before step execution.
func (d *DB) ReserveStepRound(stepResultID string, roundNumber int, trigger string) (*StepRound, error) {
	ts := now()
	round := &StepRound{
		ID:           newID(),
		StepResultID: stepResultID,
		Round:        roundNumber,
		Trigger:      trigger,
		State:        StepRoundReserved,
		StartedAt:    &ts,
		CreatedAt:    ts,
	}
	_, err := d.sql.Exec(
		`INSERT INTO step_rounds (id, step_result_id, round, trigger_type, state, started_at, duration_ms, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		round.ID, round.StepResultID, round.Round, round.Trigger, round.State, ts, 0, round.CreatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("reserve step round: %w", err)
	}
	return round, nil
}

// CompleteReservedStepRound appends the successful outcome facts to a
// reservation. A terminal round cannot be completed a second time.
func (d *DB) CompleteReservedStepRound(id string, findingsJSON *string, fixSummary *string, durationMS int64) error {
	ts := now()
	result, err := d.sql.Exec(
		`UPDATE step_rounds SET findings_json = ?, fix_summary = ?, state = ?, completed_at = ?, duration_ms = ? WHERE id = ? AND state = ?`,
		findingsJSON, fixSummary, StepRoundCompleted, ts, durationMS, id, StepRoundReserved,
	)
	if err != nil {
		return fmt.Errorf("complete reserved step round: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("complete reserved step round rows affected: %w", err)
	}
	if changed != 1 {
		return fmt.Errorf("complete reserved step round %q: reservation not active", id)
	}
	return nil
}

// TerminateReservedStepRound records a failed or cancelled execution while
// keeping it outside completed prompt and report history.
func (d *DB) TerminateReservedStepRound(id, state string, durationMS int64) error {
	if state != StepRoundFailed && state != StepRoundCancelled {
		return fmt.Errorf("invalid terminal step round state %q", state)
	}
	result, err := d.sql.Exec(
		`UPDATE step_rounds SET state = ?, completed_at = ?, duration_ms = ? WHERE id = ? AND state = ?`,
		state, now(), durationMS, id, StepRoundReserved,
	)
	if err != nil {
		return fmt.Errorf("terminate reserved step round: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("terminate reserved step round rows affected: %w", err)
	}
	if changed != 1 {
		return fmt.Errorf("terminate reserved step round %q: reservation not active", id)
	}
	return nil
}

// InsertStepRound preserves the completed-history helper used by fixtures and
// non-executor callers.
func (d *DB) InsertStepRound(stepResultID string, roundNumber int, trigger string, findingsJSON *string, fixSummary *string, durationMS int64) (*StepRound, error) {
	ts := now()
	round := &StepRound{
		ID:           newID(),
		StepResultID: stepResultID,
		Round:        roundNumber,
		Trigger:      trigger,
		FindingsJSON: findingsJSON,
		FixSummary:   fixSummary,
		State:        StepRoundCompleted,
		StartedAt:    &ts,
		CompletedAt:  &ts,
		DurationMS:   durationMS,
		CreatedAt:    ts,
	}
	_, err := d.sql.Exec(
		`INSERT INTO step_rounds (id, step_result_id, round, trigger_type, findings_json, fix_summary, state, started_at, completed_at, duration_ms, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		round.ID, round.StepResultID, round.Round, round.Trigger, round.FindingsJSON, round.FixSummary, round.State, ts, ts, round.DurationMS, round.CreatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("insert step round: %w", err)
	}
	return round, nil
}

// SetStepRoundSelection records which findings were selected for fix AFTER the
// given round produced its findings, along with whether that selection came
// from the user or auto-fix filtering. Passing a nil or empty JSON array clears
// both columns.
func (d *DB) SetStepRoundSelection(id string, selectedFindingIDs *string, source string) error {
	var selectionSource *string
	if selectedFindingIDs != nil && *selectedFindingIDs != "" && source != "" {
		selectionSource = &source
	}
	if _, err := d.sql.Exec(
		`UPDATE step_rounds SET selected_finding_ids = ?, selection_source = ? WHERE id = ?`,
		selectedFindingIDs, selectionSource, id,
	); err != nil {
		return fmt.Errorf("set step round selection: %w", err)
	}
	return nil
}

// SetStepRoundSelectedFindingIDs preserves the old API for callers that do not
// need to distinguish how the selection was made.
func (d *DB) SetStepRoundSelectedFindingIDs(id string, selectedFindingIDs *string) error {
	return d.SetStepRoundSelection(id, selectedFindingIDs, RoundSelectionSourceUser)
}

// SetStepRoundUserFindings records the merged finding list (with user
// instructions attached and user-added findings appended) that was
// dispatched to the fix agent for the round. Passing nil clears the column.
func (d *DB) SetStepRoundUserFindings(id string, userFindingsJSON *string) error {
	if _, err := d.sql.Exec(
		`UPDATE step_rounds SET user_findings_json = ? WHERE id = ?`,
		userFindingsJSON, id,
	); err != nil {
		return fmt.Errorf("set step round user findings: %w", err)
	}
	return nil
}

// GetStepRound returns any round by ID, including non-completed reservations.
func (d *DB) GetStepRound(id string) (*StepRound, error) {
	round := &StepRound{}
	if err := scanStepRound(d.sql.QueryRow(`SELECT `+stepRoundColumns+` FROM step_rounds WHERE id = ?`, id), round); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("get step round: %w", err)
	}
	return round, nil
}

// GetRoundsByStep returns completed rounds in their established order.
func (d *DB) GetRoundsByStep(stepResultID string) ([]*StepRound, error) {
	return d.getCompletedRounds(stepResultID, "")
}

// GetAllRoundsByStep returns every durable round state in ordinal order.
// Recovery callers use it to reconcile a reserved child without changing the
// completed-only history consumed by prompts and reports.
func (d *DB) GetAllRoundsByStep(stepResultID string) ([]*StepRound, error) {
	rows, err := d.sql.Query(
		`SELECT `+stepRoundColumns+` FROM step_rounds WHERE step_result_id = ? ORDER BY round, id`,
		stepResultID,
	)
	if err != nil {
		return nil, fmt.Errorf("get all rounds by step: %w", err)
	}
	defer rows.Close()
	var rounds []*StepRound
	for rows.Next() {
		round := &StepRound{}
		if err := scanStepRound(rows, round); err != nil {
			return nil, fmt.Errorf("scan step round: %w", err)
		}
		rounds = append(rounds, round)
	}
	return rounds, rows.Err()
}

// GetPriorCompletedRounds returns completed history excluding the current
// reservation explicitly, even if a caller races with its finalization.
func (d *DB) GetPriorCompletedRounds(stepResultID, currentRoundID string) ([]*StepRound, error) {
	return d.getCompletedRounds(stepResultID, currentRoundID)
}

func (d *DB) getCompletedRounds(stepResultID, excludedRoundID string) ([]*StepRound, error) {
	query := `SELECT ` + stepRoundColumns + ` FROM step_rounds WHERE step_result_id = ? AND state = ?`
	args := []any{stepResultID, StepRoundCompleted}
	if excludedRoundID != "" {
		query += ` AND id <> ?`
		args = append(args, excludedRoundID)
	}
	query += ` ORDER BY round`
	rows, err := d.sql.Query(query, args...)
	if err != nil {
		return nil, fmt.Errorf("get completed rounds by step: %w", err)
	}
	defer rows.Close()
	var rounds []*StepRound
	for rows.Next() {
		round := &StepRound{}
		if err := scanStepRound(rows, round); err != nil {
			return nil, fmt.Errorf("scan step round: %w", err)
		}
		rounds = append(rounds, round)
	}
	return rounds, rows.Err()
}

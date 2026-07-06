package db

import "fmt"

const (
	RoundSelectionSourceUser    = "user"
	RoundSelectionSourceAutoFix = "auto_fix"
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
	FixSummary *string
	DurationMS int64
	CreatedAt  int64
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

// InsertStepRound creates a new round record for a step result. fixSummary may
// be nil for non-fix rounds or when the agent produced no summary.
func (d *DB) InsertStepRound(stepResultID string, round int, trigger string, findingsJSON *string, fixSummary *string, durationMS int64) (*StepRound, error) {
	r := &StepRound{
		ID:           newID(),
		StepResultID: stepResultID,
		Round:        round,
		Trigger:      trigger,
		FindingsJSON: findingsJSON,
		FixSummary:   fixSummary,
		DurationMS:   durationMS,
		CreatedAt:    now(),
	}
	_, err := d.sql.Exec(
		`INSERT INTO step_rounds (id, step_result_id, round, trigger_type, findings_json, user_findings_json, selected_finding_ids, selection_source, fix_summary, duration_ms, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.StepResultID, r.Round, r.Trigger, r.FindingsJSON, r.UserFindingsJSON, r.SelectedFindingIDs, r.SelectionSource, r.FixSummary, r.DurationMS, r.CreatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("insert step round: %w", err)
	}
	return r, nil
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

// GetRoundsByStep returns all rounds for a step result, ordered by round number.
func (d *DB) GetRoundsByStep(stepResultID string) ([]*StepRound, error) {
	rows, err := d.sql.Query(
		`SELECT id, step_result_id, round, trigger_type, findings_json, user_findings_json, selected_finding_ids, selection_source, fix_summary, duration_ms, created_at FROM step_rounds WHERE step_result_id = ? ORDER BY round`,
		stepResultID,
	)
	if err != nil {
		return nil, fmt.Errorf("get rounds by step: %w", err)
	}
	defer rows.Close()
	var rounds []*StepRound
	for rows.Next() {
		r := &StepRound{}
		if err := rows.Scan(&r.ID, &r.StepResultID, &r.Round, &r.Trigger, &r.FindingsJSON, &r.UserFindingsJSON, &r.SelectedFindingIDs, &r.SelectionSource, &r.FixSummary, &r.DurationMS, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan step round: %w", err)
		}
		rounds = append(rounds, r)
	}
	return rounds, rows.Err()
}

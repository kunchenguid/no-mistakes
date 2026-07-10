package db

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// FindingLineage is the durable, run-wide root identity of a finding. Its ID is
// a fresh ULID assigned by no-mistakes — stable across rounds and independent
// of the model's display IDs or the finding prose. Sequence is the run-wide
// monotonic ordinal at creation time.
type FindingLineage struct {
	ID              string
	RunID           string
	OriginAttemptID string
	DisplayID       string
	Sequence        int
	CreatedAt       int64
}

// CreateFindingLineages mints one fresh root lineage per display ID, in order,
// continuing the run's monotonic sequence. A reused display ID still receives a
// distinct identity. An empty slice creates nothing.
func (d *DB) CreateFindingLineages(runID, originAttemptID string, displayIDs []string) ([]FindingLineage, error) {
	if strings.TrimSpace(runID) == "" || strings.TrimSpace(originAttemptID) == "" {
		return nil, fmt.Errorf("finding lineage requires run and origin attempt IDs")
	}
	if len(displayIDs) == 0 {
		return nil, nil
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin finding lineage tx: %w", err)
	}
	defer tx.Rollback()

	var next int
	if err := tx.QueryRow(`SELECT COALESCE(MAX(sequence)+1, 0) FROM finding_lineages WHERE run_id = ?`, runID).Scan(&next); err != nil {
		return nil, fmt.Errorf("next lineage sequence: %w", err)
	}

	ts := now()
	lineages := make([]FindingLineage, 0, len(displayIDs))
	for i, displayID := range displayIDs {
		lineage := FindingLineage{
			ID:              newID(),
			RunID:           runID,
			OriginAttemptID: originAttemptID,
			DisplayID:       displayID,
			Sequence:        next + i,
			CreatedAt:       ts,
		}
		if _, err := tx.Exec(
			`INSERT INTO finding_lineages (id, run_id, origin_attempt_id, display_id, sequence, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
			lineage.ID, lineage.RunID, lineage.OriginAttemptID, nullIfEmpty(lineage.DisplayID), lineage.Sequence, lineage.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("insert finding lineage: %w", err)
		}
		lineages = append(lineages, lineage)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit finding lineages: %w", err)
	}
	return lineages, nil
}

// GetFindingLineagesByRun returns a run's lineages in creation order.
func (d *DB) GetFindingLineagesByRun(runID string) ([]FindingLineage, error) {
	return d.queryFindingLineages(`WHERE run_id = ? ORDER BY sequence, id`, runID)
}

// GetFindingLineagesByAttempt returns the lineages an attempt first surfaced.
func (d *DB) GetFindingLineagesByAttempt(originAttemptID string) ([]FindingLineage, error) {
	return d.queryFindingLineages(`WHERE origin_attempt_id = ? ORDER BY sequence, id`, originAttemptID)
}

func (d *DB) queryFindingLineages(whereOrder string, arg any) ([]FindingLineage, error) {
	rows, err := d.sql.Query(`SELECT id, run_id, origin_attempt_id, display_id, sequence, created_at FROM finding_lineages `+whereOrder, arg)
	if err != nil {
		return nil, fmt.Errorf("query finding lineages: %w", err)
	}
	defer rows.Close()
	var lineages []FindingLineage
	for rows.Next() {
		var lineage FindingLineage
		var displayID sql.NullString
		if err := rows.Scan(&lineage.ID, &lineage.RunID, &lineage.OriginAttemptID, &displayID, &lineage.Sequence, &lineage.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan finding lineage: %w", err)
		}
		lineage.DisplayID = displayID.String
		lineages = append(lineages, lineage)
	}
	return lineages, rows.Err()
}

// GetInvocationAttemptsByStepResult returns every invocation attempt scoped to a
// step result, in durable start order, regardless of round state. This lets a
// projection reconstruct both active (no terminal) and completed routing.
func (d *DB) GetInvocationAttemptsByStepResult(stepResultID string) ([]*InvocationAttempt, error) {
	rows, err := d.sql.Query(invocationAttemptProjection+` WHERE start.step_result_id = ? ORDER BY start.started_at, start.id`, stepResultID)
	if err != nil {
		return nil, fmt.Errorf("get invocation attempts by step result: %w", err)
	}
	defer rows.Close()
	var attempts []*InvocationAttempt
	for rows.Next() {
		attempt, err := scanInvocationAttempt(rows)
		if err != nil {
			return nil, fmt.Errorf("scan invocation attempt: %w", err)
		}
		attempts = append(attempts, attempt)
	}
	return attempts, rows.Err()
}

// ReviewRouting projects the durable routing state of a run's initial review:
// the routed Candidate attempts for the review step and the run-wide finding
// lineages they produced. Both AXI and the daemon reconstruct review routing
// from this projection without parsing logs.
type ReviewRouting struct {
	StepResultID string
	Attempts     []*InvocationAttempt
	Lineages     []FindingLineage
}

// ReviewRoutingForStep returns the routing projection for a review StepResult,
// or nil when the step has recorded no routed initial-review attempt yet.
func (d *DB) ReviewRoutingForStep(stepResultID string) (*ReviewRouting, error) {
	attempts, err := d.GetInvocationAttemptsByStepResult(stepResultID)
	if err != nil {
		return nil, err
	}
	routed := make([]*InvocationAttempt, 0, len(attempts))
	for _, attempt := range attempts {
		if attempt.Start.Purpose == types.PurposeInitialReview && !attempt.Start.Candidate.IsZero() {
			routed = append(routed, attempt)
		}
	}
	if len(routed) == 0 {
		return nil, nil
	}
	proj := &ReviewRouting{StepResultID: stepResultID, Attempts: routed}
	for _, attempt := range routed {
		lineages, err := d.GetFindingLineagesByAttempt(attempt.ID)
		if err != nil {
			return nil, err
		}
		proj.Lineages = append(proj.Lineages, lineages...)
	}
	return proj, nil
}

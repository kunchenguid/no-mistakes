package db

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// PreviousBranchStepRounds returns the rounds of the named step from the most
// recent earlier run of the same repo and branch, so a step can see what a
// previous push already reported and what was done about it. Only a run that
// actually recorded findings qualifies: a run whose step never produced any
// carries no reusable history, and letting it win would hide the last run that
// did. Returns no rounds when the branch has no such history.
//
// excludeRunID is the caller's own run, which must never count as its own
// history.
func (d *DB) PreviousBranchStepRounds(repoID, branch string, step types.StepName, excludeRunID string) ([]*StepRound, error) {
	var stepResultID string
	err := d.sql.QueryRow(
		`SELECT sr.id FROM step_results sr
		 JOIN runs r ON r.id = sr.run_id
		 WHERE r.repo_id = ? AND r.branch = ? AND r.id <> ? AND sr.step_name = ?
		   AND EXISTS (
		     SELECT 1 FROM step_rounds sro
		     WHERE sro.step_result_id = sr.id AND TRIM(COALESCE(sro.findings_json, '')) <> ''
		   )
		 ORDER BY r.created_at DESC, r.id DESC
		 LIMIT 1`,
		repoID, branch, excludeRunID, string(step),
	).Scan(&stepResultID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get previous branch step: %w", err)
	}
	return d.GetRoundsByStep(stepResultID)
}

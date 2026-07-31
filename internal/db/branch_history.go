package db

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// BranchStepHistory is what a previous run on the same branch recorded for one
// step. The run's terminal status travels with the rounds because it is the
// only evidence of whether the user finished deciding: a finding left
// unselected in a completed run was accepted as-is, while the same finding in a
// failed or cancelled run was simply never reached.
type BranchStepHistory struct {
	RunID     string
	RunStatus types.RunStatus
	Rounds    []*StepRound
}

// PreviousBranchStepHistory returns the named step's rounds from the most
// recent earlier run of the same repo and branch, so a step can see what a
// previous push already reported and what was done about it. Only a run that
// actually recorded findings qualifies: a run whose step never produced any
// carries no reusable history, and letting it win would hide the last run that
// did. Returns nil when the branch has no such history.
//
// excludeRunID is the caller's own run, which must never count as its own
// history.
func (d *DB) PreviousBranchStepHistory(repoID, branch string, step types.StepName, excludeRunID string) (*BranchStepHistory, error) {
	var stepResultID, runID, runStatus string
	err := d.sql.QueryRow(
		`SELECT sr.id, r.id, r.status FROM step_results sr
		 JOIN runs r ON r.id = sr.run_id
		 WHERE r.repo_id = ? AND r.branch = ? AND r.id <> ? AND sr.step_name = ?
		   AND EXISTS (
		     SELECT 1 FROM step_rounds sro
		     WHERE sro.step_result_id = sr.id AND TRIM(COALESCE(sro.findings_json, '')) <> ''
		   )
		 ORDER BY r.created_at DESC, r.id DESC
		 LIMIT 1`,
		repoID, branch, excludeRunID, string(step),
	).Scan(&stepResultID, &runID, &runStatus)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get previous branch step: %w", err)
	}

	rounds, err := d.GetRoundsByStep(stepResultID)
	if err != nil {
		return nil, err
	}
	if len(rounds) == 0 {
		return nil, nil
	}
	return &BranchStepHistory{RunID: runID, RunStatus: types.RunStatus(runStatus), Rounds: rounds}, nil
}

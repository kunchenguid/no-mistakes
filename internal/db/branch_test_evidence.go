package db

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// BranchTestEvidence is one earlier run's recorded test findings on this
// branch, paired with the run it belongs to so a reuse can point at it.
//
// Intent is that run's recorded user intent, which is nullable because most
// runs carry none. It rides along because the live-evidence turn derives its
// scenarios FROM the intent, so a verdict is only evidence about the
// acceptance criteria it was earned against.
type BranchTestEvidence struct {
	RunID        string
	FindingsJSON string
	Intent       string
}

// GetBranchTestEvidence returns the single most recently RECORDED test step of
// another run on the same repo and branch that still carries a findings
// payload, or nil when there is none.
//
// Recorded, not completed, and deliberately so: a step that recorded a verdict
// and then parked, was aborted, or was skipped keeps its payload, and that
// verdict is still this branch's latest evidence. Filtering to completed steps
// would hide a no-go behind an older completed go, which is the exact
// inversion the newest-verdict rule exists to prevent. Recency is therefore
// taken from the row id - a ULID, so it sorts by creation time for a parked
// and a completed row alike - rather than from completed_at, which is NULL on
// a parked row and sorts last under DESC.
//
// Only the newest one is returned because only the newest one is evidence
// about where this branch now stands: an older verdict that a later run has
// already superseded must never outrank it. Every non-go outcome this now
// exposes declines reuse at the caller, so the widening only ever fails
// toward running the evidence agent.
//
// Branch scope is the whole point: a verdict is evidence about one branch's
// head, so it is never visible to another branch. A fix round can clear a
// step's final findings, in which case that run simply has nothing to offer
// and the caller falls through to running the evidence agent.
func (d *DB) GetBranchTestEvidence(repoID, branch, excludeRunID string) (*BranchTestEvidence, error) {
	var entry BranchTestEvidence
	var intent sql.NullString
	err := d.sql.QueryRow(
		`SELECT res.run_id, res.findings_json, r.intent
		   FROM step_results res
		   JOIN runs r ON r.id = res.run_id
		  WHERE r.repo_id = ? AND r.branch = ? AND r.id != ?
		    AND res.step_name = ?
		    AND res.findings_json IS NOT NULL
		  ORDER BY res.id DESC
		  LIMIT 1`,
		repoID, branch, excludeRunID,
		string(types.StepTest),
	).Scan(&entry.RunID, &entry.FindingsJSON, &intent)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get branch test evidence: %w", err)
	}
	entry.Intent = intent.String
	return &entry, nil
}

package db

import (
	"database/sql"
	"fmt"
)

// RetainedCIRepair binds one attestation-interrupted publication to its owner
// and exact heads. It is not a review approval or a replacement run head.
type RetainedCIRepair struct {
	RunID, StepID, RepoID, Branch            string
	RecordedHead, ReviewedHead, RetainedHead string
}

func (d *DB) RetainedCIRepair(runID string) (*RetainedCIRepair, error) {
	r := &RetainedCIRepair{}
	err := d.sql.QueryRow(`SELECT run_id, step_id, repo_id, branch, recorded_head, reviewed_head, retained_head FROM retained_ci_repairs WHERE run_id = ?`, runID).
		Scan(&r.RunID, &r.StepID, &r.RepoID, &r.Branch, &r.RecordedHead, &r.ReviewedHead, &r.RetainedHead)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return r, err
}

// BindRetainedCIRepair refuses to overwrite an earlier binding or bind after
// the run's authority changed. Repeating the exact record is idempotent.
func (d *DB) BindRetainedCIRepair(r RetainedCIRepair) error {
	result, err := d.sql.Exec(`INSERT INTO retained_ci_repairs
        SELECT ?, ?, ?, ?, ?, ?, ? FROM runs JOIN step_results ON step_results.run_id = runs.id
        WHERE runs.id = ? AND step_results.id = ? AND step_results.step_name = 'ci'
        AND runs.repo_id = ? AND runs.branch = ? AND runs.head_sha = ? AND runs.review_approved_head_sha = ?
        ON CONFLICT(run_id) DO UPDATE SET retained_head = excluded.retained_head
        WHERE retained_ci_repairs.step_id = excluded.step_id AND retained_ci_repairs.repo_id = excluded.repo_id
        AND retained_ci_repairs.branch = excluded.branch AND retained_ci_repairs.recorded_head = excluded.recorded_head
        AND retained_ci_repairs.reviewed_head = excluded.reviewed_head AND retained_ci_repairs.retained_head = excluded.retained_head`, r.RunID, r.StepID, r.RepoID, r.Branch, r.RecordedHead, r.ReviewedHead, r.RetainedHead,
		r.RunID, r.StepID, r.RepoID, r.Branch, r.RecordedHead, r.ReviewedHead)
	if err != nil {
		return err
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		return fmt.Errorf("retained CI repair authority changed")
	}
	stored, err := d.RetainedCIRepair(r.RunID)
	if err != nil {
		return err
	}
	if stored == nil || *stored != r {
		return fmt.Errorf("retained CI repair authority changed")
	}
	return nil
}

func (d *DB) ClearRetainedCIRepair(runID, head string) error {
	_, err := d.sql.Exec(`DELETE FROM retained_ci_repairs WHERE run_id = ? AND retained_head = ?`, runID, head)
	return err
}

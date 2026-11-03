package db

import "fmt"

// ReconcileCustody fences an explicit terminal recovery against run creation,
// publication and configuration changes while apply moves the Git refs. apply
// must not call back into DB. A failed apply leaves custody outstanding.
func (d *DB) ReconcileCustody(run *Run, repo *Repo, branchRuns int, apply func() error) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.Exec(`UPDATE runs SET custody_returned_at = ?, updated_at = ?
		WHERE id = ? AND repo_id = ? AND branch = ? AND status = ?
		AND status IN ('cancelled', 'failed', 'completed')
		AND head_sha = ? AND submitted_head_sha IS ? AND last_pushed_sha IS ?
		AND push_generation IS ? AND push_target_kind IS ?
		AND push_target_fingerprint IS ? AND push_ref IS ?
		AND terminal_head_verified_at IS ? AND terminal_head_verified_at IS NOT NULL
		AND custody_returned_at IS NULL AND push_active = 0
		AND NOT EXISTS (SELECT 1 FROM recovery_archives WHERE owner_run_id = runs.id)
		AND EXISTS (SELECT 1 FROM repos WHERE id = ? AND upstream_url = ? AND fork_url IS ? AND working_path = ?)
		AND (SELECT COUNT(*) FROM runs WHERE repo_id = ? AND branch = ?) = ?
		AND NOT EXISTS (SELECT 1 FROM runs other WHERE other.repo_id = runs.repo_id
			AND other.branch = runs.branch AND other.id != runs.id
			AND other.status IN ('pending', 'running'))`,
		now(), now(), run.ID, run.RepoID, run.Branch, run.Status,
		run.HeadSHA, run.SubmittedHeadSHA, run.LastPushedSHA, run.PushGeneration,
		run.PushTargetKind, run.PushTargetFingerprint, run.PushRef, run.TerminalHeadVerifiedAt,
		repo.ID, repo.UpstreamURL, nullableString(repo.ForkURL), repo.WorkingPath, repo.ID, run.Branch, branchRuns)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil || n != 1 {
		return fmt.Errorf("the run or repository changed since the reconciliation observation")
	}
	if err := apply(); err != nil {
		return err
	}
	return tx.Commit()
}

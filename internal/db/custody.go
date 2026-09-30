package db

import (
	"database/sql"
	"errors"
	"fmt"
)

// ReleasePublishedCustody fences the entire terminal stack against its exact
// heads and push generations. The daemon holds the branch lock; the SQL fence
// also rejects a newer owner, a changed repository target, or an active push.
// Historical heads, errors and successful-push provenance remain unchanged.
func (d *DB) ReleasePublishedCustody(repo *Repo, selected *Run, runs []*Run) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Even an idempotent retry must fence the selected generation.
	all := append([]*Run{selected}, runs...)
	for _, run := range all {
		result, err := tx.Exec(`UPDATE runs SET custody_returned_at = COALESCE(custody_returned_at, ?), updated_at = ?
		 WHERE id = ? AND repo_id = ? AND branch = ? AND status = ? AND head_sha = ? AND error IS ?
		 AND publication_branch IS ? AND publication_target_fingerprint IS ?
		 AND COALESCE(push_generation, 0) = ? AND COALESCE(push_active, 0) = 0
		 AND (SELECT id FROM runs WHERE repo_id = ? AND branch = ? ORDER BY created_at DESC, id DESC LIMIT 1) = ?
		 AND NOT EXISTS (SELECT 1 FROM runs WHERE repo_id = ? AND (branch = ? OR publication_branch = ? OR branch = ? OR publication_branch = ?) AND status IN ('pending', 'running'))
		 AND EXISTS (SELECT 1 FROM repos WHERE id = ? AND upstream_url = ? AND COALESCE(fork_url, '') = ? AND default_branch = ? AND working_path = ?)`,
			now(), now(), run.ID, repo.ID, run.Branch, run.Status, run.HeadSHA, run.Error, run.PublicationBranch, run.PublicationTargetFingerprint, int64Value(run.PushGeneration),
			repo.ID, run.Branch, selected.ID, repo.ID, run.Branch, run.Branch, selected.PublishBranch(), selected.PublishBranch(), repo.ID, repo.UpstreamURL, repo.ForkURL, repo.DefaultBranch, repo.WorkingPath)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil || n != 1 {
			return fmt.Errorf("custody generation changed; retry with the current run")
		}
	}
	return tx.Commit()
}

func int64Value(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// RebindPublication is a destination-only compare-and-swap. It does not invent
// a successful push binding. The branch locks and executor parked guard are
// held by the daemon; this fence rejects concurrent generation/target changes.
func (d *DB) RebindPublication(repo *Repo, run *Run, branch, prURL, targetFingerprint string) error {
	result, err := d.sql.Exec(`UPDATE runs SET publication_branch = ?, publication_target_fingerprint = ?, pr_url = ?, pr_state = 'open',
	 ci_ready_at = NULL, ci_ready_no_ci = 0, updated_at = ?
	 WHERE id = ? AND repo_id = ? AND branch = ? AND status = ? AND status IN ('pending', 'running') AND head_sha = ?
	 AND (SELECT owner.id FROM runs owner WHERE owner.repo_id = runs.repo_id AND owner.branch = runs.branch ORDER BY owner.created_at DESC, owner.id DESC LIMIT 1) = runs.id
	 AND COALESCE(push_generation, 0) = ? AND publication_branch IS ? AND publication_target_fingerprint IS ? AND pr_url IS ? AND COALESCE(push_active, 0) = 0
	 AND EXISTS (SELECT 1 FROM repos WHERE id = ? AND upstream_url = ? AND COALESCE(fork_url, '') = ? AND default_branch = ? AND working_path = ?)
	 AND NOT EXISTS (SELECT 1 FROM runs other WHERE other.repo_id = runs.repo_id AND other.id <> runs.id
	 AND other.status IN ('pending', 'running') AND (other.branch = ? OR other.publication_branch = ?))`,
		branch, targetFingerprint, prURL, now(), run.ID, repo.ID, run.Branch, run.Status, run.HeadSHA, int64Value(run.PushGeneration), run.PublicationBranch, run.PublicationTargetFingerprint, run.PRURL,
		repo.ID, repo.UpstreamURL, repo.ForkURL, repo.DefaultBranch, repo.WorkingPath, branch, branch)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil || n != 1 {
		return fmt.Errorf("publication ownership or generation changed; no binding was changed")
	}
	return nil
}

// PublicationOwner protects the single-publisher invariant for both ordinary
// launches and rebound destinations. Terminal bindings confer no live lease.
func (d *DB) PublicationOwner(repoID, branch string) (*Run, error) {
	row := d.sql.QueryRow(`SELECT `+runColumns+` FROM runs WHERE repo_id = ?
	 AND status IN ('pending', 'running') AND publication_branch = ? LIMIT 1`, repoID, branch)
	r := &Run{}
	if err := scanRun(row, r); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return r, nil
}

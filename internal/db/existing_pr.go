package db

import (
	"database/sql"
	"errors"
	"fmt"
)

// setBranchPRTarget records the canonical pull request a branch publishes to.
// It outlives the run that established it: later launches, push-hook runs and
// CI repairs reuse it until it is replaced by another explicit association or
// retired. It is transaction-scoped on purpose - the only writer is the run
// insert that pins the same pull request, and the two must commit together.
func setBranchPRTarget(tx *sql.Tx, repoID, branch, prURL string, ts int64) error {
	_, err := tx.Exec(
		`INSERT INTO branch_pr_targets (repo_id, branch, pr_url, created_at, updated_at) VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(repo_id, branch) DO UPDATE SET pr_url = excluded.pr_url, updated_at = excluded.updated_at`,
		repoID, branch, prURL, ts, ts,
	)
	if err != nil {
		return fmt.Errorf("set branch pr target: %w", err)
	}
	return nil
}

// GetBranchPRTarget returns the branch's canonical pull request, or "" when the
// branch has none and ordinary repository-scoped discovery applies.
func (d *DB) GetBranchPRTarget(repoID, branch string) (string, error) {
	var prURL string
	err := d.sql.QueryRow(`SELECT pr_url FROM branch_pr_targets WHERE repo_id = ? AND branch = ?`, repoID, branch).Scan(&prURL)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get branch pr target: %w", err)
	}
	return prURL, nil
}

// DeleteBranchPRTarget retires a branch's association and reports whether one
// was recorded, so a caller can tell the operator nothing was retired.
func (d *DB) DeleteBranchPRTarget(repoID, branch string) (string, error) {
	existing, err := d.GetBranchPRTarget(repoID, branch)
	if err != nil {
		return "", err
	}
	if existing == "" {
		return "", nil
	}
	if _, err := d.sql.Exec(`DELETE FROM branch_pr_targets WHERE repo_id = ? AND branch = ?`, repoID, branch); err != nil {
		return "", fmt.Errorf("retire branch pr target: %w", err)
	}
	return existing, nil
}

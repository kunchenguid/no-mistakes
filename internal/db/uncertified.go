package db

import (
	"database/sql"
	"fmt"
	"strings"
)

// UncertifiedPipelineRange is the per-branch span of pipeline-authored
// commits whose re-review did not complete. The next run on that branch
// feeds this range into the initial review so the replacement reviewer is
// not cold. The database range is the authority; commit messages are not.
type UncertifiedPipelineRange struct {
	RepoID      string
	Branch      string
	FromSHA     string
	ToSHA       string
	SourceRunID string
	CreatedAt   int64
}

// UpsertUncertifiedPipelineRange records or replaces the uncertified fixer
// range for one repo+branch. A newer uncertified HEAD replaces an older one.
func (d *DB) UpsertUncertifiedPipelineRange(repoID, branch, fromSHA, toSHA, sourceRunID string) error {
	repoID = strings.TrimSpace(repoID)
	branch = strings.TrimSpace(branch)
	fromSHA = strings.TrimSpace(fromSHA)
	toSHA = strings.TrimSpace(toSHA)
	sourceRunID = strings.TrimSpace(sourceRunID)
	if repoID == "" || branch == "" || fromSHA == "" || toSHA == "" || sourceRunID == "" {
		return fmt.Errorf("uncertified pipeline range requires repo, branch, from_sha, to_sha, and source run")
	}
	_, err := d.sql.Exec(
		`INSERT INTO uncertified_pipeline_ranges (repo_id, branch, from_sha, to_sha, source_run_id, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(repo_id, branch) DO UPDATE SET
		   from_sha = excluded.from_sha,
		   to_sha = excluded.to_sha,
		   source_run_id = excluded.source_run_id,
		   created_at = excluded.created_at`,
		repoID, branch, fromSHA, toSHA, sourceRunID, now(),
	)
	if err != nil {
		return fmt.Errorf("upsert uncertified pipeline range: %w", err)
	}
	return nil
}

// GetUncertifiedPipelineRange returns the uncertified range for a branch, or
// nil when none is recorded.
func (d *DB) GetUncertifiedPipelineRange(repoID, branch string) (*UncertifiedPipelineRange, error) {
	repoID = strings.TrimSpace(repoID)
	branch = strings.TrimSpace(branch)
	if repoID == "" || branch == "" {
		return nil, nil
	}
	row := d.sql.QueryRow(
		`SELECT repo_id, branch, from_sha, to_sha, source_run_id, created_at
		 FROM uncertified_pipeline_ranges WHERE repo_id = ? AND branch = ?`,
		repoID, branch,
	)
	var r UncertifiedPipelineRange
	if err := row.Scan(&r.RepoID, &r.Branch, &r.FromSHA, &r.ToSHA, &r.SourceRunID, &r.CreatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("get uncertified pipeline range: %w", err)
	}
	return &r, nil
}

// DeleteUncertifiedPipelineRange removes the uncertified marker for a branch.
// It is a no-op when no row exists.
func (d *DB) DeleteUncertifiedPipelineRange(repoID, branch string) error {
	repoID = strings.TrimSpace(repoID)
	branch = strings.TrimSpace(branch)
	if repoID == "" || branch == "" {
		return nil
	}
	if _, err := d.sql.Exec(
		`DELETE FROM uncertified_pipeline_ranges WHERE repo_id = ? AND branch = ?`,
		repoID, branch,
	); err != nil {
		return fmt.Errorf("delete uncertified pipeline range: %w", err)
	}
	return nil
}

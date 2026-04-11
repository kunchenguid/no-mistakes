package db

import (
	"database/sql"
	"fmt"
)

// Repo represents a registered repository.
type Repo struct {
	ID            string
	WorkingPath   string
	UpstreamURL   string
	DefaultBranch string
	CreatedAt     int64
}

// InsertRepoWithID creates a new repo record with a caller-provided ID.
func (d *DB) InsertRepoWithID(id, workingPath, upstreamURL, defaultBranch string) (*Repo, error) {
	r := &Repo{
		ID:            id,
		WorkingPath:   workingPath,
		UpstreamURL:   upstreamURL,
		DefaultBranch: defaultBranch,
		CreatedAt:     now(),
	}
	_, err := d.sql.Exec(
		`INSERT INTO repos (id, working_path, upstream_url, default_branch, created_at) VALUES (?, ?, ?, ?, ?)`,
		r.ID, r.WorkingPath, r.UpstreamURL, r.DefaultBranch, r.CreatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("insert repo: %w", err)
	}
	return r, nil
}

// InsertRepo creates a new repo record and returns it with a generated ID.
func (d *DB) InsertRepo(workingPath, upstreamURL, defaultBranch string) (*Repo, error) {
	r := &Repo{
		ID:            newID(),
		WorkingPath:   workingPath,
		UpstreamURL:   upstreamURL,
		DefaultBranch: defaultBranch,
		CreatedAt:     now(),
	}
	_, err := d.sql.Exec(
		`INSERT INTO repos (id, working_path, upstream_url, default_branch, created_at) VALUES (?, ?, ?, ?, ?)`,
		r.ID, r.WorkingPath, r.UpstreamURL, r.DefaultBranch, r.CreatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("insert repo: %w", err)
	}
	return r, nil
}

// GetRepo returns a repo by ID.
func (d *DB) GetRepo(id string) (*Repo, error) {
	r := &Repo{}
	err := d.sql.QueryRow(
		`SELECT id, working_path, upstream_url, default_branch, created_at FROM repos WHERE id = ?`, id,
	).Scan(&r.ID, &r.WorkingPath, &r.UpstreamURL, &r.DefaultBranch, &r.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get repo: %w", err)
	}
	return r, nil
}

// GetRepoByPath returns a repo by its working path.
func (d *DB) GetRepoByPath(workingPath string) (*Repo, error) {
	r := &Repo{}
	err := d.sql.QueryRow(
		`SELECT id, working_path, upstream_url, default_branch, created_at FROM repos WHERE working_path = ?`, workingPath,
	).Scan(&r.ID, &r.WorkingPath, &r.UpstreamURL, &r.DefaultBranch, &r.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get repo by path: %w", err)
	}
	return r, nil
}

// DeleteRepo deletes a repo by ID (cascade deletes runs and steps).
func (d *DB) DeleteRepo(id string) error {
	_, err := d.sql.Exec(`DELETE FROM repos WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete repo: %w", err)
	}
	return nil
}

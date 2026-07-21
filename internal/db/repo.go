package db

import (
	"database/sql"
	"fmt"
	"strings"
)

// Repo represents a registered repository.
type Repo struct {
	ID            string
	WorkingPath   string
	UpstreamURL   string
	ForkURL       string
	DefaultBranch string
	// BaseBranch is an explicit pipeline integration and trust-root override.
	// Empty means future runs use DefaultBranch.
	BaseBranch string
	CreatedAt  int64
}

// EffectiveBaseBranch returns the configured pipeline base, falling back to
// the remote's recorded default branch when no override is registered.
func (r *Repo) EffectiveBaseBranch() string {
	if r == nil {
		return ""
	}
	if base := strings.TrimSpace(r.BaseBranch); base != "" {
		return base
	}
	return strings.TrimSpace(r.DefaultBranch)
}

// PushURL returns the remote URL that should receive branch updates.
func (r *Repo) PushURL() string {
	if r == nil {
		return ""
	}
	if strings.TrimSpace(r.ForkURL) != "" {
		return r.ForkURL
	}
	return r.UpstreamURL
}

// InsertRepoWithID creates a new repo record with a caller-provided ID.
func (d *DB) InsertRepoWithID(id, workingPath, upstreamURL, defaultBranch string) (*Repo, error) {
	return d.InsertRepoWithIDAndFork(id, workingPath, upstreamURL, "", defaultBranch)
}

// InsertRepoWithIDAndFork creates a repo record with an optional fork push URL.
func (d *DB) InsertRepoWithIDAndFork(id, workingPath, upstreamURL, forkURL, defaultBranch string) (*Repo, error) {
	return d.InsertRepoWithIDAndForkAndBase(id, workingPath, upstreamURL, forkURL, defaultBranch, "")
}

// InsertRepoWithIDAndForkAndBase creates a repo record with optional fork and
// pipeline-base settings.
func (d *DB) InsertRepoWithIDAndForkAndBase(id, workingPath, upstreamURL, forkURL, defaultBranch, baseBranch string) (*Repo, error) {
	r := &Repo{
		ID:            id,
		WorkingPath:   workingPath,
		UpstreamURL:   upstreamURL,
		ForkURL:       strings.TrimSpace(forkURL),
		DefaultBranch: defaultBranch,
		BaseBranch:    strings.TrimSpace(baseBranch),
		CreatedAt:     now(),
	}
	_, err := d.sql.Exec(
		`INSERT INTO repos (id, working_path, upstream_url, fork_url, default_branch, base_branch, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.WorkingPath, r.UpstreamURL, nullableString(r.ForkURL), r.DefaultBranch, nullableString(r.BaseBranch), r.CreatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("insert repo: %w", err)
	}
	return r, nil
}

// InsertRepo creates a new repo record and returns it with a generated ID.
func (d *DB) InsertRepo(workingPath, upstreamURL, defaultBranch string) (*Repo, error) {
	return d.InsertRepoWithFork(workingPath, upstreamURL, "", defaultBranch)
}

// InsertRepoWithFork creates a new repo record with an optional fork push URL.
func (d *DB) InsertRepoWithFork(workingPath, upstreamURL, forkURL, defaultBranch string) (*Repo, error) {
	r := &Repo{
		ID:            newID(),
		WorkingPath:   workingPath,
		UpstreamURL:   upstreamURL,
		ForkURL:       strings.TrimSpace(forkURL),
		DefaultBranch: defaultBranch,
		CreatedAt:     now(),
	}
	_, err := d.sql.Exec(
		`INSERT INTO repos (id, working_path, upstream_url, fork_url, default_branch, base_branch, created_at) VALUES (?, ?, ?, ?, ?, NULL, ?)`,
		r.ID, r.WorkingPath, r.UpstreamURL, nullableString(r.ForkURL), r.DefaultBranch, r.CreatedAt,
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
		`SELECT id, working_path, upstream_url, COALESCE(fork_url, ''), default_branch, COALESCE(base_branch, ''), created_at FROM repos WHERE id = ?`, id,
	).Scan(&r.ID, &r.WorkingPath, &r.UpstreamURL, &r.ForkURL, &r.DefaultBranch, &r.BaseBranch, &r.CreatedAt)
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
		`SELECT id, working_path, upstream_url, COALESCE(fork_url, ''), default_branch, COALESCE(base_branch, ''), created_at FROM repos WHERE working_path = ?`, workingPath,
	).Scan(&r.ID, &r.WorkingPath, &r.UpstreamURL, &r.ForkURL, &r.DefaultBranch, &r.BaseBranch, &r.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get repo by path: %w", err)
	}
	return r, nil
}

// UpdateRepoMetadata refreshes mutable repository metadata while preserving the
// stable repo ID, created_at timestamp, fork push URL, and base override.
func (d *DB) UpdateRepoMetadata(id, upstreamURL, defaultBranch string) (*Repo, error) {
	_, err := d.sql.Exec(
		`UPDATE repos SET upstream_url = ?, default_branch = ? WHERE id = ?`,
		upstreamURL, defaultBranch, id,
	)
	if err != nil {
		return nil, fmt.Errorf("update repo metadata: %w", err)
	}
	return d.GetRepo(id)
}

// UpdateRepoMetadataWithFork refreshes repo metadata and explicitly sets the
// optional fork push URL while preserving the base override.
func (d *DB) UpdateRepoMetadataWithFork(id, upstreamURL, forkURL, defaultBranch string) (*Repo, error) {
	_, err := d.sql.Exec(
		`UPDATE repos SET upstream_url = ?, fork_url = ?, default_branch = ? WHERE id = ?`,
		upstreamURL, nullableString(forkURL), defaultBranch, id,
	)
	if err != nil {
		return nil, fmt.Errorf("update repo metadata: %w", err)
	}
	return d.GetRepo(id)
}

// UpdateRepoMetadataAndBase atomically refreshes parent metadata and explicitly
// sets or clears the pipeline-base override while preserving fork routing.
func (d *DB) UpdateRepoMetadataAndBase(id, upstreamURL, defaultBranch, baseBranch string) (*Repo, error) {
	_, err := d.sql.Exec(
		`UPDATE repos SET upstream_url = ?, default_branch = ?, base_branch = ? WHERE id = ?`,
		upstreamURL, defaultBranch, nullableString(baseBranch), id,
	)
	if err != nil {
		return nil, fmt.Errorf("update repo metadata and base branch: %w", err)
	}
	return d.GetRepo(id)
}

// UpdateRepoMetadataWithForkAndBase atomically refreshes all mutable
// registration metadata, including an explicit fork and pipeline base.
func (d *DB) UpdateRepoMetadataWithForkAndBase(id, upstreamURL, forkURL, defaultBranch, baseBranch string) (*Repo, error) {
	_, err := d.sql.Exec(
		`UPDATE repos SET upstream_url = ?, fork_url = ?, default_branch = ?, base_branch = ? WHERE id = ?`,
		upstreamURL, nullableString(forkURL), defaultBranch, nullableString(baseBranch), id,
	)
	if err != nil {
		return nil, fmt.Errorf("update repo metadata and base branch: %w", err)
	}
	return d.GetRepo(id)
}

// UpdateRepoForkURL sets or clears the optional fork push URL.
func (d *DB) UpdateRepoForkURL(id, forkURL string) (*Repo, error) {
	_, err := d.sql.Exec(
		`UPDATE repos SET fork_url = ? WHERE id = ?`,
		nullableString(forkURL), id,
	)
	if err != nil {
		return nil, fmt.Errorf("update repo fork URL: %w", err)
	}
	return d.GetRepo(id)
}

// UpdateRepoWorkingPath moves a repo record to a new working path, preserving
// the repo ID (and with it the gate and run history) when the working
// directory is renamed or moved on disk.
func (d *DB) UpdateRepoWorkingPath(id, workingPath string) (*Repo, error) {
	_, err := d.sql.Exec(
		`UPDATE repos SET working_path = ? WHERE id = ?`,
		workingPath, id,
	)
	if err != nil {
		return nil, fmt.Errorf("update repo working path: %w", err)
	}
	return d.GetRepo(id)
}

// DeleteRepo deletes a repo by ID (cascade deletes runs and steps).
func (d *DB) DeleteRepo(id string) error {
	_, err := d.sql.Exec(`DELETE FROM repos WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete repo: %w", err)
	}
	return nil
}

func nullableString(s string) any {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	return s
}

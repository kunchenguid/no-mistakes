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
	CreatedAt     int64

	// URLsVerified is run-scoped, in-memory evidence that the URL fields were
	// just validated against the working clone. It is never persisted.
	URLsVerified bool `json:"-"`
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
	r := &Repo{
		ID:            id,
		WorkingPath:   workingPath,
		UpstreamURL:   upstreamURL,
		ForkURL:       strings.TrimSpace(forkURL),
		DefaultBranch: defaultBranch,
		CreatedAt:     now(),
	}
	_, err := d.sql.Exec(
		`INSERT INTO repos (id, working_path, upstream_url, fork_url, default_branch, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		r.ID, r.WorkingPath, r.UpstreamURL, nullableString(r.ForkURL), r.DefaultBranch, r.CreatedAt,
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
		`INSERT INTO repos (id, working_path, upstream_url, fork_url, default_branch, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		r.ID, r.WorkingPath, r.UpstreamURL, nullableString(r.ForkURL), r.DefaultBranch, r.CreatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("insert repo: %w", err)
	}
	return r, nil
}

// RepoWorkingPaths returns the working path of every registered repository.
//
// It is the set of checkouts a run worktree placement must stay out of (see
// worktrees.CheckPlacement): a run worktree inside a checkout leaves it dirty
// for the duration of the run. Both gates that judge placement read the set from
// here - the daemon at startup and `init --worktree-root` before it prints an
// entry - so the two cannot diverge on which checkouts exist.
func (d *DB) RepoWorkingPaths() ([]string, error) {
	rows, err := d.sql.Query(`SELECT working_path FROM repos ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("get repo working paths: %w", err)
	}
	defer rows.Close()

	var paths []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			return nil, fmt.Errorf("scan repo working path: %w", err)
		}
		paths = append(paths, path)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate repo working paths: %w", err)
	}
	return paths, nil
}

// GetRepos returns every authoritative repository record ordered by ID.
func (d *DB) GetRepos() ([]*Repo, error) {
	rows, err := d.sql.Query(
		`SELECT id, working_path, upstream_url, COALESCE(fork_url, ''), default_branch, created_at FROM repos ORDER BY id`,
	)
	if err != nil {
		return nil, fmt.Errorf("get repos: %w", err)
	}
	defer rows.Close()

	var repos []*Repo
	for rows.Next() {
		r := &Repo{}
		if err := rows.Scan(&r.ID, &r.WorkingPath, &r.UpstreamURL, &r.ForkURL, &r.DefaultBranch, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan repo: %w", err)
		}
		repos = append(repos, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate repos: %w", err)
	}
	return repos, nil
}

// GetRepo returns a repo by ID.
func (d *DB) GetRepo(id string) (*Repo, error) {
	r := &Repo{}
	err := d.sql.QueryRow(
		`SELECT id, working_path, upstream_url, COALESCE(fork_url, ''), default_branch, created_at FROM repos WHERE id = ?`, id,
	).Scan(&r.ID, &r.WorkingPath, &r.UpstreamURL, &r.ForkURL, &r.DefaultBranch, &r.CreatedAt)
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
		`SELECT id, working_path, upstream_url, COALESCE(fork_url, ''), default_branch, created_at FROM repos WHERE working_path = ?`, workingPath,
	).Scan(&r.ID, &r.WorkingPath, &r.UpstreamURL, &r.ForkURL, &r.DefaultBranch, &r.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get repo by path: %w", err)
	}
	return r, nil
}

// ReplaceRepoURLs atomically replaces both registered repository URLs and
// returns the committed record. A failure leaves the exact prior registration
// intact.
func (d *DB) ReplaceRepoURLs(id, upstreamURL, forkURL string) (*Repo, error) {
	tx, err := d.sql.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin repo URL replacement: %w", err)
	}
	defer tx.Rollback()

	result, err := tx.Exec(
		`UPDATE repos SET upstream_url = ?, fork_url = ? WHERE id = ?`,
		upstreamURL, nullableString(forkURL), id,
	)
	if err != nil {
		return nil, fmt.Errorf("replace repo URLs: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil {
		return nil, fmt.Errorf("replace repo URLs rows affected: %w", err)
	} else if affected != 1 {
		return nil, fmt.Errorf("replace repo URLs: repository not found")
	}

	r := &Repo{}
	if err := tx.QueryRow(
		`SELECT id, working_path, upstream_url, COALESCE(fork_url, ''), default_branch, created_at FROM repos WHERE id = ?`, id,
	).Scan(&r.ID, &r.WorkingPath, &r.UpstreamURL, &r.ForkURL, &r.DefaultBranch, &r.CreatedAt); err != nil {
		return nil, fmt.Errorf("read replaced repo URLs: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit repo URL replacement: %w", err)
	}
	return r, nil
}

// UpdateRepoMetadata refreshes mutable repository metadata while preserving the
// stable repo ID, created_at timestamp, and any existing fork push URL.
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
// optional fork push URL.
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

package db

import (
	"database/sql"
	"errors"
	"fmt"
)

// reboundHeadExecer is whatever can run the marker write: the connection for
// the standalone call, or the rebind's own transaction.
type reboundHeadExecer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// recordUnvalidatedReboundHead durably marks ref's rebound head as one no run
// has validated. The rewritten-remote recovery's compare-and-swap is the only
// writer, and it writes inside that transaction (RebindRunPushedHead), so the
// row exists exactly while the branch's push binding names a head the pipeline
// never validated. A later publication of the exact head by any run clears it
// (ClearUnvalidatedReboundHeadOnPublication); rebinding the same ref again
// replaces the row.
func recordUnvalidatedReboundHead(ex reboundHeadExecer, repoID, ref, head string) error {
	if repoID == "" || ref == "" || head == "" {
		return errors.New("record unvalidated rebound head: repo, ref, and head are required")
	}
	_, err := ex.Exec(
		`INSERT INTO unvalidated_rebound_heads (repo_id, ref, head_sha, created_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(repo_id, ref) DO UPDATE SET head_sha = excluded.head_sha, created_at = excluded.created_at`,
		repoID, ref, head, now(),
	)
	if err != nil {
		return fmt.Errorf("record unvalidated rebound head: %w", err)
	}
	return nil
}

// GetUnvalidatedReboundHead reports the head recorded as unvalidated for ref,
// and whether such a record exists. A missing record is not an error.
func (d *DB) GetUnvalidatedReboundHead(repoID, ref string) (string, bool, error) {
	var head string
	err := d.sql.QueryRow(
		`SELECT head_sha FROM unvalidated_rebound_heads WHERE repo_id = ? AND ref = ?`,
		repoID, ref,
	).Scan(&head)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read unvalidated rebound head: %w", err)
	}
	return head, true, nil
}

// ClearUnvalidatedReboundHeadOnPublication drops the marker once any run of
// the marker's repository published the exact head on ref. The publishing
// run's id only resolves the repository, so a later run's publication clears
// an earlier run's rebound marker.
func (d *DB) ClearUnvalidatedReboundHeadOnPublication(runID, ref, head string) error {
	_, err := d.sql.Exec(
		`DELETE FROM unvalidated_rebound_heads
		WHERE ref = ? AND head_sha = ? AND repo_id = (SELECT repo_id FROM runs WHERE id = ?)`,
		ref, head, runID,
	)
	if err != nil {
		return fmt.Errorf("clear unvalidated rebound head: %w", err)
	}
	return nil
}

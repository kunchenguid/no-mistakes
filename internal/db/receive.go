package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

const (
	ReceiveReservationReserved  = "reserved"
	ReceiveReservationPrepared  = "prepared"
	ReceiveReservationCommitted = "committed"
	ReceiveReservationPublished = "published"
	ReceiveReservationRetired   = "retired"
)

var ErrReceiveReservationConflict = errors.New("another receive reservation is pending for this repository branch")

type ReceiveReservation struct {
	ID        string
	RepoID    string
	GatePath  string
	Branch    string
	Ref       string
	OldSHA    string
	NewSHA    string
	SkipSteps []types.StepName
	Intent    string
	State     string
	RunID     *string
	CreatedAt int64
	UpdatedAt int64
}

func (d *DB) ReserveReceive(repoID, gatePath, branch, ref, oldSHA, newSHA string, skipSteps []types.StepName, intent string) (*ReceiveReservation, error) {
	if strings.TrimSpace(repoID) == "" || strings.TrimSpace(gatePath) == "" || strings.TrimSpace(branch) == "" {
		return nil, fmt.Errorf("reserve receive: repository, gate path, and branch are required")
	}
	if err := validateReceiveTransition(branch, ref, oldSHA, newSHA); err != nil {
		return nil, fmt.Errorf("reserve receive: %w", err)
	}
	encodedSteps, err := json.Marshal(skipSteps)
	if err != nil {
		return nil, fmt.Errorf("reserve receive: encode skipped steps: %w", err)
	}
	ts := now()
	tx, err := d.sql.Begin()
	if err != nil {
		return nil, fmt.Errorf("reserve receive: begin: %w", err)
	}
	defer tx.Rollback()

	if existing, err := scanReceiveReservation(tx.QueryRow(`SELECT id, repo_id, gate_path, branch, ref, old_sha, new_sha, skip_steps, intent, state, run_id, created_at, updated_at FROM receive_reservations WHERE repo_id = ? AND branch = ? AND ref = ? AND old_sha = ? AND new_sha = ? AND state IN (?, ?, ?) ORDER BY created_at, id LIMIT 1`, repoID, branch, ref, oldSHA, newSHA, ReceiveReservationReserved, ReceiveReservationPrepared, ReceiveReservationCommitted)); err == nil {
		return existing, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("reserve receive: read existing reservation: %w", err)
	}
	var pendingID string
	err = tx.QueryRow(`SELECT id FROM receive_reservations WHERE repo_id = ? AND branch = ? AND state IN (?, ?, ?) ORDER BY created_at, id LIMIT 1`, repoID, branch, ReceiveReservationReserved, ReceiveReservationPrepared, ReceiveReservationCommitted).Scan(&pendingID)
	if err == nil {
		return nil, fmt.Errorf("%w: %s", ErrReceiveReservationConflict, pendingID)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("reserve receive: check pending reservation: %w", err)
	}
	id := newID()
	if _, err := tx.Exec(`INSERT INTO receive_reservations (id, repo_id, gate_path, branch, ref, old_sha, new_sha, skip_steps, intent, state, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, id, repoID, gatePath, branch, ref, oldSHA, newSHA, string(encodedSteps), intent, ReceiveReservationReserved, ts, ts); err != nil {
		return nil, fmt.Errorf("reserve receive: insert: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("reserve receive: commit: %w", err)
	}
	return &ReceiveReservation{ID: id, RepoID: repoID, GatePath: gatePath, Branch: branch, Ref: ref, OldSHA: oldSHA, NewSHA: newSHA, SkipSteps: skipSteps, Intent: intent, State: ReceiveReservationReserved, CreatedAt: ts, UpdatedAt: ts}, nil
}

func (d *DB) GetPendingReceiveReservation(repoID, branch, ref, oldSHA, newSHA string) (*ReceiveReservation, error) {
	reservation, err := scanReceiveReservation(d.sql.QueryRow(`SELECT id, repo_id, gate_path, branch, ref, old_sha, new_sha, skip_steps, intent, state, run_id, created_at, updated_at FROM receive_reservations WHERE repo_id = ? AND branch = ? AND ref = ? AND old_sha = ? AND new_sha = ? AND state IN (?, ?, ?) ORDER BY created_at, id LIMIT 1`, repoID, branch, ref, oldSHA, newSHA, ReceiveReservationReserved, ReceiveReservationPrepared, ReceiveReservationCommitted))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get pending receive reservation: %w", err)
	}
	return reservation, nil
}

func (d *DB) GetLatestReceiveReservation(repoID, branch, ref, oldSHA, newSHA string) (*ReceiveReservation, error) {
	reservation, err := scanReceiveReservation(d.sql.QueryRow(`SELECT id, repo_id, gate_path, branch, ref, old_sha, new_sha, skip_steps, intent, state, run_id, created_at, updated_at FROM receive_reservations WHERE repo_id = ? AND branch = ? AND ref = ? AND old_sha = ? AND new_sha = ? ORDER BY created_at DESC, id DESC LIMIT 1`, repoID, branch, ref, oldSHA, newSHA))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get latest receive reservation: %w", err)
	}
	return reservation, nil
}

func (d *DB) GetReceiveReservation(id string) (*ReceiveReservation, error) {
	reservation, err := scanReceiveReservation(d.sql.QueryRow(`SELECT id, repo_id, gate_path, branch, ref, old_sha, new_sha, skip_steps, intent, state, run_id, created_at, updated_at FROM receive_reservations WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get receive reservation: %w", err)
	}
	return reservation, nil
}

func (d *DB) GetPendingReceiveReservations() ([]*ReceiveReservation, error) {
	rows, err := d.sql.Query(`SELECT id, repo_id, gate_path, branch, ref, old_sha, new_sha, skip_steps, intent, state, run_id, created_at, updated_at FROM receive_reservations WHERE state IN (?, ?, ?) ORDER BY created_at, id`, ReceiveReservationReserved, ReceiveReservationPrepared, ReceiveReservationCommitted)
	if err != nil {
		return nil, fmt.Errorf("get pending receive reservations: %w", err)
	}
	defer rows.Close()
	var reservations []*ReceiveReservation
	for rows.Next() {
		reservation, err := scanReceiveReservation(rows)
		if err != nil {
			return nil, fmt.Errorf("scan receive reservation: %w", err)
		}
		reservations = append(reservations, reservation)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate receive reservations: %w", err)
	}
	return reservations, nil
}

func (d *DB) GetPendingReceiveReservationsForBranch(repoID, branch string) ([]*ReceiveReservation, error) {
	rows, err := d.sql.Query(`SELECT id, repo_id, gate_path, branch, ref, old_sha, new_sha, skip_steps, intent, state, run_id, created_at, updated_at FROM receive_reservations WHERE repo_id = ? AND branch = ? AND state IN (?, ?, ?) ORDER BY created_at, id`, repoID, branch, ReceiveReservationReserved, ReceiveReservationPrepared, ReceiveReservationCommitted)
	if err != nil {
		return nil, fmt.Errorf("get pending receive reservations for branch: %w", err)
	}
	defer rows.Close()
	var reservations []*ReceiveReservation
	for rows.Next() {
		reservation, err := scanReceiveReservation(rows)
		if err != nil {
			return nil, fmt.Errorf("scan receive reservation: %w", err)
		}
		reservations = append(reservations, reservation)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate receive reservations: %w", err)
	}
	return reservations, nil
}

func (d *DB) CompleteReceiveReservation(id, runID string) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("complete receive reservation: id is required")
	}
	runID = strings.TrimSpace(runID)
	var result sql.Result
	var err error
	if runID == "" {
		result, err = d.sql.Exec(`UPDATE receive_reservations SET state = ?, run_id = NULL, updated_at = ? WHERE id = ? AND state IN (?, ?)`, ReceiveReservationPublished, now(), id, ReceiveReservationReserved, ReceiveReservationCommitted)
	} else {
		result, err = d.sql.Exec(`UPDATE receive_reservations SET state = ?, run_id = ?, updated_at = ? WHERE id = ? AND state IN (?, ?)`, ReceiveReservationPublished, runID, now(), id, ReceiveReservationReserved, ReceiveReservationCommitted)
	}
	if err != nil {
		return fmt.Errorf("complete receive reservation: %w", err)
	}
	if rows, err := result.RowsAffected(); err != nil {
		return fmt.Errorf("complete receive reservation: affected rows: %w", err)
	} else if rows == 1 {
		return nil
	}
	current, err := d.GetReceiveReservation(id)
	if err != nil {
		return err
	}
	if current != nil && current.State == ReceiveReservationPublished {
		if runID == "" && current.RunID == nil {
			return nil
		}
		if current.RunID != nil && *current.RunID == runID {
			return nil
		}
	}
	return fmt.Errorf("complete receive reservation: ownership changed")
}

func (d *DB) MarkReceivePrepared(repoID, branch, ref, oldSHA, newSHA string) error {
	return d.markReceivePrepared("", repoID, branch, ref, oldSHA, newSHA)
}

func (d *DB) MarkReceivePreparedForID(id, repoID, branch, ref, oldSHA, newSHA string) error {
	return d.markReceivePrepared(id, repoID, branch, ref, oldSHA, newSHA)
}

func (d *DB) markReceivePrepared(id, repoID, branch, ref, oldSHA, newSHA string) error {
	if err := validateReceiveTransition(branch, ref, oldSHA, newSHA); err != nil {
		return fmt.Errorf("mark receive prepared: %w", err)
	}
	query := `UPDATE receive_reservations SET state = ?, updated_at = ? WHERE repo_id = ? AND branch = ? AND ref = ? AND old_sha = ? AND new_sha = ? AND state = ?`
	args := []any{ReceiveReservationPrepared, now(), repoID, branch, ref, oldSHA, newSHA, ReceiveReservationReserved}
	if id != "" {
		query = `UPDATE receive_reservations SET state = ?, updated_at = ? WHERE id = ? AND repo_id = ? AND branch = ? AND ref = ? AND old_sha = ? AND new_sha = ? AND state = ?`
		args = []any{ReceiveReservationPrepared, now(), id, repoID, branch, ref, oldSHA, newSHA, ReceiveReservationReserved}
	}
	result, err := d.sql.Exec(query, args...)
	if err != nil {
		return fmt.Errorf("mark receive prepared: %w", err)
	}
	if rows, err := result.RowsAffected(); err != nil {
		return fmt.Errorf("mark receive prepared: affected rows: %w", err)
	} else if rows == 1 {
		return nil
	}
	var current *ReceiveReservation
	if id != "" {
		current, err = d.GetReceiveReservation(id)
	} else {
		current, err = d.GetLatestReceiveReservation(repoID, branch, ref, oldSHA, newSHA)
	}
	if err != nil {
		return err
	}
	if current != nil && (current.State == ReceiveReservationPrepared || current.State == ReceiveReservationCommitted) {
		return nil
	}
	return fmt.Errorf("mark receive prepared: exact reservation is not pending")
}

func (d *DB) MarkReceiveCommitted(repoID, branch, ref, oldSHA, newSHA string) error {
	return d.markReceiveCommitted("", repoID, branch, ref, oldSHA, newSHA)
}

func (d *DB) MarkReceiveCommittedForID(id, repoID, branch, ref, oldSHA, newSHA string) error {
	return d.markReceiveCommitted(id, repoID, branch, ref, oldSHA, newSHA)
}

func (d *DB) markReceiveCommitted(id, repoID, branch, ref, oldSHA, newSHA string) error {
	if err := validateReceiveTransition(branch, ref, oldSHA, newSHA); err != nil {
		return fmt.Errorf("mark receive committed: %w", err)
	}
	query := `UPDATE receive_reservations SET state = ?, updated_at = ? WHERE repo_id = ? AND branch = ? AND ref = ? AND old_sha = ? AND new_sha = ? AND state = ?`
	args := []any{ReceiveReservationCommitted, now(), repoID, branch, ref, oldSHA, newSHA, ReceiveReservationPrepared}
	if id != "" {
		query = `UPDATE receive_reservations SET state = ?, updated_at = ? WHERE id = ? AND repo_id = ? AND branch = ? AND ref = ? AND old_sha = ? AND new_sha = ? AND state = ?`
		args = []any{ReceiveReservationCommitted, now(), id, repoID, branch, ref, oldSHA, newSHA, ReceiveReservationPrepared}
	}
	result, err := d.sql.Exec(query, args...)
	if err != nil {
		return fmt.Errorf("mark receive committed: %w", err)
	}
	if rows, err := result.RowsAffected(); err != nil {
		return fmt.Errorf("mark receive committed: affected rows: %w", err)
	} else if rows == 1 {
		return nil
	}
	var current *ReceiveReservation
	if id != "" {
		current, err = d.GetReceiveReservation(id)
	} else {
		current, err = d.GetLatestReceiveReservation(repoID, branch, ref, oldSHA, newSHA)
	}
	if err != nil {
		return err
	}
	if current != nil && current.State == ReceiveReservationCommitted {
		return nil
	}
	return fmt.Errorf("mark receive committed: exact prepared reservation is missing")
}

func (d *DB) MarkReceiveAborted(repoID, branch, ref, oldSHA, newSHA string) error {
	return d.markReceiveAborted("", repoID, branch, ref, oldSHA, newSHA)
}

func (d *DB) MarkReceiveAbortedForID(id, repoID, branch, ref, oldSHA, newSHA string) error {
	return d.markReceiveAborted(id, repoID, branch, ref, oldSHA, newSHA)
}

func (d *DB) markReceiveAborted(id, repoID, branch, ref, oldSHA, newSHA string) error {
	if err := validateReceiveTransition(branch, ref, oldSHA, newSHA); err != nil {
		return fmt.Errorf("mark receive aborted: %w", err)
	}
	query := `UPDATE receive_reservations SET state = ?, updated_at = ? WHERE repo_id = ? AND branch = ? AND ref = ? AND old_sha = ? AND new_sha = ? AND state IN (?, ?)`
	args := []any{ReceiveReservationRetired, now(), repoID, branch, ref, oldSHA, newSHA, ReceiveReservationReserved, ReceiveReservationPrepared}
	if id != "" {
		query = `UPDATE receive_reservations SET state = ?, updated_at = ? WHERE id = ? AND repo_id = ? AND branch = ? AND ref = ? AND old_sha = ? AND new_sha = ? AND state IN (?, ?)`
		args = []any{ReceiveReservationRetired, now(), id, repoID, branch, ref, oldSHA, newSHA, ReceiveReservationReserved, ReceiveReservationPrepared}
	}
	result, err := d.sql.Exec(query, args...)
	if err != nil {
		return fmt.Errorf("mark receive aborted: %w", err)
	}
	if rows, err := result.RowsAffected(); err != nil {
		return fmt.Errorf("mark receive aborted: affected rows: %w", err)
	} else if rows == 1 {
		return nil
	}
	var current *ReceiveReservation
	if id != "" {
		current, err = d.GetReceiveReservation(id)
	} else {
		current, err = d.GetLatestReceiveReservation(repoID, branch, ref, oldSHA, newSHA)
	}
	if err != nil {
		return err
	}
	if current != nil && current.State == ReceiveReservationRetired {
		return nil
	}
	return fmt.Errorf("mark receive aborted: exact reservation cannot be aborted")
}

func (d *DB) RetireReceiveReservation(id string) error {
	result, err := d.sql.Exec(`UPDATE receive_reservations SET state = ?, updated_at = ? WHERE id = ? AND state IN (?, ?)`, ReceiveReservationRetired, now(), id, ReceiveReservationReserved, ReceiveReservationPrepared)
	if err != nil {
		return fmt.Errorf("retire receive reservation: %w", err)
	}
	if rows, err := result.RowsAffected(); err != nil {
		return fmt.Errorf("retire receive reservation: affected rows: %w", err)
	} else if rows == 1 {
		return nil
	}
	current, err := d.GetReceiveReservation(id)
	if err != nil {
		return err
	}
	if current != nil && current.State == ReceiveReservationRetired {
		return nil
	}
	return fmt.Errorf("retire receive reservation: ownership changed")
}

type receiveReservationScanner interface {
	Scan(...any) error
}

func scanReceiveReservation(row receiveReservationScanner) (*ReceiveReservation, error) {
	reservation := &ReceiveReservation{}
	var encodedSteps, intent, runID sql.NullString
	if err := row.Scan(&reservation.ID, &reservation.RepoID, &reservation.GatePath, &reservation.Branch, &reservation.Ref, &reservation.OldSHA, &reservation.NewSHA, &encodedSteps, &intent, &reservation.State, &runID, &reservation.CreatedAt, &reservation.UpdatedAt); err != nil {
		return nil, err
	}
	if encodedSteps.Valid && encodedSteps.String != "" {
		if err := json.Unmarshal([]byte(encodedSteps.String), &reservation.SkipSteps); err != nil {
			return nil, fmt.Errorf("decode skipped steps: %w", err)
		}
	}
	if intent.Valid {
		reservation.Intent = intent.String
	}
	if runID.Valid {
		reservation.RunID = &runID.String
	}
	return reservation, nil
}

func receiveObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func validateReceiveTransition(branch, ref, oldSHA, newSHA string) error {
	if ref != "refs/heads/"+branch {
		return fmt.Errorf("ref %q does not match branch %q", ref, branch)
	}
	if !receiveObjectID(oldSHA) || !receiveObjectID(newSHA) || oldSHA == newSHA {
		return fmt.Errorf("old and new values must be distinct full object IDs")
	}
	return nil
}

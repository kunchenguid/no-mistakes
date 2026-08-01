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
	if ref != "refs/heads/"+branch {
		return nil, fmt.Errorf("reserve receive: ref %q does not match branch %q", ref, branch)
	}
	if !receiveObjectID(oldSHA) || !receiveObjectID(newSHA) || oldSHA == newSHA {
		return nil, fmt.Errorf("reserve receive: old and new values must be distinct full object IDs")
	}
	if newSHA == zeroObjectID(len(newSHA)) {
		return nil, fmt.Errorf("reserve receive: ref deletion has no pipeline reservation")
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

	if existing, err := scanReceiveReservation(tx.QueryRow(`SELECT id, repo_id, gate_path, branch, ref, old_sha, new_sha, skip_steps, intent, state, run_id, created_at, updated_at FROM receive_reservations WHERE repo_id = ? AND branch = ? AND ref = ? AND old_sha = ? AND new_sha = ? AND state = ? ORDER BY created_at, id LIMIT 1`, repoID, branch, ref, oldSHA, newSHA, ReceiveReservationReserved)); err == nil {
		return existing, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("reserve receive: read existing reservation: %w", err)
	}
	var pendingID string
	err = tx.QueryRow(`SELECT id FROM receive_reservations WHERE repo_id = ? AND branch = ? AND state = ? ORDER BY created_at, id LIMIT 1`, repoID, branch, ReceiveReservationReserved).Scan(&pendingID)
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
	reservation, err := scanReceiveReservation(d.sql.QueryRow(`SELECT id, repo_id, gate_path, branch, ref, old_sha, new_sha, skip_steps, intent, state, run_id, created_at, updated_at FROM receive_reservations WHERE repo_id = ? AND branch = ? AND ref = ? AND old_sha = ? AND new_sha = ? AND state = ? ORDER BY created_at, id LIMIT 1`, repoID, branch, ref, oldSHA, newSHA, ReceiveReservationReserved))
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
	rows, err := d.sql.Query(`SELECT id, repo_id, gate_path, branch, ref, old_sha, new_sha, skip_steps, intent, state, run_id, created_at, updated_at FROM receive_reservations WHERE state = ? ORDER BY created_at, id`, ReceiveReservationReserved)
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
	rows, err := d.sql.Query(`SELECT id, repo_id, gate_path, branch, ref, old_sha, new_sha, skip_steps, intent, state, run_id, created_at, updated_at FROM receive_reservations WHERE repo_id = ? AND branch = ? AND state = ? ORDER BY created_at, id`, repoID, branch, ReceiveReservationReserved)
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
	if strings.TrimSpace(id) == "" || strings.TrimSpace(runID) == "" {
		return fmt.Errorf("complete receive reservation: id and run are required")
	}
	result, err := d.sql.Exec(`UPDATE receive_reservations SET state = ?, run_id = ?, updated_at = ? WHERE id = ? AND state = ?`, ReceiveReservationPublished, runID, now(), id, ReceiveReservationReserved)
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
	if current != nil && current.State == ReceiveReservationPublished && current.RunID != nil && *current.RunID == runID {
		return nil
	}
	return fmt.Errorf("complete receive reservation: ownership changed")
}

func (d *DB) RetireReceiveReservation(id string) error {
	result, err := d.sql.Exec(`UPDATE receive_reservations SET state = ?, updated_at = ? WHERE id = ? AND state = ?`, ReceiveReservationRetired, now(), id, ReceiveReservationReserved)
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

func zeroObjectID(length int) string {
	return strings.Repeat("0", length)
}

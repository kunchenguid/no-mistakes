package db

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
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
var ErrReceiveSessionPending = errors.New("receive session has non-terminal reservations")

const (
	receiveSessionPhaseIssued   = "issued"
	receiveSessionPhaseAdmitted = "admitted"
	receiveSessionPhaseAborted  = "aborted"
	receiveSessionPhaseRetired  = "retired"
)

type ReceiveReservation struct {
	ID             string
	RepoID         string
	GatePath       string
	Branch         string
	Ref            string
	OldSHA         string
	NewSHA         string
	SessionID      string
	CapabilityHash string
	SkipSteps      []types.StepName
	Intent         string
	State          string
	RunID          *string
	CreatedAt      int64
	UpdatedAt      int64
}

type ReceiveSession struct {
	ID       string
	RepoID   string
	GatePath string
	Phase    string
}

type ReceiveReservationInput struct {
	RepoID    string
	GatePath  string
	Branch    string
	Ref       string
	OldSHA    string
	NewSHA    string
	SkipSteps []types.StepName
	Intent    string
}

type ReceiveTransactionInput struct {
	ID     string
	RepoID string
	Branch string
	Ref    string
	OldSHA string
	NewSHA string
}

const receiveReservationSelect = `SELECT id, repo_id, gate_path, branch, ref, old_sha, new_sha, receive_session_id, receive_capability_hash, skip_steps, intent, state, run_id, created_at, updated_at FROM receive_reservations`

func (d *DB) ReserveReceiveForSession(repoID, gatePath, branch, ref, oldSHA, newSHA, sessionID, capability string, skipSteps []types.StepName, intent string) (*ReceiveReservation, error) {
	return d.reserveReceive(repoID, gatePath, branch, ref, oldSHA, newSHA, sessionID, capability, skipSteps, intent, false)
}

func (d *DB) ReserveReceiveForAuthenticatedSession(repoID, gatePath, branch, ref, oldSHA, newSHA, sessionID, capability string, skipSteps []types.StepName, intent string) (*ReceiveReservation, error) {
	reservations, err := d.ReserveReceivesForAuthenticatedSession(sessionID, capability, []ReceiveReservationInput{{RepoID: repoID, GatePath: gatePath, Branch: branch, Ref: ref, OldSHA: oldSHA, NewSHA: newSHA, SkipSteps: skipSteps, Intent: intent}})
	if err != nil {
		return nil, err
	}
	return reservations[0], nil
}

func (d *DB) ReserveReceivesForAuthenticatedSession(sessionID, capability string, inputs []ReceiveReservationInput) ([]*ReceiveReservation, error) {
	sessionID = strings.TrimSpace(sessionID)
	capability = strings.TrimSpace(capability)
	if sessionID == "" || capability == "" {
		return nil, fmt.Errorf("reserve receive batch: authenticated session and capability are required")
	}
	if len(inputs) == 0 {
		return nil, fmt.Errorf("reserve receive batch: at least one transition is required")
	}
	repoID := strings.TrimSpace(inputs[0].RepoID)
	gatePath := strings.TrimSpace(inputs[0].GatePath)
	if repoID == "" || gatePath == "" {
		return nil, fmt.Errorf("reserve receive batch: repository and gate path are required")
	}
	seen := make(map[string]struct{}, len(inputs))
	for _, input := range inputs {
		if strings.TrimSpace(input.RepoID) != repoID || strings.TrimSpace(input.GatePath) != gatePath || strings.TrimSpace(input.Branch) == "" {
			return nil, fmt.Errorf("reserve receive batch: repository, gate path, and branch are required")
		}
		if err := validateReceiveTransition(input.Branch, input.Ref, input.OldSHA, input.NewSHA); err != nil {
			return nil, fmt.Errorf("reserve receive batch: %w", err)
		}
		key := strings.Join([]string{input.RepoID, input.Branch, input.Ref, input.OldSHA, input.NewSHA}, "\x00")
		if _, ok := seen[key]; ok {
			return nil, fmt.Errorf("reserve receive batch: duplicate transition %s", input.Ref)
		}
		seen[key] = struct{}{}
	}
	capabilityHash := receiveCapabilityHash(capability)
	batchHash := receiveBatchHash(inputs)
	tx, err := d.sql.Begin()
	if err != nil {
		return nil, fmt.Errorf("reserve receive batch: begin: %w", err)
	}
	defer tx.Rollback()
	var sessionRepo, sessionGate, sessionHash, sessionState, sessionPhase, storedBatchHash string
	if err := tx.QueryRow(`SELECT repo_id, gate_path, capability_hash, state, phase, batch_hash FROM receive_sessions WHERE id = ?`, sessionID).Scan(&sessionRepo, &sessionGate, &sessionHash, &sessionState, &sessionPhase, &storedBatchHash); err != nil {
		return nil, fmt.Errorf("reserve receive batch: verify session: %w", err)
	}
	if sessionRepo != repoID || sessionGate != gatePath || sessionHash != capabilityHash || sessionState != "active" {
		return nil, fmt.Errorf("reserve receive batch: receive session is not active")
	}
	firstAdmission := false
	switch sessionPhase {
	case receiveSessionPhaseIssued:
		if storedBatchHash != "" {
			return nil, fmt.Errorf("reserve receive batch: receive session has an invalid batch seal")
		}
		result, err := tx.Exec(`UPDATE receive_sessions SET phase = ?, batch_hash = ?, updated_at = ? WHERE id = ? AND state = 'active' AND phase = ? AND batch_hash = ''`, receiveSessionPhaseAdmitted, batchHash, now(), sessionID, receiveSessionPhaseIssued)
		if err != nil {
			return nil, fmt.Errorf("reserve receive batch: seal session: %w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil || affected != 1 {
			return nil, fmt.Errorf("reserve receive batch: session ownership changed")
		}
		firstAdmission = true
	case receiveSessionPhaseAdmitted:
		if storedBatchHash != batchHash {
			return nil, fmt.Errorf("reserve receive batch: receive session is sealed to another batch")
		}
	case receiveSessionPhaseAborted, receiveSessionPhaseRetired:
		return nil, fmt.Errorf("reserve receive batch: receive session is no longer admitting transitions")
	default:
		return nil, fmt.Errorf("reserve receive batch: unknown receive session phase %q", sessionPhase)
	}
	for _, input := range inputs {
		var boundRef, boundOld, boundNew string
		err := tx.QueryRow(`SELECT ref, old_sha, new_sha FROM receive_reservations WHERE repo_id = ? AND receive_session_id = ? AND branch = ? AND NOT (ref = ? AND old_sha = ? AND new_sha = ?) ORDER BY created_at, id LIMIT 1`, input.RepoID, sessionID, input.Branch, input.Ref, input.OldSHA, input.NewSHA).Scan(&boundRef, &boundOld, &boundNew)
		if err == nil {
			return nil, fmt.Errorf("reserve receive batch: receive session is already bound to branch %s", input.Branch)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("reserve receive batch: check session branch: %w", err)
		}
		var pendingID string
		err = tx.QueryRow(`SELECT id FROM receive_reservations WHERE repo_id = ? AND branch = ? AND state IN (?, ?, ?) AND NOT (receive_session_id = ? AND ref = ? AND old_sha = ? AND new_sha = ?) ORDER BY created_at, id LIMIT 1`, input.RepoID, input.Branch, ReceiveReservationReserved, ReceiveReservationPrepared, ReceiveReservationCommitted, sessionID, input.Ref, input.OldSHA, input.NewSHA).Scan(&pendingID)
		if err == nil {
			return nil, fmt.Errorf("%w: %s", ErrReceiveReservationConflict, pendingID)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("reserve receive batch: check pending reservation: %w", err)
		}
	}
	results := make([]*ReceiveReservation, len(inputs))
	ts := now()
	for i, input := range inputs {
		existing, err := scanReceiveReservation(tx.QueryRow(receiveReservationSelect+` WHERE repo_id = ? AND receive_session_id = ? AND receive_capability_hash = ? AND branch = ? AND ref = ? AND old_sha = ? AND new_sha = ? ORDER BY created_at DESC, id DESC LIMIT 1`, input.RepoID, sessionID, capabilityHash, input.Branch, input.Ref, input.OldSHA, input.NewSHA))
		if err == nil {
			results[i] = existing
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("reserve receive batch: read existing reservation: %w", err)
		}
		if !firstAdmission {
			return nil, fmt.Errorf("reserve receive batch: sealed batch reservation is missing")
		}
		encodedSteps, err := json.Marshal(input.SkipSteps)
		if err != nil {
			return nil, fmt.Errorf("reserve receive batch: encode skipped steps: %w", err)
		}
		id := newID()
		if _, err := tx.Exec(`INSERT INTO receive_reservations (id, repo_id, gate_path, branch, ref, old_sha, new_sha, receive_session_id, receive_capability_hash, skip_steps, intent, state, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, id, input.RepoID, input.GatePath, input.Branch, input.Ref, input.OldSHA, input.NewSHA, sessionID, capabilityHash, string(encodedSteps), input.Intent, ReceiveReservationReserved, ts, ts); err != nil {
			return nil, fmt.Errorf("reserve receive batch: insert: %w", err)
		}
		results[i] = &ReceiveReservation{ID: id, RepoID: input.RepoID, GatePath: input.GatePath, Branch: input.Branch, Ref: input.Ref, OldSHA: input.OldSHA, NewSHA: input.NewSHA, SessionID: sessionID, CapabilityHash: capabilityHash, SkipSteps: input.SkipSteps, Intent: input.Intent, State: ReceiveReservationReserved, CreatedAt: ts, UpdatedAt: ts}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("reserve receive batch: commit: %w", err)
	}
	return results, nil
}

func (d *DB) RegisterReceiveSession(repoID, gatePath, sessionID, capability string) error {
	repoID = strings.TrimSpace(repoID)
	gatePath = strings.TrimSpace(gatePath)
	sessionID = strings.TrimSpace(sessionID)
	capability = strings.TrimSpace(capability)
	if repoID == "" || gatePath == "" || sessionID == "" || capability == "" {
		return fmt.Errorf("register receive session: repository, gate path, session, and capability are required")
	}
	hash := receiveCapabilityHash(capability)
	tx, err := d.sql.Begin()
	if err != nil {
		return fmt.Errorf("register receive session: begin: %w", err)
	}
	defer tx.Rollback()
	var existingRepo, existingGate, existingHash, state, phase, batchHash string
	err = tx.QueryRow(`SELECT repo_id, gate_path, capability_hash, state, phase, batch_hash FROM receive_sessions WHERE id = ?`, sessionID).Scan(&existingRepo, &existingGate, &existingHash, &state, &phase, &batchHash)
	switch {
	case err == nil:
		if existingRepo != repoID || existingGate != gatePath || existingHash != hash {
			return fmt.Errorf("register receive session: identity is already bound")
		}
		if state != "active" {
			return fmt.Errorf("register receive session: session is no longer active")
		}
	case errors.Is(err, sql.ErrNoRows):
		stamp := now()
		if _, err := tx.Exec(`INSERT INTO receive_sessions (id, repo_id, gate_path, capability_hash, state, phase, batch_hash, created_at, updated_at) VALUES (?, ?, ?, ?, 'active', ?, '', ?, ?)`, sessionID, repoID, gatePath, hash, receiveSessionPhaseIssued, stamp, stamp); err != nil {
			return fmt.Errorf("register receive session: insert: %w", err)
		}
	default:
		return fmt.Errorf("register receive session: read existing: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("register receive session: commit: %w", err)
	}
	return nil
}

func (d *DB) VerifyReceiveSession(repoID, gatePath, sessionID, capability string) (bool, error) {
	var count int
	err := d.sql.QueryRow(`SELECT COUNT(*) FROM receive_sessions WHERE repo_id = ? AND gate_path = ? AND id = ? AND capability_hash = ? AND state = 'active'`, strings.TrimSpace(repoID), strings.TrimSpace(gatePath), strings.TrimSpace(sessionID), receiveCapabilityHash(capability)).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("verify receive session: %w", err)
	}
	return count == 1, nil
}

func (d *DB) RetireReceiveSession(sessionID string) error {
	sessionID = strings.TrimSpace(sessionID)
	tx, err := d.sql.Begin()
	if err != nil {
		return fmt.Errorf("retire receive session: begin: %w", err)
	}
	defer tx.Rollback()
	var state string
	err = tx.QueryRow(`SELECT state FROM receive_sessions WHERE id = ?`, sessionID).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) || state == "retired" {
		return nil
	}
	if err != nil {
		return fmt.Errorf("retire receive session: read current: %w", err)
	}
	if state != "active" {
		return fmt.Errorf("retire receive session: ownership changed")
	}
	var pending int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM receive_reservations WHERE receive_session_id = ? AND state IN (?, ?, ?)`, sessionID, ReceiveReservationReserved, ReceiveReservationPrepared, ReceiveReservationCommitted).Scan(&pending); err != nil {
		return fmt.Errorf("retire receive session: check pending reservations: %w", err)
	}
	if pending != 0 {
		return fmt.Errorf("%w: %d reservation(s) are not terminal", ErrReceiveSessionPending, pending)
	}
	result, err := tx.Exec(`UPDATE receive_sessions SET state = 'retired', phase = ?, updated_at = ? WHERE id = ? AND state = 'active'`, receiveSessionPhaseRetired, now(), sessionID)
	if err != nil {
		return fmt.Errorf("retire receive session: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("retire receive session: affected rows: %w", err)
	}
	if affected != 1 {
		return fmt.Errorf("retire receive session: ownership changed")
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("retire receive session: commit: %w", err)
	}
	return nil
}

func (d *DB) AbortReceiveSession(repoID, gatePath, sessionID, capability string) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return fmt.Errorf("abort receive session: begin: %w", err)
	}
	defer tx.Rollback()
	phase, err := receiveSessionPhaseTxWithRepo(tx, repoID, gatePath, sessionID, capability)
	if err != nil {
		return fmt.Errorf("abort receive session: %w", err)
	}
	if phase == receiveSessionPhaseAborted {
		return tx.Commit()
	}
	if phase != receiveSessionPhaseAdmitted {
		return fmt.Errorf("abort receive session: receive session is not admitting abort")
	}
	var committed int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM receive_reservations WHERE receive_session_id = ? AND state = ?`, strings.TrimSpace(sessionID), ReceiveReservationCommitted).Scan(&committed); err != nil {
		return fmt.Errorf("abort receive session: check committed reservations: %w", err)
	}
	if committed != 0 {
		return fmt.Errorf("abort receive session: committed evidence already exists")
	}
	if _, err := tx.Exec(`UPDATE receive_reservations SET state = ?, updated_at = ? WHERE receive_session_id = ? AND state IN (?, ?)`, ReceiveReservationRetired, now(), strings.TrimSpace(sessionID), ReceiveReservationReserved, ReceiveReservationPrepared); err != nil {
		return fmt.Errorf("abort receive session: retire reservations: %w", err)
	}
	result, err := tx.Exec(`UPDATE receive_sessions SET phase = ?, updated_at = ? WHERE id = ? AND state = 'active' AND phase = ?`, receiveSessionPhaseAborted, now(), strings.TrimSpace(sessionID), receiveSessionPhaseAdmitted)
	if err != nil {
		return fmt.Errorf("abort receive session: seal abort: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		return fmt.Errorf("abort receive session: ownership changed")
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("abort receive session: commit: %w", err)
	}
	return nil
}

func (d *DB) reserveReceive(repoID, gatePath, branch, ref, oldSHA, newSHA, sessionID, capability string, skipSteps []types.StepName, intent string, requireAuthenticatedSession bool) (*ReceiveReservation, error) {
	if strings.TrimSpace(repoID) == "" || strings.TrimSpace(gatePath) == "" || strings.TrimSpace(branch) == "" {
		return nil, fmt.Errorf("reserve receive: repository, gate path, and branch are required")
	}
	if err := validateReceiveTransition(branch, ref, oldSHA, newSHA); err != nil {
		return nil, fmt.Errorf("reserve receive: %w", err)
	}
	sessionID = strings.TrimSpace(sessionID)
	capability = strings.TrimSpace(capability)
	if (sessionID == "") != (capability == "") {
		return nil, fmt.Errorf("reserve receive: receive session and capability are required together")
	}
	capabilityHash := ""
	if capability != "" {
		capabilityHash = receiveCapabilityHash(capability)
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
	if requireAuthenticatedSession {
		var count int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM receive_sessions WHERE repo_id = ? AND gate_path = ? AND id = ? AND capability_hash = ? AND state = 'active'`, repoID, gatePath, sessionID, capabilityHash).Scan(&count); err != nil {
			return nil, fmt.Errorf("reserve receive: verify session: %w", err)
		}
		if count != 1 {
			return nil, fmt.Errorf("reserve receive: receive session capability is not authenticated for this gate")
		}
		var boundBranch, boundRef, boundOld, boundNew string
		err := tx.QueryRow(`SELECT branch, ref, old_sha, new_sha FROM receive_reservations WHERE repo_id = ? AND receive_session_id = ? AND state IN (?, ?, ?, ?) ORDER BY created_at, id LIMIT 1`, repoID, sessionID, ReceiveReservationReserved, ReceiveReservationPrepared, ReceiveReservationCommitted, ReceiveReservationPublished).Scan(&boundBranch, &boundRef, &boundOld, &boundNew)
		if err == nil && (boundBranch != branch || boundRef != ref || boundOld != oldSHA || boundNew != newSHA) {
			return nil, fmt.Errorf("reserve receive: receive session is already bound to another transition")
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("reserve receive: check session transition: %w", err)
		}
	}

	existingQuery := receiveReservationSelect + ` WHERE repo_id = ? AND branch = ? AND ref = ? AND old_sha = ? AND new_sha = ? AND state IN (?, ?, ?) ORDER BY created_at, id LIMIT 1`
	existingArgs := []any{repoID, branch, ref, oldSHA, newSHA, ReceiveReservationReserved, ReceiveReservationPrepared, ReceiveReservationCommitted}
	if sessionID != "" {
		existingQuery = receiveReservationSelect + ` WHERE repo_id = ? AND receive_session_id = ? AND ref = ? AND old_sha = ? AND new_sha = ? ORDER BY created_at DESC, id DESC LIMIT 1`
		existingArgs = []any{repoID, sessionID, ref, oldSHA, newSHA}
	}
	if existing, err := scanReceiveReservation(tx.QueryRow(existingQuery, existingArgs...)); err == nil {
		if sessionID != "" && existing.CapabilityHash != capabilityHash {
			return nil, fmt.Errorf("reserve receive: capability does not match existing session")
		}
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
	var sessionArg, capabilityArg any
	if sessionID != "" {
		sessionArg, capabilityArg = sessionID, capabilityHash
	}
	if _, err := tx.Exec(`INSERT INTO receive_reservations (id, repo_id, gate_path, branch, ref, old_sha, new_sha, receive_session_id, receive_capability_hash, skip_steps, intent, state, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, id, repoID, gatePath, branch, ref, oldSHA, newSHA, sessionArg, capabilityArg, string(encodedSteps), intent, ReceiveReservationReserved, ts, ts); err != nil {
		return nil, fmt.Errorf("reserve receive: insert: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("reserve receive: commit: %w", err)
	}
	return &ReceiveReservation{ID: id, RepoID: repoID, GatePath: gatePath, Branch: branch, Ref: ref, OldSHA: oldSHA, NewSHA: newSHA, SessionID: sessionID, CapabilityHash: capabilityHash, SkipSteps: skipSteps, Intent: intent, State: ReceiveReservationReserved, CreatedAt: ts, UpdatedAt: ts}, nil
}

func (d *DB) GetPendingReceiveReservation(repoID, branch, ref, oldSHA, newSHA string) (*ReceiveReservation, error) {
	reservation, err := scanReceiveReservation(d.sql.QueryRow(receiveReservationSelect+` WHERE repo_id = ? AND branch = ? AND ref = ? AND old_sha = ? AND new_sha = ? AND state IN (?, ?, ?) ORDER BY created_at, id LIMIT 1`, repoID, branch, ref, oldSHA, newSHA, ReceiveReservationReserved, ReceiveReservationPrepared, ReceiveReservationCommitted))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get pending receive reservation: %w", err)
	}
	return reservation, nil
}

func (d *DB) GetPendingReceiveReservationForSession(repoID, branch, ref, oldSHA, newSHA, sessionID, capability string) (*ReceiveReservation, error) {
	reservation, err := scanReceiveReservation(d.sql.QueryRow(receiveReservationSelect+` WHERE repo_id = ? AND branch = ? AND ref = ? AND old_sha = ? AND new_sha = ? AND receive_session_id = ? AND receive_capability_hash = ? AND state IN (?, ?, ?) ORDER BY created_at, id LIMIT 1`, repoID, branch, ref, oldSHA, newSHA, strings.TrimSpace(sessionID), receiveCapabilityHash(capability), ReceiveReservationReserved, ReceiveReservationPrepared, ReceiveReservationCommitted))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get pending receive reservation for session: %w", err)
	}
	return reservation, nil
}

func (d *DB) GetLatestReceiveReservation(repoID, branch, ref, oldSHA, newSHA string) (*ReceiveReservation, error) {
	reservation, err := scanReceiveReservation(d.sql.QueryRow(receiveReservationSelect+` WHERE repo_id = ? AND branch = ? AND ref = ? AND old_sha = ? AND new_sha = ? ORDER BY created_at DESC, id DESC LIMIT 1`, repoID, branch, ref, oldSHA, newSHA))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get latest receive reservation: %w", err)
	}
	return reservation, nil
}

func (d *DB) GetLatestReceiveReservationForSession(repoID, branch, ref, oldSHA, newSHA, sessionID, capability string) (*ReceiveReservation, error) {
	reservation, err := scanReceiveReservation(d.sql.QueryRow(receiveReservationSelect+` WHERE repo_id = ? AND branch = ? AND ref = ? AND old_sha = ? AND new_sha = ? AND receive_session_id = ? AND receive_capability_hash = ? ORDER BY created_at DESC, id DESC LIMIT 1`, repoID, branch, ref, oldSHA, newSHA, strings.TrimSpace(sessionID), receiveCapabilityHash(capability)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get latest receive reservation for session: %w", err)
	}
	return reservation, nil
}

func (d *DB) GetReceiveReservation(id string) (*ReceiveReservation, error) {
	reservation, err := scanReceiveReservation(d.sql.QueryRow(receiveReservationSelect+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get receive reservation: %w", err)
	}
	return reservation, nil
}

func (d *DB) GetPendingReceiveReservations() ([]*ReceiveReservation, error) {
	rows, err := d.sql.Query(receiveReservationSelect+` WHERE state IN (?, ?, ?) ORDER BY created_at, id`, ReceiveReservationReserved, ReceiveReservationPrepared, ReceiveReservationCommitted)
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
	rows, err := d.sql.Query(receiveReservationSelect+` WHERE repo_id = ? AND branch = ? AND state IN (?, ?, ?) ORDER BY created_at, id`, repoID, branch, ReceiveReservationReserved, ReceiveReservationPrepared, ReceiveReservationCommitted)
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

func (d *DB) GetActiveReceiveSessions() ([]ReceiveSession, error) {
	rows, err := d.sql.Query(`SELECT id, repo_id, gate_path, phase FROM receive_sessions WHERE state = 'active' ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("get active receive sessions: %w", err)
	}
	defer rows.Close()
	var sessions []ReceiveSession
	for rows.Next() {
		var session ReceiveSession
		if err := rows.Scan(&session.ID, &session.RepoID, &session.GatePath, &session.Phase); err != nil {
			return nil, fmt.Errorf("scan active receive session: %w", err)
		}
		sessions = append(sessions, session)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate active receive sessions: %w", err)
	}
	return sessions, nil
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

func (d *DB) CompleteReceiveReservationForSession(id, runID, sessionID, capability string) error {
	if strings.TrimSpace(id) == "" || strings.TrimSpace(sessionID) == "" || strings.TrimSpace(capability) == "" {
		return fmt.Errorf("complete receive reservation: authenticated identity is required")
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return fmt.Errorf("complete receive reservation: begin: %w", err)
	}
	defer tx.Rollback()
	if _, err := receiveSessionPhaseTx(tx, sessionID, capability); err != nil {
		return fmt.Errorf("complete receive reservation: %w", err)
	}
	result, err := tx.Exec(`UPDATE receive_reservations SET state = ?, run_id = ?, updated_at = ? WHERE id = ? AND receive_session_id = ? AND receive_capability_hash = ? AND state IN (?, ?)`, ReceiveReservationPublished, nullableString(strings.TrimSpace(runID)), now(), id, strings.TrimSpace(sessionID), receiveCapabilityHash(capability), ReceiveReservationReserved, ReceiveReservationCommitted)
	if err != nil {
		return fmt.Errorf("complete receive reservation: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil {
		return fmt.Errorf("complete receive reservation: affected rows: %w", err)
	} else if affected == 1 {
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("complete receive reservation: commit: %w", err)
		}
		return nil
	}
	current, err := scanReceiveReservation(tx.QueryRow(receiveReservationSelect+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("complete receive reservation: ownership changed")
	}
	if err != nil {
		return fmt.Errorf("complete receive reservation: read current: %w", err)
	}
	if current != nil && current.MatchesSession(sessionID, capability) && current.State == ReceiveReservationPublished {
		if current.RunID == nil {
			if strings.TrimSpace(runID) == "" {
				if err := tx.Commit(); err != nil {
					return fmt.Errorf("complete receive reservation: commit: %w", err)
				}
				return nil
			}
			return fmt.Errorf("complete receive reservation: ownership changed")
		}
		if *current.RunID == strings.TrimSpace(runID) {
			if err := tx.Commit(); err != nil {
				return fmt.Errorf("complete receive reservation: commit: %w", err)
			}
			return nil
		}
		return fmt.Errorf("complete receive reservation: ownership changed")
	}
	return fmt.Errorf("complete receive reservation: ownership changed")
}

func (d *DB) ApplyReceiveTransactionBatch(phase, sessionID, capability string, inputs []ReceiveTransactionInput) error {
	if phase != "prepared" && phase != "committed" && phase != "aborted" {
		return fmt.Errorf("receive transaction: unsupported phase %q", phase)
	}
	if strings.TrimSpace(sessionID) == "" || strings.TrimSpace(capability) == "" || len(inputs) == 0 {
		return fmt.Errorf("receive transaction: authenticated session and transitions are required")
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return fmt.Errorf("receive transaction: begin: %w", err)
	}
	defer tx.Rollback()
	sessionPhase, err := receiveSessionPhaseTx(tx, sessionID, capability)
	if err != nil {
		return fmt.Errorf("receive transaction: %w", err)
	}
	if phase != "aborted" && sessionPhase != receiveSessionPhaseAdmitted {
		return fmt.Errorf("receive transaction: receive session is not admitted")
	}
	if phase == "aborted" && sessionPhase != receiveSessionPhaseAdmitted && sessionPhase != receiveSessionPhaseAborted {
		return fmt.Errorf("receive transaction: receive session is not admitting abort")
	}
	rows, err := tx.Query(`SELECT id FROM receive_reservations WHERE receive_session_id = ?`, strings.TrimSpace(sessionID))
	if err != nil {
		return fmt.Errorf("receive transaction: read sealed batch: %w", err)
	}
	expectedIDs := make(map[string]struct{})
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fmt.Errorf("receive transaction: read sealed reservation: %w", err)
		}
		expectedIDs[id] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("receive transaction: read sealed batch: %w", err)
	}
	rows.Close()
	if len(expectedIDs) != len(inputs) {
		return fmt.Errorf("receive transaction: full sealed batch is required")
	}
	seen := make(map[string]struct{}, len(inputs))
	for _, input := range inputs {
		if strings.TrimSpace(input.ID) == "" {
			return fmt.Errorf("receive transaction: reservation identity is required")
		}
		if err := validateReceiveTransition(input.Branch, input.Ref, input.OldSHA, input.NewSHA); err != nil {
			return fmt.Errorf("receive transaction: %w", err)
		}
		if _, ok := seen[input.ID]; ok {
			return fmt.Errorf("receive transaction: duplicate reservation %s", input.ID)
		}
		if _, ok := expectedIDs[input.ID]; !ok {
			return fmt.Errorf("receive transaction: reservation %s is outside the sealed batch", input.ID)
		}
		seen[input.ID] = struct{}{}
		var repoID, branch, ref, oldSHA, newSHA, storedSession, storedHash, state string
		err := tx.QueryRow(`SELECT repo_id, branch, ref, old_sha, new_sha, receive_session_id, receive_capability_hash, state FROM receive_reservations WHERE id = ?`, input.ID).Scan(&repoID, &branch, &ref, &oldSHA, &newSHA, &storedSession, &storedHash, &state)
		if err != nil {
			return fmt.Errorf("receive transaction: read reservation %s: %w", input.ID, err)
		}
		if repoID != input.RepoID || branch != input.Branch || ref != input.Ref || oldSHA != input.OldSHA || newSHA != input.NewSHA || storedSession != strings.TrimSpace(sessionID) || storedHash != receiveCapabilityHash(capability) {
			return fmt.Errorf("receive transaction: reservation %s does not match the exact receive", input.ID)
		}
		var from []string
		var to string
		switch phase {
		case "prepared":
			from, to = []string{ReceiveReservationReserved}, ReceiveReservationPrepared
			if state == ReceiveReservationPrepared {
				continue
			}
		case "committed":
			from, to = []string{ReceiveReservationPrepared}, ReceiveReservationCommitted
			if state == ReceiveReservationCommitted {
				continue
			}
		case "aborted":
			from, to = []string{ReceiveReservationReserved, ReceiveReservationPrepared}, ReceiveReservationRetired
			if state == ReceiveReservationRetired {
				continue
			}
		}
		args := make([]any, 0, 5+len(from))
		args = append(args, to, now(), input.ID, strings.TrimSpace(sessionID), receiveCapabilityHash(capability))
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(from)), ",")
		args = append(args, anySlice(from)...)
		result, err := tx.Exec(`UPDATE receive_reservations SET state = ?, updated_at = ? WHERE id = ? AND receive_session_id = ? AND receive_capability_hash = ? AND state IN (`+placeholders+`)`, args...)
		if err != nil {
			return fmt.Errorf("receive transaction: update reservation %s: %w", input.ID, err)
		}
		affected, err := result.RowsAffected()
		if err != nil || affected != 1 {
			return fmt.Errorf("receive transaction: reservation %s is not in expected %s phase", input.ID, phase)
		}
		if phase == "committed" && strings.HasPrefix(input.Ref, "refs/heads/") {
			var managedHead string
			managedErr := tx.QueryRow(`SELECT head FROM managed_gate_refs WHERE repo_id = ? AND gate_path = (SELECT gate_path FROM receive_reservations WHERE id = ?) AND ref = ?`, input.RepoID, input.ID, input.Ref).Scan(&managedHead)
			switch {
			case managedErr == sql.ErrNoRows:
				if _, err := tx.Exec(`INSERT INTO managed_gate_refs (repo_id, gate_path, ref, head, updated_at) SELECT repo_id, gate_path, ref, ?, ? FROM receive_reservations WHERE id = ?`, NormalizeManagedGateHead(input.NewSHA), now(), input.ID); err != nil {
					return fmt.Errorf("receive transaction: record managed gate ref: %w", err)
				}
			case managedErr != nil:
				return fmt.Errorf("receive transaction: read managed gate ref: %w", managedErr)
			case NormalizeManagedGateHead(managedHead) != NormalizeManagedGateHead(input.OldSHA):
				return fmt.Errorf("receive transaction: managed gate journal changed from %s to %s", input.OldSHA, managedHead)
			default:
				if _, err := tx.Exec(`UPDATE managed_gate_refs SET head = ?, updated_at = ? WHERE repo_id = ? AND gate_path = (SELECT gate_path FROM receive_reservations WHERE id = ?) AND ref = ? AND head = ?`, NormalizeManagedGateHead(input.NewSHA), now(), input.RepoID, input.ID, input.Ref, NormalizeManagedGateHead(input.OldSHA)); err != nil {
					return fmt.Errorf("receive transaction: advance managed gate ref: %w", err)
				}
			}
		}
	}
	if phase == "aborted" && sessionPhase == receiveSessionPhaseAdmitted {
		result, err := tx.Exec(`UPDATE receive_sessions SET phase = ?, updated_at = ? WHERE id = ? AND state = 'active' AND phase = ?`, receiveSessionPhaseAborted, now(), strings.TrimSpace(sessionID), receiveSessionPhaseAdmitted)
		if err != nil {
			return fmt.Errorf("receive transaction: abort session: %w", err)
		}
		affected, err := result.RowsAffected()
		if err != nil || affected != 1 {
			return fmt.Errorf("receive transaction: abort session ownership changed")
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("receive transaction: commit: %w", err)
	}
	return nil
}

func anySlice(values []string) []any {
	args := make([]any, len(values))
	for i, value := range values {
		args[i] = value
	}
	return args
}

func verifyReceiveSessionTx(tx *sql.Tx, repoID, gatePath, sessionID, capability string) error {
	_, err := receiveSessionPhaseTxWithRepo(tx, repoID, gatePath, sessionID, capability)
	return err
}

func receiveSessionPhaseTx(tx *sql.Tx, sessionID, capability string) (string, error) {
	return receiveSessionPhaseTxWithRepo(tx, "", "", sessionID, capability)
}

func receiveSessionPhaseTxWithRepo(tx *sql.Tx, repoID, gatePath, sessionID, capability string) (string, error) {
	query := `SELECT phase FROM receive_sessions WHERE id = ? AND capability_hash = ? AND state = 'active'`
	args := []any{strings.TrimSpace(sessionID), receiveCapabilityHash(capability)}
	if strings.TrimSpace(repoID) != "" {
		query = `SELECT phase FROM receive_sessions WHERE repo_id = ? AND gate_path = ? AND id = ? AND capability_hash = ? AND state = 'active'`
		args = []any{strings.TrimSpace(repoID), strings.TrimSpace(gatePath), strings.TrimSpace(sessionID), receiveCapabilityHash(capability)}
	}
	var phase string
	if err := tx.QueryRow(query, args...).Scan(&phase); errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("receive session is not active")
	} else if err != nil {
		return "", fmt.Errorf("verify session: %w", err)
	}
	return phase, nil
}

func (d *DB) MarkReceivePrepared(repoID, branch, ref, oldSHA, newSHA string) error {
	return d.markReceivePrepared("", repoID, branch, ref, oldSHA, newSHA)
}

func (d *DB) MarkReceivePreparedForID(id, repoID, branch, ref, oldSHA, newSHA string) error {
	return d.markReceivePrepared(id, repoID, branch, ref, oldSHA, newSHA)
}

func (d *DB) MarkReceivePreparedForSession(id, repoID, branch, ref, oldSHA, newSHA, sessionID, capability string) error {
	if err := validateReceiveTransition(branch, ref, oldSHA, newSHA); err != nil {
		return fmt.Errorf("mark receive prepared: %w", err)
	}
	err := d.ApplyReceiveTransactionBatch("prepared", sessionID, capability, []ReceiveTransactionInput{{ID: id, RepoID: repoID, Branch: branch, Ref: ref, OldSHA: oldSHA, NewSHA: newSHA}})
	if err != nil {
		return fmt.Errorf("mark receive prepared: %w", err)
	}
	return nil
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

func (d *DB) MarkReceiveCommittedForSession(id, repoID, branch, ref, oldSHA, newSHA, sessionID, capability string) error {
	if err := validateReceiveTransition(branch, ref, oldSHA, newSHA); err != nil {
		return fmt.Errorf("mark receive committed: %w", err)
	}
	err := d.ApplyReceiveTransactionBatch("committed", sessionID, capability, []ReceiveTransactionInput{{ID: id, RepoID: repoID, Branch: branch, Ref: ref, OldSHA: oldSHA, NewSHA: newSHA}})
	if err != nil {
		return fmt.Errorf("mark receive committed: %w", err)
	}
	return nil
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

func (d *DB) MarkReceiveAbortedForSession(id, repoID, branch, ref, oldSHA, newSHA, sessionID, capability string) error {
	if err := validateReceiveTransition(branch, ref, oldSHA, newSHA); err != nil {
		return fmt.Errorf("mark receive aborted: %w", err)
	}
	err := d.ApplyReceiveTransactionBatch("aborted", sessionID, capability, []ReceiveTransactionInput{{ID: id, RepoID: repoID, Branch: branch, Ref: ref, OldSHA: oldSHA, NewSHA: newSHA}})
	if err != nil {
		return fmt.Errorf("mark receive aborted: %w", err)
	}
	return nil
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
	var sessionID, capabilityHash, encodedSteps, intent, runID sql.NullString
	if err := row.Scan(&reservation.ID, &reservation.RepoID, &reservation.GatePath, &reservation.Branch, &reservation.Ref, &reservation.OldSHA, &reservation.NewSHA, &sessionID, &capabilityHash, &encodedSteps, &intent, &reservation.State, &runID, &reservation.CreatedAt, &reservation.UpdatedAt); err != nil {
		return nil, err
	}
	if sessionID.Valid {
		reservation.SessionID = sessionID.String
	}
	if capabilityHash.Valid {
		reservation.CapabilityHash = capabilityHash.String
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

func receiveCapabilityHash(capability string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(capability)))
	return fmt.Sprintf("%x", sum[:])
}

func receiveBatchHash(inputs []ReceiveReservationInput) string {
	type batchEntry struct {
		RepoID   string   `json:"repo_id"`
		GatePath string   `json:"gate_path"`
		Branch   string   `json:"branch"`
		Ref      string   `json:"ref"`
		OldSHA   string   `json:"old_sha"`
		NewSHA   string   `json:"new_sha"`
		Skip     []string `json:"skip_steps"`
		Intent   string   `json:"intent"`
	}
	entries := make([]batchEntry, len(inputs))
	for i, input := range inputs {
		steps := make([]string, len(input.SkipSteps))
		for j, step := range input.SkipSteps {
			steps[j] = string(step)
		}
		sort.Strings(steps)
		entries[i] = batchEntry{
			RepoID: input.RepoID, GatePath: input.GatePath, Branch: input.Branch,
			Ref: input.Ref, OldSHA: input.OldSHA, NewSHA: input.NewSHA,
			Skip: steps, Intent: input.Intent,
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		left, _ := json.Marshal(entries[i])
		right, _ := json.Marshal(entries[j])
		return string(left) < string(right)
	})
	encoded, _ := json.Marshal(entries)
	sum := sha256.Sum256(encoded)
	return fmt.Sprintf("%x", sum[:])
}

func (r *ReceiveReservation) MatchesSession(sessionID, capability string) bool {
	return r != nil && r.SessionID == strings.TrimSpace(sessionID) && r.CapabilityHash != "" && r.CapabilityHash == receiveCapabilityHash(capability)
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

package db

import (
	"bytes"
	"crypto/ed25519"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ownerdecision"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type OwnerDecisionAuthority struct {
	RunID          string
	PublicKey      ed25519.PublicKey
	KeyID          string
	RepoID         string
	Branch         string
	InitialHeadSHA string
	GenesisHead    string
	CreatedAt      int64
}

// OwnerDecisionProjection is the mutable round state materialized by one
// signed decision. It is included in the hash-chained journal record.
type OwnerDecisionProjection struct {
	RoundID            string  `json:"round_id"`
	SelectedFindingIDs string  `json:"selected_finding_ids"`
	SelectionSource    string  `json:"selection_source"`
	UserFindingsJSON   *string `json:"user_findings_json,omitempty"`
}

type OwnerDecisionAppendResult struct {
	Sequence int
	Head     string
	Replay   bool
}

// ProtectedCrashRecovery reports protected active rows closed before ordinary
// stale-run recovery.
type ProtectedCrashRecovery struct {
	PendingFailed      int
	CancellationRunIDs []string
}

type ownerDecisionJournalRecord struct {
	Envelope   ownerdecision.Envelope   `json:"envelope"`
	Projection *OwnerDecisionProjection `json:"projection,omitempty"`
}

type ownerDecisionEvent struct {
	Sequence     int
	GateID       string
	PreviousHead string
	RecordDigest string
	HistoryHead  string
	EnvelopeJSON string
	Projection   *OwnerDecisionProjection
}

// ProtectRunOwnerDecisions binds a run to exactly one Ed25519 public key and
// its immutable submitted repository, branch, and initial head identity.
// Repeating the same binding is idempotent; replacing it is forbidden.
func (d *DB) ProtectRunOwnerDecisions(runID string, publicKey ed25519.PublicKey) (*OwnerDecisionAuthority, error) {
	if len(publicKey) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("protect owner decisions: invalid Ed25519 public key length %d", len(publicKey))
	}
	keyID, err := ownerdecision.KeyID(publicKey)
	if err != nil {
		return nil, err
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin owner decision protection: %w", err)
	}
	defer tx.Rollback()

	var repoID, branch, currentHead string
	var submittedHead sql.NullString
	if err := tx.QueryRow(`SELECT repo_id, branch, head_sha, submitted_head_sha FROM runs WHERE id = ?`, runID).Scan(&repoID, &branch, &currentHead, &submittedHead); err != nil {
		if err == sql.ErrNoRows {
			return nil, errors.New("protect owner decisions: run not found")
		}
		return nil, fmt.Errorf("find protected run: %w", err)
	}
	if !submittedHead.Valid || submittedHead.String == "" || currentHead != submittedHead.String {
		return nil, errors.New("protect owner decisions: run is not at its immutable submitted head")
	}
	genesisHead, err := ownerdecision.GenesisHeadForRun(publicKey, repoID, branch, submittedHead.String)
	if err != nil {
		return nil, err
	}
	var stored []byte
	var storedKeyID string
	var storedRepoID, storedBranch, storedInitialHead string
	var storedGenesis string
	var createdAt int64
	err = tx.QueryRow(
		`SELECT public_key, key_id, repo_id, branch, initial_head_sha, genesis_head, created_at FROM owner_decision_authorities WHERE run_id = ?`,
		runID,
	).Scan(&stored, &storedKeyID, &storedRepoID, &storedBranch, &storedInitialHead, &storedGenesis, &createdAt)
	if err == nil {
		if !bytes.Equal(stored, publicKey) || storedKeyID != keyID || storedRepoID != repoID || storedBranch != branch ||
			storedInitialHead != submittedHead.String || storedGenesis != genesisHead {
			return nil, errors.New("protect owner decisions: run is already bound to a different authority")
		}
		return &OwnerDecisionAuthority{RunID: runID, PublicKey: slices.Clone(publicKey), KeyID: keyID, RepoID: repoID, Branch: branch, InitialHeadSHA: submittedHead.String, GenesisHead: genesisHead, CreatedAt: createdAt}, nil
	}
	if err != sql.ErrNoRows {
		return nil, fmt.Errorf("read owner decision authority: %w", err)
	}
	createdAt = now()
	if _, err := tx.Exec(
		`INSERT INTO owner_decision_authorities (run_id, public_key, key_id, repo_id, branch, initial_head_sha, genesis_head, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		runID, []byte(publicKey), keyID, repoID, branch, submittedHead.String, genesisHead, createdAt,
	); err != nil {
		return nil, fmt.Errorf("insert owner decision authority: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit owner decision authority: %w", err)
	}
	return &OwnerDecisionAuthority{RunID: runID, PublicKey: slices.Clone(publicKey), KeyID: keyID, RepoID: repoID, Branch: branch, InitialHeadSHA: submittedHead.String, GenesisHead: genesisHead, CreatedAt: createdAt}, nil
}

func (d *DB) GetOwnerDecisionAuthority(runID string) (*OwnerDecisionAuthority, error) {
	authority := &OwnerDecisionAuthority{RunID: runID}
	var publicKey []byte
	err := d.sql.QueryRow(
		`SELECT public_key, key_id, repo_id, branch, initial_head_sha, genesis_head, created_at FROM owner_decision_authorities WHERE run_id = ?`,
		runID,
	).Scan(&publicKey, &authority.KeyID, &authority.RepoID, &authority.Branch, &authority.InitialHeadSHA, &authority.GenesisHead, &authority.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get owner decision authority: %w", err)
	}
	if len(publicKey) != ed25519.PublicKeySize {
		return nil, errors.New("get owner decision authority: stored public key is invalid")
	}
	authority.PublicKey = ed25519.PublicKey(slices.Clone(publicKey))
	derivedKeyID, err := ownerdecision.KeyID(authority.PublicKey)
	if err != nil || derivedKeyID != authority.KeyID {
		return nil, errors.New("get owner decision authority: stored key id is invalid")
	}
	if derived, err := ownerdecision.GenesisHeadForRun(authority.PublicKey, authority.RepoID, authority.Branch, authority.InitialHeadSHA); err != nil || derived != authority.GenesisHead {
		return nil, errors.New("get owner decision authority: stored key binding is invalid")
	}
	return authority, nil
}

// OwnerDecisionHead returns the recomputed current journal head. An
// unprotected legacy run returns ("", false, nil).
func (d *DB) OwnerDecisionHead(runID string) (string, bool, error) {
	authority, err := d.GetOwnerDecisionAuthority(runID)
	if err != nil || authority == nil {
		return "", false, err
	}
	head, err := d.verifyOwnerDecisionHistory(runID, authority, "", d.sql)
	return head, true, err
}

// AppendOwnerDecision verifies and appends a signed journal record and
// materializes its round projection in one transaction. The transaction
// commits before the executor is allowed to release its approval wait.
func (d *DB) AppendOwnerDecision(runID, gateID string, envelope ownerdecision.Envelope, expected ownerdecision.Challenge, projection *OwnerDecisionProjection, admittedAt time.Time) (OwnerDecisionAppendResult, error) {
	tx, err := d.sql.Begin()
	if err != nil {
		return OwnerDecisionAppendResult{}, fmt.Errorf("begin owner decision append: %w", err)
	}
	defer tx.Rollback()

	authority, err := getOwnerDecisionAuthorityTx(tx, runID)
	if err != nil {
		return OwnerDecisionAppendResult{}, err
	}
	if authority == nil {
		return OwnerDecisionAppendResult{}, errors.New("append owner decision: run is not protected")
	}
	if err := ownerdecision.Verify(authority.PublicKey, envelope, expected, admittedAt); err != nil {
		return OwnerDecisionAppendResult{}, fmt.Errorf("append owner decision: %w", err)
	}
	if err := validateOwnerChallengeTx(tx, authority, runID, gateID, expected); err != nil {
		return OwnerDecisionAppendResult{}, err
	}
	if err := validateOwnerProjectionQuery(tx, runID, envelope, projection); err != nil {
		return OwnerDecisionAppendResult{}, err
	}

	recordDigest, envelopeJSON, err := encodeOwnerDecisionRecord(envelope, projection)
	if err != nil {
		return OwnerDecisionAppendResult{}, err
	}
	if existing, err := getOwnerDecisionEventTx(tx, runID, gateID); err != nil {
		return OwnerDecisionAppendResult{}, err
	} else if existing != nil {
		if existing.RecordDigest != recordDigest || existing.EnvelopeJSON != envelopeJSON || !equalOwnerProjection(existing.Projection, projection) {
			return OwnerDecisionAppendResult{}, errors.New("append owner decision: gate already has a different decision")
		}
		if err := verifyOwnerProjectionTx(tx, existing.Projection); err != nil {
			return OwnerDecisionAppendResult{}, fmt.Errorf("append owner decision replay: %w", err)
		}
		currentHead, err := d.verifyOwnerDecisionHistory(runID, authority, "", tx)
		if err != nil {
			return OwnerDecisionAppendResult{}, fmt.Errorf("append owner decision replay: existing history: %w", err)
		}
		if existing.HistoryHead != currentHead {
			return OwnerDecisionAppendResult{}, errors.New("append owner decision replay: gate event is not the history tip")
		}
		return OwnerDecisionAppendResult{Sequence: existing.Sequence, Head: existing.HistoryHead, Replay: true}, nil
	}

	currentHead, err := d.verifyOwnerDecisionHistory(runID, authority, "", tx)
	if err != nil {
		return OwnerDecisionAppendResult{}, fmt.Errorf("append owner decision: existing history: %w", err)
	}
	if expected.PreviousHead != currentHead {
		return OwnerDecisionAppendResult{}, fmt.Errorf("append owner decision: history head is %s, challenge expects %s", currentHead, expected.PreviousHead)
	}
	sequence, err := ownerDecisionEventCountTx(tx, runID)
	if err != nil {
		return OwnerDecisionAppendResult{}, err
	}
	sequence++
	envelopeDigest, err := ownerdecision.EnvelopeDigest(envelope)
	if err != nil {
		return OwnerDecisionAppendResult{}, err
	}
	historyHead, err := ownerdecision.NextHead(currentHead, envelopeDigest)
	if err != nil {
		return OwnerDecisionAppendResult{}, err
	}
	if projection != nil {
		result, err := tx.Exec(
			`UPDATE step_rounds
			    SET selected_finding_ids = ?, selection_source = ?, user_findings_json = ?
			  WHERE id = ? AND selection_source IS NULL AND selected_finding_ids IS NULL AND user_findings_json IS NULL`,
			projection.SelectedFindingIDs, projection.SelectionSource, projection.UserFindingsJSON, projection.RoundID,
		)
		if err != nil {
			return OwnerDecisionAppendResult{}, fmt.Errorf("append owner decision projection: %w", err)
		}
		changed, err := result.RowsAffected()
		if err != nil || changed != 1 {
			return OwnerDecisionAppendResult{}, fmt.Errorf("append owner decision projection: expected one untouched round, changed %d: %w", changed, err)
		}
	}
	createdAt := admittedAt.Unix()
	var roundID, selected, source any
	var userFindings any
	if projection != nil {
		roundID = projection.RoundID
		selected = projection.SelectedFindingIDs
		source = projection.SelectionSource
		userFindings = projection.UserFindingsJSON
	}
	if _, err := tx.Exec(
		`INSERT INTO owner_decision_events
		 (run_id, sequence, gate_id, previous_head, record_digest, history_head, envelope_json,
		  projection_round_id, selected_finding_ids, selection_source, user_findings_json, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		runID, sequence, gateID, currentHead, recordDigest, historyHead, envelopeJSON,
		roundID, selected, source, userFindings, createdAt,
	); err != nil {
		return OwnerDecisionAppendResult{}, fmt.Errorf("insert owner decision event: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return OwnerDecisionAppendResult{}, fmt.Errorf("commit owner decision append: %w", err)
	}
	return OwnerDecisionAppendResult{Sequence: sequence, Head: historyHead}, nil
}

// VerifyOwnerDecisionHistory verifies signatures, the complete hash chain,
// the external expected head, and every mutable round projection.
func (d *DB) VerifyOwnerDecisionHistory(runID, expectedHead string) error {
	authority, err := d.GetOwnerDecisionAuthority(runID)
	if err != nil {
		return err
	}
	if authority == nil {
		return errors.New("verify owner decision history: run is not protected")
	}
	_, err = d.verifyOwnerDecisionHistory(runID, authority, expectedHead, d.sql)
	return err
}

// VerifyRecoveredOwnerDecisionRun verifies the locally available protected
// history and immutable run identity before restart recovery performs any
// agent or provider setup. If the parked gate already has a committed response
// event, it additionally binds the current run head to that exact signed gate;
// an old response can therefore never be replayed after a local head rewrite.
func (d *DB) VerifyRecoveredOwnerDecisionRun(runID string) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return fmt.Errorf("begin recovered owner decision verification: %w", err)
	}
	defer tx.Rollback()
	authority, err := getOwnerDecisionAuthorityTx(tx, runID)
	if err != nil {
		return err
	}
	if authority == nil {
		return errors.New("verify recovered owner decision run: run is not protected")
	}
	head, err := d.verifyOwnerDecisionHistory(runID, authority, "", tx)
	if err != nil {
		return fmt.Errorf("verify recovered owner decision run: %w", err)
	}
	rows, err := tx.Query(
		`SELECT (SELECT r.id FROM step_rounds r WHERE r.step_result_id = sr.id ORDER BY r.round DESC LIMIT 1)
		   FROM step_results sr
		  WHERE sr.run_id = ? AND sr.status IN (?, ?)
		  ORDER BY sr.step_order`,
		runID, types.StepStatusAwaitingApproval, types.StepStatusFixReview,
	)
	if err != nil {
		return fmt.Errorf("verify recovered owner decision gate: %w", err)
	}
	var gates []sql.NullString
	for rows.Next() {
		var gate sql.NullString
		if err := rows.Scan(&gate); err != nil {
			rows.Close()
			return err
		}
		gates = append(gates, gate)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if len(gates) != 1 || !gates[0].Valid {
		return fmt.Errorf("verify recovered owner decision gate: expected one parked step with a round, found %d", len(gates))
	}
	gateID := ownerdecision.PurposeRespond + ":" + gates[0].String
	event, err := getOwnerDecisionEventTx(tx, runID, gateID)
	if err != nil {
		return err
	}
	if event == nil {
		return nil
	}
	if event.HistoryHead != head {
		return errors.New("verify recovered owner decision gate: committed response is not the history tip")
	}
	var envelope ownerdecision.Envelope
	if err := json.Unmarshal([]byte(event.EnvelopeJSON), &envelope); err != nil {
		return fmt.Errorf("verify recovered owner decision gate: decode envelope: %w", err)
	}
	if err := validateOwnerChallengeTx(tx, authority, runID, gateID, envelope.Challenge); err != nil {
		return fmt.Errorf("verify recovered owner decision gate: %w", err)
	}
	return nil
}

// CommittedOwnerResponse returns the already-admitted response for one
// recovered gate when that event is the verified journal tip bound by the
// controller's external expected head. This closes the crash window after the
// append transaction commits but before the in-memory approval channel is
// released: recovery replays the durable response, never a second event.
func (d *DB) CommittedOwnerResponse(runID, gateID, expectedHead string) (ownerdecision.Envelope, bool, error) {
	tx, err := d.sql.Begin()
	if err != nil {
		return ownerdecision.Envelope{}, false, fmt.Errorf("begin committed owner response: %w", err)
	}
	defer tx.Rollback()
	authority, err := getOwnerDecisionAuthorityTx(tx, runID)
	if err != nil {
		return ownerdecision.Envelope{}, false, err
	}
	if authority == nil {
		return ownerdecision.Envelope{}, false, errors.New("committed owner response: run is not protected")
	}
	head, err := d.verifyOwnerDecisionHistory(runID, authority, expectedHead, tx)
	if err != nil {
		return ownerdecision.Envelope{}, false, fmt.Errorf("committed owner response: verify history: %w", err)
	}
	event, err := getOwnerDecisionEventTx(tx, runID, gateID)
	if err != nil {
		return ownerdecision.Envelope{}, false, err
	}
	if event == nil {
		return ownerdecision.Envelope{}, false, nil
	}
	count, err := ownerDecisionEventCountTx(tx, runID)
	if err != nil {
		return ownerdecision.Envelope{}, false, err
	}
	if event.Sequence != count || event.HistoryHead != head {
		return ownerdecision.Envelope{}, false, errors.New("committed owner response: recovered gate event is not the verified history tip")
	}
	var envelope ownerdecision.Envelope
	if err := json.Unmarshal([]byte(event.EnvelopeJSON), &envelope); err != nil {
		return ownerdecision.Envelope{}, false, fmt.Errorf("committed owner response: decode envelope: %w", err)
	}
	if envelope.Challenge.Purpose != ownerdecision.PurposeRespond || gateID != ownerGateID(envelope.Challenge) {
		return ownerdecision.Envelope{}, false, errors.New("committed owner response: event is not a response for the recovered gate")
	}
	if err := validateOwnerChallengeTx(tx, authority, runID, gateID, envelope.Challenge); err != nil {
		return ownerdecision.Envelope{}, false, fmt.Errorf("committed owner response: %w", err)
	}
	if err := validateOwnerProjection(envelope, event.Projection); err != nil {
		return ownerdecision.Envelope{}, false, err
	}
	return envelope.Clone(), true, nil
}

// CommittedOwnerCancellation reports whether the verified journal tip is a
// signed cancellation that committed before a process crash could invoke the
// in-memory cancel function.
func (d *DB) CommittedOwnerCancellation(runID, expectedHead string) (bool, error) {
	tx, err := d.sql.Begin()
	if err != nil {
		return false, fmt.Errorf("begin committed owner cancellation: %w", err)
	}
	defer tx.Rollback()
	return d.committedOwnerCancellationTx(tx, runID, expectedHead)
}

func (d *DB) committedOwnerCancellationTx(tx *sql.Tx, runID, expectedHead string) (bool, error) {
	authority, err := getOwnerDecisionAuthorityTx(tx, runID)
	if err != nil {
		return false, err
	}
	if authority == nil {
		return false, errors.New("committed owner cancellation: run is not protected")
	}
	head, err := d.verifyOwnerDecisionHistory(runID, authority, expectedHead, tx)
	if err != nil {
		return false, fmt.Errorf("committed owner cancellation: verify history: %w", err)
	}
	row := tx.QueryRow(
		`SELECT sequence, gate_id, previous_head, record_digest, history_head, envelope_json,
		        projection_round_id, selected_finding_ids, selection_source, user_findings_json
		   FROM owner_decision_events WHERE run_id = ? ORDER BY sequence DESC LIMIT 1`,
		runID,
	)
	event, err := scanOwnerDecisionEvent(row)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if event.HistoryHead != head {
		return false, errors.New("committed owner cancellation: tip does not match verified history head")
	}
	var envelope ownerdecision.Envelope
	if err := json.Unmarshal([]byte(event.EnvelopeJSON), &envelope); err != nil {
		return false, fmt.Errorf("committed owner cancellation: decode envelope: %w", err)
	}
	if envelope.Challenge.Purpose != ownerdecision.PurposeCancel {
		return false, nil
	}
	if event.GateID != ownerGateID(envelope.Challenge) {
		return false, errors.New("committed owner cancellation: gate binding is invalid")
	}
	if err := validateOwnerChallengeTx(tx, authority, runID, event.GateID, envelope.Challenge); err != nil {
		return false, fmt.Errorf("committed owner cancellation: %w", err)
	}
	return true, nil
}

// RecoverCommittedOwnerCancellation projects a reverified signed
// cancellation after its managed worktree head has been anchored by startup
// recovery. The verification and terminal transition share one transaction so
// a same-UID local rewrite cannot enter between them.
func (d *DB) RecoverCommittedOwnerCancellation(runID string) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return fmt.Errorf("begin recover committed owner cancellation: %w", err)
	}
	defer tx.Rollback()
	committed, err := d.committedOwnerCancellationTx(tx, runID, "")
	if err != nil {
		return err
	}
	if !committed {
		return errors.New("recover committed owner cancellation: cancellation is no longer the verified history tip")
	}
	ts := now()
	changed, err := tx.Exec(
		`UPDATE runs SET status = ?, error = ?, awaiting_agent_since = NULL, push_active = 0, updated_at = ?
		  WHERE id = ? AND status = ?`,
		types.RunCancelled, types.RunCancelReasonAbortedByUser, ts, runID, types.RunRunning,
	)
	if err != nil {
		return fmt.Errorf("project committed protected cancellation: %w", err)
	}
	count, err := changed.RowsAffected()
	if err != nil || count != 1 {
		return fmt.Errorf("project committed protected cancellation: expected one row, changed %d: %w", count, err)
	}
	if _, err := tx.Exec(
		`UPDATE step_results SET status = ?, error = ?, completed_at = ?, agent_pid = NULL
		  WHERE run_id = ? AND status IN (?, ?, ?, ?)`,
		types.StepStatusFailed, types.RunCancelReasonAbortedByUser, ts, runID,
		types.StepStatusRunning, types.StepStatusAwaitingApproval, types.StepStatusFixing, types.StepStatusFixReview,
	); err != nil {
		return fmt.Errorf("project committed protected cancellation steps: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit recovered owner cancellation: %w", err)
	}
	return nil
}

// FailUnboundRecoveredRun terminalizes a parked run whose protected-vs-legacy
// identity cannot be proven after restart. Because all local markers are
// writable by the workload UID, treating "authority row absent" as legacy
// would be a protected-mode downgrade. The already-validated worktree head is
// preserved as terminal custody evidence for a safe later rerun.
func (d *DB) FailUnboundRecoveredRun(runID, verifiedHead, reason string) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	ts := now()
	result, err := tx.Exec(
		`UPDATE runs
		    SET status = ?, error = ?, head_sha = ?, terminal_head_verified_at = ?,
		        awaiting_agent_since = NULL, push_active = 0, updated_at = ?
		  WHERE id = ? AND status = ? AND awaiting_agent_since IS NOT NULL`,
		types.RunFailed, reason, verifiedHead, ts, ts, runID, types.RunRunning,
	)
	if err != nil {
		return fmt.Errorf("fail unbound recovered run: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 1 {
		return fmt.Errorf("fail unbound recovered run: expected one parked run, changed %d: %w", changed, err)
	}
	if _, err := tx.Exec(
		`UPDATE step_results SET status = ?, error = ?, completed_at = ?, agent_pid = NULL
		  WHERE run_id = ? AND status IN (?, ?)`,
		types.StepStatusFailed, reason, ts, runID, types.StepStatusAwaitingApproval, types.StepStatusFixReview,
	); err != nil {
		return fmt.Errorf("fail unbound recovered step: %w", err)
	}
	return tx.Commit()
}

// ReconcileProtectedCrashStates closes protected crash seams that do not have
// an executor to resume. A protected pending row can only be the durable half
// of run creation, so it is restored to its sealed identity and failed before
// new work is admitted. Verified cancellation IDs are returned for the daemon
// to anchor their exact managed-worktree heads before a separate transactional
// terminal projection; neither phase recreates an executor or invokes a
// provider.
func (d *DB) ReconcileProtectedCrashStates() (ProtectedCrashRecovery, error) {
	tx, err := d.sql.Begin()
	if err != nil {
		return ProtectedCrashRecovery{}, fmt.Errorf("begin protected crash recovery: %w", err)
	}
	defer tx.Rollback()
	rows, err := tx.Query(
		`SELECT r.id, r.status
		   FROM runs r JOIN owner_decision_authorities a ON a.run_id = r.id
		  WHERE r.status IN (?, ?)
		  ORDER BY r.created_at, r.id`,
		types.RunPending, types.RunRunning,
	)
	if err != nil {
		return ProtectedCrashRecovery{}, fmt.Errorf("list protected crash states: %w", err)
	}
	type activeProtected struct {
		id     string
		status types.RunStatus
	}
	var runs []activeProtected
	for rows.Next() {
		var run activeProtected
		if err := rows.Scan(&run.id, &run.status); err != nil {
			rows.Close()
			return ProtectedCrashRecovery{}, err
		}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return ProtectedCrashRecovery{}, err
	}
	if err := rows.Close(); err != nil {
		return ProtectedCrashRecovery{}, err
	}

	result := ProtectedCrashRecovery{}
	for _, run := range runs {
		authority, err := getOwnerDecisionAuthorityTx(tx, run.id)
		if err != nil {
			// The generic stale-run recovery that follows will still terminalize
			// unreadable rows. Never let one corrupt authority prevent other
			// protected pending rows from being closed first.
			continue
		}
		if run.status == types.RunPending {
			reason := "daemon crashed before protected executor registration"
			ts := now()
			changed, err := tx.Exec(
				`UPDATE runs
				    SET repo_id = ?, branch = ?, head_sha = ?, submitted_head_sha = ?,
				        status = ?, error = ?, terminal_head_verified_at = ?,
				        awaiting_agent_since = NULL, push_active = 0, updated_at = ?
				  WHERE id = ? AND status = ?`,
				authority.RepoID, authority.Branch, authority.InitialHeadSHA, authority.InitialHeadSHA,
				types.RunFailed, reason, ts, ts, run.id, types.RunPending,
			)
			if err != nil {
				return ProtectedCrashRecovery{}, fmt.Errorf("fail incomplete protected run: %w", err)
			}
			count, err := changed.RowsAffected()
			if err != nil || count != 1 {
				return ProtectedCrashRecovery{}, fmt.Errorf("fail incomplete protected run: expected one row, changed %d: %w", count, err)
			}
			if _, err := tx.Exec(
				`UPDATE step_results SET status = ?, error = ?, completed_at = ?, agent_pid = NULL
				  WHERE run_id = ? AND status IN (?, ?, ?, ?)`,
				types.StepStatusFailed, reason, ts, run.id,
				types.StepStatusRunning, types.StepStatusAwaitingApproval, types.StepStatusFixing, types.StepStatusFixReview,
			); err != nil {
				return ProtectedCrashRecovery{}, fmt.Errorf("fail incomplete protected steps: %w", err)
			}
			result.PendingFailed++
			continue
		}

		head, err := d.verifyOwnerDecisionHistory(run.id, authority, "", tx)
		if err != nil {
			continue
		}
		row := tx.QueryRow(
			`SELECT sequence, gate_id, previous_head, record_digest, history_head, envelope_json,
			        projection_round_id, selected_finding_ids, selection_source, user_findings_json
			   FROM owner_decision_events WHERE run_id = ? ORDER BY sequence DESC LIMIT 1`,
			run.id,
		)
		event, err := scanOwnerDecisionEvent(row)
		if err == sql.ErrNoRows {
			continue
		}
		if err != nil {
			return ProtectedCrashRecovery{}, err
		}
		var envelope ownerdecision.Envelope
		if event.HistoryHead != head || json.Unmarshal([]byte(event.EnvelopeJSON), &envelope) != nil || envelope.Challenge.Purpose != ownerdecision.PurposeCancel {
			continue
		}
		if err := validateOwnerChallengeTx(tx, authority, run.id, event.GateID, envelope.Challenge); err != nil {
			continue
		}
		result.CancellationRunIDs = append(result.CancellationRunIDs, run.id)
	}
	if err := tx.Commit(); err != nil {
		return ProtectedCrashRecovery{}, fmt.Errorf("commit protected crash recovery: %w", err)
	}
	return result, nil
}

type ownerDecisionQuerier interface {
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

func (d *DB) verifyOwnerDecisionHistory(runID string, authority *OwnerDecisionAuthority, expectedHead string, query ownerDecisionQuerier) (string, error) {
	if err := verifyOwnerDecisionRunIdentityQuery(query, runID, authority); err != nil {
		return "", err
	}
	rows, err := query.Query(
		`SELECT sequence, gate_id, previous_head, record_digest, history_head, envelope_json,
		        projection_round_id, selected_finding_ids, selection_source, user_findings_json
		   FROM owner_decision_events WHERE run_id = ? ORDER BY sequence`,
		runID,
	)
	if err != nil {
		return "", fmt.Errorf("read owner decision history: %w", err)
	}
	var events []*ownerDecisionEvent
	for rows.Next() {
		event, err := scanOwnerDecisionEvent(rows)
		if err != nil {
			rows.Close()
			return "", err
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return "", err
	}
	if err := rows.Close(); err != nil {
		return "", err
	}
	head := authority.GenesisHead
	wantSequence := 1
	for _, event := range events {
		if event.Sequence != wantSequence || event.PreviousHead != head {
			return "", errors.New("owner decision history sequence or previous head is invalid")
		}
		var envelope ownerdecision.Envelope
		if err := json.Unmarshal([]byte(event.EnvelopeJSON), &envelope); err != nil {
			return "", fmt.Errorf("decode owner decision envelope: %w", err)
		}
		if err := ownerdecision.VerifySignature(authority.PublicKey, envelope); err != nil {
			return "", fmt.Errorf("verify owner decision event %d: %w", event.Sequence, err)
		}
		if envelope.Challenge.RunID != runID || envelope.Challenge.RepoID != authority.RepoID ||
			envelope.Challenge.Branch != authority.Branch || envelope.Challenge.HeadSHA != authority.InitialHeadSHA ||
			envelope.Challenge.PreviousHead != event.PreviousHead || event.GateID != ownerGateID(envelope.Challenge) {
			return "", errors.New("owner decision history gate binding is invalid")
		}
		if err := verifyHistoricalOwnerChallengeQuery(query, runID, envelope.Challenge); err != nil {
			return "", fmt.Errorf("verify owner decision gate at sequence %d: %w", event.Sequence, err)
		}
		recordDigest, canonicalEnvelope, err := encodeOwnerDecisionRecord(envelope, event.Projection)
		if err != nil {
			return "", err
		}
		if canonicalEnvelope != event.EnvelopeJSON || recordDigest != event.RecordDigest {
			return "", errors.New("owner decision history record digest is invalid")
		}
		envelopeDigest, err := ownerdecision.EnvelopeDigest(envelope)
		if err != nil {
			return "", err
		}
		nextHead, err := ownerdecision.NextHead(head, envelopeDigest)
		if err != nil || nextHead != event.HistoryHead {
			return "", errors.New("owner decision history head is invalid")
		}
		if err := verifyOwnerProjectionQuery(query, event.Projection); err != nil {
			return "", fmt.Errorf("verify owner decision projection at sequence %d: %w", event.Sequence, err)
		}
		if err := validateOwnerProjectionQuery(query, runID, envelope, event.Projection); err != nil {
			return "", fmt.Errorf("verify owner decision projection authority at sequence %d: %w", event.Sequence, err)
		}
		head = nextHead
		wantSequence++
	}
	if expectedHead != "" && head != expectedHead {
		return "", fmt.Errorf("owner decision history head is %s, expected %s", head, expectedHead)
	}
	return head, nil
}

func getOwnerDecisionAuthorityTx(tx *sql.Tx, runID string) (*OwnerDecisionAuthority, error) {
	authority := &OwnerDecisionAuthority{RunID: runID}
	var publicKey []byte
	err := tx.QueryRow(
		`SELECT public_key, key_id, repo_id, branch, initial_head_sha, genesis_head, created_at FROM owner_decision_authorities WHERE run_id = ?`,
		runID,
	).Scan(&publicKey, &authority.KeyID, &authority.RepoID, &authority.Branch, &authority.InitialHeadSHA, &authority.GenesisHead, &authority.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get owner decision authority: %w", err)
	}
	if len(publicKey) != ed25519.PublicKeySize {
		return nil, errors.New("get owner decision authority: stored public key is invalid")
	}
	authority.PublicKey = ed25519.PublicKey(slices.Clone(publicKey))
	derivedKeyID, err := ownerdecision.KeyID(authority.PublicKey)
	if err != nil || derivedKeyID != authority.KeyID {
		return nil, errors.New("get owner decision authority: stored key id is invalid")
	}
	if derived, err := ownerdecision.GenesisHeadForRun(authority.PublicKey, authority.RepoID, authority.Branch, authority.InitialHeadSHA); err != nil || derived != authority.GenesisHead {
		return nil, errors.New("get owner decision authority: stored key binding is invalid")
	}
	return authority, nil
}

func verifyOwnerDecisionRunIdentityQuery(query ownerDecisionQuerier, runID string, authority *OwnerDecisionAuthority) error {
	var repoID, branch string
	var submittedHead sql.NullString
	if err := query.QueryRow(`SELECT repo_id, branch, submitted_head_sha FROM runs WHERE id = ?`, runID).Scan(&repoID, &branch, &submittedHead); err != nil {
		return fmt.Errorf("verify owner decision run identity: %w", err)
	}
	if repoID != authority.RepoID || branch != authority.Branch || !submittedHead.Valid || submittedHead.String != authority.InitialHeadSHA {
		return errors.New("owner decision: run identity does not match its sealed authority")
	}
	return nil
}

func validateOwnerChallengeTx(tx *sql.Tx, authority *OwnerDecisionAuthority, runID, gateID string, challenge ownerdecision.Challenge) error {
	var repoID, branch, headSHA string
	var submittedHead sql.NullString
	var runStatus types.RunStatus
	var awaitingAgent sql.NullInt64
	if err := tx.QueryRow(`SELECT repo_id, branch, head_sha, submitted_head_sha, status, awaiting_agent_since FROM runs WHERE id = ?`, runID).Scan(&repoID, &branch, &headSHA, &submittedHead, &runStatus, &awaitingAgent); err != nil {
		return fmt.Errorf("append owner decision: read run binding: %w", err)
	}
	if runStatus != types.RunPending && runStatus != types.RunRunning {
		return errors.New("append owner decision: run is already terminal")
	}
	if repoID != authority.RepoID || branch != authority.Branch || !submittedHead.Valid || submittedHead.String != authority.InitialHeadSHA {
		return errors.New("append owner decision: current run identity does not match sealed authority")
	}
	if challenge.RunID != runID || challenge.RepoID != authority.RepoID || challenge.Branch != authority.Branch ||
		challenge.HeadSHA != authority.InitialHeadSHA || challenge.GateHeadSHA != headSHA {
		return errors.New("append owner decision: challenge does not match current run identity")
	}
	if gateID != ownerGateID(challenge) {
		return errors.New("append owner decision: gate id does not match challenge")
	}
	if challenge.Purpose != ownerdecision.PurposeRespond {
		return nil
	}
	var storedRunID string
	var step types.StepName
	var stepStatus types.StepStatus
	var findings *string
	if err := tx.QueryRow(
		`SELECT sr.run_id, sr.step_name, sr.status, r.findings_json
		   FROM step_rounds r JOIN step_results sr ON sr.id = r.step_result_id
		  WHERE r.id = ? AND sr.id = ?`,
		challenge.RoundID, challenge.StepResultID,
	).Scan(&storedRunID, &step, &stepStatus, &findings); err != nil {
		return fmt.Errorf("append owner decision: read response gate: %w", err)
	}
	if !awaitingAgent.Valid || (stepStatus != types.StepStatusAwaitingApproval && stepStatus != types.StepStatusFixReview) ||
		storedRunID != runID || step != challenge.Step || findings == nil || ownerdecision.DigestBytes([]byte(*findings)) != challenge.FindingsDigest {
		return errors.New("append owner decision: response gate binding does not match durable round")
	}
	return nil
}

func validateOwnerProjection(envelope ownerdecision.Envelope, projection *OwnerDecisionProjection) error {
	if envelope.Challenge.Purpose == ownerdecision.PurposeCancel {
		if projection != nil {
			return errors.New("append owner decision: cancel cannot change a round projection")
		}
		return nil
	}
	if projection == nil || projection.RoundID != envelope.Challenge.RoundID {
		return errors.New("append owner decision: response is missing its exact round projection")
	}
	var selected []string
	if err := json.Unmarshal([]byte(projection.SelectedFindingIDs), &selected); err != nil || selected == nil {
		return errors.New("append owner decision: selected finding ids must be a JSON array")
	}
	switch envelope.Response.Action {
	case types.ActionApprove, types.ActionSkip, types.ActionAbort:
		if len(selected) != 0 || projection.SelectionSource != RoundSelectionSourceUserDeclined || projection.UserFindingsJSON != nil {
			return errors.New("append owner decision: decline projection does not match signed action")
		}
	case types.ActionFix:
		if projection.SelectionSource != RoundSelectionSourceUser || len(selected) == 0 {
			return errors.New("append owner decision: fix projection is incomplete")
		}
		for _, id := range envelope.Response.FindingIDs {
			if !slices.Contains(selected, id) {
				return fmt.Errorf("append owner decision: signed finding %q is absent from projection", id)
			}
		}
	default:
		return errors.New("append owner decision: unsupported signed action")
	}
	return nil
}

func validateOwnerProjectionQuery(query ownerDecisionQuerier, runID string, envelope ownerdecision.Envelope, projection *OwnerDecisionProjection) error {
	if err := validateOwnerProjection(envelope, projection); err != nil {
		return err
	}
	if envelope.Challenge.Purpose != ownerdecision.PurposeRespond || envelope.Response.Action != types.ActionFix {
		return nil
	}
	findings, err := ownerChallengeFindingsQuery(query, runID, envelope.Challenge)
	if err != nil {
		return err
	}
	expected, err := ownerdecision.MaterializeProjection(findings, envelope.Response)
	if err != nil {
		return err
	}
	if projection.SelectedFindingIDs != expected.SelectedFindingIDs {
		return errors.New("append owner decision: selected finding projection does not exactly match signed response")
	}
	if projection.UserFindingsJSON == nil || expected.UserFindingsJSON == nil {
		if projection.UserFindingsJSON != nil || expected.UserFindingsJSON != nil {
			return errors.New("append owner decision: user findings projection does not exactly match signed response")
		}
	} else if *projection.UserFindingsJSON != *expected.UserFindingsJSON {
		return errors.New("append owner decision: user findings projection does not exactly match signed response")
	}
	return nil
}

func encodeOwnerDecisionRecord(envelope ownerdecision.Envelope, projection *OwnerDecisionProjection) (string, string, error) {
	envelopeBytes, err := json.Marshal(envelope)
	if err != nil {
		return "", "", fmt.Errorf("encode owner decision envelope: %w", err)
	}
	recordBytes, err := json.Marshal(ownerDecisionJournalRecord{Envelope: envelope, Projection: projection})
	if err != nil {
		return "", "", fmt.Errorf("encode owner decision record: %w", err)
	}
	return ownerdecision.DigestBytes(recordBytes), string(envelopeBytes), nil
}

func getOwnerDecisionEventTx(tx *sql.Tx, runID, gateID string) (*ownerDecisionEvent, error) {
	row := tx.QueryRow(
		`SELECT sequence, gate_id, previous_head, record_digest, history_head, envelope_json,
		        projection_round_id, selected_finding_ids, selection_source, user_findings_json
		   FROM owner_decision_events WHERE run_id = ? AND gate_id = ?`,
		runID, gateID,
	)
	event, err := scanOwnerDecisionEvent(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return event, err
}

type ownerDecisionScanner interface {
	Scan(dest ...any) error
}

func scanOwnerDecisionEvent(scanner ownerDecisionScanner) (*ownerDecisionEvent, error) {
	event := &ownerDecisionEvent{}
	var roundID, selected, source, userFindings sql.NullString
	if err := scanner.Scan(
		&event.Sequence, &event.GateID, &event.PreviousHead, &event.RecordDigest, &event.HistoryHead, &event.EnvelopeJSON,
		&roundID, &selected, &source, &userFindings,
	); err != nil {
		return nil, err
	}
	if roundID.Valid || selected.Valid || source.Valid || userFindings.Valid {
		if !roundID.Valid || !selected.Valid || !source.Valid {
			return nil, errors.New("owner decision event has a partial projection")
		}
		event.Projection = &OwnerDecisionProjection{
			RoundID:            roundID.String,
			SelectedFindingIDs: selected.String,
			SelectionSource:    source.String,
		}
		if userFindings.Valid {
			value := userFindings.String
			event.Projection.UserFindingsJSON = &value
		}
	}
	return event, nil
}

func verifyOwnerProjectionTx(tx *sql.Tx, projection *OwnerDecisionProjection) error {
	return verifyOwnerProjectionQuery(tx, projection)
}

func verifyOwnerProjectionQuery(query ownerDecisionQuerier, projection *OwnerDecisionProjection) error {
	if projection == nil {
		return nil
	}
	var selected, source, userFindings sql.NullString
	if err := query.QueryRow(
		`SELECT selected_finding_ids, selection_source, user_findings_json FROM step_rounds WHERE id = ?`,
		projection.RoundID,
	).Scan(&selected, &source, &userFindings); err != nil {
		return err
	}
	if !selected.Valid || selected.String != projection.SelectedFindingIDs || !source.Valid || source.String != projection.SelectionSource {
		return errors.New("materialized round decision does not match journal")
	}
	if projection.UserFindingsJSON == nil {
		if userFindings.Valid {
			return errors.New("materialized user findings do not match journal")
		}
	} else if !userFindings.Valid || userFindings.String != *projection.UserFindingsJSON {
		return errors.New("materialized user findings do not match journal")
	}
	return nil
}

// verifyHistoricalOwnerChallengeQuery keeps the signed gate tied to the
// durable evidence it authorized even after that gate is no longer active.
// Run status and the mutable run head legitimately advance, but the reviewed
// round and its findings are append-only inputs to the decision history.
func verifyHistoricalOwnerChallengeQuery(query ownerDecisionQuerier, runID string, challenge ownerdecision.Challenge) error {
	if challenge.Purpose != ownerdecision.PurposeRespond {
		return nil
	}
	_, err := ownerChallengeFindingsQuery(query, runID, challenge)
	return err
}

func ownerChallengeFindingsQuery(query ownerDecisionQuerier, runID string, challenge ownerdecision.Challenge) (string, error) {
	var storedRunID string
	var step types.StepName
	var findings *string
	if err := query.QueryRow(
		`SELECT sr.run_id, sr.step_name, r.findings_json
		   FROM step_rounds r JOIN step_results sr ON sr.id = r.step_result_id
		  WHERE r.id = ? AND sr.id = ?`,
		challenge.RoundID, challenge.StepResultID,
	).Scan(&storedRunID, &step, &findings); err != nil {
		return "", fmt.Errorf("read historical response gate: %w", err)
	}
	if storedRunID != runID || step != challenge.Step || findings == nil ||
		ownerdecision.DigestBytes([]byte(*findings)) != challenge.FindingsDigest {
		return "", errors.New("historical response gate does not match signed findings")
	}
	return *findings, nil
}

func ownerDecisionEventCountTx(tx *sql.Tx, runID string) (int, error) {
	var count int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM owner_decision_events WHERE run_id = ?`, runID).Scan(&count); err != nil {
		return 0, fmt.Errorf("count owner decision events: %w", err)
	}
	return count, nil
}

func ownerGateID(challenge ownerdecision.Challenge) string {
	if challenge.Purpose == ownerdecision.PurposeRespond {
		return ownerdecision.PurposeRespond + ":" + challenge.RoundID
	}
	return ownerdecision.PurposeCancel + ":" + challenge.Nonce
}

func equalOwnerProjection(left, right *OwnerDecisionProjection) bool {
	if left == nil || right == nil {
		return left == right
	}
	if left.RoundID != right.RoundID || left.SelectedFindingIDs != right.SelectedFindingIDs || left.SelectionSource != right.SelectionSource {
		return false
	}
	if left.UserFindingsJSON == nil || right.UserFindingsJSON == nil {
		return left.UserFindingsJSON == nil && right.UserFindingsJSON == nil
	}
	return *left.UserFindingsJSON == *right.UserFindingsJSON
}

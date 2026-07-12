package db

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// PublicationKind identifies the transport whose durable publication is being
// journaled.
type PublicationKind string

const (
	PublicationKindPush PublicationKind = "push"
	PublicationKindCI   PublicationKind = "ci"
)

// PublicationState records the durable progress of one publication. Transport
// must not begin until a publication reaches PublicationStatePrepared.
// Accepted means the remote has the sealed SHA but local post-publication
// restoration may still need recovery. Completed is durable only after that
// restoration has succeeded.
type PublicationState string

const (
	PublicationStatePrepared  PublicationState = "prepared"
	PublicationStateAttempted PublicationState = "attempted"
	PublicationStateAccepted  PublicationState = "accepted"
	PublicationStateCompleted PublicationState = "completed"
)

// PublicationRecoveryStepMode controls the optional step projection committed
// with publication completion after crash recovery.
type PublicationRecoveryStepMode string

const (
	PublicationRecoveryNone         PublicationRecoveryStepMode = "none"
	PublicationRecoveryCompletePush PublicationRecoveryStepMode = "complete_push"
	PublicationRecoveryResetCI      PublicationRecoveryStepMode = "reset_ci"
)

// Publication is the durable transaction journal for a transport operation.
// All transport inputs are immutable after preparation.
type Publication struct {
	ID                 string
	RunID              string
	Kind               PublicationKind
	SealID             string
	SealSHA            string
	DestinationURL     string
	DestinationRef     string
	ExpectedRemoteSHA  string
	Force              bool
	CleanupSnapshotDir string
	State              PublicationState
	CreatedAt          int64
	AttemptedAt        *int64
	AcceptedAt         *int64
	CompletedAt        *int64
}

// PreparePublicationInput contains the exact immutable transport inputs that
// must be durable before transport begins. An empty ExpectedRemoteSHA
// explicitly records that the destination ref is expected to be absent.
type PreparePublicationInput struct {
	RunID              string
	Kind               PublicationKind
	SealID             string
	SealSHA            string
	DestinationURL     string
	DestinationRef     string
	ExpectedRemoteSHA  string
	Force              bool
	CleanupSnapshotDir string
}

// PrepareCISealedPublicationInput contains the candidate SHA and immutable
// transport inputs for one CI republication. The journal supplies the CI kind
// and the exact ci_republish seal linking the transaction to that SHA.
type PrepareCISealedPublicationInput struct {
	RunID              string
	SHA                string
	DestinationURL     string
	DestinationRef     string
	ExpectedRemoteSHA  string
	Force              bool
	CleanupSnapshotDir string
}

const publicationColumns = `id, run_id, kind, seal_id, seal_sha, destination_url, destination_ref, expected_remote_sha, force, cleanup_snapshot_dir, state, created_at, attempted_at, accepted_at, completed_at`

func scanPublication(row interface{ Scan(...any) error }, publication *Publication) error {
	var force int
	if err := row.Scan(
		&publication.ID,
		&publication.RunID,
		&publication.Kind,
		&publication.SealID,
		&publication.SealSHA,
		&publication.DestinationURL,
		&publication.DestinationRef,
		&publication.ExpectedRemoteSHA,
		&force,
		&publication.CleanupSnapshotDir,
		&publication.State,
		&publication.CreatedAt,
		&publication.AttemptedAt,
		&publication.AcceptedAt,
		&publication.CompletedAt,
	); err != nil {
		return err
	}
	publication.Force = force != 0
	return nil
}

// PreparePublication durably records immutable transport inputs. Repeating the
// same logical preparation returns the original row; a retry with different
// immutable inputs fails without changing it.
func (d *DB) PreparePublication(input PreparePublicationInput) (*Publication, error) {
	if err := validatePreparePublicationInput(input); err != nil {
		return nil, err
	}

	tx, err := d.sql.Begin()
	if err != nil {
		return nil, fmt.Errorf("begin prepare publication: %w", err)
	}
	defer tx.Rollback()

	publication, err := preparePublication(tx, input)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit publication preparation: %w", err)
	}
	return publication, nil
}

const ciRepublishSealReason = "ci_republish"

// PrepareCISealedPublication atomically finds or appends the exact
// ci_republish seal for a candidate SHA and prepares its immutable CI
// publication transaction. A different SHA cannot be prepared while another
// CI publication remains incomplete.
func (d *DB) PrepareCISealedPublication(input PrepareCISealedPublicationInput) (*Seal, *Publication, error) {
	candidateSeal := &Seal{
		ID:       newID(),
		RunID:    input.RunID,
		SHA:      input.SHA,
		Reason:   ciRepublishSealReason,
		SealedAt: now(),
	}
	publicationInput := PreparePublicationInput{
		RunID:              input.RunID,
		Kind:               PublicationKindCI,
		SealID:             candidateSeal.ID,
		SealSHA:            candidateSeal.SHA,
		DestinationURL:     input.DestinationURL,
		DestinationRef:     input.DestinationRef,
		ExpectedRemoteSHA:  input.ExpectedRemoteSHA,
		Force:              input.Force,
		CleanupSnapshotDir: input.CleanupSnapshotDir,
	}
	if err := validatePreparePublicationInput(publicationInput); err != nil {
		return nil, nil, err
	}

	tx, err := d.sql.Begin()
	if err != nil {
		return nil, nil, fmt.Errorf("begin prepare CI sealed publication: %w", err)
	}
	defer tx.Rollback()

	var pendingCount int
	if err := tx.QueryRow(
		`SELECT count(*) FROM publication_transactions WHERE run_id = ? AND kind = ? AND state <> ?`,
		input.RunID,
		PublicationKindCI,
		PublicationStateCompleted,
	).Scan(&pendingCount); err != nil {
		return nil, nil, fmt.Errorf("count incomplete CI publications: %w", err)
	}
	if pendingCount > 1 {
		return nil, nil, fmt.Errorf("prepare CI sealed publication: run %q has %d incomplete CI publications", input.RunID, pendingCount)
	}

	if pendingCount == 1 {
		publication := &Publication{}
		if err := scanPublication(tx.QueryRow(
			`SELECT `+publicationColumns+` FROM publication_transactions WHERE run_id = ? AND kind = ? AND state <> ?`,
			input.RunID,
			PublicationKindCI,
			PublicationStateCompleted,
		), publication); err != nil {
			return nil, nil, fmt.Errorf("get incomplete CI publication: %w", err)
		}
		if publication.SealSHA != input.SHA {
			return nil, nil, fmt.Errorf(
				"prepare CI sealed publication: incomplete transaction %q is for SHA %q, not %q",
				publication.ID,
				publication.SealSHA,
				input.SHA,
			)
		}

		seal := &Seal{}
		err := tx.QueryRow(
			`SELECT id, run_id, sha, reason, sealed_at FROM run_seals WHERE id = ?`,
			publication.SealID,
		).Scan(&seal.ID, &seal.RunID, &seal.SHA, &seal.Reason, &seal.SealedAt)
		if err != nil {
			return nil, nil, fmt.Errorf("get incomplete CI publication seal: %w", err)
		}
		if seal.Reason != ciRepublishSealReason {
			return nil, nil, fmt.Errorf(
				"prepare CI sealed publication: incomplete transaction %q uses seal reason %q",
				publication.ID,
				seal.Reason,
			)
		}

		publicationInput.SealID = seal.ID
		publicationInput.SealSHA = seal.SHA
		prepared, err := preparePublication(tx, publicationInput)
		if err != nil {
			return nil, nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, nil, fmt.Errorf("commit CI sealed publication preparation: %w", err)
		}
		return seal, prepared, nil
	}

	seal := &Seal{}
	err = tx.QueryRow(
		`SELECT id, run_id, sha, reason, sealed_at FROM run_seals WHERE run_id = ? AND reason = ? ORDER BY sealed_at DESC, id DESC LIMIT 1`,
		input.RunID,
		ciRepublishSealReason,
	).Scan(&seal.ID, &seal.RunID, &seal.SHA, &seal.Reason, &seal.SealedAt)
	if err != nil && err != sql.ErrNoRows {
		return nil, nil, fmt.Errorf("get latest CI republish seal: %w", err)
	}
	if err == sql.ErrNoRows || seal.SHA != input.SHA {
		seal = candidateSeal
		if _, err := tx.Exec(
			`INSERT INTO run_seals (id, run_id, sha, reason, sealed_at) VALUES (?, ?, ?, ?, ?)`,
			seal.ID,
			seal.RunID,
			seal.SHA,
			seal.Reason,
			seal.SealedAt,
		); err != nil {
			return nil, nil, fmt.Errorf("insert CI republish seal: %w", err)
		}
	}

	publicationInput.SealID = seal.ID
	publicationInput.SealSHA = seal.SHA
	publication, err := preparePublication(tx, publicationInput)
	if err != nil {
		return nil, nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("commit CI sealed publication preparation: %w", err)
	}
	return seal, publication, nil
}

func preparePublication(tx *sql.Tx, input PreparePublicationInput) (*Publication, error) {
	var sealRunID, sealSHA string
	err := tx.QueryRow(`SELECT run_id, sha FROM run_seals WHERE id = ?`, input.SealID).Scan(&sealRunID, &sealSHA)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("prepare publication: seal %q not found", input.SealID)
	}
	if err != nil {
		return nil, fmt.Errorf("validate publication seal: %w", err)
	}
	if sealRunID != input.RunID {
		return nil, fmt.Errorf("prepare publication: seal %q does not belong to run %q", input.SealID, input.RunID)
	}
	if sealSHA != input.SealSHA {
		return nil, fmt.Errorf("prepare publication: seal SHA %q does not match durable seal SHA %q", input.SealSHA, sealSHA)
	}

	candidateID := newID()
	createdAt := now()
	force := 0
	if input.Force {
		force = 1
	}
	if _, err := tx.Exec(`
		INSERT INTO publication_transactions
		    (id, run_id, kind, seal_id, seal_sha, destination_url,
		     destination_ref, expected_remote_sha, force, cleanup_snapshot_dir, state, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(run_id, kind, seal_id) DO NOTHING`,
		candidateID,
		input.RunID,
		input.Kind,
		input.SealID,
		input.SealSHA,
		input.DestinationURL,
		input.DestinationRef,
		input.ExpectedRemoteSHA,
		force,
		input.CleanupSnapshotDir,
		PublicationStatePrepared,
		createdAt,
	); err != nil {
		return nil, fmt.Errorf("insert publication: %w", err)
	}

	publication := &Publication{}
	if err := scanPublication(tx.QueryRow(
		`SELECT `+publicationColumns+` FROM publication_transactions WHERE run_id = ? AND kind = ? AND seal_id = ?`,
		input.RunID,
		input.Kind,
		input.SealID,
	), publication); err != nil {
		return nil, fmt.Errorf("get prepared publication: %w", err)
	}
	if !publicationMatchesInput(publication, input) {
		return nil, fmt.Errorf("prepare publication: immutable input differs from existing transaction %q", publication.ID)
	}
	return publication, nil
}

func validatePreparePublicationInput(input PreparePublicationInput) error {
	if strings.TrimSpace(input.RunID) == "" || strings.TrimSpace(input.SealID) == "" || strings.TrimSpace(input.SealSHA) == "" {
		return fmt.Errorf("prepare publication: run ID, seal ID, and seal SHA are required")
	}
	if strings.TrimSpace(input.DestinationURL) == "" || strings.TrimSpace(input.DestinationRef) == "" {
		return fmt.Errorf("prepare publication: destination URL and ref are required")
	}
	switch input.Kind {
	case PublicationKindPush, PublicationKindCI:
		return nil
	default:
		return fmt.Errorf("prepare publication: invalid kind %q", input.Kind)
	}
}

func publicationMatchesInput(publication *Publication, input PreparePublicationInput) bool {
	return publication.RunID == input.RunID &&
		publication.Kind == input.Kind &&
		publication.SealID == input.SealID &&
		publication.SealSHA == input.SealSHA &&
		publication.DestinationURL == input.DestinationURL &&
		publication.DestinationRef == input.DestinationRef &&
		publication.ExpectedRemoteSHA == input.ExpectedRemoteSHA &&
		publication.Force == input.Force &&
		publication.CleanupSnapshotDir == input.CleanupSnapshotDir
}

// GetPublication returns a publication by ID, or nil when it does not exist.
func (d *DB) GetPublication(id string) (*Publication, error) {
	publication := &Publication{}
	err := scanPublication(d.sql.QueryRow(
		`SELECT `+publicationColumns+` FROM publication_transactions WHERE id = ?`,
		id,
	), publication)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get publication: %w", err)
	}
	return publication, nil
}

// LatestPublication returns the latest publication for a run and kind, or nil
// when none exists.
func (d *DB) LatestPublication(runID string, kind PublicationKind) (*Publication, error) {
	publication := &Publication{}
	err := scanPublication(d.sql.QueryRow(
		`SELECT `+publicationColumns+` FROM publication_transactions WHERE run_id = ? AND kind = ? ORDER BY created_at DESC, id DESC LIMIT 1`,
		runID,
		kind,
	), publication)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("latest publication: %w", err)
	}
	return publication, nil
}

// PendingPublications returns all incomplete publications in preparation order.
func (d *DB) PendingPublications() ([]*Publication, error) {
	rows, err := d.sql.Query(`
		SELECT ` + publicationColumns + `
		FROM publication_transactions
		WHERE state <> 'completed'
		ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("list pending publications: %w", err)
	}
	defer rows.Close()

	var publications []*Publication
	for rows.Next() {
		publication := &Publication{}
		if err := scanPublication(rows, publication); err != nil {
			return nil, fmt.Errorf("scan pending publication: %w", err)
		}
		publications = append(publications, publication)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list pending publications: %w", err)
	}
	return publications, nil
}

// RecoverablePublications returns incomplete publications plus completed
// publications whose matching executor step still needs recovery.
func (d *DB) RecoverablePublications() ([]*Publication, error) {
	rows, err := d.sql.Query(`
		WITH candidates AS (
			SELECT publication_transactions.*,
			       ROW_NUMBER() OVER (
					PARTITION BY publication_transactions.run_id, publication_transactions.kind
					ORDER BY
						CASE WHEN publication_transactions.state <> ? THEN 0 ELSE 1 END,
						publication_transactions.created_at DESC,
						publication_transactions.id DESC
			       ) AS recovery_rank
			FROM publication_transactions
			JOIN runs ON runs.id = publication_transactions.run_id
			WHERE runs.status IN (?, ?)
			  AND (
					publication_transactions.state <> ?
					OR EXISTS (
						SELECT 1
						FROM step_results
						WHERE step_results.run_id = publication_transactions.run_id
						  AND step_results.status = ?
						  AND (
								(publication_transactions.kind = ? AND step_results.step_name = ?)
								OR (publication_transactions.kind = ? AND step_results.step_name = ?)
						  )
					)
			  )
		)
		SELECT `+publicationColumns+`
		FROM candidates
		WHERE recovery_rank = 1
		ORDER BY created_at, id`,
		PublicationStateCompleted,
		types.RunPending,
		types.RunRunning,
		PublicationStateCompleted,
		types.StepStatusRunning,
		PublicationKindPush,
		types.StepPush,
		PublicationKindCI,
		types.StepCI,
	)
	if err != nil {
		return nil, fmt.Errorf("list recoverable publications: %w", err)
	}
	defer rows.Close()

	var publications []*Publication
	for rows.Next() {
		publication := &Publication{}
		if err := scanPublication(rows, publication); err != nil {
			return nil, fmt.Errorf("scan recoverable publication: %w", err)
		}
		publications = append(publications, publication)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list recoverable publications: %w", err)
	}
	return publications, nil
}

// MarkPublicationAttempted records that transport began. Repeated calls and
// calls after a later state are monotonic no-ops.
func (d *DB) MarkPublicationAttempted(id string) error {
	result, err := d.sql.Exec(`
		UPDATE publication_transactions
		SET state = ?, attempted_at = ?
		WHERE id = ? AND state = ?`,
		PublicationStateAttempted,
		now(),
		id,
		PublicationStatePrepared,
	)
	if err != nil {
		return fmt.Errorf("mark publication attempted: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("mark publication attempted: %w", err)
	}
	if changed == 1 {
		return nil
	}
	state, err := d.publicationState(id)
	if err != nil {
		return err
	}
	switch state {
	case PublicationStateAttempted, PublicationStateAccepted, PublicationStateCompleted:
		return nil
	default:
		return fmt.Errorf("mark publication attempted: publication %q is in illegal state %q", id, state)
	}
}

// MarkPublicationAccepted records that the remote accepted the transport.
// Repeated calls and calls after completion are monotonic no-ops.
func (d *DB) MarkPublicationAccepted(id string) error {
	result, err := d.sql.Exec(`
		UPDATE publication_transactions
		SET state = ?, accepted_at = ?
		WHERE id = ? AND state = ?`,
		PublicationStateAccepted,
		now(),
		id,
		PublicationStateAttempted,
	)
	if err != nil {
		return fmt.Errorf("mark publication accepted: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("mark publication accepted: %w", err)
	}
	if changed == 1 {
		return nil
	}
	state, err := d.publicationState(id)
	if err != nil {
		return err
	}
	switch state {
	case PublicationStateAccepted, PublicationStateCompleted:
		return nil
	default:
		return fmt.Errorf("mark publication accepted: publication %q is in illegal state %q", id, state)
	}
}

func (d *DB) publicationState(id string) (PublicationState, error) {
	var state PublicationState
	err := d.sql.QueryRow(`SELECT state FROM publication_transactions WHERE id = ?`, id).Scan(&state)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("publication %q not found", id)
	}
	if err != nil {
		return "", fmt.Errorf("get publication state: %w", err)
	}
	return state, nil
}

// CompletePublication atomically marks an accepted publication completed,
// projects its sealed SHA onto the run, and optionally repairs the matching
// running publication step after crash recovery.
func (d *DB) CompletePublication(id string, recoveryStepMode PublicationRecoveryStepMode) error {
	if err := validatePublicationRecoveryStepMode(recoveryStepMode); err != nil {
		return err
	}

	tx, err := d.sql.Begin()
	if err != nil {
		return fmt.Errorf("begin publication completion: %w", err)
	}
	defer tx.Rollback()

	publication := &Publication{}
	if err := scanPublication(tx.QueryRow(
		`SELECT `+publicationColumns+` FROM publication_transactions WHERE id = ?`,
		id,
	), publication); err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("complete publication: publication %q not found", id)
		}
		return fmt.Errorf("get publication for completion: %w", err)
	}
	if err := validatePublicationRecoveryKind(publication.Kind, recoveryStepMode); err != nil {
		return err
	}
	if publication.State == PublicationStateCompleted && recoveryStepMode == PublicationRecoveryNone {
		return nil
	}
	if publication.State != PublicationStateAccepted && publication.State != PublicationStateCompleted {
		return fmt.Errorf("complete publication: publication %q is in illegal state %q", id, publication.State)
	}

	timestamp := now()
	if publication.State == PublicationStateAccepted {
		result, err := tx.Exec(`
			UPDATE publication_transactions
			SET state = ?, completed_at = ?
			WHERE id = ? AND state = ?`,
			PublicationStateCompleted,
			timestamp,
			publication.ID,
			PublicationStateAccepted,
		)
		if err != nil {
			return fmt.Errorf("mark publication completed: %w", err)
		}
		if err := requireOneChangedRow(result, "publication completion"); err != nil {
			return err
		}
	}

	result, err := tx.Exec(`
		UPDATE runs
		SET head_sha = ?,
		    updated_at = CASE WHEN head_sha = ? THEN updated_at ELSE ? END
		WHERE id = ?`,
		publication.SealSHA,
		publication.SealSHA,
		timestamp,
		publication.RunID,
	)
	if err != nil {
		return fmt.Errorf("project publication run head: %w", err)
	}
	if err := requireOneChangedRow(result, "publication run projection"); err != nil {
		return err
	}

	if recoveryStepMode != PublicationRecoveryNone {
		stepName := types.StepCI
		requireStarted := 0
		if recoveryStepMode == PublicationRecoveryCompletePush {
			stepName = types.StepPush
			requireStarted = 1
		}
		if _, err := tx.Exec(`
			UPDATE step_rounds
			SET state = ?, completed_at = ?,
			    duration_ms = max(0, (? - COALESCE(started_at, ?)) * 1000)
			WHERE state = ?
			  AND step_result_id IN (
					SELECT id
					FROM step_results
					WHERE run_id = ? AND step_name = ? AND status = ?
					  AND (? = 0 OR started_at IS NOT NULL)
			  )`,
			StepRoundCompleted,
			timestamp,
			timestamp,
			timestamp,
			StepRoundReserved,
			publication.RunID,
			stepName,
			types.StepStatusRunning,
			requireStarted,
		); err != nil {
			return fmt.Errorf("complete publication recovery rounds: %w", err)
		}
	}

	switch recoveryStepMode {
	case PublicationRecoveryNone:
	case PublicationRecoveryCompletePush:
		result, err = tx.Exec(`
			UPDATE step_results
			SET status = ?, exit_code = COALESCE(exit_code, 0),
			    duration_ms = COALESCE(duration_ms, 0), log_path = COALESCE(log_path, ''), error = NULL,
			    completed_at = COALESCE(completed_at, ?), last_activity_at = ?, last_activity = ?, agent_pid = NULL
			WHERE run_id = ? AND step_name = ? AND status = ? AND started_at IS NOT NULL`,
			types.StepStatusCompleted,
			timestamp,
			timestamp,
			"status: completed",
			publication.RunID,
			types.StepPush,
			types.StepStatusRunning,
		)
	case PublicationRecoveryResetCI:
		result, err = tx.Exec(`
			UPDATE step_results
			SET status = ?, exit_code = NULL, duration_ms = NULL, log_path = NULL, error = NULL,
			    started_at = NULL, completed_at = NULL, last_activity_at = ?, last_activity = ?, agent_pid = NULL
			WHERE run_id = ? AND step_name = ? AND status = ?`,
			types.StepStatusPending,
			timestamp,
			"status: pending",
			publication.RunID,
			types.StepCI,
			types.StepStatusRunning,
		)
	}
	if recoveryStepMode != PublicationRecoveryNone {
		if err != nil {
			return fmt.Errorf("project publication recovery step: %w", err)
		}
		if err := requireOneChangedRow(result, "publication recovery step projection"); err != nil {
			return err
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit publication completion: %w", err)
	}
	return nil
}

func validatePublicationRecoveryStepMode(mode PublicationRecoveryStepMode) error {
	switch mode {
	case PublicationRecoveryNone, PublicationRecoveryCompletePush, PublicationRecoveryResetCI:
		return nil
	default:
		return fmt.Errorf("complete publication: invalid recovery step mode %q", mode)
	}
}

func validatePublicationRecoveryKind(kind PublicationKind, mode PublicationRecoveryStepMode) error {
	if mode == PublicationRecoveryCompletePush && kind != PublicationKindPush {
		return fmt.Errorf("complete publication: push recovery requires a push publication, got %q", kind)
	}
	if mode == PublicationRecoveryResetCI && kind != PublicationKindCI {
		return fmt.Errorf("complete publication: CI recovery requires a CI publication, got %q", kind)
	}
	return nil
}

func requireOneChangedRow(result sql.Result, operation string) error {
	changed, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s: %w", operation, err)
	}
	if changed != 1 {
		return fmt.Errorf("%s changed %d rows, want exactly 1", operation, changed)
	}
	return nil
}

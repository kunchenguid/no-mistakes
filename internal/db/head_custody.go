package db

import (
	"database/sql"
	"errors"
	"fmt"
)

// ErrRunHeadCAS reports that an expected-old/state-scoped run-head transition
// lost its compare-and-swap. Callers must preserve any immutable Git anchor and
// retry from durable state; they must never roll a ref or database head back.
var ErrRunHeadCAS = errors.New("run head compare-and-swap lost")

// ActiveRunHeadAdvance is the exact authority tuple for a live pipeline head
// transition. The corresponding immutable Git anchor must already exist.
type ActiveRunHeadAdvance struct {
	RunID        string
	RepoID       string
	Branch       string
	StepName     string
	ExpectedHead string
	Candidate    string
	AnchorRef    string
}

// GetActiveRunHeadAdvance returns one durable live-transition journal row.
// It is intentionally keyed by both run and candidate because a long run may
// make several strict-forward step transitions.
func (d *DB) GetActiveRunHeadAdvance(runID, candidate string) (*ActiveRunHeadAdvance, error) {
	a := &ActiveRunHeadAdvance{}
	err := d.sql.QueryRow(
		`SELECT run_id, repo_id, branch, step_name, expected_head_sha, candidate_head_sha, anchor_ref
		 FROM run_head_advances WHERE run_id = ? AND candidate_head_sha = ?`,
		runID, candidate,
	).Scan(&a.RunID, &a.RepoID, &a.Branch, &a.StepName, &a.ExpectedHead, &a.Candidate, &a.AnchorRef)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get active run head advance: %w", err)
	}
	return a, nil
}

// AdvanceActiveRunHeadCAS journals and advances a live run head in one SQLite
// transaction. A retry is accepted only when the existing journal tuple is an
// exact match and the same run is still the sole latest active run.
func (d *DB) AdvanceActiveRunHeadCAS(a ActiveRunHeadAdvance) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return fmt.Errorf("advance active run head: begin transaction: %w", err)
	}
	defer tx.Rollback()

	var existing ActiveRunHeadAdvance
	err = tx.QueryRow(
		`SELECT run_id, repo_id, branch, step_name, expected_head_sha, candidate_head_sha, anchor_ref
		 FROM run_head_advances WHERE run_id = ? AND candidate_head_sha = ?`,
		a.RunID, a.Candidate,
	).Scan(&existing.RunID, &existing.RepoID, &existing.Branch, &existing.StepName, &existing.ExpectedHead, &existing.Candidate, &existing.AnchorRef)
	switch {
	case err == nil:
		if existing != a {
			return fmt.Errorf("advance active run head: conflicting durable journal: %w", ErrRunHeadCAS)
		}
		result, updateErr := tx.Exec(activeRunHeadCASUpdateSQL, a.Candidate, now(), a.RunID, a.RepoID, a.Branch, a.Candidate)
		if updateErr != nil {
			return fmt.Errorf("advance active run head: verify retry: %w", updateErr)
		}
		if rows, rowsErr := result.RowsAffected(); rowsErr != nil || rows != 1 {
			return fmt.Errorf("advance active run head: retry no longer eligible: %w", ErrRunHeadCAS)
		}
	case errors.Is(err, sql.ErrNoRows):
		if _, err := tx.Exec(
			`INSERT INTO run_head_advances
			 (run_id, repo_id, branch, step_name, expected_head_sha, candidate_head_sha, anchor_ref, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			a.RunID, a.RepoID, a.Branch, a.StepName, a.ExpectedHead, a.Candidate, a.AnchorRef, now(),
		); err != nil {
			return fmt.Errorf("advance active run head: insert journal: %w", err)
		}
		result, updateErr := tx.Exec(activeRunHeadCASUpdateSQL, a.Candidate, now(), a.RunID, a.RepoID, a.Branch, a.ExpectedHead)
		if updateErr != nil {
			return fmt.Errorf("advance active run head: update run: %w", updateErr)
		}
		if rows, rowsErr := result.RowsAffected(); rowsErr != nil || rows != 1 {
			return fmt.Errorf("advance active run head: %w", ErrRunHeadCAS)
		}
	default:
		return fmt.Errorf("advance active run head: read journal: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("advance active run head: commit: %w", err)
	}
	return nil
}

const activeRunHeadCASUpdateSQL = `
	UPDATE runs SET head_sha = ?, updated_at = ?
	 WHERE id = ? AND repo_id = ? AND branch = ? AND head_sha = ?
	   AND status = 'running' AND error IS NULL AND awaiting_agent_since IS NULL
	   AND custody_returned_at IS NULL AND COALESCE(push_active, 0) = 0
	   AND last_pushed_sha IS NULL AND push_target_kind IS NULL
	   AND push_target_fingerprint IS NULL AND push_ref IS NULL
	   AND last_pushed_at IS NULL AND push_generation IS NULL
	   AND pr_url IS NULL AND COALESCE(pr_state, 'none') = 'none' AND ci_ready_at IS NULL
	   AND NOT EXISTS (
		SELECT 1 FROM runs newer
		 WHERE newer.repo_id = runs.repo_id AND newer.branch = runs.branch
		   AND (newer.created_at > runs.created_at OR (newer.created_at = runs.created_at AND newer.id > runs.id))
	   )
	   AND NOT EXISTS (
		SELECT 1 FROM runs active
		 WHERE active.repo_id = runs.repo_id AND active.branch = runs.branch AND active.id <> runs.id
		   AND active.status IN ('pending', 'running')
	   )`

// RunHeadRecovery is the immutable audit tuple for one operator-authorized
// completed local-only recovery.
type RunHeadRecovery struct {
	RunID             string
	RepoID            string
	Branch            string
	BaseSHA           string
	ExpectedHeadSHA   string
	CandidateHeadSHA  string
	LocalHeadSHA      string
	AnchorRef         string
	ReviewApprovedSHA *string
	CreatedAt         int64
}

// GetRunHeadRecovery returns the durable recovery audit record for a run.
func (d *DB) GetRunHeadRecovery(runID string) (*RunHeadRecovery, error) {
	r := &RunHeadRecovery{}
	err := d.sql.QueryRow(
		`SELECT run_id, repo_id, branch, base_sha, expected_head_sha, candidate_head_sha, local_head_sha,
		        review_approved_head_sha, anchor_ref, created_at
		 FROM run_head_recoveries WHERE run_id = ?`, runID,
	).Scan(&r.RunID, &r.RepoID, &r.Branch, &r.BaseSHA, &r.ExpectedHeadSHA, &r.CandidateHeadSHA,
		&r.LocalHeadSHA, &r.ReviewApprovedSHA, &r.AnchorRef, &r.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get run head recovery: %w", err)
	}
	return r, nil
}

// AdoptCompletedRunHeadCAS atomically records the operator authorization and
// changes head_sha from the exact stale value to the exact candidate. Every
// mutable run/step authority predicate is repeated in the UPDATE; a preflight
// query is never treated as authorization.
func (d *DB) AdoptCompletedRunHeadCAS(a RunHeadRecovery) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return fmt.Errorf("adopt completed run head: begin transaction: %w", err)
	}
	defer tx.Rollback()

	var existing RunHeadRecovery
	err = tx.QueryRow(
		`SELECT run_id, repo_id, branch, base_sha, expected_head_sha, candidate_head_sha, local_head_sha,
		        review_approved_head_sha, anchor_ref, created_at
		 FROM run_head_recoveries WHERE run_id = ?`, a.RunID,
	).Scan(&existing.RunID, &existing.RepoID, &existing.Branch, &existing.BaseSHA, &existing.ExpectedHeadSHA,
		&existing.CandidateHeadSHA, &existing.LocalHeadSHA, &existing.ReviewApprovedSHA, &existing.AnchorRef, &existing.CreatedAt)
	if err == nil {
		if !sameRecoveryTuple(existing, a) {
			return fmt.Errorf("adopt completed run head: conflicting durable audit record: %w", ErrRunHeadCAS)
		}
		result, verifyErr := tx.Exec(completedRecoveryHeadCASUpdateSQL,
			a.CandidateHeadSHA, now(), a.RunID, a.RepoID, a.Branch, a.CandidateHeadSHA,
			a.BaseSHA, a.LocalHeadSHA, nullableStringPointer(a.ReviewApprovedSHA),
		)
		if verifyErr != nil {
			return fmt.Errorf("adopt completed run head: verify retry: %w", verifyErr)
		}
		if rows, rowsErr := result.RowsAffected(); rowsErr != nil || rows != 1 {
			return fmt.Errorf("adopt completed run head: retry no longer eligible: %w", ErrRunHeadCAS)
		}
	} else if errors.Is(err, sql.ErrNoRows) {
		createdAt := now()
		if _, err := tx.Exec(
			`INSERT INTO run_head_recoveries
			 (run_id, repo_id, branch, base_sha, expected_head_sha, candidate_head_sha, local_head_sha,
			  review_approved_head_sha, anchor_ref, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			a.RunID, a.RepoID, a.Branch, a.BaseSHA, a.ExpectedHeadSHA, a.CandidateHeadSHA,
			a.LocalHeadSHA, nullableStringPointer(a.ReviewApprovedSHA), a.AnchorRef, createdAt,
		); err != nil {
			return fmt.Errorf("adopt completed run head: insert audit record: %w", err)
		}
		result, updateErr := tx.Exec(completedRecoveryHeadCASUpdateSQL,
			a.CandidateHeadSHA, now(), a.RunID, a.RepoID, a.Branch, a.ExpectedHeadSHA,
			a.BaseSHA, a.LocalHeadSHA, nullableStringPointer(a.ReviewApprovedSHA),
		)
		if updateErr != nil {
			return fmt.Errorf("adopt completed run head: update run: %w", updateErr)
		}
		if rows, rowsErr := result.RowsAffected(); rowsErr != nil || rows != 1 {
			return fmt.Errorf("adopt completed run head: %w", ErrRunHeadCAS)
		}
	} else {
		return fmt.Errorf("adopt completed run head: read audit record: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("adopt completed run head: commit: %w", err)
	}
	return nil
}

// CompleteRunHeadRecoveryCAS is the only custody stamp accepted for an
// operator-authorized forward-head repair. The Git caller must first prove the
// local branch, gate and immutable anchor all equal CandidateHeadSHA while the
// daemon still owns the repo+branch lock.
func (d *DB) CompleteRunHeadRecoveryCAS(a RunHeadRecovery) (bool, error) {
	tx, err := d.sql.Begin()
	if err != nil {
		return false, fmt.Errorf("complete run head recovery: begin transaction: %w", err)
	}
	defer tx.Rollback()

	ts := now()
	result, err := tx.Exec(completedRecoveryCustodyCASUpdateSQL,
		ts, ts, a.RunID, a.RepoID, a.Branch, a.CandidateHeadSHA,
		a.BaseSHA, a.LocalHeadSHA, nullableStringPointer(a.ReviewApprovedSHA),
		a.BaseSHA, a.ExpectedHeadSHA, a.CandidateHeadSHA, a.LocalHeadSHA,
		nullableStringPointer(a.ReviewApprovedSHA), a.AnchorRef,
	)
	if err != nil {
		return false, fmt.Errorf("complete run head recovery: update run: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("complete run head recovery: affected rows: %w", err)
	}
	if rows == 0 {
		var count int
		err = tx.QueryRow(completedRecoveryAlreadyStampedSQL,
			a.RunID, a.RepoID, a.Branch, a.CandidateHeadSHA,
			a.BaseSHA, a.LocalHeadSHA, nullableStringPointer(a.ReviewApprovedSHA),
			a.BaseSHA, a.ExpectedHeadSHA, a.CandidateHeadSHA, a.LocalHeadSHA,
			nullableStringPointer(a.ReviewApprovedSHA), a.AnchorRef,
		).Scan(&count)
		if err != nil {
			return false, fmt.Errorf("complete run head recovery: verify retry: %w", err)
		}
		if count != 1 {
			return false, fmt.Errorf("complete run head recovery: %w", ErrRunHeadCAS)
		}
		if err := tx.Commit(); err != nil {
			return false, fmt.Errorf("complete run head recovery: commit retry: %w", err)
		}
		return false, nil
	}
	if rows != 1 {
		return false, fmt.Errorf("complete run head recovery: affected %d rows: %w", rows, ErrRunHeadCAS)
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("complete run head recovery: commit: %w", err)
	}
	return true, nil
}

const completedRecoveryHeadCASUpdateSQL = `
	UPDATE runs SET head_sha = ?, updated_at = ?
	 WHERE id = ? AND repo_id = ? AND branch = ? AND head_sha = ?
	   AND base_sha = ? AND submitted_head_sha = ?
	   AND review_approved_head_sha IS ?
	   AND ` + completedLocalOnlyRunPredicate

const completedRecoveryCustodyCASUpdateSQL = `
	UPDATE runs SET custody_returned_at = ?, updated_at = ?
	 WHERE id = ? AND repo_id = ? AND branch = ? AND head_sha = ?
	   AND base_sha = ? AND submitted_head_sha = ?
	   AND review_approved_head_sha IS ?
	   AND custody_returned_at IS NULL
	   AND EXISTS (
		SELECT 1 FROM run_head_recoveries recovery
		 WHERE recovery.run_id = runs.id AND recovery.repo_id = runs.repo_id AND recovery.branch = runs.branch
		   AND recovery.base_sha = ?
		   AND recovery.expected_head_sha = ? AND recovery.candidate_head_sha = ?
		   AND recovery.local_head_sha = ? AND recovery.review_approved_head_sha IS ? AND recovery.anchor_ref = ?
	   )
	   AND ` + completedLocalOnlyRunPredicateWithoutCustody

const completedRecoveryAlreadyStampedSQL = `
	SELECT COUNT(*) FROM runs
	 WHERE id = ? AND repo_id = ? AND branch = ? AND head_sha = ?
	   AND base_sha = ? AND submitted_head_sha = ?
	   AND review_approved_head_sha IS ?
	   AND custody_returned_at IS NOT NULL
	   AND EXISTS (
		SELECT 1 FROM run_head_recoveries recovery
		 WHERE recovery.run_id = runs.id AND recovery.repo_id = runs.repo_id AND recovery.branch = runs.branch
		   AND recovery.base_sha = ?
		   AND recovery.expected_head_sha = ? AND recovery.candidate_head_sha = ?
		   AND recovery.local_head_sha = ? AND recovery.review_approved_head_sha IS ? AND recovery.anchor_ref = ?
	   )
	   AND ` + completedLocalOnlyRunPredicateWithoutCustody

const completedLocalOnlyRunPredicate = `
	status = 'completed' AND error IS NULL AND awaiting_agent_since IS NULL
	AND custody_returned_at IS NULL
	AND ` + completedLocalOnlyRunPredicateWithoutCustody

const completedLocalOnlyRunPredicateWithoutCustody = `
	status = 'completed' AND error IS NULL AND awaiting_agent_since IS NULL
	AND COALESCE(push_active, 0) = 0
	AND last_pushed_sha IS NULL AND push_target_kind IS NULL
	AND push_target_fingerprint IS NULL AND push_ref IS NULL
	AND last_pushed_at IS NULL AND push_generation IS NULL
	AND pr_url IS NULL AND COALESCE(pr_state, 'none') = 'none'
	AND pr_state_observed_at IS NULL AND ci_ready_at IS NULL
	AND NOT EXISTS (
		SELECT 1 FROM runs newer
		 WHERE newer.repo_id = runs.repo_id AND newer.branch = runs.branch
		   AND (newer.created_at > runs.created_at OR (newer.created_at = runs.created_at AND newer.id > runs.id))
	)
	AND NOT EXISTS (
		SELECT 1 FROM runs active
		 WHERE active.repo_id = runs.repo_id AND active.branch = runs.branch AND active.id <> runs.id
		   AND active.status IN ('pending', 'running')
	)
	AND (SELECT COUNT(*) FROM step_results all_steps WHERE all_steps.run_id = runs.id) = 9
	AND (SELECT COUNT(DISTINCT all_steps.step_name) FROM step_results all_steps WHERE all_steps.run_id = runs.id) = 9
	AND (
		SELECT COUNT(*) FROM step_results validation
		 WHERE validation.run_id = runs.id
		   AND validation.step_name IN ('intent', 'rebase', 'review', 'test', 'document', 'lint')
		   AND validation.status = 'completed' AND validation.exit_code = 0
		   AND validation.started_at IS NOT NULL AND validation.completed_at IS NOT NULL
		   AND validation.error IS NULL
	) = 6
	AND (
		SELECT COUNT(*) FROM step_results publication
		 WHERE publication.run_id = runs.id
		   AND publication.step_name IN ('push', 'pr', 'ci')
		   AND publication.status = 'skipped' AND publication.exit_code = 0
		   AND publication.completed_at IS NOT NULL AND publication.error IS NULL
	) = 3`

func sameRecoveryTuple(a, b RunHeadRecovery) bool {
	return a.RunID == b.RunID && a.RepoID == b.RepoID && a.Branch == b.Branch &&
		a.BaseSHA == b.BaseSHA &&
		a.ExpectedHeadSHA == b.ExpectedHeadSHA && a.CandidateHeadSHA == b.CandidateHeadSHA &&
		a.LocalHeadSHA == b.LocalHeadSHA && sameOptionalString(a.ReviewApprovedSHA, b.ReviewApprovedSHA) &&
		a.AnchorRef == b.AnchorRef
}

func sameOptionalString(a, b *string) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

func nullableStringPointer(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

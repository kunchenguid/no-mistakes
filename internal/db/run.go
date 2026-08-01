package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// ErrRunCustodyCAS reports that the exact terminal run authority changed
// before custody could be stamped. Callers must retain immutable recovery
// anchors and retry from freshly inspected state.
var ErrRunCustodyCAS = errors.New("run custody compare-and-swap lost")
var ErrRunPublicationCAS = errors.New("run publication compare-and-swap lost")

const (
	CustodyPhasePreparing = "preparing"
	CustodyPhaseStaged    = "staged"
	CustodyPhaseGateMoved = "gate_moved"
	CustodyPhaseRestoring = "restoring"
	CustodyPhaseStamped   = "stamped"

	PublicationJournalReady     = "ready"
	PublicationJournalAttempted = "attempted"
)

type CustodyTransition struct {
	db     *DB
	runID  string
	token  string
	closed bool
}

func (t *CustodyTransition) Token() string {
	if t == nil {
		return ""
	}
	return t.token
}

func (t *CustodyTransition) Phase(ctx context.Context) (string, error) {
	if t == nil || t.db == nil || t.runID == "" || t.token == "" {
		return "", ErrRunCustodyCAS
	}
	var phase sql.NullString
	err := t.db.sql.QueryRowContext(ctx, `SELECT custody_transition_phase FROM runs WHERE id = ? AND custody_transition_token = ?`, t.runID, t.token).Scan(&phase)
	if err == sql.ErrNoRows || !phase.Valid || phase.String == "" {
		return "", ErrRunCustodyCAS
	}
	if err != nil {
		return "", fmt.Errorf("read run custody transition phase: %w", err)
	}
	return phase.String, nil
}

func (t *CustodyTransition) Advance(ctx context.Context, from, to string) error {
	if t == nil || t.db == nil || t.runID == "" || t.token == "" || from == "" || to == "" {
		return ErrRunCustodyCAS
	}
	result, err := t.db.sql.ExecContext(
		ctx,
		`UPDATE runs SET custody_transition_phase = ?, updated_at = ?
		 WHERE id = ? AND custody_transition_token = ? AND custody_transition_phase = ? AND custody_returned_at IS NULL`,
		to, now(), t.runID, t.token, from,
	)
	if err != nil {
		return fmt.Errorf("advance run custody transition: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("advance run custody transition: affected rows: %w", err)
	}
	if rows != 1 {
		return ErrRunCustodyCAS
	}
	return nil
}

func (t *CustodyTransition) BeginRestore(ctx context.Context) error {
	return t.Advance(ctx, CustodyPhaseGateMoved, CustodyPhaseRestoring)
}

func (t *CustodyTransition) FinishRestore(ctx context.Context) error {
	if t == nil || t.db == nil || t.runID == "" || t.token == "" {
		return ErrRunCustodyCAS
	}
	result, err := t.db.sql.ExecContext(ctx, `UPDATE runs SET custody_transition_token = NULL, custody_transition_phase = NULL, updated_at = ? WHERE id = ? AND custody_transition_token = ? AND custody_transition_phase = ? AND custody_returned_at IS NULL`, now(), t.runID, t.token, CustodyPhaseRestoring)
	if err != nil {
		return fmt.Errorf("finish run custody restore: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("finish run custody restore: affected rows: %w", err)
	}
	if rows != 1 {
		return ErrRunCustodyCAS
	}
	t.closed = true
	return nil
}

func (t *CustodyTransition) Complete(ctx context.Context, expected *Run) error {
	if t == nil || t.db == nil || expected == nil || t.runID == "" || t.token == "" {
		return ErrRunCustodyCAS
	}
	args := []any{now(), CustodyPhaseStamped, now()}
	args = append(args, custodyAuthorityArgs(expected)...)
	args = append(args, t.token, CustodyPhaseGateMoved, PublicationJournalReady, nullableRunString(expected.Error), nullableRunInt64(expected.AwaitingAgentSince))
	result, err := t.db.sql.ExecContext(
		ctx,
		`UPDATE runs SET custody_returned_at = ?, custody_transition_phase = ?, updated_at = ?
		 WHERE `+custodyAuthorityPredicate+`
		   AND custody_returned_at IS NULL AND custody_transition_token = ? AND custody_transition_phase = ?
		   AND COALESCE(push_active, 0) = 0
		   AND publication_journal_state = ? AND publication_journal_target_kind IS NOT NULL AND publication_journal_target_fingerprint IS NOT NULL AND publication_journal_ref IS NOT NULL AND publication_journal_target_version IS NOT NULL
		   AND publication_attempt_head_sha IS NULL AND publication_attempt_target_kind IS NULL AND publication_attempt_target_fingerprint IS NULL AND publication_attempt_ref IS NULL
		   AND error IS ? AND awaiting_agent_since IS ?`,
		args...,
	)
	if err != nil {
		return fmt.Errorf("complete run custody transition: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("complete run custody transition: affected rows: %w", err)
	}
	if rows != 1 {
		return ErrRunCustodyCAS
	}
	return nil
}

func (t *CustodyTransition) Release() error {
	if t == nil || t.db == nil || t.closed {
		return nil
	}
	result, err := t.db.sql.Exec(`UPDATE runs SET custody_transition_token = NULL, custody_transition_phase = NULL, updated_at = ? WHERE id = ? AND custody_transition_token = ? AND custody_returned_at IS NULL AND custody_transition_phase IN (?, ?)`, now(), t.runID, t.token, CustodyPhasePreparing, CustodyPhaseStaged)
	if err != nil {
		return fmt.Errorf("release run custody transition: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("release run custody transition: affected rows: %w", err)
	}
	if rows != 1 {
		current, getErr := t.db.GetRun(t.runID)
		if getErr == nil && current != nil && current.CustodyTransitionToken == nil {
			t.closed = true
			return nil
		}
		return ErrRunCustodyCAS
	}
	t.closed = true
	return nil
}

func (t *CustodyTransition) ReleaseStamped(ctx context.Context, runID string) error {
	if t == nil || t.db == nil || t.token == "" {
		return nil
	}
	if runID == "" {
		runID = t.runID
	}
	return t.db.ClearRunCustodyTransition(ctx, runID, t.token)
}

// Run represents a pipeline run.
type Run struct {
	ID                   string
	RepoID               string
	Branch               string
	HeadSHA              string
	BaseSHA              string
	SubmittedHeadSHA     *string
	ReceiveReservationID *string
	// ReviewApprovedHeadSHA is the exact commit approved by the last
	// successfully completed full review. It is nil for legacy runs and until
	// review completes; mutable run/worktree heads never infer this authority.
	ReviewApprovedHeadSHA               *string
	Status                              types.RunStatus
	PRURL                               *string
	PRState                             *string
	PRStateObservedAt                   *int64
	CIReadyAt                           *int64
	CIReadyNoCI                         bool
	LastPushedSHA                       *string
	PushTargetKind                      *string
	PushTargetFingerprint               *string
	PushRef                             *string
	LastPushedAt                        *int64
	PushGeneration                      *int64
	PushActive                          bool
	TerminalHeadVerifiedAt              *int64
	PublicationJournalState             *string
	PublicationJournalTargetKind        *string
	PublicationJournalTargetFingerprint *string
	PublicationJournalRef               *string
	PublicationJournalTargetVersion     *int64
	PublicationAttemptHeadSHA           *string
	PublicationAttemptTargetKind        *string
	PublicationAttemptTargetFingerprint *string
	PublicationAttemptRef               *string
	// CustodyReturnedAt is non-nil once a guarded branch-sync recovery
	// explicitly ended this run's ownership of an unpublished pipeline head
	// (terminal run whose head was never successfully pushed, or moved after
	// the last push). It never changes push provenance; it only records that
	// the operator worktree took the branch back.
	CustodyReturnedAt      *int64
	CustodyTransitionToken *string
	CustodyTransitionPhase *string
	Error                  *string
	// AwaitingAgentSince is the unix-seconds timestamp at which the run parked
	// at a gate awaiting the driving agent's response (an awaiting_approval or
	// fix_review step). It is nil whenever the run is not parked: the executor
	// sets it on gate entry and clears it the moment the agent responds (or the
	// wait is cancelled). It is observability only and does not affect gate
	// resolution.
	AwaitingAgentSince *int64
	// ParkedMS accumulates the run's total parked-at-gate wall time in
	// milliseconds across every gate wait (local performance telemetry;
	// step duration_ms values exclude this time).
	ParkedMS        int64
	Intent          *string
	IntentSource    *string
	IntentSessionID *string
	IntentScore     *float64
	CreatedAt       int64
	UpdatedAt       int64
}

const runColumns = `id, repo_id, branch, head_sha, base_sha, submitted_head_sha, receive_reservation_id, review_approved_head_sha, status, pr_url, pr_state, pr_state_observed_at, ci_ready_at, COALESCE(ci_ready_no_ci, 0), last_pushed_sha, push_target_kind, push_target_fingerprint, push_ref, last_pushed_at, push_generation, COALESCE(push_active, 0), terminal_head_verified_at, publication_journal_state, publication_journal_target_kind, publication_journal_target_fingerprint, publication_journal_ref, publication_journal_target_version, publication_attempt_head_sha, publication_attempt_target_kind, publication_attempt_target_fingerprint, publication_attempt_ref, custody_returned_at, custody_transition_token, custody_transition_phase, error, awaiting_agent_since, COALESCE(parked_ms, 0), intent, intent_source, intent_session_id, intent_score, created_at, updated_at`

const custodyAuthorityPredicate = `id = ? AND repo_id = ? AND branch = ? AND head_sha = ? AND base_sha = ?
		   AND submitted_head_sha IS ? AND review_approved_head_sha IS ? AND status = ?
		   AND pr_url IS ? AND pr_state IS ? AND pr_state_observed_at IS ? AND ci_ready_at IS ?
		   AND last_pushed_sha IS ? AND push_target_kind IS ? AND push_target_fingerprint IS ?
		   AND push_ref IS ? AND last_pushed_at IS ? AND push_generation IS ?
		   AND publication_journal_state IS ? AND publication_journal_target_kind IS ? AND publication_journal_target_fingerprint IS ? AND publication_journal_ref IS ?
		   AND publication_journal_target_version IS ?
		   AND publication_attempt_head_sha IS ? AND publication_attempt_target_kind IS ? AND publication_attempt_target_fingerprint IS ? AND publication_attempt_ref IS ?
		   AND EXISTS (SELECT 1 FROM repos current_repo WHERE current_repo.id = runs.repo_id AND COALESCE(current_repo.url_version, 0) = ?)
		   AND status IN ('completed', 'failed', 'cancelled')
		   AND NOT EXISTS (
			SELECT 1 FROM runs newer
			 WHERE newer.repo_id = runs.repo_id AND newer.branch = runs.branch
			   AND (newer.created_at > runs.created_at OR (newer.created_at = runs.created_at AND newer.id > runs.id))
		   )`

func scanRun(row interface {
	Scan(...any) error
}, r *Run) error {
	return row.Scan(
		&r.ID, &r.RepoID, &r.Branch, &r.HeadSHA, &r.BaseSHA, &r.SubmittedHeadSHA, &r.ReceiveReservationID, &r.ReviewApprovedHeadSHA, &r.Status,
		&r.PRURL, &r.PRState, &r.PRStateObservedAt, &r.CIReadyAt, &r.CIReadyNoCI,
		&r.LastPushedSHA, &r.PushTargetKind, &r.PushTargetFingerprint, &r.PushRef,
		&r.LastPushedAt, &r.PushGeneration, &r.PushActive, &r.TerminalHeadVerifiedAt,
		&r.PublicationJournalState, &r.PublicationJournalTargetKind, &r.PublicationJournalTargetFingerprint, &r.PublicationJournalRef,
		&r.PublicationJournalTargetVersion,
		&r.PublicationAttemptHeadSHA, &r.PublicationAttemptTargetKind, &r.PublicationAttemptTargetFingerprint, &r.PublicationAttemptRef,
		&r.CustodyReturnedAt, &r.CustodyTransitionToken, &r.CustodyTransitionPhase, &r.Error, &r.AwaitingAgentSince, &r.ParkedMS,
		&r.Intent, &r.IntentSource, &r.IntentSessionID, &r.IntentScore,
		&r.CreatedAt, &r.UpdatedAt,
	)
}

// InsertRun creates a new run record.
func (d *DB) InsertRun(repoID, branch, headSHA, baseSHA string) (*Run, error) {
	return d.InsertRunWithIntentAndReceiveReservation(repoID, branch, headSHA, baseSHA, nil, "")
}

func (d *DB) InsertRunWithIntent(repoID, branch, headSHA, baseSHA string, intent *RunIntent) (*Run, error) {
	return d.InsertRunWithIntentAndReceiveReservation(repoID, branch, headSHA, baseSHA, intent, "")
}

func (d *DB) InsertRunWithIntentAndReceiveReservation(repoID, branch, headSHA, baseSHA string, intent *RunIntent, reservationID string) (*Run, error) {
	ts := now()
	repo, err := d.GetRepo(repoID)
	if err != nil {
		return nil, fmt.Errorf("load run publication target: %w", err)
	}
	r := &Run{
		ID:               newID(),
		RepoID:           repoID,
		Branch:           branch,
		HeadSHA:          headSHA,
		BaseSHA:          baseSHA,
		SubmittedHeadSHA: &headSHA,
		Status:           types.RunPending,
		CreatedAt:        ts,
		UpdatedAt:        ts,
	}
	if reservationID = strings.TrimSpace(reservationID); reservationID != "" {
		r.ReceiveReservationID = &reservationID
	}
	if repo != nil {
		journalState := PublicationJournalReady
		journalKind := publicationTargetKind(repo)
		journalFingerprint := publicationTargetFingerprint(repo.PushURL())
		journalRef := publicationRef(branch)
		journalVersion := repo.URLVersion
		r.PublicationJournalState = &journalState
		r.PublicationJournalTargetKind = &journalKind
		r.PublicationJournalTargetFingerprint = &journalFingerprint
		r.PublicationJournalRef = &journalRef
		r.PublicationJournalTargetVersion = &journalVersion
	}
	if intent != nil {
		r.Intent = &intent.Summary
		r.IntentSource = &intent.Source
		r.IntentSessionID = &intent.SessionID
		r.IntentScore = &intent.Score
	}
	_, err = d.sql.Exec(
		`INSERT INTO runs (id, repo_id, branch, head_sha, base_sha, submitted_head_sha, receive_reservation_id, status, pr_state, publication_journal_state, publication_journal_target_kind, publication_journal_target_fingerprint, publication_journal_ref, publication_journal_target_version, intent, intent_source, intent_session_id, intent_score, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'none', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.RepoID, r.Branch, r.HeadSHA, r.BaseSHA, headSHA, r.ReceiveReservationID, r.Status, r.PublicationJournalState, r.PublicationJournalTargetKind, r.PublicationJournalTargetFingerprint, r.PublicationJournalRef, r.PublicationJournalTargetVersion, r.Intent, r.IntentSource, r.IntentSessionID, r.IntentScore, r.CreatedAt, r.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("insert run: %w", err)
	}
	return r, nil
}

func (d *DB) GetRunByReceiveReservation(reservationID string) (*Run, error) {
	if strings.TrimSpace(reservationID) == "" {
		return nil, nil
	}
	r := &Run{}
	err := scanRun(d.sql.QueryRow(`SELECT `+runColumns+` FROM runs WHERE receive_reservation_id = ?`, reservationID), r)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get run by receive reservation: %w", err)
	}
	return r, nil
}

func publicationTargetKind(repo *Repo) string {
	if repo != nil && strings.TrimSpace(repo.ForkURL) != "" {
		return "fork"
	}
	return "upstream"
}

func publicationRef(branch string) string {
	branch = strings.TrimPrefix(strings.TrimSpace(branch), "refs/heads/")
	return "refs/heads/" + branch
}

func publicationTargetFingerprint(raw string) string {
	sum := sha256.Sum256([]byte(publicationCanonicalTarget(raw)))
	return hex.EncodeToString(sum[:])
}

func publicationCanonicalTarget(raw string) string {
	raw = strings.TrimSpace(raw)
	parsed, err := url.Parse(raw)
	if err == nil && parsed.Scheme != "" {
		if parsed.Scheme == "http" || parsed.Scheme == "https" {
			parsed.User = nil
			parsed.Scheme = strings.ToLower(parsed.Scheme)
			parsed.Host = strings.ToLower(parsed.Host)
		}
		parsed.Fragment = ""
		return strings.TrimSuffix(parsed.String(), "/")
	}
	return strings.TrimSuffix(raw, "/")
}

// GetRun returns a run by ID.
func (d *DB) GetRun(id string) (*Run, error) {
	r := &Run{}
	err := scanRun(d.sql.QueryRow(`SELECT `+runColumns+` FROM runs WHERE id = ?`, id), r)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get run: %w", err)
	}
	return r, nil
}

// GetRunsByRepo returns all runs for a repo, newest first.
func (d *DB) GetRunsByRepo(repoID string) ([]*Run, error) {
	rows, err := d.sql.Query(`SELECT `+runColumns+` FROM runs WHERE repo_id = ? ORDER BY created_at DESC, id DESC`, repoID)
	if err != nil {
		return nil, fmt.Errorf("get runs by repo: %w", err)
	}
	defer rows.Close()
	var runs []*Run
	for rows.Next() {
		r := &Run{}
		if err := scanRun(rows, r); err != nil {
			return nil, fmt.Errorf("scan run: %w", err)
		}
		runs = append(runs, r)
	}
	return runs, rows.Err()
}

// GetRunsByRepoHead returns the runs for a repo matching an exact branch and
// head SHA, newest first. It lets a caller detect the run created by a specific
// push without scanning (and rebuilding step data for) the repo's entire run
// history, so the cost stays bounded to the handful of runs for one head.
func (d *DB) GetRunsByRepoHead(repoID, branch, headSHA string) ([]*Run, error) {
	rows, err := d.sql.Query(
		`SELECT `+runColumns+` FROM runs WHERE repo_id = ? AND branch = ? AND head_sha = ? ORDER BY created_at DESC, id DESC`,
		repoID, branch, headSHA,
	)
	if err != nil {
		return nil, fmt.Errorf("get runs by repo head: %w", err)
	}
	defer rows.Close()
	var runs []*Run
	for rows.Next() {
		r := &Run{}
		if err := scanRun(rows, r); err != nil {
			return nil, fmt.Errorf("scan run: %w", err)
		}
		runs = append(runs, r)
	}
	return runs, rows.Err()
}

// GetActiveRun returns the currently active run (pending or running) for a repo,
// if any. When branch is non-empty, only a run on that exact branch is returned
// - the setup wizard relies on this to decide whether a new run is needed for
// the current branch. When branch is empty, returns the most recently created
// active run across any branch.
func (d *DB) GetActiveRun(repoID, branch string) (*Run, error) {
	r := &Run{}
	var err error
	if branch == "" {
		err = scanRun(d.sql.QueryRow(
			`SELECT `+runColumns+` FROM runs WHERE repo_id = ? AND status IN ('pending', 'running') ORDER BY created_at DESC, id DESC LIMIT 1`, repoID,
		), r)
	} else {
		err = scanRun(d.sql.QueryRow(
			`SELECT `+runColumns+` FROM runs WHERE repo_id = ? AND branch = ? AND status IN ('pending', 'running') ORDER BY created_at DESC, id DESC LIMIT 1`, repoID, branch,
		), r)
	}
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get active run: %w", err)
	}
	return r, nil
}

// GetActiveRuns returns all pending or running runs across all repos, newest first.
func (d *DB) GetActiveRuns() ([]*Run, error) {
	rows, err := d.sql.Query(
		`SELECT `+runColumns+` FROM runs WHERE status IN (?, ?) ORDER BY created_at DESC, id DESC`,
		types.RunPending, types.RunRunning,
	)
	if err != nil {
		return nil, fmt.Errorf("get active runs: %w", err)
	}
	defer rows.Close()

	var runs []*Run
	for rows.Next() {
		r := &Run{}
		if err := scanRun(rows, r); err != nil {
			return nil, fmt.Errorf("scan run: %w", err)
		}
		runs = append(runs, r)
	}
	return runs, rows.Err()
}

// UpdateRunStatus updates a run's status and updated_at timestamp.
func (d *DB) UpdateRunStatus(id string, status types.RunStatus) error {
	_, err := d.sql.Exec(`UPDATE runs SET status = ?, push_active = CASE WHEN ? IN ('completed', 'failed', 'cancelled') THEN 0 ELSE push_active END, terminal_head_verified_at = NULL, updated_at = ? WHERE id = ? AND custody_returned_at IS NULL AND custody_transition_token IS NULL AND custody_transition_phase IS NULL`, status, status, now(), id)
	if err != nil {
		return fmt.Errorf("update run status: %w", err)
	}
	return nil
}

// UpdateRunPRURL sets the PR URL on a run. A delayed PR-step write must not
// regress terminal lifecycle truth already observed by the CI monitor.
func (d *DB) UpdateRunPRURL(id, prURL string) error {
	ts := now()
	result, err := d.sql.Exec(`UPDATE runs SET pr_url = ?, pr_state = CASE WHEN pr_state IN ('merged', 'closed') THEN pr_state ELSE 'open' END, pr_state_observed_at = ?, updated_at = ? WHERE id = ? AND custody_returned_at IS NULL AND custody_transition_token IS NULL AND custody_transition_phase IS NULL`, prURL, ts, ts, id)
	if err != nil {
		return fmt.Errorf("update run pr url: %w", err)
	}
	if rows, err := result.RowsAffected(); err != nil {
		return fmt.Errorf("update run pr url: affected rows: %w", err)
	} else if rows != 1 {
		return ErrRunCustodyCAS
	}
	return nil
}

// PushBinding records the exact target and commit proven by a successful
// pipeline-owned push. TargetFingerprint is a one-way digest and must never be
// a raw URL.
type PushBinding struct {
	HeadSHA           string
	TargetKind        string
	TargetFingerprint string
	Ref               string
}

type PublicationAttempt struct {
	HeadSHA           string
	TargetKind        string
	TargetFingerprint string
	Ref               string
}

func (d *DB) RecordRunPublicationAttempt(id string, attempt PublicationAttempt) error {
	if attempt.HeadSHA == "" || attempt.TargetKind == "" || attempt.TargetFingerprint == "" || attempt.Ref == "" {
		return ErrRunPublicationCAS
	}
	result, err := d.sql.Exec(`UPDATE runs SET publication_journal_state = ?, publication_attempt_head_sha = ?, publication_attempt_target_kind = ?, publication_attempt_target_fingerprint = ?, publication_attempt_ref = ?, updated_at = ?
		WHERE id = ? AND custody_returned_at IS NULL AND custody_transition_token IS NULL AND custody_transition_phase IS NULL
		AND publication_journal_state = ? AND publication_journal_target_kind = ? AND publication_journal_target_fingerprint = ? AND publication_journal_ref = ?
		AND publication_journal_target_version IS NOT NULL
		AND publication_attempt_head_sha IS NULL AND publication_attempt_target_kind IS NULL AND publication_attempt_target_fingerprint IS NULL AND publication_attempt_ref IS NULL`,
		PublicationJournalAttempted, attempt.HeadSHA, attempt.TargetKind, attempt.TargetFingerprint, attempt.Ref, now(), id,
		PublicationJournalReady, attempt.TargetKind, attempt.TargetFingerprint, attempt.Ref)
	if err != nil {
		return fmt.Errorf("record run publication attempt: %w", err)
	}
	if rows, err := result.RowsAffected(); err != nil {
		return fmt.Errorf("record run publication attempt: affected rows: %w", err)
	} else if rows == 1 {
		return nil
	}
	current, getErr := d.GetRun(id)
	if getErr != nil {
		return getErr
	}
	if current != nil && samePublicationAttempt(current, attempt) {
		return nil
	}
	if current != nil && (current.CustodyReturnedAt != nil || current.CustodyTransitionToken != nil || current.CustodyTransitionPhase != nil) {
		return ErrRunCustodyCAS
	}
	return ErrRunPublicationCAS
}

func (d *DB) ReconcileRunPublicationAttempt(id string) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return fmt.Errorf("begin publication reconciliation: %w", err)
	}
	defer tx.Rollback()

	var journalState, journalKind, journalFingerprint, journalRef sql.NullString
	var journalVersion sql.NullInt64
	var attemptHead, attemptKind, attemptFingerprint, attemptRef sql.NullString
	var pushedHead, pushedKind, pushedFingerprint, pushedRef sql.NullString
	if err := tx.QueryRow(`SELECT publication_journal_state, publication_journal_target_kind, publication_journal_target_fingerprint, publication_journal_ref, publication_journal_target_version, publication_attempt_head_sha, publication_attempt_target_kind, publication_attempt_target_fingerprint, publication_attempt_ref, last_pushed_sha, push_target_kind, push_target_fingerprint, push_ref FROM runs WHERE id = ?`, id).Scan(
		&journalState, &journalKind, &journalFingerprint, &journalRef, &journalVersion, &attemptHead, &attemptKind, &attemptFingerprint, &attemptRef, &pushedHead, &pushedKind, &pushedFingerprint, &pushedRef,
	); err == sql.ErrNoRows {
		return ErrRunPublicationCAS
	} else if err != nil {
		return fmt.Errorf("read publication reconciliation: %w", err)
	}
	if !attemptHead.Valid && !attemptKind.Valid && !attemptFingerprint.Valid && !attemptRef.Valid {
		if journalState.Valid && journalState.String != PublicationJournalReady {
			return ErrRunPublicationCAS
		}
		return nil
	}
	if !journalState.Valid || journalState.String != PublicationJournalAttempted || !journalKind.Valid || !journalFingerprint.Valid || !journalRef.Valid || !journalVersion.Valid ||
		!attemptHead.Valid || !attemptKind.Valid || !attemptFingerprint.Valid || !attemptRef.Valid ||
		journalKind.String != attemptKind.String || journalFingerprint.String != attemptFingerprint.String || journalRef.String != attemptRef.String ||
		!pushedHead.Valid || !pushedKind.Valid || !pushedFingerprint.Valid || !pushedRef.Valid ||
		attemptHead.String != pushedHead.String || attemptKind.String != pushedKind.String || attemptFingerprint.String != pushedFingerprint.String || attemptRef.String != pushedRef.String {
		return ErrRunPublicationCAS
	}
	result, err := tx.Exec(`UPDATE runs SET publication_journal_state = ?, publication_attempt_head_sha = NULL, publication_attempt_target_kind = NULL, publication_attempt_target_fingerprint = NULL, publication_attempt_ref = NULL, updated_at = ?
		WHERE id = ? AND publication_attempt_head_sha = ? AND publication_attempt_target_kind = ? AND publication_attempt_target_fingerprint = ? AND publication_attempt_ref = ?`,
		PublicationJournalReady, now(), id, attemptHead.String, attemptKind.String, attemptFingerprint.String, attemptRef.String)
	if err != nil {
		return fmt.Errorf("clear reconciled publication attempt: %w", err)
	}
	if rows, err := result.RowsAffected(); err != nil {
		return fmt.Errorf("clear reconciled publication attempt: affected rows: %w", err)
	} else if rows != 1 {
		return ErrRunPublicationCAS
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit publication reconciliation: %w", err)
	}
	return nil
}

func samePublicationAttempt(run *Run, attempt PublicationAttempt) bool {
	return run != nil && run.PublicationAttemptHeadSHA != nil && run.PublicationAttemptTargetKind != nil && run.PublicationAttemptTargetFingerprint != nil && run.PublicationAttemptRef != nil &&
		*run.PublicationAttemptHeadSHA == attempt.HeadSHA && *run.PublicationAttemptTargetKind == attempt.TargetKind && *run.PublicationAttemptTargetFingerprint == attempt.TargetFingerprint && *run.PublicationAttemptRef == attempt.Ref
}

// UpdateRunPushBinding advances a run's successful-push provenance and
// increments its generation. It is called for both a completed push and a
// freshly verified already-up-to-date push.
func (d *DB) UpdateRunPushBinding(id string, binding PushBinding) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return fmt.Errorf("begin update run push binding: %w", err)
	}
	defer tx.Rollback()
	var journalState, journalKind, journalFingerprint, journalRef sql.NullString
	var journalVersion sql.NullInt64
	var attemptHead, attemptKind, attemptFingerprint, attemptRef sql.NullString
	var custodyReturned sql.NullInt64
	var transitionToken, transitionPhase sql.NullString
	if err := tx.QueryRow(`SELECT publication_journal_state, publication_journal_target_kind, publication_journal_target_fingerprint, publication_journal_ref, publication_journal_target_version, publication_attempt_head_sha, publication_attempt_target_kind, publication_attempt_target_fingerprint, publication_attempt_ref, custody_returned_at, custody_transition_token, custody_transition_phase FROM runs WHERE id = ?`, id).Scan(
		&journalState, &journalKind, &journalFingerprint, &journalRef, &journalVersion, &attemptHead, &attemptKind, &attemptFingerprint, &attemptRef, &custodyReturned, &transitionToken, &transitionPhase,
	); err == sql.ErrNoRows {
		return ErrRunCustodyCAS
	} else if err != nil {
		return fmt.Errorf("read run push binding authority: %w", err)
	}
	if custodyReturned.Valid || transitionToken.Valid || transitionPhase.Valid {
		return ErrRunCustodyCAS
	}
	if journalState.Valid && journalState.String == PublicationJournalAttempted && (!journalVersion.Valid || !attemptHead.Valid || !attemptKind.Valid || !attemptFingerprint.Valid || !attemptRef.Valid) {
		return ErrRunPublicationCAS
	}
	journalPresent := attemptHead.Valid || attemptKind.Valid || attemptFingerprint.Valid || attemptRef.Valid
	if journalPresent && (!attemptHead.Valid || !attemptKind.Valid || !attemptFingerprint.Valid || !attemptRef.Valid || attemptHead.String != binding.HeadSHA || attemptKind.String != binding.TargetKind || attemptFingerprint.String != binding.TargetFingerprint || attemptRef.String != binding.Ref) {
		return ErrRunPublicationCAS
	}
	ts := now()
	result, err := tx.Exec(
		`UPDATE runs SET last_pushed_sha = ?, push_target_kind = ?, push_target_fingerprint = ?, push_ref = ?, last_pushed_at = ?, push_generation = COALESCE(push_generation, 0) + 1, publication_journal_state = ?, publication_journal_target_kind = ?, publication_journal_target_fingerprint = ?, publication_journal_ref = ?, publication_attempt_head_sha = NULL, publication_attempt_target_kind = NULL, publication_attempt_target_fingerprint = NULL, publication_attempt_ref = NULL, updated_at = ? WHERE id = ? AND custody_returned_at IS NULL AND custody_transition_token IS NULL AND custody_transition_phase IS NULL`,
		binding.HeadSHA, binding.TargetKind, binding.TargetFingerprint, binding.Ref, ts, PublicationJournalReady, binding.TargetKind, binding.TargetFingerprint, binding.Ref, ts, id,
	)
	if err != nil {
		return fmt.Errorf("update run push binding: %w", err)
	}
	if rows, err := result.RowsAffected(); err != nil {
		return fmt.Errorf("update run push binding: affected rows: %w", err)
	} else if rows != 1 {
		return ErrRunCustodyCAS
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit run push binding: %w", err)
	}
	return nil
}

// SetRunCustodyReturnedCAS stamps custody only while the complete authority
// tuple observed by recovery is unchanged and the run is still the newest run
// for its repository branch. This is the durable half of the recovery's final
// Git-proof/database boundary; a concurrent run or publication write loses the
// CAS instead of stamping stale authority.
func (d *DB) SetRunCustodyReturnedCAS(expected *Run) error {
	if expected == nil {
		return ErrRunCustodyCAS
	}
	ts := now()
	args := append([]any{ts, ts}, custodyAuthorityArgs(expected)...)
	result, err := d.sql.Exec(`
		UPDATE runs SET custody_returned_at = ?, updated_at = ?
			 WHERE `+custodyAuthorityPredicate+`
		   AND COALESCE(push_active, 0) = 0 AND custody_returned_at IS NULL AND custody_transition_token IS NULL AND custody_transition_phase IS NULL
		   AND publication_journal_state = ? AND publication_journal_target_kind IS NOT NULL AND publication_journal_target_fingerprint IS NOT NULL AND publication_journal_ref IS NOT NULL AND publication_journal_target_version IS NOT NULL
		   AND publication_attempt_head_sha IS NULL AND publication_attempt_target_kind IS NULL AND publication_attempt_target_fingerprint IS NULL AND publication_attempt_ref IS NULL
		   AND error IS ? AND awaiting_agent_since IS ?`,
		append(args, PublicationJournalReady, nullableRunString(expected.Error), nullableRunInt64(expected.AwaitingAgentSince))...,
	)
	if err != nil {
		return fmt.Errorf("set run custody returned CAS: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("set run custody returned CAS: affected rows: %w", err)
	}
	if rows != 1 {
		return ErrRunCustodyCAS
	}
	return nil
}

func (d *DB) BeginRunCustodyTransition(ctx context.Context, expected *Run) (*CustodyTransition, error) {
	if expected == nil {
		return nil, ErrRunCustodyCAS
	}
	token := newID()
	args := []any{token, CustodyPhasePreparing, now()}
	args = append(args, custodyAuthorityArgs(expected)...)
	args = append(args, PublicationJournalReady, nullableRunString(expected.Error), nullableRunInt64(expected.AwaitingAgentSince))
	result, err := d.sql.ExecContext(
		ctx,
		`UPDATE runs SET custody_transition_token = ?, custody_transition_phase = ?, updated_at = ? WHERE `+custodyAuthorityPredicate+`
		   AND custody_returned_at IS NULL AND custody_transition_token IS NULL
		   AND custody_transition_phase IS NULL AND COALESCE(push_active, 0) = 0
		   AND publication_journal_state = ? AND publication_journal_target_kind IS NOT NULL AND publication_journal_target_fingerprint IS NOT NULL AND publication_journal_ref IS NOT NULL AND publication_journal_target_version IS NOT NULL
		   AND publication_attempt_head_sha IS NULL AND publication_attempt_target_kind IS NULL AND publication_attempt_target_fingerprint IS NULL AND publication_attempt_ref IS NULL
		   AND error IS ? AND awaiting_agent_since IS ?`,
		args...,
	)
	if err != nil {
		return nil, fmt.Errorf("begin run custody transition: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("begin run custody transition: affected rows: %w", err)
	}
	if rows != 1 {
		return nil, ErrRunCustodyCAS
	}
	return &CustodyTransition{db: d, runID: expected.ID, token: token}, nil
}

func (d *DB) ResumeRunCustodyTransition(ctx context.Context, expected *Run) (*CustodyTransition, error) {
	if expected == nil || expected.CustodyTransitionToken == nil || expected.CustodyTransitionPhase == nil || *expected.CustodyTransitionToken == "" || *expected.CustodyTransitionPhase == "" {
		return nil, ErrRunCustodyCAS
	}
	args := custodyAuthorityArgs(expected)
	args = append(args, *expected.CustodyTransitionToken, *expected.CustodyTransitionPhase, PublicationJournalReady, nullableRunString(expected.Error), nullableRunInt64(expected.AwaitingAgentSince))
	var token, phase string
	err := d.sql.QueryRowContext(ctx, `SELECT custody_transition_token, custody_transition_phase FROM runs WHERE `+custodyAuthorityPredicate+`
		AND custody_returned_at IS NULL AND custody_transition_token = ? AND custody_transition_phase = ?
		AND COALESCE(push_active, 0) = 0
		AND publication_journal_state = ? AND publication_journal_target_kind IS NOT NULL AND publication_journal_target_fingerprint IS NOT NULL AND publication_journal_ref IS NOT NULL AND publication_journal_target_version IS NOT NULL
		AND publication_attempt_head_sha IS NULL AND publication_attempt_target_kind IS NULL AND publication_attempt_target_fingerprint IS NULL AND publication_attempt_ref IS NULL
		AND error IS ? AND awaiting_agent_since IS ?`, args...).Scan(&token, &phase)
	if err == sql.ErrNoRows {
		return nil, ErrRunCustodyCAS
	}
	if err != nil {
		return nil, fmt.Errorf("resume run custody transition: %w", err)
	}
	return &CustodyTransition{db: d, runID: expected.ID, token: token}, nil
}

func (d *DB) ClearRunCustodyTransition(ctx context.Context, runID, token string) error {
	if runID == "" || token == "" {
		return nil
	}
	result, err := d.sql.ExecContext(ctx,
		`UPDATE runs SET custody_transition_token = NULL, custody_transition_phase = NULL, updated_at = ?
		 WHERE id = ? AND custody_transition_token = ? AND custody_transition_phase = ? AND custody_returned_at IS NOT NULL`,
		now(), runID, token, CustodyPhaseStamped,
	)
	if err != nil {
		return fmt.Errorf("clear run custody transition: %w", err)
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("clear run custody transition: affected rows: %w", err)
	}
	if rows != 1 {
		current, getErr := d.GetRun(runID)
		if getErr == nil && current != nil && current.CustodyReturnedAt != nil && current.CustodyTransitionToken == nil && current.CustodyTransitionPhase == nil {
			return nil
		}
		return ErrRunCustodyCAS
	}
	return nil
}

func custodyAuthorityArgs(expected *Run) []any {
	return []any{
		expected.ID, expected.RepoID, expected.Branch, expected.HeadSHA, expected.BaseSHA,
		nullableRunString(expected.SubmittedHeadSHA), nullableRunString(expected.ReviewApprovedHeadSHA), expected.Status,
		nullableRunString(expected.PRURL), nullableRunString(expected.PRState), nullableRunInt64(expected.PRStateObservedAt), nullableRunInt64(expected.CIReadyAt),
		nullableRunString(expected.LastPushedSHA), nullableRunString(expected.PushTargetKind), nullableRunString(expected.PushTargetFingerprint),
		nullableRunString(expected.PushRef), nullableRunInt64(expected.LastPushedAt), nullableRunInt64(expected.PushGeneration),
		nullableRunString(expected.PublicationJournalState), nullableRunString(expected.PublicationJournalTargetKind), nullableRunString(expected.PublicationJournalTargetFingerprint), nullableRunString(expected.PublicationJournalRef),
		nullableRunInt64(expected.PublicationJournalTargetVersion),
		nullableRunString(expected.PublicationAttemptHeadSHA), nullableRunString(expected.PublicationAttemptTargetKind), nullableRunString(expected.PublicationAttemptTargetFingerprint), nullableRunString(expected.PublicationAttemptRef),
		nullableRunInt64(expected.PublicationJournalTargetVersion),
	}
}

func nullableRunString(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullableRunInt64(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

// SetRunPushActive marks whether a pipeline phase currently owns a possible
// branch-head update. Sync refuses while this marker is set.
func (d *DB) SetRunPushActive(id string, active bool) error {
	where := `id = ?`
	args := []any{active, now(), id}
	if active {
		where += ` AND custody_returned_at IS NULL AND custody_transition_token IS NULL AND custody_transition_phase IS NULL`
	}
	result, err := d.sql.Exec(`UPDATE runs SET push_active = ?, updated_at = ? WHERE `+where, args...)
	if err != nil {
		return fmt.Errorf("set run push active: %w", err)
	}
	if active {
		if rows, err := result.RowsAffected(); err != nil {
			return fmt.Errorf("set run push active: affected rows: %w", err)
		} else if rows != 1 {
			return ErrRunCustodyCAS
		}
	}
	return nil
}

// UpdateRunPRState persists normalized lifecycle truth independently of logs.
// A merged or closed PR is also the terminal outcome of the final CI monitor
// step, so the PR observation and active-run finalization are committed in one
// transaction. This makes the database authoritative even if execution stops
// before the executor's ordinary follow-up completion write.
func (d *DB) UpdateRunPRState(id, state string) error {
	state = strings.ToLower(strings.TrimSpace(state))
	ts := now()
	tx, err := d.sql.Begin()
	if err != nil {
		return fmt.Errorf("update run PR state: begin transaction: %w", err)
	}
	defer tx.Rollback()

	var current, transitionToken, transitionPhase sql.NullString
	var custodyReturned sql.NullInt64
	if err := tx.QueryRow(`SELECT pr_state, custody_transition_token, custody_transition_phase, custody_returned_at FROM runs WHERE id = ?`, id).Scan(&current, &transitionToken, &transitionPhase, &custodyReturned); err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		return fmt.Errorf("update run PR state: read current state: %w", err)
	}
	if transitionToken.Valid || transitionPhase.Valid || custodyReturned.Valid {
		return ErrRunCustodyCAS
	}
	state = monotonicPRState(current.String, state)
	result, err := tx.Exec(`UPDATE runs SET pr_state = ?, pr_state_observed_at = ?, updated_at = ? WHERE id = ? AND custody_transition_token IS NULL AND custody_transition_phase IS NULL AND custody_returned_at IS NULL`, state, ts, ts, id)
	if err != nil {
		return fmt.Errorf("update run PR state: %w", err)
	}
	if rows, err := result.RowsAffected(); err != nil {
		return fmt.Errorf("update run PR state: affected rows: %w", err)
	} else if rows != 1 {
		return ErrRunCustodyCAS
	}
	if terminalPRState(state) {
		if err := finalizeTerminalPRRun(tx, id, ts); err != nil {
			return fmt.Errorf("update run PR state: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("update run PR state: commit: %w", err)
	}
	return nil
}

// ReconcileTerminalPRRuns repairs active rows written by an older or
// interrupted daemon after terminal PR truth became durable but before the
// separate run completion write. It is called during exclusive daemon startup
// before parked-run planning and generic crash recovery.
func (d *DB) ReconcileTerminalPRRuns() (int, error) {
	ts := now()
	tx, err := d.sql.Begin()
	if err != nil {
		return 0, fmt.Errorf("reconcile terminal PR runs: begin transaction: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.Query(`SELECT id FROM runs WHERE status IN (?, ?) AND pr_state IN ('merged', 'closed')`, types.RunPending, types.RunRunning)
	if err != nil {
		return 0, fmt.Errorf("reconcile terminal PR runs: list runs: %w", err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return 0, fmt.Errorf("reconcile terminal PR runs: scan run: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("reconcile terminal PR runs: close rows: %w", err)
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("reconcile terminal PR runs: list runs: %w", err)
	}

	for _, id := range ids {
		if err := finalizeTerminalPRRun(tx, id, ts); err != nil {
			return 0, fmt.Errorf("reconcile terminal PR runs: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("reconcile terminal PR runs: commit: %w", err)
	}
	return len(ids), nil
}

func monotonicPRState(current, observed string) string {
	current = strings.ToLower(strings.TrimSpace(current))
	observed = strings.ToLower(strings.TrimSpace(observed))
	switch {
	case current == "merged":
		return current
	case observed == "merged":
		return observed
	case current == "closed":
		return current
	default:
		return observed
	}
}

func terminalPRState(state string) bool {
	return state == "merged" || state == "closed"
}

func finalizeTerminalPRRun(tx *sql.Tx, id string, ts int64) error {
	if _, err := tx.Exec(
		`UPDATE step_results SET status = ?, exit_code = COALESCE(exit_code, 0), completed_at = COALESCE(completed_at, ?),
			last_activity_at = ?, last_activity = ?, agent_pid = NULL
		 WHERE run_id = ? AND step_name = ? AND status IN (?, ?, ?, ?)
		   AND EXISTS (SELECT 1 FROM runs WHERE id = ? AND status IN (?, ?))`,
		types.StepStatusCompleted, ts, ts, "status: completed", id, types.StepCI,
		types.StepStatusRunning, types.StepStatusAwaitingApproval, types.StepStatusFixing, types.StepStatusFixReview,
		id, types.RunPending, types.RunRunning,
	); err != nil {
		return fmt.Errorf("complete terminal CI step: %w", err)
	}
	if _, err := tx.Exec(
		`UPDATE runs SET
			status = CASE WHEN status IN (?, ?) THEN ? ELSE status END,
			push_active = 0,
			parked_ms = COALESCE(parked_ms, 0) + CASE
				WHEN awaiting_agent_since IS NOT NULL AND ? > awaiting_agent_since
				THEN (? - awaiting_agent_since) * 1000 ELSE 0 END,
			awaiting_agent_since = NULL, updated_at = ?
		 WHERE id = ?`,
		types.RunPending, types.RunRunning, types.RunCompleted, ts, ts, ts, id,
	); err != nil {
		return fmt.Errorf("finalize terminal PR run: %w", err)
	}
	return nil
}

// SetRunCIReady persists checks-passed readiness so fresh TUI and AXI attaches
// do not depend on receiving a historical log line.
func (d *DB) SetRunCIReady(id string, ready bool) error {
	return d.SetRunCIReadyWithReason(id, ready, false)
}

func (d *DB) SetRunCIReadyWithReason(id string, ready, declaredNoCI bool) error {
	readyValue := 0
	declaredValue := 0
	var readyAt any
	if ready {
		readyValue = 1
		readyAt = now()
		if declaredNoCI {
			declaredValue = 1
		}
	}
	_, err := d.sql.Exec(`UPDATE runs SET ci_ready_at = ?, ci_ready_no_ci = ?, updated_at = ? WHERE id = ? AND custody_returned_at IS NULL AND custody_transition_token IS NULL AND custody_transition_phase IS NULL AND ((ci_ready_at IS NULL AND ? = 1) OR (ci_ready_at IS NOT NULL AND ? = 0) OR (COALESCE(ci_ready_no_ci, 0) != ?))`, readyAt, declaredValue, now(), id, readyValue, readyValue, declaredValue)
	if err != nil {
		return fmt.Errorf("set run CI ready: %w", err)
	}
	return nil
}

// UpdateRunReviewApprovedHeadSHA replaces the run's review authority with the
// exact commit approved by the latest successfully completed full review.
func (d *DB) UpdateRunReviewApprovedHeadSHA(id, headSHA string) error {
	result, err := d.sql.Exec(`UPDATE runs SET review_approved_head_sha = ?, updated_at = ? WHERE id = ? AND custody_returned_at IS NULL AND custody_transition_token IS NULL AND custody_transition_phase IS NULL`, headSHA, now(), id)
	if err != nil {
		return fmt.Errorf("update run review-approved head sha: %w", err)
	}
	if rows, err := result.RowsAffected(); err != nil {
		return fmt.Errorf("update run review-approved head sha: affected rows: %w", err)
	} else if rows != 1 {
		return ErrRunCustodyCAS
	}
	return nil
}

// UpdateRunHeadSHA updates the run head SHA and timestamp.
func (d *DB) UpdateRunHeadSHA(id, headSHA string) error {
	result, err := d.sql.Exec(`UPDATE runs SET head_sha = ?, updated_at = ? WHERE id = ? AND custody_returned_at IS NULL AND custody_transition_token IS NULL AND custody_transition_phase IS NULL`, headSHA, now(), id)
	if err != nil {
		return fmt.Errorf("update run head sha: %w", err)
	}
	if rows, err := result.RowsAffected(); err != nil {
		return fmt.Errorf("update run head sha: affected rows: %w", err)
	} else if rows != 1 {
		return ErrRunCustodyCAS
	}
	return nil
}

// UpdateRunError sets the error message on a run.
func (d *DB) UpdateRunError(id, errMsg string) error {
	return d.UpdateRunErrorStatus(id, errMsg, types.RunFailed)
}

// UpdateRunErrorStatus sets the error message and terminal status on a run.
func (d *DB) UpdateRunErrorStatus(id, errMsg string, status types.RunStatus) error {
	_, err := d.sql.Exec(`UPDATE runs SET error = ?, status = ?, push_active = 0, terminal_head_verified_at = NULL, updated_at = ? WHERE id = ? AND custody_returned_at IS NULL AND custody_transition_token IS NULL AND custody_transition_phase IS NULL`, errMsg, status, now(), id)
	if err != nil {
		return fmt.Errorf("update run error: %w", err)
	}
	return nil
}

func (d *DB) UpdateRunErrorStatusWithVerifiedHead(id, errMsg string, status types.RunStatus, headSHA string) error {
	ts := now()
	_, err := d.sql.Exec(`UPDATE runs SET error = ?, status = ?, head_sha = ?, push_active = 0, terminal_head_verified_at = ?, updated_at = ? WHERE id = ?`, errMsg, status, headSHA, ts, ts, id)
	if err != nil {
		return fmt.Errorf("update run error with verified head: %w", err)
	}
	return nil
}

func (d *DB) UpdateRunStatusWithVerifiedHead(id string, status types.RunStatus, headSHA string) error {
	ts := now()
	_, err := d.sql.Exec(`UPDATE runs SET status = ?, head_sha = ?, push_active = 0, terminal_head_verified_at = ?, updated_at = ? WHERE id = ?`, status, headSHA, ts, ts, id)
	if err != nil {
		return fmt.Errorf("update run status with verified head: %w", err)
	}
	return nil
}

// RunIntentSourceAgent is the intent_source value stamped when the driving
// agent supplied the intent explicitly via `axi run --intent`. It marks an
// authoritative, author-stated goal (score 1) as opposed to a transcript
// inference (whose source is the matched agent name: "claude", "codex", ...).
// Prompt-construction code branches on this to frame an explicit intent as
// authoritative acceptance criteria rather than a low-confidence hint.
const RunIntentSourceAgent = "agent"

// RunIntentSourceRerun marks an authoritative intent inherited from the run
// selected for a rerun. It remains authoritative, but the distinct value keeps
// inherited intent inspectable instead of confusing it with a new override.
const RunIntentSourceRerun = "rerun"

// IsAuthoritativeRunIntentSource reports whether a run's intent came from an
// explicit operator/agent contract, either directly or through rerun
// inheritance.
func IsAuthoritativeRunIntentSource(source string) bool {
	return source == RunIntentSourceAgent || source == RunIntentSourceRerun
}

// RunIntent carries the four intent-related columns persisted on a run.
type RunIntent struct {
	Summary   string
	Source    string
	SessionID string
	Score     float64
}

// UpdateRunIntent persists the inferred user intent for a run.
func (d *DB) UpdateRunIntent(id string, intent RunIntent) error {
	_, err := d.sql.Exec(
		`UPDATE runs SET intent = ?, intent_source = ?, intent_session_id = ?, intent_score = ?, updated_at = ? WHERE id = ?`,
		intent.Summary, intent.Source, intent.SessionID, intent.Score, now(), id,
	)
	if err != nil {
		return fmt.Errorf("update run intent: %w", err)
	}
	return nil
}

// SetRunAwaitingAgent marks a run as parked awaiting the driving agent,
// stamping awaiting_agent_since with the current time. Called by the executor
// when a step enters a gate (awaiting_approval / fix_review). This is a pollable
// observability signal only; it does not change gate resolution.
func (d *DB) SetRunAwaitingAgent(id string) error {
	ts := now()
	_, err := d.sql.Exec(`UPDATE runs SET awaiting_agent_since = ?, updated_at = ? WHERE id = ?`, ts, ts, id)
	if err != nil {
		return fmt.Errorf("set run awaiting agent: %w", err)
	}
	return nil
}

// ClearRunAwaitingAgent clears the awaiting-agent marker on a run. Called by the
// executor the moment the agent responds (or the approval wait is cancelled) and
// the run resumes, so awaiting_agent_since is non-nil exactly while a gate is
// actually parked.
func (d *DB) ClearRunAwaitingAgent(id string) error {
	_, err := d.sql.Exec(`UPDATE runs SET awaiting_agent_since = NULL, updated_at = ? WHERE id = ?`, now(), id)
	if err != nil {
		return fmt.Errorf("clear run awaiting agent: %w", err)
	}
	return nil
}

// AddRunParkedDuration accumulates parked-at-gate wall time onto a run's
// total. Called by the executor when a gate wait ends.
func (d *DB) AddRunParkedDuration(id string, ms int64) error {
	if ms <= 0 {
		return nil
	}
	_, err := d.sql.Exec(`UPDATE runs SET parked_ms = COALESCE(parked_ms, 0) + ?, updated_at = ? WHERE id = ?`, ms, now(), id)
	if err != nil {
		return fmt.Errorf("add run parked duration: %w", err)
	}
	return nil
}

func (d *DB) CompleteRunAwaitingAgent(id string, ms int64) error {
	if ms < 0 {
		ms = 0
	}
	_, err := d.sql.Exec(
		`UPDATE runs SET awaiting_agent_since = NULL,
			parked_ms = COALESCE(parked_ms, 0) + CASE WHEN awaiting_agent_since IS NOT NULL THEN ? ELSE 0 END,
			updated_at = ? WHERE id = ?`,
		ms, now(), id,
	)
	if err != nil {
		return fmt.Errorf("complete run awaiting agent: %w", err)
	}
	return nil
}

// RecoverStaleRuns marks any runs stuck in pending/running status as failed
// and fails any in-progress steps. This is called at daemon startup to clean
// up after a previous crash. Returns the number of recovered runs.
func (d *DB) RecoverStaleRuns(errMsg string) (int, error) {
	return d.RecoverStaleRunsExcept(errMsg, nil)
}

// RecoverStaleRunsExcept marks active runs as failed unless their IDs appear
// in preserved. Callers use preserved only after independently proving a run
// can be reconstructed safely.
func (d *DB) RecoverStaleRunsExcept(errMsg string, preserved map[string]struct{}) (int, error) {
	ts := now()

	tx, err := d.sql.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback()

	placeholders, args := recoveryExclusionClause(preserved)
	stepArgs := []any{
		types.StepStatusFailed, errMsg, ts,
		types.StepStatusRunning, types.StepStatusAwaitingApproval, types.StepStatusFixing, types.StepStatusFixReview,
		types.RunPending, types.RunRunning,
	}
	stepArgs = append(stepArgs, args...)
	_, err = tx.Exec(
		`UPDATE step_results SET status = ?, error = ?, completed_at = ?
		 WHERE status IN (?, ?, ?, ?) AND run_id IN (
			SELECT id FROM runs WHERE status IN (?, ?)`+placeholders+`
		 )`,
		stepArgs...,
	)
	if err != nil {
		return 0, fmt.Errorf("recover stale steps: %w", err)
	}

	// Fail stale runs. Clear any awaiting-agent marker so a recovered (now
	// failed) run is never reported as still parked awaiting the agent,
	// accumulating the marker's elapsed time into the run's parked total so
	// the parked evidence survives the crash.
	runArgs := []any{types.RunFailed, errMsg, ts, ts, ts, types.RunPending, types.RunRunning}
	runArgs = append(runArgs, args...)
	result, err := tx.Exec(
		`UPDATE runs SET status = ?, error = ?, push_active = 0,
			parked_ms = COALESCE(parked_ms, 0) + CASE
				WHEN awaiting_agent_since IS NOT NULL AND ? > awaiting_agent_since
				THEN (? - awaiting_agent_since) * 1000 ELSE 0 END,
			awaiting_agent_since = NULL, updated_at = ? WHERE status IN (?, ?)`+placeholders,
		runArgs...,
	)
	if err != nil {
		return 0, fmt.Errorf("recover stale runs: %w", err)
	}

	count, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("rows affected: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit transaction: %w", err)
	}
	return int(count), nil
}

func recoveryExclusionClause(preserved map[string]struct{}) (string, []any) {
	if len(preserved) == 0 {
		return "", nil
	}
	args := make([]any, 0, len(preserved))
	placeholders := make([]string, 0, len(preserved))
	for id := range preserved {
		placeholders = append(placeholders, "?")
		args = append(args, id)
	}
	return " AND id NOT IN (" + strings.Join(placeholders, ", ") + ")", args
}

// GetRunCIRerunState returns the CI step's persisted rerun budget for a run, or
// the empty string when the run has never spent one. The payload is opaque
// here: the CI step owns its shape, and the database only guarantees that what
// was written survives a restart.
func (d *DB) GetRunCIRerunState(id string) (string, error) {
	var state sql.NullString
	err := d.sql.QueryRow(`SELECT ci_rerun_state FROM runs WHERE id = ?`, id).Scan(&state)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("get run ci rerun state: %w", err)
	}
	return state.String, nil
}

// SetRunCIRerunState persists the CI step's rerun budget. The CI step calls
// this before asking the provider to re-run a check, so a crash between the
// reservation and the request costs the budget instead of handing the recovered
// run a rerun the limit already accounted for.
func (d *DB) SetRunCIRerunState(id, state string) error {
	_, err := d.sql.Exec(`UPDATE runs SET ci_rerun_state = ?, updated_at = ? WHERE id = ?`, state, now(), id)
	if err != nil {
		return fmt.Errorf("set run ci rerun state: %w", err)
	}
	return nil
}

package db

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Run represents a pipeline run.
type Run struct {
	ID                    string
	RepoID                string
	Branch                string
	HeadSHA               string
	BaseSHA               string
	SubmittedHeadSHA      *string
	Status                types.RunStatus
	PRURL                 *string
	PRState               *string
	PRStateObservedAt     *int64
	CIReadyAt             *int64
	LastPushedSHA         *string
	PushTargetKind        *string
	PushTargetFingerprint *string
	PushRef               *string
	LastPushedAt          *int64
	PushGeneration        *int64
	PushActive            bool
	// CustodyReturnedAt is non-nil once a guarded branch-sync recovery
	// explicitly ended this run's ownership of an unpublished pipeline head
	// (terminal run whose head was never successfully pushed, or moved after
	// the last push). It never changes push provenance; it only records that
	// the operator worktree took the branch back.
	CustodyReturnedAt *int64
	Error             *string
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

const runColumns = `id, repo_id, branch, head_sha, base_sha, submitted_head_sha, status, pr_url, pr_state, pr_state_observed_at, ci_ready_at, last_pushed_sha, push_target_kind, push_target_fingerprint, push_ref, last_pushed_at, push_generation, COALESCE(push_active, 0), custody_returned_at, error, awaiting_agent_since, COALESCE(parked_ms, 0), intent, intent_source, intent_session_id, intent_score, created_at, updated_at`

func scanRun(row interface {
	Scan(...any) error
}, r *Run) error {
	return row.Scan(
		&r.ID, &r.RepoID, &r.Branch, &r.HeadSHA, &r.BaseSHA, &r.SubmittedHeadSHA, &r.Status,
		&r.PRURL, &r.PRState, &r.PRStateObservedAt, &r.CIReadyAt,
		&r.LastPushedSHA, &r.PushTargetKind, &r.PushTargetFingerprint, &r.PushRef,
		&r.LastPushedAt, &r.PushGeneration, &r.PushActive,
		&r.CustodyReturnedAt, &r.Error, &r.AwaitingAgentSince, &r.ParkedMS,
		&r.Intent, &r.IntentSource, &r.IntentSessionID, &r.IntentScore,
		&r.CreatedAt, &r.UpdatedAt,
	)
}

// InsertRun creates a new run record.
func (d *DB) InsertRun(repoID, branch, headSHA, baseSHA string) (*Run, error) {
	ts := now()
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
	_, err := d.sql.Exec(
		`INSERT INTO runs (id, repo_id, branch, head_sha, base_sha, submitted_head_sha, status, pr_state, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, 'none', ?, ?)`,
		r.ID, r.RepoID, r.Branch, r.HeadSHA, r.BaseSHA, headSHA, r.Status, r.CreatedAt, r.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("insert run: %w", err)
	}
	return r, nil
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
	_, err := d.sql.Exec(`UPDATE runs SET status = ?, push_active = CASE WHEN ? IN ('completed', 'failed', 'cancelled') THEN 0 ELSE push_active END, updated_at = ? WHERE id = ?`, status, status, now(), id)
	if err != nil {
		return fmt.Errorf("update run status: %w", err)
	}
	return nil
}

// UpdateRunPRURL sets the PR URL on a run. A delayed PR-step write must not
// regress terminal lifecycle truth already observed by the CI monitor.
func (d *DB) UpdateRunPRURL(id, prURL string) error {
	ts := now()
	_, err := d.sql.Exec(`UPDATE runs SET pr_url = ?, pr_state = CASE WHEN pr_state IN ('merged', 'closed') THEN pr_state ELSE 'open' END, pr_state_observed_at = ?, updated_at = ? WHERE id = ?`, prURL, ts, ts, id)
	if err != nil {
		return fmt.Errorf("update run pr url: %w", err)
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

// UpdateRunPushBinding advances a run's successful-push provenance and
// increments its generation. It is called for both a completed push and a
// freshly verified already-up-to-date push.
func (d *DB) UpdateRunPushBinding(id string, binding PushBinding) error {
	ts := now()
	_, err := d.sql.Exec(
		`UPDATE runs SET last_pushed_sha = ?, push_target_kind = ?, push_target_fingerprint = ?, push_ref = ?, last_pushed_at = ?, push_generation = COALESCE(push_generation, 0) + 1, updated_at = ? WHERE id = ?`,
		binding.HeadSHA, binding.TargetKind, binding.TargetFingerprint, binding.Ref, ts, ts, id,
	)
	if err != nil {
		return fmt.Errorf("update run push binding: %w", err)
	}
	return nil
}

// SetRunCustodyReturned stamps the moment a guarded recovery explicitly
// returned custody of this run's branch to the operator worktree. Stamping is
// idempotent: the first timestamp wins so the record keeps the original
// recovery moment.
func (d *DB) SetRunCustodyReturned(id string) error {
	ts := now()
	_, err := d.sql.Exec(`UPDATE runs SET custody_returned_at = COALESCE(custody_returned_at, ?), updated_at = ? WHERE id = ?`, ts, ts, id)
	if err != nil {
		return fmt.Errorf("set run custody returned: %w", err)
	}
	return nil
}

// SetRunPushActive marks whether a pipeline phase currently owns a possible
// branch-head update. Sync refuses while this marker is set.
func (d *DB) SetRunPushActive(id string, active bool) error {
	_, err := d.sql.Exec(`UPDATE runs SET push_active = ?, updated_at = ? WHERE id = ?`, active, now(), id)
	if err != nil {
		return fmt.Errorf("set run push active: %w", err)
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

	var current sql.NullString
	if err := tx.QueryRow(`SELECT pr_state FROM runs WHERE id = ?`, id).Scan(&current); err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		return fmt.Errorf("update run PR state: read current state: %w", err)
	}
	state = monotonicPRState(current.String, state)
	if _, err := tx.Exec(`UPDATE runs SET pr_state = ?, pr_state_observed_at = ?, updated_at = ? WHERE id = ?`, state, ts, ts, id); err != nil {
		return fmt.Errorf("update run PR state: %w", err)
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
	var readyAt any
	if ready {
		readyAt = now()
	}
	_, err := d.sql.Exec(`UPDATE runs SET ci_ready_at = ?, updated_at = ? WHERE id = ? AND ((ci_ready_at IS NULL AND ? = 1) OR (ci_ready_at IS NOT NULL AND ? = 0))`, readyAt, now(), id, ready, ready)
	if err != nil {
		return fmt.Errorf("set run CI ready: %w", err)
	}
	return nil
}

// UpdateRunHeadSHA updates the run head SHA and timestamp.
func (d *DB) UpdateRunHeadSHA(id, headSHA string) error {
	_, err := d.sql.Exec(`UPDATE runs SET head_sha = ?, updated_at = ? WHERE id = ?`, headSHA, now(), id)
	if err != nil {
		return fmt.Errorf("update run head sha: %w", err)
	}
	return nil
}

// UpdateRunError sets the error message on a run.
func (d *DB) UpdateRunError(id, errMsg string) error {
	return d.UpdateRunErrorStatus(id, errMsg, types.RunFailed)
}

// UpdateRunErrorStatus sets the error message and terminal status on a run.
func (d *DB) UpdateRunErrorStatus(id, errMsg string, status types.RunStatus) error {
	_, err := d.sql.Exec(`UPDATE runs SET error = ?, status = ?, push_active = 0, updated_at = ? WHERE id = ?`, errMsg, status, now(), id)
	if err != nil {
		return fmt.Errorf("update run error: %w", err)
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

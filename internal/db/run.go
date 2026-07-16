package db

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Run represents a pipeline run.
type Run struct {
	ID                      string
	RepoID                  string
	Branch                  string
	HeadSHA                 string
	BaseSHA                 string
	Status                  types.RunStatus
	ProvisioningPhase       string
	ProvisioningProgress    int
	ProvisioningError       *string
	ProvisioningStartedAt   *int64
	ProvisioningCompletedAt *int64
	PRURL                   *string
	Error                   *string
	BlockedReason           *string
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

const runColumns = `id, repo_id, branch, head_sha, base_sha, status, provisioning_phase, provisioning_progress, provisioning_error, provisioning_started_at, provisioning_completed_at, pr_url, error, blocked_reason, awaiting_agent_since, COALESCE(parked_ms, 0), intent, intent_source, intent_session_id, intent_score, created_at, updated_at`

func scanRun(row interface {
	Scan(...any) error
}, r *Run) error {
	return row.Scan(
		&r.ID, &r.RepoID, &r.Branch, &r.HeadSHA, &r.BaseSHA, &r.Status,
		&r.ProvisioningPhase, &r.ProvisioningProgress, &r.ProvisioningError, &r.ProvisioningStartedAt, &r.ProvisioningCompletedAt,
		&r.PRURL, &r.Error, &r.BlockedReason, &r.AwaitingAgentSince, &r.ParkedMS,
		&r.Intent, &r.IntentSource, &r.IntentSessionID, &r.IntentScore,
		&r.CreatedAt, &r.UpdatedAt,
	)
}

// InsertRun creates a new run record.
func (d *DB) InsertRun(repoID, branch, headSHA, baseSHA string) (*Run, error) {
	ts := now()
	r := &Run{
		ID:                newID(),
		RepoID:            repoID,
		Branch:            branch,
		HeadSHA:           headSHA,
		BaseSHA:           baseSHA,
		Status:            types.RunPending,
		CreatedAt:         ts,
		UpdatedAt:         ts,
		ProvisioningPhase: "created",
	}
	_, err := d.sql.Exec(
		`INSERT INTO runs (id, repo_id, branch, head_sha, base_sha, status, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.RepoID, r.Branch, r.HeadSHA, r.BaseSHA, r.Status, r.CreatedAt, r.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("insert run: %w", err)
	}
	if err := d.AppendLifecycleEvent(LifecycleEvent{RunID: r.ID, EventType: "run_created", Status: string(r.Status), Metadata: map[string]any{"branch": branch, "head_sha": headSHA, "base_sha": baseSHA}}); err != nil {
		return nil, err
	}
	return r, nil
}

func (d *DB) SetRunProvisioning(id, phase string, progress int, provisioningErr string) error {
	if progress < 0 {
		progress = 0
	}
	if progress > 100 {
		progress = 100
	}
	ts := now()
	_, err := d.sql.Exec(`UPDATE runs SET status = ?, provisioning_phase = ?, provisioning_progress = ?, provisioning_error = ?, provisioning_started_at = COALESCE(provisioning_started_at, ?), updated_at = ? WHERE id = ?`,
		types.RunProvisioning, phase, progress, nullableString(provisioningErr), ts, ts, id)
	if err != nil {
		return fmt.Errorf("set run provisioning: %w", err)
	}
	return d.AppendLifecycleEvent(LifecycleEvent{RunID: id, EventType: "provisioning", Status: string(types.RunProvisioning), Error: provisioningErr, Metadata: map[string]any{"phase": phase, "progress": progress}})
}

func (d *DB) CompleteRunProvisioning(id string) error {
	ts := now()
	_, err := d.sql.Exec(`UPDATE runs SET status = ?, provisioning_phase = 'ready', provisioning_progress = 100, provisioning_error = NULL, provisioning_completed_at = ?, updated_at = ? WHERE id = ?`, types.RunPending, ts, ts, id)
	if err != nil {
		return fmt.Errorf("complete run provisioning: %w", err)
	}
	return d.AppendLifecycleEvent(LifecycleEvent{RunID: id, EventType: "provisioning_completed", Status: string(types.RunPending), Metadata: map[string]any{"progress": 100}})
}

func (d *DB) FailRunProvisioning(id, phase string, provisioningErr error) error {
	message := "provisioning failed"
	if provisioningErr != nil {
		message = provisioningErr.Error()
	}
	if err := d.SetRunProvisioning(id, phase, 100, message); err != nil {
		return err
	}
	_, err := d.sql.Exec(`UPDATE runs SET status = ?, error = ?, provisioning_error = ?, updated_at = ? WHERE id = ?`, types.RunFailed, message, message, now(), id)
	if err != nil {
		return fmt.Errorf("fail run provisioning: %w", err)
	}
	return d.AppendLifecycleEvent(LifecycleEvent{RunID: id, EventType: "provisioning_failure", Status: string(types.RunFailed), Error: message})
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
			`SELECT `+runColumns+` FROM runs WHERE repo_id = ? AND status IN ('pending', 'provisioning', 'running', 'awaiting_auth') ORDER BY created_at DESC, id DESC LIMIT 1`, repoID,
		), r)
	} else {
		err = scanRun(d.sql.QueryRow(
			`SELECT `+runColumns+` FROM runs WHERE repo_id = ? AND branch = ? AND status IN ('pending', 'provisioning', 'running', 'awaiting_auth') ORDER BY created_at DESC, id DESC LIMIT 1`, repoID, branch,
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
		`SELECT `+runColumns+` FROM runs WHERE status IN (?, ?, ?, ?) ORDER BY created_at DESC, id DESC`,
		types.RunPending, types.RunProvisioning, types.RunRunning, types.RunAwaitingAuth,
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

// GetProvisioningRuns returns runs whose durable setup phase must be resumed
// after a daemon restart.
func (d *DB) GetProvisioningRuns() ([]*Run, error) {
	rows, err := d.sql.Query(`SELECT `+runColumns+` FROM runs WHERE status = ? ORDER BY created_at, id`, types.RunProvisioning)
	if err != nil {
		return nil, fmt.Errorf("get provisioning runs: %w", err)
	}
	defer rows.Close()
	var runs []*Run
	for rows.Next() {
		r := &Run{}
		if err := scanRun(rows, r); err != nil {
			return nil, fmt.Errorf("scan provisioning run: %w", err)
		}
		runs = append(runs, r)
	}
	return runs, rows.Err()
}

// UpdateRunStatus updates a run's status and updated_at timestamp.
func (d *DB) UpdateRunStatus(id string, status types.RunStatus) error {
	_, err := d.sql.Exec(`UPDATE runs SET status = ?, updated_at = ? WHERE id = ?`, status, now(), id)
	if err != nil {
		return fmt.Errorf("update run status: %w", err)
	}
	if err := d.AppendLifecycleEvent(LifecycleEvent{RunID: id, EventType: "run_status", Status: string(status)}); err != nil {
		return err
	}
	return nil
}

// UpdateRunPRURL sets the PR URL on a run.
func (d *DB) UpdateRunPRURL(id, prURL string) error {
	_, err := d.sql.Exec(`UPDATE runs SET pr_url = ?, updated_at = ? WHERE id = ?`, prURL, now(), id)
	if err != nil {
		return fmt.Errorf("update run pr url: %w", err)
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
	_, err := d.sql.Exec(`UPDATE runs SET error = ?, status = ?, updated_at = ? WHERE id = ?`, errMsg, status, now(), id)
	if err != nil {
		return fmt.Errorf("update run error: %w", err)
	}
	if err := d.AppendLifecycleEvent(LifecycleEvent{RunID: id, EventType: "run_failure", Status: string(status), Error: errMsg}); err != nil {
		return err
	}
	return nil
}

// SetRunBlockedReason keeps a recoverable external blockage in the current
// projection while the lifecycle ledger preserves every preceding failure.
func (d *DB) SetRunBlockedReason(id, reason string) error {
	_, err := d.sql.Exec(`UPDATE runs SET blocked_reason = ?, updated_at = ? WHERE id = ?`, reason, now(), id)
	if err != nil {
		return fmt.Errorf("set run blocked reason: %w", err)
	}
	return d.AppendLifecycleEvent(LifecycleEvent{RunID: id, EventType: "run_blocked", Status: string(types.RunRunning), Error: reason})
}

func (d *DB) ClearRunBlockedReason(id string) error {
	_, err := d.sql.Exec(`UPDATE runs SET blocked_reason = NULL, updated_at = ? WHERE id = ?`, now(), id)
	if err != nil {
		return fmt.Errorf("clear run blocked reason: %w", err)
	}
	return d.AppendLifecycleEvent(LifecycleEvent{RunID: id, EventType: "run_unblocked", Status: string(types.RunRunning)})
}

// ParkRunForAuthorization records a recoverable provider-auth transition.
// It intentionally does not clear any prior lifecycle events or session rows.
func (d *DB) ParkRunForAuthorization(id, step, detail string) error {
	reason := "authorization required"
	if strings.TrimSpace(detail) != "" {
		reason = "authorization required: " + strings.TrimSpace(detail)
	}
	ts := now()
	_, err := d.sql.Exec(`UPDATE runs SET status = ?, blocked_reason = ?, error = ?, updated_at = ? WHERE id = ?`, types.RunAwaitingAuth, reason, reason, ts, id)
	if err != nil {
		return fmt.Errorf("park run for authorization: %w", err)
	}
	return d.AppendLifecycleEvent(LifecycleEvent{RunID: id, StepName: step, EventType: "authorization_required", Status: string(types.RunAwaitingAuth), Error: reason})
}

// ResumeRunAfterAuthorization clears only the current recoverable blockage.
// The authorization_required lifecycle event remains immutable evidence.
func (d *DB) ResumeRunAfterAuthorization(id, step string) error {
	_, err := d.sql.Exec(`UPDATE runs SET status = ?, blocked_reason = NULL, error = NULL, updated_at = ? WHERE id = ?`, types.RunRunning, now(), id)
	if err != nil {
		return fmt.Errorf("resume run after authorization: %w", err)
	}
	return d.AppendLifecycleEvent(LifecycleEvent{RunID: id, StepName: step, EventType: "authorization_resumed", Status: string(types.RunRunning)})
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
		`UPDATE runs SET awaiting_agent_since = NULL, parked_ms = COALESCE(parked_ms, 0) + ?, updated_at = ? WHERE id = ?`,
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
	recoveryStatuses := []any{types.RunPending, types.RunProvisioning, types.RunRunning, types.RunAwaitingAuth}
	recoveryArgs := append(append([]any{}, recoveryStatuses...), args...)
	rows, err := tx.Query(`SELECT id FROM runs WHERE status IN (?, ?, ?, ?)`+placeholders, recoveryArgs...)
	if err != nil {
		return 0, fmt.Errorf("list stale runs: %w", err)
	}
	var recoveredIDs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan stale run: %w", err)
		}
		recoveredIDs = append(recoveredIDs, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, fmt.Errorf("read stale runs: %w", err)
	}
	rows.Close()
	stepArgs := []any{
		types.StepStatusFailed, errMsg, ts,
		types.StepStatusRunning, types.StepStatusAwaitingApproval, types.StepStatusFixing, types.StepStatusFixReview,
		types.RunPending, types.RunProvisioning, types.RunRunning, types.RunAwaitingAuth,
	}
	stepArgs = append(stepArgs, args...)
	_, err = tx.Exec(
		`UPDATE step_results SET status = ?, error = ?, completed_at = ?
			 WHERE status IN (?, ?, ?, ?) AND run_id IN (
			SELECT id FROM runs WHERE status IN (?, ?, ?, ?)`+placeholders+`
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
	runArgs := []any{types.RunFailed, errMsg, ts, ts, ts, types.RunPending, types.RunProvisioning, types.RunRunning, types.RunAwaitingAuth}
	runArgs = append(runArgs, args...)
	result, err := tx.Exec(
		`UPDATE runs SET status = ?, error = ?,
			parked_ms = COALESCE(parked_ms, 0) + CASE
				WHEN awaiting_agent_since IS NOT NULL AND ? > awaiting_agent_since
				THEN (? - awaiting_agent_since) * 1000 ELSE 0 END,
		awaiting_agent_since = NULL, blocked_reason = NULL, updated_at = ? WHERE status IN (?, ?, ?, ?)`+placeholders,
		runArgs...,
	)
	if err != nil {
		return 0, fmt.Errorf("recover stale runs: %w", err)
	}
	for _, id := range recoveredIDs {
		if _, err := tx.Exec(`INSERT INTO lifecycle_events (id, run_id, event_type, status, error, created_at) VALUES (?, ?, ?, ?, ?, ?)`, newID(), id, "run_recovered", string(types.RunFailed), errMsg, ts); err != nil {
			return 0, fmt.Errorf("record recovered run: %w", err)
		}
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

package db

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// UtilityScope is a durable owner for agent work that does not belong to a
// pipeline run, step result, or round.
type UtilityScope struct {
	ID        string
	Kind      types.UtilityScopeKind
	OwnerPID  int
	CreatedAt int64
}

// InsertUtilityScope creates a standalone invocation owner without fabricating
// pipeline records.
func (d *DB) InsertUtilityScope(kind types.UtilityScopeKind, ownerPID int) (*UtilityScope, error) {
	if kind != types.UtilityScopeWizard {
		return nil, fmt.Errorf("unknown utility scope kind %q", kind)
	}
	if ownerPID <= 0 {
		return nil, fmt.Errorf("utility scope owner PID must be positive")
	}
	scope := &UtilityScope{ID: newID(), Kind: kind, OwnerPID: ownerPID, CreatedAt: time.Now().UnixNano()}
	if _, err := d.sql.Exec(
		`INSERT INTO utility_scopes (id, kind, owner_pid, created_at) VALUES (?, ?, ?, ?)`,
		scope.ID, scope.Kind, scope.OwnerPID, scope.CreatedAt,
	); err != nil {
		return nil, fmt.Errorf("insert utility scope: %w", err)
	}
	return scope, nil
}

// GetOpenUtilityScopes returns utility owners with at least one attempt that
// has no terminal fact. The owner PID and observed-at timestamp let startup
// recovery distinguish a live owner from a dead process or reused PID.
func (d *DB) GetOpenUtilityScopes() ([]*UtilityScope, error) {
	rows, err := d.sql.Query(`
		SELECT scope.id, scope.kind, scope.owner_pid, scope.created_at
		FROM utility_scopes AS scope
		WHERE EXISTS (
			SELECT 1
			FROM invocation_attempt_starts AS start
			LEFT JOIN invocation_attempt_terminals AS terminal ON terminal.attempt_id = start.id
			WHERE start.utility_scope_id = scope.id AND terminal.attempt_id IS NULL
		)
		ORDER BY scope.created_at, scope.id`)
	if err != nil {
		return nil, fmt.Errorf("get open utility scopes: %w", err)
	}
	defer rows.Close()
	var scopes []*UtilityScope
	for rows.Next() {
		scope := &UtilityScope{}
		if err := rows.Scan(&scope.ID, &scope.Kind, &scope.OwnerPID, &scope.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan open utility scope: %w", err)
		}
		scopes = append(scopes, scope)
	}
	return scopes, rows.Err()
}

// InterruptUtilityScopeAttempts appends interruption terminals for every open
// attempt owned by one utility scope. Callers must first prove the owner ended.
func (d *DB) InterruptUtilityScopeAttempts(utilityScopeID string) (int64, error) {
	ts := now()
	result, err := d.sql.Exec(
		`INSERT INTO invocation_attempt_terminals (attempt_id, outcome, terminal_at, duration_ms, input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens)
		 SELECT start.id, ?, ?, max(0, (? - start.started_at) * 1000), 0, 0, 0, 0
		 FROM invocation_attempt_starts AS start
		 LEFT JOIN invocation_attempt_terminals AS terminal ON terminal.attempt_id = start.id
		 WHERE start.utility_scope_id = ? AND terminal.attempt_id IS NULL`,
		types.InvocationOutcomeInterrupted, ts, ts, utilityScopeID,
	)
	if err != nil {
		return 0, fmt.Errorf("interrupt utility scope attempts: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("interrupt utility scope attempts rows affected: %w", err)
	}
	return count, nil
}

// InvocationAttempt projects an immutable start and its optional immutable
// terminal fact. A nil Terminal means the attempt is active.
type InvocationAttempt struct {
	ID         string
	Start      types.InvocationAttemptStart
	StartedAt  int64
	Terminal   *types.InvocationAttemptTerminal
	TerminalAt *int64
}

// StartInvocationAttempt commits the start fact before native process launch.
func (d *DB) StartInvocationAttempt(start types.InvocationAttemptStart) (string, error) {
	definition, err := types.PurposeDefinitionFor(start.Purpose)
	if err != nil {
		return "", err
	}
	if start.Role != definition.Role {
		return "", fmt.Errorf("invocation role %q does not match purpose %q role %q", start.Role, start.Purpose, definition.Role)
	}
	if err := start.Scope.Validate(); err != nil {
		return "", fmt.Errorf("invocation scope: %w", err)
	}
	if strings.TrimSpace(start.CandidateKey) == "" {
		return "", fmt.Errorf("invocation candidate key is required")
	}
	if !start.Candidate.IsZero() {
		if err := start.Candidate.Validate(); err != nil {
			return "", fmt.Errorf("invocation candidate: %w", err)
		}
	}
	if err := d.validateInvocationScopeOwnership(start.Scope); err != nil {
		return "", err
	}

	var profile, runner, model, effort, tier, candidateIndex any
	if !start.Candidate.IsZero() {
		profile = start.Candidate.Profile
		tier = start.Candidate.Tier
		candidateIndex = start.Candidate.CandidateIndex
		runner = string(start.Candidate.Runner)
		model = start.Candidate.Model
		effort = string(start.Candidate.Effort)
	}

	id := newID()
	_, err = d.sql.Exec(
		`INSERT INTO invocation_attempt_starts (id, purpose, role, scope_kind, run_id, step_result_id, step_round_id, utility_scope_id, candidate_key, profile, tier, candidate_index, runner, model, effort, started_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, start.Purpose, start.Role, start.Scope.Kind,
		nullIfEmpty(start.Scope.RunID), nullIfEmpty(start.Scope.StepResultID), nullIfEmpty(start.Scope.StepRoundID), nullIfEmpty(start.Scope.UtilityScopeID),
		start.CandidateKey, profile, tier, candidateIndex, runner, model, effort, now(),
	)
	if err != nil {
		return "", fmt.Errorf("insert invocation attempt start: %w", err)
	}
	return id, nil
}

func (d *DB) validateInvocationScopeOwnership(scope types.InvocationScope) error {
	switch scope.Kind {
	case types.InvocationScopePipeline:
		var count int
		if err := d.sql.QueryRow(
			`SELECT count(*) FROM step_rounds AS round JOIN step_results AS step ON step.id = round.step_result_id WHERE round.id = ? AND round.step_result_id = ? AND step.run_id = ? AND round.state = ?`,
			scope.StepRoundID, scope.StepResultID, scope.RunID, StepRoundReserved,
		).Scan(&count); err != nil {
			return fmt.Errorf("validate pipeline invocation scope: %w", err)
		}
		if count != 1 {
			return fmt.Errorf("pipeline invocation scope does not identify one active run/step/round chain")
		}
	case types.InvocationScopeUtility:
		var count int
		if err := d.sql.QueryRow(`SELECT count(*) FROM utility_scopes WHERE id = ?`, scope.UtilityScopeID).Scan(&count); err != nil {
			return fmt.Errorf("validate utility invocation scope: %w", err)
		}
		if count != 1 {
			return fmt.Errorf("utility invocation scope %q does not exist", scope.UtilityScopeID)
		}
	}
	return nil
}

func nullIfEmpty(value string) any {
	if value == "" {
		return nil
	}
	return value
}

// FinishInvocationAttempt appends exactly one terminal fact. Start facts are
// never updated, and a second terminal append fails through the primary key.
func (d *DB) FinishInvocationAttempt(attemptID string, terminal types.InvocationAttemptTerminal) error {
	if err := terminal.Validate(); err != nil {
		return err
	}
	_, err := d.sql.Exec(
		`INSERT INTO invocation_attempt_terminals (attempt_id, outcome, failure_domain, terminal_at, duration_ms, input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		attemptID, terminal.Outcome, nullFailureDomain(terminal.FailureDomain), now(), terminal.DurationMS,
		terminal.InputTokens, terminal.OutputTokens, terminal.CacheReadTokens, terminal.CacheCreationTokens,
	)
	if err != nil {
		return fmt.Errorf("insert invocation attempt terminal: %w", err)
	}
	return nil
}

func nullFailureDomain(domain types.FailureDomain) any {
	if domain == "" {
		return nil
	}
	return domain
}

const invocationAttemptProjection = `
	SELECT start.id, start.purpose, start.role, start.scope_kind,
	       start.run_id, start.step_result_id, start.step_round_id, start.utility_scope_id,
	       start.candidate_key, start.profile, start.tier, start.candidate_index,
	       start.runner, start.model, start.effort, start.started_at,
	       terminal.outcome, terminal.failure_domain, terminal.terminal_at, terminal.duration_ms,
	       terminal.input_tokens, terminal.output_tokens, terminal.cache_read_tokens, terminal.cache_creation_tokens
	FROM invocation_attempt_starts AS start
	LEFT JOIN invocation_attempt_terminals AS terminal ON terminal.attempt_id = start.id`

// GetInvocationAttempt returns the active or completed projection by ID.
func (d *DB) GetInvocationAttempt(id string) (*InvocationAttempt, error) {
	attempt, err := scanInvocationAttempt(d.sql.QueryRow(invocationAttemptProjection+` WHERE start.id = ?`, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get invocation attempt: %w", err)
	}
	return attempt, nil
}

// GetInvocationAttemptsByRound returns attempts in durable start order.
func (d *DB) GetInvocationAttemptsByRound(roundID string) ([]*InvocationAttempt, error) {
	rows, err := d.sql.Query(invocationAttemptProjection+` WHERE start.step_round_id = ? ORDER BY start.started_at, start.id`, roundID)
	if err != nil {
		return nil, fmt.Errorf("get invocation attempts by round: %w", err)
	}
	defer rows.Close()
	var attempts []*InvocationAttempt
	for rows.Next() {
		attempt, err := scanInvocationAttempt(rows)
		if err != nil {
			return nil, fmt.Errorf("scan invocation attempt: %w", err)
		}
		attempts = append(attempts, attempt)
	}
	return attempts, rows.Err()
}

// GetInvocationAttemptsByUtilityScope returns standalone attempts in durable
// start order without synthesizing pipeline ownership.
func (d *DB) GetInvocationAttemptsByUtilityScope(utilityScopeID string) ([]*InvocationAttempt, error) {
	rows, err := d.sql.Query(invocationAttemptProjection+` WHERE start.utility_scope_id = ? ORDER BY start.started_at, start.id`, utilityScopeID)
	if err != nil {
		return nil, fmt.Errorf("get invocation attempts by utility scope: %w", err)
	}
	defer rows.Close()
	var attempts []*InvocationAttempt
	for rows.Next() {
		attempt, err := scanInvocationAttempt(rows)
		if err != nil {
			return nil, fmt.Errorf("scan invocation attempt: %w", err)
		}
		attempts = append(attempts, attempt)
	}
	return attempts, rows.Err()
}

func scanInvocationAttempt(row interface{ Scan(...any) error }) (*InvocationAttempt, error) {
	attempt := &InvocationAttempt{}
	var runID, stepResultID, stepRoundID, utilityScopeID sql.NullString
	var profile, runner, model, effort sql.NullString
	var tier, candidateIndex sql.NullInt64
	var outcome, failureDomain sql.NullString
	var terminalAt, durationMS, inputTokens, outputTokens, cacheReadTokens, cacheCreationTokens sql.NullInt64
	if err := row.Scan(
		&attempt.ID, &attempt.Start.Purpose, &attempt.Start.Role, &attempt.Start.Scope.Kind,
		&runID, &stepResultID, &stepRoundID, &utilityScopeID,
		&attempt.Start.CandidateKey, &profile, &tier, &candidateIndex,
		&runner, &model, &effort, &attempt.StartedAt,
		&outcome, &failureDomain, &terminalAt, &durationMS,
		&inputTokens, &outputTokens, &cacheReadTokens, &cacheCreationTokens,
	); err != nil {
		return nil, err
	}
	attempt.Start.Scope.RunID = runID.String
	attempt.Start.Scope.StepResultID = stepResultID.String
	attempt.Start.Scope.StepRoundID = stepRoundID.String
	attempt.Start.Scope.UtilityScopeID = utilityScopeID.String
	attempt.Start.Candidate = types.InvocationCandidate{
		Profile:        profile.String,
		Tier:           int(tier.Int64),
		CandidateIndex: int(candidateIndex.Int64),
		Runner:         types.Runner(runner.String),
		Model:          model.String,
		Effort:         types.Effort(effort.String),
	}
	if outcome.Valid {
		attempt.Terminal = &types.InvocationAttemptTerminal{
			Outcome:             types.InvocationOutcome(outcome.String),
			FailureDomain:       types.FailureDomain(failureDomain.String),
			DurationMS:          durationMS.Int64,
			InputTokens:         inputTokens.Int64,
			OutputTokens:        outputTokens.Int64,
			CacheReadTokens:     cacheReadTokens.Int64,
			CacheCreationTokens: cacheCreationTokens.Int64,
		}
		attempt.TerminalAt = &terminalAt.Int64
	}
	return attempt, nil
}

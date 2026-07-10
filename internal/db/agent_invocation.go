package db

import "fmt"

// Agent invocation session modes recorded for local performance telemetry.
const (
	InvocationModeCold     = "cold"     // no durable session involved
	InvocationModeStarted  = "started"  // began a new durable session
	InvocationModeResumed  = "resumed"  // resumed an existing durable session
	InvocationModeFallback = "fallback" // fresh session after a failed resume
)

// AgentInvocation is one agent process invocation's local performance
// evidence. It stores identity, timing, session mode, and token usage only -
// never prompts, model outputs, diffs, or credentials - and it stays local:
// no per-invocation record is ever sent to remote telemetry.
type AgentInvocation struct {
	ID       string
	RunID    string
	StepName string
	Round    int
	// Purpose is the pipeline duty served: review, review-fix,
	// test-evidence, housekeeping, document, lint, pr, intent, or a
	// step-derived default.
	Purpose string
	Agent   string
	Model   string
	// SessionMode is one of the InvocationMode constants.
	SessionMode string
	// SessionKey is a privacy-safe fingerprint (truncated SHA-256) of the
	// adapter-native session identity, so session reuse is auditable without
	// storing the raw resumable identity in a second place.
	SessionKey          string
	StartedAt           int64
	CompletedAt         int64
	DurationMS          int64
	ExitStatus          string // ok | error | cancelled
	FailureCategory     string // parse | exit | spawn | cancelled | other ("" when ok)
	InputTokens         int
	OutputTokens        int
	CacheReadTokens     int
	CacheCreationTokens int
}

const agentInvocationColumns = `id, run_id, step_name, round, purpose, agent, model, session_mode, session_key,
	started_at, completed_at, duration_ms, exit_status, failure_category,
	input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens`

// InsertAgentInvocation records one completed agent invocation.
func (d *DB) InsertAgentInvocation(inv AgentInvocation) (*AgentInvocation, error) {
	inv.ID = newID()
	_, err := d.sql.Exec(
		`INSERT INTO agent_invocations (`+agentInvocationColumns+`)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		inv.ID, inv.RunID, inv.StepName, inv.Round, inv.Purpose, inv.Agent, inv.Model, inv.SessionMode, inv.SessionKey,
		inv.StartedAt, inv.CompletedAt, inv.DurationMS, inv.ExitStatus, inv.FailureCategory,
		inv.InputTokens, inv.OutputTokens, inv.CacheReadTokens, inv.CacheCreationTokens,
	)
	if err != nil {
		return nil, fmt.Errorf("insert agent invocation: %w", err)
	}
	return &inv, nil
}

// GetAgentInvocationsByRun returns a run's invocations in execution order.
func (d *DB) GetAgentInvocationsByRun(runID string) ([]AgentInvocation, error) {
	rows, err := d.sql.Query(
		`SELECT `+agentInvocationColumns+` FROM agent_invocations WHERE run_id = ? ORDER BY started_at, id`,
		runID,
	)
	if err != nil {
		return nil, fmt.Errorf("get agent invocations: %w", err)
	}
	defer rows.Close()

	var invocations []AgentInvocation
	for rows.Next() {
		var inv AgentInvocation
		if err := rows.Scan(
			&inv.ID, &inv.RunID, &inv.StepName, &inv.Round, &inv.Purpose, &inv.Agent, &inv.Model, &inv.SessionMode, &inv.SessionKey,
			&inv.StartedAt, &inv.CompletedAt, &inv.DurationMS, &inv.ExitStatus, &inv.FailureCategory,
			&inv.InputTokens, &inv.OutputTokens, &inv.CacheReadTokens, &inv.CacheCreationTokens,
		); err != nil {
			return nil, fmt.Errorf("scan agent invocation: %w", err)
		}
		invocations = append(invocations, inv)
	}
	return invocations, rows.Err()
}

// AgentInvocationAggregate summarizes invocations for one purpose, powering
// the read-only performance report.
type AgentInvocationAggregate struct {
	Purpose             string
	Count               int
	TotalDurationMS     int64
	AvgDurationMS       int64
	Cold                int
	Started             int
	Resumed             int
	Fallback            int
	Errors              int
	InputTokens         int64
	OutputTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64
}

// AgentInvocationAggregates returns per-purpose aggregates across all runs,
// largest total duration first.
func (d *DB) AgentInvocationAggregates() ([]AgentInvocationAggregate, error) {
	rows, err := d.sql.Query(`
		SELECT purpose,
		       COUNT(*),
		       COALESCE(SUM(duration_ms), 0),
		       COALESCE(SUM(CASE WHEN session_mode = 'cold' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN session_mode = 'started' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN session_mode = 'resumed' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN session_mode = 'fallback' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN exit_status != 'ok' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(input_tokens), 0),
		       COALESCE(SUM(output_tokens), 0),
		       COALESCE(SUM(cache_read_tokens), 0),
		       COALESCE(SUM(cache_creation_tokens), 0)
		FROM agent_invocations
		GROUP BY purpose
		ORDER BY SUM(duration_ms) DESC`)
	if err != nil {
		return nil, fmt.Errorf("agent invocation aggregates: %w", err)
	}
	defer rows.Close()

	var aggregates []AgentInvocationAggregate
	for rows.Next() {
		var a AgentInvocationAggregate
		if err := rows.Scan(
			&a.Purpose, &a.Count, &a.TotalDurationMS,
			&a.Cold, &a.Started, &a.Resumed, &a.Fallback, &a.Errors,
			&a.InputTokens, &a.OutputTokens, &a.CacheReadTokens, &a.CacheCreationTokens,
		); err != nil {
			return nil, fmt.Errorf("scan agent invocation aggregate: %w", err)
		}
		if a.Count > 0 {
			a.AvgDurationMS = a.TotalDurationMS / int64(a.Count)
		}
		aggregates = append(aggregates, a)
	}
	return aggregates, rows.Err()
}

// RunInvocationSummary is the low-cardinality per-run rollup used for the
// bounded terminal remote summary (counts only - no ids, paths, or models).
type RunInvocationSummary struct {
	Count           int
	Resumed         int
	Fallback        int
	TotalDurationMS int64
}

// AgentInvocationSummaryForRun returns the run's invocation rollup.
func (d *DB) AgentInvocationSummaryForRun(runID string) (RunInvocationSummary, error) {
	var s RunInvocationSummary
	err := d.sql.QueryRow(`
		SELECT COUNT(*),
		       COALESCE(SUM(CASE WHEN session_mode = 'resumed' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN session_mode = 'fallback' THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(duration_ms), 0)
		FROM agent_invocations WHERE run_id = ?`, runID).
		Scan(&s.Count, &s.Resumed, &s.Fallback, &s.TotalDurationMS)
	if err != nil {
		return RunInvocationSummary{}, fmt.Errorf("agent invocation summary: %w", err)
	}
	return s, nil
}

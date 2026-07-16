package db

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// LifecycleEvent is immutable operational evidence. The current run/step
// rows are projections; these records are never updated or deleted as a run
// is cancelled, superseded, recovered, or restarted.
type LifecycleEvent struct {
	ID        string
	RunID     string
	StepName  string
	EventType string
	Status    string
	Error     string
	Metadata  map[string]any
	CreatedAt int64
}

func (d *DB) AppendLifecycleEvent(event LifecycleEvent) error {
	return d.appendLifecycleEvent(d.sql, event)
}

type lifecycleExecutor interface {
	Exec(query string, args ...any) (sql.Result, error)
}

func (d *DB) appendLifecycleEvent(exec lifecycleExecutor, event LifecycleEvent) error {
	metadata := ""
	if event.Metadata != nil {
		encoded, err := json.Marshal(event.Metadata)
		if err != nil {
			return fmt.Errorf("marshal lifecycle metadata: %w", err)
		}
		metadata = string(encoded)
	}
	if event.ID == "" {
		event.ID = newID()
	}
	if event.CreatedAt == 0 {
		event.CreatedAt = now()
	}
	_, err := exec.Exec(`INSERT INTO lifecycle_events
		(id, run_id, step_name, event_type, status, error, metadata, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, event.ID, nullable(event.RunID), nullable(event.StepName),
		nullable(event.EventType), nullable(event.Status), nullable(event.Error), nullable(metadata), event.CreatedAt)
	if err != nil {
		return fmt.Errorf("append lifecycle event: %w", err)
	}
	return nil
}

func (d *DB) withLifecycleTx(fn func(*sql.Tx) error) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return fmt.Errorf("begin lifecycle transaction: %w", err)
	}
	if err := fn(tx); err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit lifecycle transaction: %w", err)
	}
	return nil
}

func (d *DB) LifecycleEvents(runID string) ([]LifecycleEvent, error) {
	rows, err := d.sql.Query(`SELECT id, COALESCE(run_id,''), COALESCE(step_name,''), event_type,
		COALESCE(status,''), COALESCE(error,''), COALESCE(metadata,''), created_at
		FROM lifecycle_events WHERE run_id = ? ORDER BY created_at, id`, runID)
	if err != nil {
		return nil, fmt.Errorf("get lifecycle events: %w", err)
	}
	defer rows.Close()
	var events []LifecycleEvent
	for rows.Next() {
		var e LifecycleEvent
		var metadata string
		if err := rows.Scan(&e.ID, &e.RunID, &e.StepName, &e.EventType, &e.Status, &e.Error, &metadata, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan lifecycle event: %w", err)
		}
		if metadata != "" {
			_ = json.Unmarshal([]byte(metadata), &e.Metadata)
		}
		events = append(events, e)
	}
	return events, rows.Err()
}

type RouteDecision struct {
	ID                      string
	RunID                   string
	StepName                string
	Round                   int
	RequestedHarness        string
	EffectiveHarness        string
	RequestedModel          string
	EffectiveModel          string
	RequestedEffort         string
	EffectiveEffort         string
	PolicyVersion           string
	Phase                   string
	Risk                    string
	Reason                  string
	SourceConfiguration     string
	ConfigurationGeneration string
	Repository              string
	PromptSHA256            string
	PromptBytes             int
	PromptTransport         string
	CreatedAt               int64
}

func (d *DB) InsertRouteDecision(decision RouteDecision) error {
	if decision.ID == "" {
		decision.ID = newID()
	}
	if decision.CreatedAt == 0 {
		decision.CreatedAt = now()
	}
	if decision.Risk == "" {
		decision.Risk = "unknown"
	}
	if decision.PromptTransport == "" {
		decision.PromptTransport = "stdin"
	}
	query := `INSERT INTO route_decisions
		(id, run_id, step_name, round, requested_harness, effective_harness,
		 requested_model, effective_model, requested_effort, effective_effort,
		 policy_version, phase, risk, reason, source_configuration, configuration_generation,
		 repository, prompt_sha256, prompt_bytes, prompt_transport, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	_, err := d.sql.Exec(query,
		decision.ID, decision.RunID, nullable(decision.StepName), decision.Round,
		decision.RequestedHarness, decision.EffectiveHarness, nullable(decision.RequestedModel), nullable(decision.EffectiveModel),
		nullable(decision.RequestedEffort), nullable(decision.EffectiveEffort), decision.PolicyVersion, decision.Phase, decision.Risk,
		decision.Reason, nullable(decision.SourceConfiguration), nullable(decision.ConfigurationGeneration), nullable(decision.Repository),
		nullable(decision.PromptSHA256), decision.PromptBytes, nullable(decision.PromptTransport), decision.CreatedAt)
	if err != nil {
		return fmt.Errorf("insert route decision: %w", err)
	}
	return nil
}

func (d *DB) RouteDecisions(runID string) ([]RouteDecision, error) {
	rows, err := d.sql.Query(`SELECT id, run_id, COALESCE(step_name,''), round,
		requested_harness, effective_harness, COALESCE(requested_model,''), COALESCE(effective_model,''),
		COALESCE(requested_effort,''), COALESCE(effective_effort,''), policy_version, phase, COALESCE(risk,'unknown'), reason,
		COALESCE(source_configuration,''), COALESCE(configuration_generation,''), COALESCE(repository,''),
		COALESCE(prompt_sha256,''), COALESCE(prompt_bytes,0), COALESCE(prompt_transport,'stdin'), created_at
		FROM route_decisions WHERE run_id = ? ORDER BY created_at, id`, runID)
	if err != nil {
		return nil, fmt.Errorf("get route decisions: %w", err)
	}
	defer rows.Close()
	var decisions []RouteDecision
	for rows.Next() {
		var d RouteDecision
		if err := rows.Scan(&d.ID, &d.RunID, &d.StepName, &d.Round, &d.RequestedHarness, &d.EffectiveHarness,
			&d.RequestedModel, &d.EffectiveModel, &d.RequestedEffort, &d.EffectiveEffort, &d.PolicyVersion,
			&d.Phase, &d.Risk, &d.Reason, &d.SourceConfiguration, &d.ConfigurationGeneration, &d.Repository,
			&d.PromptSHA256, &d.PromptBytes, &d.PromptTransport, &d.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan route decision: %w", err)
		}
		decisions = append(decisions, d)
	}
	return decisions, rows.Err()
}

// RouteResult is immutable post-result evidence for a review classification.
// Route decisions are captured before launch; these rows record the completed
// review/fix-review result that recovery must use as policy input.
type RouteResult struct {
	ID        string
	RunID     string
	StepName  string
	Round     int
	Phase     string
	Risk      string
	AppendSeq int64
	CreatedAt int64
}

func (d *DB) InsertRouteResult(result RouteResult) error {
	if result.ID == "" {
		result.ID = newID()
	}
	if result.Risk == "" {
		result.Risk = "unknown"
	}
	if result.CreatedAt == 0 {
		result.CreatedAt = now()
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return fmt.Errorf("begin route result insert: %w", err)
	}
	rollback := func(err error) error {
		_ = tx.Rollback()
		return err
	}
	if _, err := tx.Exec(`INSERT OR IGNORE INTO route_result_sequence (id, next_seq) VALUES (1, 0)`); err != nil {
		return rollback(fmt.Errorf("initialize route result sequence: %w", err))
	}
	if _, err := tx.Exec(`UPDATE route_result_sequence SET next_seq = next_seq + 1 WHERE id = 1`); err != nil {
		return rollback(fmt.Errorf("advance route result sequence: %w", err))
	}
	if err := tx.QueryRow(`SELECT next_seq FROM route_result_sequence WHERE id = 1`).Scan(&result.AppendSeq); err != nil {
		return rollback(fmt.Errorf("read route result sequence: %w", err))
	}
	if _, err := tx.Exec(`INSERT INTO route_results
		(id, run_id, step_name, round, phase, risk, append_seq, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, result.ID, result.RunID, result.StepName,
		result.Round, result.Phase, result.Risk, result.AppendSeq, result.CreatedAt); err != nil {
		return rollback(fmt.Errorf("insert route result: %w", err))
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit route result: %w", err)
	}
	return nil
}

func (d *DB) RouteResults(runID string) ([]RouteResult, error) {
	rows, err := d.sql.Query(`SELECT id, run_id, step_name, round, phase, risk,
		COALESCE(CAST(append_seq AS INTEGER), 0), created_at
		FROM route_results WHERE run_id = ? ORDER BY
			CASE WHEN COALESCE(CAST(append_seq AS INTEGER), 0) > 0 THEN 0 ELSE 1 END,
			COALESCE(CAST(append_seq AS INTEGER), 0), id`, runID)
	if err != nil {
		return nil, fmt.Errorf("get route results: %w", err)
	}
	defer rows.Close()
	var results []RouteResult
	for rows.Next() {
		var result RouteResult
		if err := rows.Scan(&result.ID, &result.RunID, &result.StepName, &result.Round,
			&result.Phase, &result.Risk, &result.AppendSeq, &result.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan route result: %w", err)
		}
		results = append(results, result)
	}
	return results, rows.Err()
}

func nullable(value string) any {
	if value == "" {
		return sql.NullString{}
	}
	return value
}

// PromptEvidence returns content-free evidence suitable for route records.
// The full prompt never enters the database or process argv.
func PromptEvidence(prompt string) (string, int) {
	sum := sha256.Sum256([]byte(prompt))
	return hex.EncodeToString(sum[:]), len(prompt)
}

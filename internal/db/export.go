package db

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// RunExport is an explicit allowlist of local delivery facts. Do not embed
// storage records: they contain prompts, configuration, logs and private paths.
// All timestamps are stored Unix seconds and all durations are milliseconds.
type RunExport struct {
	SchemaVersion int                `json:"schema_version"`
	Run           ExportRun          `json:"run"`
	Steps         []ExportStep       `json:"steps"`
	Invocations   []ExportInvocation `json:"invocations"`
}

type ExportRun struct {
	ID                 string  `json:"id"`
	RepoID             string  `json:"repo_id"`
	Branch             string  `json:"branch"`
	HeadSHA            string  `json:"head_sha"`
	BaseSHA            string  `json:"base_sha"`
	SubmittedHeadSHA   *string `json:"submitted_head_sha"`
	PRURL              *string `json:"pr_url"`
	Status             string  `json:"status"`
	CreatedAt          int64   `json:"created_at"`
	UpdatedAt          int64   `json:"updated_at"`
	ParkedMS           *int64  `json:"parked_ms"`
	AwaitingAgentSince *int64  `json:"awaiting_agent_since"`
	CIReadyAt          *int64  `json:"ci_ready_at"`
}

type ExportStep struct {
	ID             string        `json:"id"`
	Name           string        `json:"name"`
	Order          int           `json:"order"`
	Status         string        `json:"status"`
	ExitCode       *int          `json:"exit_code"`
	StartedAt      *int64        `json:"started_at"`
	CompletedAt    *int64        `json:"completed_at"`
	DurationMS     *int64        `json:"duration_ms"`
	ResponseOrigin string        `json:"response_origin"`
	CIAttempt      *string       `json:"ci_attempt"`
	Rounds         []ExportRound `json:"rounds"`
}

type ExportRound struct {
	ID                 string     `json:"id"`
	Round              int        `json:"round"`
	Trigger            string     `json:"trigger"`
	CreatedAt          int64      `json:"created_at"`
	DurationMS         int64      `json:"duration_ms"`
	ReviewedHeadSHA    *string    `json:"reviewed_head_sha"`
	SelectionSource    *string    `json:"selection_source"`
	SelectedFindingIDs []string   `json:"selected_finding_ids"`
	ResponseOrigin     string     `json:"response_origin"`
	RepairPublished    bool       `json:"repair_published"`
	Gate               ExportGate `json:"gate"`
}

// ExportGate reserves unknown facts without asserting that a round parked.
// Round creation, execution duration and selection source cannot supply them.
type ExportGate struct {
	FirstParkedAt      *int64  `json:"first_parked_at"`
	Reason             *string `json:"reason"`
	Owner              *string `json:"owner"`
	NotifiedAt         *int64  `json:"notified_at"`
	EscalatedAt        *int64  `json:"escalated_at"`
	RespondedAt        *int64  `json:"responded_at"`
	ResponderIdentity  *string `json:"responder_identity"`
	Action             *string `json:"action"`
	ResolutionEvidence *string `json:"resolution_evidence"`
}

type ExportInvocation struct {
	ID                   string  `json:"id"`
	StepName             string  `json:"step_name"`
	Round                int     `json:"round"`
	Purpose              string  `json:"purpose"`
	Agent                string  `json:"agent"`
	Model                *string `json:"model"`
	ModelProvider        *string `json:"model_provider"`
	SessionMode          string  `json:"session_mode"`
	StartedAt            int64   `json:"started_at"`
	CompletedAt          int64   `json:"completed_at"`
	DurationMS           int64   `json:"duration_ms"`
	SubprocessWaitMS     *int64  `json:"subprocess_wait_ms"`
	ExitStatus           string  `json:"exit_status"`
	FailureCategory      *string `json:"failure_category"`
	InputTokens          *int64  `json:"input_tokens"`
	OutputTokens         *int64  `json:"output_tokens"`
	CacheReadTokens      *int64  `json:"cache_read_tokens"`
	CacheCreationTokens  *int64  `json:"cache_creation_tokens"`
	FreshInputTokens     *int64  `json:"fresh_input_tokens"`
	ReasoningTokens      *int64  `json:"reasoning_tokens"`
	DeltaInputTokens     *int64  `json:"delta_input_tokens"`
	DeltaOutputTokens    *int64  `json:"delta_output_tokens"`
	DeltaCacheReadTokens *int64  `json:"delta_cache_read_tokens"`
}

// ExportRunFacts reads one consistent SQLite snapshot. It neither migrates
// storage nor contacts an agent, daemon, forge or telemetry endpoint.
func (d *DB) ExportRunFacts(ctx context.Context, runID string) (*RunExport, error) {
	tx, err := d.sql.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin run export: %w", err)
	}
	defer tx.Rollback()
	out := &RunExport{SchemaVersion: 1, Steps: []ExportStep{}, Invocations: []ExportInvocation{}}
	r := &out.Run
	err = tx.QueryRowContext(ctx, `SELECT id, repo_id, branch, head_sha, base_sha, submitted_head_sha, pr_url, status, created_at, updated_at, parked_ms, awaiting_agent_since, ci_ready_at FROM runs WHERE id = ?`, runID).Scan(
		&r.ID, &r.RepoID, &r.Branch, &r.HeadSHA, &r.BaseSHA, &r.SubmittedHeadSHA, &r.PRURL, &r.Status, &r.CreatedAt, &r.UpdatedAt, &r.ParkedMS, &r.AwaitingAgentSince, &r.CIReadyAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("run %q not found", runID)
	}
	if err != nil {
		return nil, fmt.Errorf("read export run: %w", err)
	}
	byStep := map[string]int{}
	err = readExportRows(ctx, tx, `SELECT id, step_name, step_order, status, exit_code, started_at, completed_at, duration_ms, response_origin FROM step_results WHERE run_id = ? ORDER BY step_order, id`, runID, func(row scanner) error {
		s := ExportStep{Rounds: []ExportRound{}}
		if err := row.Scan(&s.ID, &s.Name, &s.Order, &s.Status, &s.ExitCode, &s.StartedAt, &s.CompletedAt, &s.DurationMS, &s.ResponseOrigin); err != nil {
			return err
		}
		byStep[s.ID] = len(out.Steps)
		out.Steps = append(out.Steps, s)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read export steps: %w", err)
	}
	err = readExportRows(ctx, tx, `SELECT r.id, r.step_result_id, r.round, r.trigger_type, r.created_at, r.duration_ms, r.reviewed_head_sha, r.selection_source, r.selected_finding_ids, r.response_origin, r.repair_published FROM step_rounds r JOIN step_results s ON s.id = r.step_result_id WHERE s.run_id = ? ORDER BY s.step_order, s.id, r.round, r.id`, runID, func(row scanner) error {
		var round ExportRound
		var stepID string
		var selected *string
		if err := row.Scan(&round.ID, &stepID, &round.Round, &round.Trigger, &round.CreatedAt, &round.DurationMS, &round.ReviewedHeadSHA, &round.SelectionSource, &selected, &round.ResponseOrigin, &round.RepairPublished); err != nil {
			return err
		}
		if selected != nil {
			if err := json.Unmarshal([]byte(*selected), &round.SelectedFindingIDs); err != nil {
				// Never include the stored payload in diagnostics.
				return fmt.Errorf("invalid selected finding IDs for round %s", round.ID)
			}
		}
		i := byStep[stepID]
		out.Steps[i].Rounds = append(out.Steps[i].Rounds, round)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read export rounds: %w", err)
	}
	err = readExportRows(ctx, tx, `SELECT id, step_name, round, purpose, agent, model, model_provider, session_mode, started_at, completed_at, duration_ms, subprocess_wait_ms, exit_status, failure_category, input_tokens, output_tokens, cache_read_tokens, cache_creation_tokens, fresh_input_tokens, reasoning_tokens, delta_input_tokens, delta_output_tokens, delta_cache_read_tokens FROM agent_invocations WHERE run_id = ? ORDER BY started_at, id`, runID, func(row scanner) error {
		var i ExportInvocation
		if err := row.Scan(&i.ID, &i.StepName, &i.Round, &i.Purpose, &i.Agent, &i.Model, &i.ModelProvider, &i.SessionMode, &i.StartedAt, &i.CompletedAt, &i.DurationMS, &i.SubprocessWaitMS, &i.ExitStatus, &i.FailureCategory, &i.InputTokens, &i.OutputTokens, &i.CacheReadTokens, &i.CacheCreationTokens, &i.FreshInputTokens, &i.ReasoningTokens, &i.DeltaInputTokens, &i.DeltaOutputTokens, &i.DeltaCacheReadTokens); err != nil {
			return err
		}
		out.Invocations = append(out.Invocations, i)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read export invocations: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("finish run export: %w", err)
	}
	return out, nil
}

func readExportRows(ctx context.Context, tx *sql.Tx, query, runID string, visit func(scanner) error) error {
	rows, err := tx.QueryContext(ctx, query, runID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := visit(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

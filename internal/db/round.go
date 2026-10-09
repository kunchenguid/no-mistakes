package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

const (
	RoundSelectionSourceUser    = "user"
	RoundSelectionSourceAutoFix = "auto_fix"
	// RoundSelectionSourceUserDeclined records that a human resolved the
	// round's approval gate without selecting any finding to fix: approve,
	// skip, or abort, or a fix response that declined every finding the gate
	// showed (--ignore with no --findings). Before this existed, those three resolutions wrote no
	// finding-level state at all, so "the human declined every finding" and
	// "there were no findings" were the same row and no later step or run
	// could tell them apart. The decline itself is still stored the way a
	// partial selection stores one, as the complement of
	// SelectedFindingIDs, so this source is written with an explicit empty
	// JSON array rather than a NULL.
	RoundSelectionSourceUserDeclined = "user_declined"
)

// DeclinedSelectionJSON is the SelectedFindingIDs value written alongside
// RoundSelectionSourceUserDeclined. It must be an empty JSON *array* and not
// an empty string: readers derive the declined set as
// findings_json minus selected_finding_ids, and a NULL column means "no
// decision was recorded" rather than "nothing was selected".
const DeclinedSelectionJSON = "[]"

// StepRound represents one execution round within a pipeline step.
type StepRound struct {
	ID           string
	StepResultID string
	Round        int
	// Trigger is "initial", "auto_fix", or "answer" (a review turn resumed
	// once every question it left open was answered - not a fix round, since
	// no code changed); legacy "user_fix" is treated as "auto_fix".
	Trigger          string
	FindingsJSON     *string // nullable - findings produced by this round
	ReviewedHeadSHA  *string // non-authoritative commit candidate captured by a review round
	StartingHeadSHA  *string
	TrustedConfigSHA *string
	GlobalConfigYAML []byte
	RepoConfigYAML   []byte
	// UserFindingsJSON, when non-nil, is the merged finding list that was
	// dispatched to the fix agent after the user edited per-finding
	// instructions or added their own findings. It includes both the
	// selected agent-produced findings (with any attached user
	// instructions) and the user-authored findings.
	UserFindingsJSON *string
	// SelectedFindingIDs, when non-nil, is a JSON array of finding IDs that
	// were chosen (by the user or auto-fix filter) to be fixed AFTER this
	// round. It is populated on the round whose findings triggered the next
	// round, so that later rounds' prompts can tell which findings were
	// deliberately left unselected.
	SelectedFindingIDs *string
	SelectionSource    *string
	// FixSummary, when non-nil, records a fix round's result.
	FixSummary      *string
	RepairPublished bool
	DurationMS      int64
	CreatedAt       int64
}

// StepRoundStats summarizes execution rounds for a step. It lets status
// surfaces show whether a running/fixing step is in an initial pass or a fix
// pass without reloading every round in callers.
type StepRoundStats struct {
	TotalRounds        int
	FixRounds          int
	LatestRound        int
	LatestRoundID      string
	LatestTrigger      string
	LatestSelection    string
	LatestRoundAt      int64
	LatestFixRound     int
	LatestFixRoundAt   int64
	SelectedForFix     bool
	AutoSelectedForFix bool
	PendingFixSource   string
}

// IsFixRound reports whether this round was a fix attempt. Legacy "user_fix"
// rounds count: they were fix rounds dispatched by an explicit user selection.
func (r *StepRound) IsFixRound() bool {
	return r.Trigger == "auto_fix" || r.Trigger == "user_fix"
}

// StepFixSummaries returns one result per fix round for a step, in round order.
func (d *DB) StepFixSummaries(stepResultID string) ([]string, error) {
	rounds, err := d.GetRoundsByStep(stepResultID)
	if err != nil {
		return nil, err
	}
	var summaries []string
	for _, r := range rounds {
		if !r.IsFixRound() {
			continue
		}
		summary := ""
		if r.FixSummary != nil {
			summary = *r.FixSummary
		}
		summaries = append(summaries, summary)
	}
	return summaries, nil
}

// StepRoundStats returns aggregate round information for a step result.
func (d *DB) StepRoundStats(stepResultID string) (StepRoundStats, error) {
	rounds, err := d.GetRoundsByStep(stepResultID)
	if err != nil {
		return StepRoundStats{}, err
	}
	var stats StepRoundStats
	latestSelectedRound := 0
	latestSelectedSource := ""
	for _, r := range rounds {
		stats.TotalRounds++
		stats.LatestRound = r.Round
		stats.LatestRoundID = r.ID
		stats.LatestTrigger = r.Trigger
		stats.LatestRoundAt = r.CreatedAt
		if r.SelectionSource != nil {
			stats.LatestSelection = *r.SelectionSource
		}
		if hasSelectedFinding(r.SelectedFindingIDs) {
			stats.SelectedForFix = true
			stats.AutoSelectedForFix = r.SelectionSource != nil && *r.SelectionSource == RoundSelectionSourceAutoFix
			latestSelectedRound = r.Round
			latestSelectedSource = stats.LatestSelection
		}
		if r.IsFixRound() {
			stats.FixRounds++
			stats.LatestFixRound = stats.FixRounds
			stats.LatestFixRoundAt = r.CreatedAt
		}
	}
	if latestSelectedRound == stats.LatestRound {
		stats.PendingFixSource = latestSelectedSource
	}
	return stats, nil
}

func hasSelectedFinding(raw *string) bool {
	if raw == nil {
		return false
	}
	var ids []string
	if json.Unmarshal([]byte(*raw), &ids) != nil {
		return false
	}
	for _, id := range ids {
		if id != "" {
			return true
		}
	}
	return false
}

// InsertStepRound creates a new round record for a step result. fixSummary may
// be nil for non-fix rounds or when the agent produced no summary.
func (d *DB) InsertStepRound(stepResultID string, round int, trigger string, findingsJSON *string, fixSummary *string, durationMS int64) (*StepRound, error) {
	return d.InsertStepRoundWithRepair(stepResultID, round, trigger, findingsJSON, fixSummary, false, durationMS)
}

func (d *DB) InsertStepRoundWithRepair(stepResultID string, round int, trigger string, findingsJSON *string, fixSummary *string, repairPublished bool, durationMS int64) (*StepRound, error) {
	return d.insertStepRound(stepResultID, round, trigger, findingsJSON, fixSummary, nil, nil, nil, nil, nil, repairPublished, durationMS)
}

// InsertReviewStepRound persists a review round's examined commit as a
// non-authoritative candidate. A recovered parked gate can promote this exact
// candidate only after approval; merely storing it grants no push authority.
func (d *DB) InsertReviewStepRound(stepResultID string, round int, trigger string, findingsJSON *string, fixSummary *string, reviewedHeadSHA string, durationMS int64) (*StepRound, error) {
	return d.InsertReviewStepRoundWithProvenance(stepResultID, round, trigger, findingsJSON, fixSummary, reviewedHeadSHA, "", "", nil, nil, durationMS)
}

func (d *DB) InsertReviewStepRoundWithProvenance(stepResultID string, round int, trigger string, findingsJSON *string, fixSummary *string, reviewedHeadSHA, startingHeadSHA, trustedConfigSHA string, globalConfigYAML, repoConfigYAML []byte, durationMS int64) (*StepRound, error) {
	var reviewed, starting, trusted *string
	if reviewedHeadSHA != "" {
		reviewed = &reviewedHeadSHA
	}
	if startingHeadSHA != "" {
		starting = &startingHeadSHA
	}
	if trustedConfigSHA != "" {
		trusted = &trustedConfigSHA
	}
	return d.insertStepRound(stepResultID, round, trigger, findingsJSON, fixSummary, reviewed, starting, trusted, globalConfigYAML, repoConfigYAML, false, durationMS)
}

func (d *DB) insertStepRound(stepResultID string, round int, trigger string, findingsJSON *string, fixSummary, reviewedHeadSHA, startingHeadSHA, trustedConfigSHA *string, globalConfigYAML, repoConfigYAML []byte, repairPublished bool, durationMS int64) (*StepRound, error) {
	r := &StepRound{
		ID:               newID(),
		StepResultID:     stepResultID,
		Round:            round,
		Trigger:          trigger,
		FindingsJSON:     findingsJSON,
		ReviewedHeadSHA:  reviewedHeadSHA,
		StartingHeadSHA:  startingHeadSHA,
		TrustedConfigSHA: trustedConfigSHA,
		GlobalConfigYAML: append([]byte(nil), globalConfigYAML...),
		RepoConfigYAML:   append([]byte(nil), repoConfigYAML...),
		FixSummary:       fixSummary,
		RepairPublished:  repairPublished,
		DurationMS:       durationMS,
		CreatedAt:        now(),
	}
	_, err := d.sql.Exec(
		`INSERT INTO step_rounds (id, step_result_id, round, trigger_type, findings_json, reviewed_head_sha, starting_head_sha, trusted_config_sha, global_config_yaml, repo_config_yaml, user_findings_json, selected_finding_ids, selection_source, fix_summary, repair_published, duration_ms, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.StepResultID, r.Round, r.Trigger, r.FindingsJSON, r.ReviewedHeadSHA, r.StartingHeadSHA, r.TrustedConfigSHA, r.GlobalConfigYAML, r.RepoConfigYAML, r.UserFindingsJSON, r.SelectedFindingIDs, r.SelectionSource, r.FixSummary, r.RepairPublished, r.DurationMS, r.CreatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("insert step round: %w", err)
	}
	return r, nil
}

// SetStepRoundSelection records which findings were selected for fix AFTER the
// given round produced its findings, along with whether that selection came
// from the user or auto-fix filtering.
//
// Passing nil or an empty *string* clears both columns, which means "no
// decision was recorded". Passing DeclinedSelectionJSON ("[]") with a source
// is different and deliberate: it records a real decision that selected
// nothing, so readers can distinguish a decline from an unresolved round.
func (d *DB) SetStepRoundSelection(id string, selectedFindingIDs *string, source string) error {
	var selectionSource *string
	if selectedFindingIDs != nil && *selectedFindingIDs != "" && source != "" {
		selectionSource = &source
	}
	if _, err := d.sql.Exec(
		`UPDATE step_rounds SET selected_finding_ids = ?, selection_source = ? WHERE id = ?`,
		selectedFindingIDs, selectionSource, id,
	); err != nil {
		return fmt.Errorf("set step round selection: %w", err)
	}
	return nil
}

// SetStepRoundUserDecision records the user's resolution of a round: which
// findings were selected for fix, how the selection was made, and the merged
// finding list dispatched to the fix agent. The same empty-string versus
// DeclinedSelectionJSON distinction described on SetStepRoundSelection
// applies here. The decline set is derived on read as the complement of the
// selection (minus any finding an earlier user round of the same step chose to
// fix), never stored, so there is no decline column to keep in step with it.
func (d *DB) SetStepRoundUserDecision(id string, selectedFindingIDs *string, source string, userFindingsJSON *string) error {
	var selectionSource *string
	if selectedFindingIDs != nil && *selectedFindingIDs != "" && source != "" {
		selectionSource = &source
	}
	if _, err := d.sql.Exec(
		`UPDATE step_rounds SET selected_finding_ids = ?, selection_source = ?, user_findings_json = ? WHERE id = ?`,
		selectedFindingIDs, selectionSource, userFindingsJSON, id,
	); err != nil {
		return fmt.Errorf("set step round user decision: %w", err)
	}
	return nil
}

// SetStepRoundSelectedFindingIDs preserves the old API for callers that do not
// need to distinguish how the selection was made.
func (d *DB) SetStepRoundSelectedFindingIDs(id string, selectedFindingIDs *string) error {
	return d.SetStepRoundSelection(id, selectedFindingIDs, RoundSelectionSourceUser)
}

// SetStepRoundUserFindings records the merged finding list (with user
// instructions attached and user-added findings appended) that was
// dispatched to the fix agent for the round. Passing nil clears the column.
func (d *DB) SetStepRoundUserFindings(id string, userFindingsJSON *string) error {
	if _, err := d.sql.Exec(
		`UPDATE step_rounds SET user_findings_json = ? WHERE id = ?`,
		userFindingsJSON, id,
	); err != nil {
		return fmt.Errorf("set step round user findings: %w", err)
	}
	return nil
}

// GetRoundsByStep returns all rounds for a step result, ordered by round number.
func (d *DB) GetRoundsByStep(stepResultID string) ([]*StepRound, error) {
	rows, err := d.sql.Query(
		`SELECT id, step_result_id, round, trigger_type, findings_json, reviewed_head_sha, starting_head_sha, trusted_config_sha, global_config_yaml, repo_config_yaml, user_findings_json, selected_finding_ids, selection_source, fix_summary, repair_published, duration_ms, created_at FROM step_rounds WHERE step_result_id = ? ORDER BY round`,
		stepResultID,
	)
	if err != nil {
		return nil, fmt.Errorf("get rounds by step: %w", err)
	}
	defer rows.Close()
	var rounds []*StepRound
	for rows.Next() {
		r := &StepRound{}
		if err := rows.Scan(&r.ID, &r.StepResultID, &r.Round, &r.Trigger, &r.FindingsJSON, &r.ReviewedHeadSHA, &r.StartingHeadSHA, &r.TrustedConfigSHA, &r.GlobalConfigYAML, &r.RepoConfigYAML, &r.UserFindingsJSON, &r.SelectedFindingIDs, &r.SelectionSource, &r.FixSummary, &r.RepairPublished, &r.DurationMS, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan step round: %w", err)
		}
		rounds = append(rounds, r)
	}
	return rounds, rows.Err()
}

// BoundResponseDecision is the exact durable input for one response-scoped CI
// revalidation operation. The selected findings and merged payload are written
// in the same statement as the operation receipt, so an acknowledged request
// can never exist without the fixer input it promised.
type BoundResponseDecision struct {
	OperationID      string
	Fingerprint      string
	RunID            string
	RepoID           string
	Branch           string
	HeadSHA          string
	StepResultID     string
	RoundID          string
	SelectedIDsJSON  string
	UserFindingsJSON string
	DispositionsJSON string
}

// BoundResponseOperation is the queryable receipt for an accepted operation.
type BoundResponseOperation struct {
	OperationID      string
	Fingerprint      string
	RunID            string
	RepoID           string
	Branch           string
	HeadSHA          string
	StepResultID     string
	RoundID          string
	DispositionsJSON string
	AcceptedAt       int64
}

// RecordBoundResponseDecision atomically records a tighten-only response on
// one exact active, published CI gate. All four validation steps must have
// completed on the current head; a run that skipped one cannot claim this
// operation will perform a full Review-through-Lint revalidation.
func (d *DB) RecordBoundResponseDecision(decision BoundResponseDecision) (bool, error) {
	if strings.TrimSpace(decision.OperationID) == "" || strings.TrimSpace(decision.Fingerprint) == "" ||
		decision.RunID == "" || decision.RepoID == "" || decision.Branch == "" || decision.HeadSHA == "" ||
		decision.StepResultID == "" || decision.RoundID == "" || decision.SelectedIDsJSON == "" ||
		decision.UserFindingsJSON == "" || decision.DispositionsJSON == "" {
		return false, nil
	}
	ts := now()
	result, err := d.sql.Exec(`UPDATE step_rounds
		SET selected_finding_ids = ?, selection_source = ?, user_findings_json = ?,
			response_operation_id = ?, response_fingerprint = ?, response_repo_id = ?,
			response_branch = ?, response_head_sha = ?,
			response_dispositions_json = ?, response_accepted_at = COALESCE(response_accepted_at, ?)
		WHERE id = ? AND step_result_id = ?
			AND (response_operation_id IS NULL OR (
				response_operation_id = ? AND response_fingerprint = ? AND response_repo_id = ?
				AND response_branch = ? AND response_head_sha = ?
				AND response_dispositions_json = ?))
			AND EXISTS (
				SELECT 1 FROM step_results current
				JOIN runs run ON run.id = current.run_id
				WHERE current.id = step_rounds.step_result_id AND current.id = ?
					AND current.step_name = ? AND current.status IN (?, ?)
					AND run.id = ? AND run.repo_id = ? AND run.branch = ? AND run.head_sha = ?
					AND run.status = ? AND run.awaiting_agent_since IS NOT NULL
					AND run.pr_url IS NOT NULL AND run.last_pushed_sha = run.head_sha
					AND NOT EXISTS (
						SELECT 1 FROM step_results required
						WHERE required.run_id = run.id
							AND required.step_name IN (?, ?, ?, ?)
							AND required.status <> ?
					)
					AND 4 = (
						SELECT COUNT(*) FROM step_results required
						WHERE required.run_id = run.id
							AND required.step_name IN (?, ?, ?, ?)
					)
			)`,
		decision.SelectedIDsJSON, RoundSelectionSourceUser, decision.UserFindingsJSON,
		decision.OperationID, decision.Fingerprint, decision.RepoID, decision.Branch, decision.HeadSHA,
		decision.DispositionsJSON, ts,
		decision.RoundID, decision.StepResultID,
		decision.OperationID, decision.Fingerprint, decision.RepoID, decision.Branch, decision.HeadSHA, decision.DispositionsJSON,
		decision.StepResultID, types.StepCI, types.StepStatusAwaitingApproval, types.StepStatusFixReview,
		decision.RunID, decision.RepoID, decision.Branch, decision.HeadSHA, types.RunRunning,
		types.StepReview, types.StepTest, types.StepDocument, types.StepLint, types.StepStatusCompleted,
		types.StepReview, types.StepTest, types.StepDocument, types.StepLint,
	)
	if err != nil {
		return false, fmt.Errorf("record bound response decision: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("record bound response decision: %w", err)
	}
	return affected == 1, nil
}

// GetBoundResponseOperation returns the immutable receipt for operationID.
func (d *DB) GetBoundResponseOperation(operationID string) (*BoundResponseOperation, error) {
	if strings.TrimSpace(operationID) == "" {
		return nil, nil
	}
	op := &BoundResponseOperation{}
	err := d.sql.QueryRow(`SELECT round.response_operation_id, round.response_fingerprint,
		run.id, round.response_repo_id, round.response_branch, round.response_head_sha,
		round.step_result_id, round.id, round.response_dispositions_json,
		round.response_accepted_at
		FROM step_rounds round
		JOIN step_results step ON step.id = round.step_result_id
		JOIN runs run ON run.id = step.run_id
		WHERE round.response_operation_id = ?`, operationID).Scan(
		&op.OperationID, &op.Fingerprint, &op.RunID, &op.RepoID, &op.Branch, &op.HeadSHA,
		&op.StepResultID, &op.RoundID, &op.DispositionsJSON, &op.AcceptedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("get bound response operation: %w", err)
	}
	return op, nil
}

// GetBoundResponseOperationIDForRound reports the immutable operation already
// accepted on roundID. A recovered receipt-bearing gate may only consume that
// exact bound retry; an ordinary response must not replace its fingerprinted
// payload or drop its full-revalidation demand.
func (d *DB) GetBoundResponseOperationIDForRound(roundID string) (string, error) {
	if strings.TrimSpace(roundID) == "" {
		return "", nil
	}
	var operationID sql.NullString
	err := d.sql.QueryRow(`SELECT response_operation_id FROM step_rounds WHERE id = ?`, roundID).Scan(&operationID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("get bound response operation for round: %w", err)
	}
	return strings.TrimSpace(operationID.String), nil
}

// GetBoundResponseRoundIDsByStep returns the rounds on stepResultID that own a
// bound response receipt. Receipt details stay on the dedicated operation
// query; recovery needs only these markers to recognize a contiguous retained
// protected-path retry without making the one-shot demand sticky forever.
func (d *DB) GetBoundResponseRoundIDsByStep(stepResultID string) ([]string, error) {
	rows, err := d.sql.Query(`SELECT id FROM step_rounds WHERE step_result_id = ? AND response_operation_id IS NOT NULL ORDER BY round`, stepResultID)
	if err != nil {
		return nil, fmt.Errorf("get bound response rounds by step: %w", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan bound response round: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

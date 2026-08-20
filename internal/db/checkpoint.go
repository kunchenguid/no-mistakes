package db

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// ValidationCheckpoint binds reusable validation to exact mechanical
// evidence. EvidenceHashes is opaque to the database; the pipeline owns its
// keys and recomputes every value before reuse.
type ValidationCheckpoint struct {
	RunID           string
	Version         int
	ValidatedSHA    string
	BaseSHA         string
	ConfigHash      string
	IntentHash      string
	EvidenceHashes  map[string]string
	ReusedFromRunID *string
	CreatedAt       int64
}

func (d *DB) PutValidationCheckpoint(checkpoint *ValidationCheckpoint) error {
	if checkpoint == nil {
		return fmt.Errorf("validation checkpoint is nil")
	}
	raw, err := json.Marshal(checkpoint.EvidenceHashes)
	if err != nil {
		return fmt.Errorf("marshal validation checkpoint evidence: %w", err)
	}
	createdAt := checkpoint.CreatedAt
	if createdAt == 0 {
		createdAt = now()
	}
	_, err = d.sql.Exec(`INSERT INTO validation_checkpoints
		(run_id, version, validated_sha, base_sha, config_hash, intent_hash, evidence_hashes, reused_from_run_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(run_id) DO UPDATE SET
			version = excluded.version,
			validated_sha = excluded.validated_sha,
			base_sha = excluded.base_sha,
			config_hash = excluded.config_hash,
			intent_hash = excluded.intent_hash,
			evidence_hashes = excluded.evidence_hashes,
			reused_from_run_id = excluded.reused_from_run_id,
			created_at = excluded.created_at`,
		checkpoint.RunID, checkpoint.Version, checkpoint.ValidatedSHA, checkpoint.BaseSHA,
		checkpoint.ConfigHash, checkpoint.IntentHash, string(raw), checkpoint.ReusedFromRunID, createdAt)
	if err != nil {
		return fmt.Errorf("put validation checkpoint: %w", err)
	}
	checkpoint.CreatedAt = createdAt
	return nil
}

func (d *DB) GetValidationCheckpoint(runID string) (*ValidationCheckpoint, error) {
	checkpoint := &ValidationCheckpoint{RunID: runID}
	var raw string
	err := d.sql.QueryRow(`SELECT version, validated_sha, base_sha, config_hash, intent_hash,
		evidence_hashes, reused_from_run_id, created_at
		FROM validation_checkpoints WHERE run_id = ?`, runID).Scan(
		&checkpoint.Version, &checkpoint.ValidatedSHA, &checkpoint.BaseSHA,
		&checkpoint.ConfigHash, &checkpoint.IntentHash, &raw,
		&checkpoint.ReusedFromRunID, &checkpoint.CreatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get validation checkpoint: %w", err)
	}
	if err := json.Unmarshal([]byte(raw), &checkpoint.EvidenceHashes); err != nil {
		return nil, fmt.Errorf("decode validation checkpoint evidence: %w", err)
	}
	return checkpoint, nil
}

func (d *DB) DeleteValidationCheckpoint(runID string) error {
	if _, err := d.sql.Exec(`DELETE FROM validation_checkpoints WHERE run_id = ?`, runID); err != nil {
		return fmt.Errorf("delete validation checkpoint: %w", err)
	}
	return nil
}

// FailRunAndInvalidateValidationCheckpoint makes dirty/uncertain worktree
// invalidation and terminalization one durable decision. A retry can observe
// the failed run only after its optional reuse authority is gone.
func (d *DB) FailRunAndInvalidateValidationCheckpoint(runID, errMsg string, status types.RunStatus, verifiedHead *string) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return fmt.Errorf("begin checkpoint invalidation: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM validation_checkpoints WHERE run_id = ?`, runID); err != nil {
		return fmt.Errorf("invalidate validation checkpoint: %w", err)
	}
	ts := now()
	var result sql.Result
	if verifiedHead != nil {
		result, err = tx.Exec(`UPDATE runs SET error = ?, status = ?, head_sha = ?, push_active = 0,
			terminal_head_verified_at = ?, updated_at = ? WHERE id = ?`, errMsg, status, *verifiedHead, ts, ts, runID)
	} else {
		result, err = tx.Exec(`UPDATE runs SET error = ?, status = ?, push_active = 0,
			terminal_head_verified_at = NULL, updated_at = ? WHERE id = ?`, errMsg, status, ts, runID)
	}
	if err != nil {
		return fmt.Errorf("terminalize invalidated run: %w", err)
	}
	if changed, _ := result.RowsAffected(); changed != 1 {
		return fmt.Errorf("terminalize invalidated run: run not found")
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit checkpoint invalidation: %w", err)
	}
	return nil
}

func (d *DB) FailActiveRecoveredRun(runID, errMsg string, invalidateCheckpoint bool) (bool, error) {
	tx, err := d.sql.Begin()
	if err != nil {
		return false, fmt.Errorf("begin recovered run failure: %w", err)
	}
	defer tx.Rollback()
	ts := now()
	result, err := tx.Exec(`UPDATE runs SET error = ?, status = ?, push_active = 0,
		terminal_head_verified_at = NULL, updated_at = ?
		WHERE id = ? AND status IN (?, ?)`, errMsg, types.RunFailed, ts, runID, types.RunPending, types.RunRunning)
	if err != nil {
		return false, fmt.Errorf("terminalize active recovered run: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("inspect recovered run terminalization: %w", err)
	}
	if changed == 0 {
		return false, nil
	}
	if changed != 1 {
		return false, fmt.Errorf("terminalize active recovered run: changed %d rows", changed)
	}
	if invalidateCheckpoint {
		if _, err := tx.Exec(`DELETE FROM validation_checkpoints WHERE run_id = ?`, runID); err != nil {
			return false, fmt.Errorf("invalidate recovered validation checkpoint: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("commit recovered run failure: %w", err)
	}
	return true, nil
}

// RearmDeliveryAfterCrash converts the one interrupted delivery step back to
// pending and clears the crash-stale push-active marker. Completed delivery
// steps remain completed, so the executor resumes at the first unfinished
// push/PR/CI boundary.
func (d *DB) RearmDeliveryAfterCrash(runID string) (int, error) {
	tx, err := d.sql.Begin()
	if err != nil {
		return 0, fmt.Errorf("begin rearm delivery: %w", err)
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT id, step_name, step_order, status FROM step_results
		WHERE run_id = ? AND step_order >= ? ORDER BY step_order`, runID, types.StepPush.Order())
	if err != nil {
		return 0, fmt.Errorf("read delivery steps: %w", err)
	}
	type deliveryRow struct {
		id     string
		name   types.StepName
		order  int
		status types.StepStatus
	}
	var delivery []deliveryRow
	for rows.Next() {
		var row deliveryRow
		if err := rows.Scan(&row.id, &row.name, &row.order, &row.status); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan delivery step: %w", err)
		}
		delivery = append(delivery, row)
	}
	if err := rows.Close(); err != nil {
		return 0, fmt.Errorf("close delivery steps: %w", err)
	}
	if len(delivery) != 3 {
		return 0, fmt.Errorf("rearm delivery: expected 3 delivery steps, got %d", len(delivery))
	}
	start := -1
	for _, row := range delivery {
		if row.status == types.StepStatusCompleted {
			if start >= 0 {
				return 0, fmt.Errorf("rearm delivery: completed step after unfinished step")
			}
			continue
		}
		if start < 0 {
			if row.name == types.StepCI && row.status == types.StepStatusRunning {
				return 0, fmt.Errorf("rearm delivery: interrupted CI monitor has volatile retry state")
			}
			start = row.order - 1
			if row.status == types.StepStatusRunning {
				if _, err := tx.Exec(`UPDATE step_results SET status = ?, agent_pid = NULL,
					last_activity_at = ?, last_activity = ? WHERE id = ?`,
					types.StepStatusPending, now(), "delivery rearmed after daemon crash", row.id); err != nil {
					return 0, fmt.Errorf("rearm delivery step %s: %w", row.name, err)
				}
			} else if row.status != types.StepStatusPending {
				return 0, fmt.Errorf("rearm delivery: step %s is %s", row.name, row.status)
			}
			continue
		}
		if row.status != types.StepStatusPending {
			return 0, fmt.Errorf("rearm delivery: later step %s is %s", row.name, row.status)
		}
	}
	if start < 0 {
		start = len(types.AllSteps())
	} else if start < types.StepPush.Order()-1 {
		return 0, fmt.Errorf("rearm delivery: invalid unfinished delivery step")
	}
	result, err := tx.Exec(`UPDATE runs SET status = ?, error = NULL, push_active = 0,
		awaiting_agent_since = NULL, updated_at = ? WHERE id = ? AND status IN (?, ?)`,
		types.RunRunning, now(), runID, types.RunPending, types.RunRunning)
	if err != nil {
		return 0, fmt.Errorf("rearm delivery run: %w", err)
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("inspect rearmed delivery run: %w", err)
	}
	if changed != 1 {
		return 0, fmt.Errorf("rearm delivery run: active run changed concurrently")
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit rearmed delivery: %w", err)
	}
	return start, nil
}

// CloneValidatedSteps copies the already-certified pre-delivery history into
// a fresh run and creates pending delivery rows in one transaction. The caller
// first copies and verifies the referenced log/evidence files. Any crash before
// this commit leaves an ordinary fresh run; any crash after it leaves a fully
// reconstructable delivery-resume run.
func (d *DB) CloneValidatedSteps(sourceRunID, targetRunID, targetLogDir string, checkpoint *ValidationCheckpoint) error {
	if checkpoint == nil || checkpoint.RunID != sourceRunID {
		return fmt.Errorf("clone validated steps: source checkpoint mismatch")
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return fmt.Errorf("begin clone validated steps: %w", err)
	}
	defer tx.Rollback()

	type runBoundary struct {
		repoID, branch, head, base  string
		version, build              sql.NullString
		status                      types.RunStatus
		custody, terminal, awaiting sql.NullInt64
	}
	readBoundary := func(runID string) (runBoundary, error) {
		var boundary runBoundary
		err := tx.QueryRow(`SELECT repo_id, branch, head_sha, base_sha,
			no_mistakes_version, no_mistakes_build_sha, status,
			custody_returned_at, terminal_head_verified_at, awaiting_agent_since
			FROM runs WHERE id = ?`, runID).Scan(
			&boundary.repoID, &boundary.branch, &boundary.head, &boundary.base,
			&boundary.version, &boundary.build, &boundary.status,
			&boundary.custody, &boundary.terminal, &boundary.awaiting,
		)
		return boundary, err
	}
	sourceBoundary, err := readBoundary(sourceRunID)
	if err != nil {
		return fmt.Errorf("clone validated steps: read source boundary: %w", err)
	}
	targetBoundary, err := readBoundary(targetRunID)
	if err != nil {
		return fmt.Errorf("clone validated steps: read target boundary: %w", err)
	}
	if sourceBoundary.status != types.RunFailed || sourceBoundary.custody.Valid || !sourceBoundary.terminal.Valid ||
		targetBoundary.status != types.RunPending || targetBoundary.custody.Valid || targetBoundary.awaiting.Valid {
		return fmt.Errorf("clone validated steps: run boundary changed")
	}
	if sourceBoundary.repoID != targetBoundary.repoID || sourceBoundary.branch != targetBoundary.branch ||
		sourceBoundary.head != checkpoint.ValidatedSHA || targetBoundary.head != checkpoint.ValidatedSHA ||
		sourceBoundary.base != checkpoint.BaseSHA || targetBoundary.base != checkpoint.BaseSHA ||
		!sourceBoundary.version.Valid || !targetBoundary.version.Valid || sourceBoundary.version.String != targetBoundary.version.String ||
		!sourceBoundary.build.Valid || !targetBoundary.build.Valid || sourceBoundary.build.String != targetBoundary.build.String {
		return fmt.Errorf("clone validated steps: commit, base, build, or branch changed")
	}
	var latestRunID string
	if err := tx.QueryRow(`SELECT id FROM runs WHERE repo_id = ? AND branch = ?
		ORDER BY created_at DESC, id DESC LIMIT 1`, targetBoundary.repoID, targetBoundary.branch).Scan(&latestRunID); err != nil {
		return fmt.Errorf("clone validated steps: read latest branch run: %w", err)
	}
	if latestRunID != targetRunID {
		return fmt.Errorf("clone validated steps: target was superseded")
	}

	var existing int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM step_results WHERE run_id = ?`, targetRunID).Scan(&existing); err != nil {
		return fmt.Errorf("count target steps: %w", err)
	}
	if existing != 0 {
		return fmt.Errorf("clone validated steps: target already has %d step records", existing)
	}

	rows, err := tx.Query(`SELECT `+stepResultColumns+` FROM step_results
		WHERE run_id = ? AND step_order <= ? ORDER BY step_order`, sourceRunID, types.StepLint.Order())
	if err != nil {
		return fmt.Errorf("list source validation steps: %w", err)
	}
	var sourceSteps []*StepResult
	for rows.Next() {
		step := &StepResult{}
		if err := rows.Scan(&step.ID, &step.RunID, &step.StepName, &step.StepOrder, &step.Status,
			&step.ExitCode, &step.DurationMS, &step.LogPath, &step.FindingsJSON, &step.Error,
			&step.StartedAt, &step.CompletedAt, &step.LastActivityAt, &step.LastActivity,
			&step.AgentPID, &step.AutoFixLimit); err != nil {
			rows.Close()
			return fmt.Errorf("scan source validation step: %w", err)
		}
		sourceSteps = append(sourceSteps, step)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close source validation steps: %w", err)
	}
	if len(sourceSteps) != types.StepLint.Order() {
		return fmt.Errorf("clone validated steps: source has %d validation steps", len(sourceSteps))
	}

	for _, source := range sourceSteps {
		targetStepID := newID()
		logPath := filepath.Join(targetLogDir, string(source.StepName)+".log")
		if _, err := tx.Exec(`INSERT INTO step_results
			(id, run_id, step_name, step_order, status, exit_code, duration_ms, log_path,
			 findings_json, error, started_at, completed_at, last_activity_at, last_activity,
			 agent_pid, auto_fix_limit)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, ?)`,
			targetStepID, targetRunID, source.StepName, source.StepOrder, source.Status,
			source.ExitCode, source.DurationMS, logPath, source.FindingsJSON, source.Error,
			source.StartedAt, source.CompletedAt, source.LastActivityAt, source.LastActivity,
			source.AutoFixLimit); err != nil {
			return fmt.Errorf("clone validation step %s: %w", source.StepName, err)
		}

		roundRows, err := tx.Query(`SELECT round, trigger_type, findings_json, reviewed_head_sha,
			starting_head_sha, trusted_config_sha, global_config_yaml, repo_config_yaml,
			user_findings_json, selected_finding_ids, selection_source, fix_summary,
			duration_ms, created_at FROM step_rounds WHERE step_result_id = ? ORDER BY round`, source.ID)
		if err != nil {
			return fmt.Errorf("list %s rounds: %w", source.StepName, err)
		}
		for roundRows.Next() {
			var round int
			var trigger string
			var findings, reviewed, starting, trusted, userFindings, selected, selection, summary sql.NullString
			var globalYAML, repoYAML []byte
			var duration, created int64
			if err := roundRows.Scan(&round, &trigger, &findings, &reviewed, &starting, &trusted,
				&globalYAML, &repoYAML, &userFindings, &selected, &selection, &summary,
				&duration, &created); err != nil {
				roundRows.Close()
				return fmt.Errorf("scan %s round: %w", source.StepName, err)
			}
			if _, err := tx.Exec(`INSERT INTO step_rounds
				(id, step_result_id, round, trigger_type, findings_json, reviewed_head_sha,
				 starting_head_sha, trusted_config_sha, global_config_yaml, repo_config_yaml,
				 user_findings_json, selected_finding_ids, selection_source, fix_summary,
				 duration_ms, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				newID(), targetStepID, round, trigger, checkpointNullableString(findings), checkpointNullableString(reviewed),
				checkpointNullableString(starting), checkpointNullableString(trusted), globalYAML, repoYAML,
				checkpointNullableString(userFindings), checkpointNullableString(selected), checkpointNullableString(selection),
				checkpointNullableString(summary), duration, created); err != nil {
				roundRows.Close()
				return fmt.Errorf("clone %s round: %w", source.StepName, err)
			}
		}
		if err := roundRows.Close(); err != nil {
			return fmt.Errorf("close %s rounds: %w", source.StepName, err)
		}
	}

	for _, name := range []types.StepName{types.StepPush, types.StepPR, types.StepCI} {
		if _, err := tx.Exec(`INSERT INTO step_results (id, run_id, step_name, step_order, status)
			VALUES (?, ?, ?, ?, ?)`, newID(), targetRunID, name, name.Order(), types.StepStatusPending); err != nil {
			return fmt.Errorf("insert pending delivery step %s: %w", name, err)
		}
	}

	var approved sql.NullString
	if err := tx.QueryRow(`SELECT review_approved_head_sha FROM runs WHERE id = ?`, sourceRunID).Scan(&approved); err != nil {
		return fmt.Errorf("read source review authority: %w", err)
	}
	if !approved.Valid || approved.String == "" {
		return fmt.Errorf("clone validated steps: source has no review authority")
	}
	if result, err := tx.Exec(`UPDATE runs SET review_approved_head_sha = ?, updated_at = ? WHERE id = ?`, approved.String, now(), targetRunID); err != nil {
		return fmt.Errorf("copy review authority: %w", err)
	} else if changed, _ := result.RowsAffected(); changed != 1 {
		return fmt.Errorf("copy review authority: target run not found")
	}

	evidenceJSON, err := json.Marshal(checkpoint.EvidenceHashes)
	if err != nil {
		return fmt.Errorf("marshal cloned checkpoint evidence: %w", err)
	}
	if _, err := tx.Exec(`INSERT INTO validation_checkpoints
		(run_id, version, validated_sha, base_sha, config_hash, intent_hash, evidence_hashes, reused_from_run_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, targetRunID, checkpoint.Version,
		checkpoint.ValidatedSHA, checkpoint.BaseSHA, checkpoint.ConfigHash, checkpoint.IntentHash,
		string(evidenceJSON), sourceRunID, now()); err != nil {
		return fmt.Errorf("insert cloned validation checkpoint: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit cloned validation steps: %w", err)
	}
	return nil
}

// ResetValidationReuse removes only imported/prepared pipeline state so the
// same pending run can fall back to an ordinary full execution when its
// evidence changes between preparation and executor start.
func (d *DB) ResetValidationReuse(runID string) error {
	tx, err := d.sql.Begin()
	if err != nil {
		return fmt.Errorf("begin reset validation reuse: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM validation_checkpoints WHERE run_id = ?`, runID); err != nil {
		return fmt.Errorf("delete validation checkpoint: %w", err)
	}
	if _, err := tx.Exec(`DELETE FROM step_results WHERE run_id = ?`, runID); err != nil {
		return fmt.Errorf("delete prepared steps: %w", err)
	}
	if _, err := tx.Exec(`UPDATE runs SET review_approved_head_sha = NULL, updated_at = ? WHERE id = ?`, now(), runID); err != nil {
		return fmt.Errorf("clear prepared review authority: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit reset validation reuse: %w", err)
	}
	return nil
}

func checkpointNullableString(value sql.NullString) any {
	if !value.Valid {
		return nil
	}
	return value.String
}

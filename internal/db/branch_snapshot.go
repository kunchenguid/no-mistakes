package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// BranchRunSnapshot holds one run and its pinned plan inputs from the same read
// transaction as the complete branch inventory. It contains no live Git facts.
type BranchRunSnapshot struct {
	Run       *Run
	GatesJSON string
	Steps     []*StepResult
}

// GetBranchSnapshot reads every run on exactly one repository and branch,
// newest first under (created_at DESC, id DESC), and their step states in one
// SQLite snapshot. No active-run preference or inventory limit is applied.
func (d *DB) GetBranchSnapshot(ctx context.Context, repoID, branch string) ([]BranchRunSnapshot, error) {
	tx, err := d.sql.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin branch snapshot: %w", err)
	}
	defer tx.Rollback()
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT 1 FROM repos WHERE id = ?`, repoID).Scan(&exists); err != nil {
		return nil, fmt.Errorf("branch snapshot repository: %w", err)
	}
	// Select only snapshot facts. Optional launch metadata added in newer
	// releases must not require a migration just to read publication history.
	schema, err := tx.QueryContext(ctx, `SELECT name FROM pragma_table_info('runs')`)
	if err != nil {
		return nil, fmt.Errorf("read run schema: %w", err)
	}
	columns := make(map[string]bool)
	for schema.Next() {
		var name string
		if err := schema.Scan(&name); err != nil {
			schema.Close()
			return nil, fmt.Errorf("read run schema: %w", err)
		}
		columns[name] = true
	}
	err = schema.Err()
	schema.Close()
	if err != nil {
		return nil, fmt.Errorf("read run schema: %w", err)
	}
	optional := func(name string) string {
		if columns[name] {
			return name
		}
		return "NULL"
	}
	runColumns := []string{"id", "repo_id", "branch", "head_sha", optional("review_approved_head_sha"), "status", "pr_url", optional("last_pushed_sha"), optional("push_target_kind"), optional("push_target_fingerprint"), optional("push_ref"), optional("last_pushed_at"), optional("push_generation"), "COALESCE(" + optional("push_active") + ", 0)", "created_at", optional("gates_json")}
	rows, err := tx.QueryContext(ctx, `SELECT `+strings.Join(runColumns, ", ")+` FROM runs WHERE repo_id = ? AND branch = ? ORDER BY created_at DESC, id DESC`, repoID, branch)
	if err != nil {
		return nil, fmt.Errorf("read branch runs: %w", err)
	}
	out := make([]BranchRunSnapshot, 0)
	for rows.Next() {
		run := &Run{}
		var gates sql.NullString
		if err := rows.Scan(&run.ID, &run.RepoID, &run.Branch, &run.HeadSHA, &run.ReviewApprovedHeadSHA, &run.Status, &run.PRURL, &run.LastPushedSHA, &run.PushTargetKind, &run.PushTargetFingerprint, &run.PushRef, &run.LastPushedAt, &run.PushGeneration, &run.PushActive, &run.CreatedAt, &gates); err != nil {
			rows.Close()
			return nil, fmt.Errorf("read branch run: %w", err)
		}
		out = append(out, BranchRunSnapshot{Run: run, GatesJSON: gates.String})
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, fmt.Errorf("read branch runs: %w", err)
	}
	// As in GetStepsByRun, legacy rows can predate optional outcome reasons.
	// Inspect schema within this same transaction; unavailable reasons stay NULL.
	// Only the newest run needs Test evidence for its terminal outcome. Older
	// inventory rows expose phase and status, not findings or outcomes.
	newestID := ""
	if len(out) > 0 {
		newestID = out[0].Run.ID
	}
	stepColumns := "id, run_id, step_name, step_order, status, CASE WHEN step_name = 'test' AND run_id = ? THEN findings_json ELSE NULL END"
	for _, column := range []string{"override_reason", "approval_reason", "skip_reason"} {
		var present int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('step_results') WHERE name = ?`, column).Scan(&present); err != nil {
			return nil, fmt.Errorf("read step schema: %w", err)
		}
		if present == 0 {
			stepColumns += ", NULL"
		} else {
			stepColumns += ", " + column
		}
	}
	byRun := make(map[string]int, len(out))
	for i := range out {
		byRun[out[i].Run.ID] = i
	}
	// Read all steps in one batch without an inventory-sized parameter list
	// or repeated scans of step_results. Gate pins were read with the runs.
	// Execute even for an empty branch so missing storage remains an error.
	rows, err = tx.QueryContext(ctx, `SELECT `+stepColumns+` FROM step_results WHERE run_id IN (SELECT id FROM runs WHERE repo_id = ? AND branch = ?) ORDER BY step_order, id`, newestID, repoID, branch)
	if err != nil {
		return nil, fmt.Errorf("read branch steps: %w", err)
	}
	for rows.Next() {
		step := &StepResult{}
		if err := rows.Scan(&step.ID, &step.RunID, &step.StepName, &step.StepOrder, &step.Status, &step.FindingsJSON, &step.OverrideReason, &step.ApprovalReason, &step.SkipReason); err != nil {
			rows.Close()
			return nil, fmt.Errorf("read branch step: %w", err)
		}
		snapshot := &out[byRun[step.RunID]]
		snapshot.Steps = append(snapshot.Steps, step)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, fmt.Errorf("read branch steps: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("finish branch snapshot: %w", err)
	}
	return out, nil
}

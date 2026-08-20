package db

import (
	"fmt"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// MaxBranchDecisionRounds bounds how many prior-run decision rounds are loaded
// for one branch. Every loaded round is rendered verbatim into an agent
// prompt, so this is a prompt-budget ceiling, not a correctness limit: a
// branch with a longer decision history simply carries its most recent
// decisions.
const MaxBranchDecisionRounds = 20

// BranchDecisionRound is one step round that recorded a human decision,
// paired with the run and step it belongs to so a prompt can attribute it.
type BranchDecisionRound struct {
	RunID    string
	StepName types.StepName
	Round    *StepRound
}

// GetBranchDecisionRounds returns rounds from OTHER runs on the same repo and
// branch that recorded a human decision, most recent first, bounded by limit.
// The truncated result reports whether older matching rounds exist beyond the
// returned recency window.
//
// It deliberately returns every round whose selection came from a human,
// declined or partial, and leaves the "does this round actually carry a
// decline" question to the caller: the declined set is derived by subtracting
// selected_finding_ids from findings_json, and that parsing belongs with the
// finding types rather than in the storage layer.
//
// Nothing ever deletes these rows. A completed review clears the uncertified
// pipeline range, which is why that channel could not carry a decision across
// runs; a recorded decision has no such expiry and stays readable for the life
// of the branch's run history.
func (d *DB) GetBranchDecisionRounds(repoID, branch, excludeRunID string, limit int) ([]*BranchDecisionRound, bool, error) {
	if limit <= 0 {
		limit = MaxBranchDecisionRounds
	}
	queryLimit := limit + 1
	rows, err := d.sql.Query(
		`SELECT sr.id, sr.step_result_id, sr.round, sr.trigger_type, sr.findings_json,
		        sr.reviewed_head_sha, sr.starting_head_sha, sr.trusted_config_sha,
		        sr.global_config_yaml, sr.repo_config_yaml, sr.user_findings_json,
		        sr.selected_finding_ids, sr.selection_source, sr.fix_summary,
		        sr.duration_ms, sr.created_at,
		        res.step_name, res.run_id
		   FROM step_rounds sr
		   JOIN step_results res ON res.id = sr.step_result_id
		   JOIN runs r ON r.id = res.run_id
		  WHERE r.repo_id = ? AND r.branch = ? AND r.id != ?
		    AND sr.selection_source IN (?, ?)
		    AND sr.findings_json IS NOT NULL
		  ORDER BY sr.created_at DESC, sr.id DESC
		  LIMIT ?`,
		repoID, branch, excludeRunID,
		RoundSelectionSourceUser, RoundSelectionSourceUserDeclined,
		queryLimit,
	)
	if err != nil {
		return nil, false, fmt.Errorf("get branch decision rounds: %w", err)
	}
	defer rows.Close()

	var decisions []*BranchDecisionRound
	for rows.Next() {
		r := &StepRound{}
		entry := &BranchDecisionRound{Round: r}
		if err := rows.Scan(
			&r.ID, &r.StepResultID, &r.Round, &r.Trigger, &r.FindingsJSON,
			&r.ReviewedHeadSHA, &r.StartingHeadSHA, &r.TrustedConfigSHA,
			&r.GlobalConfigYAML, &r.RepoConfigYAML, &r.UserFindingsJSON,
			&r.SelectedFindingIDs, &r.SelectionSource, &r.FixSummary,
			&r.DurationMS, &r.CreatedAt,
			&entry.StepName, &entry.RunID,
		); err != nil {
			return nil, false, fmt.Errorf("scan branch decision round: %w", err)
		}
		decisions = append(decisions, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	truncated := len(decisions) > limit
	if truncated {
		decisions = decisions[:limit]
	}
	return decisions, truncated, nil
}

// SetStepRoundDeclined records that a human resolved this round's approval
// gate without selecting any finding to fix.
//
// The write is conditional on selection_source still being NULL so a decline
// can never overwrite a selection that was already recorded. Reaching an
// approval gate on a round that already carries an auto-fix selection is not
// currently possible - auto-fix continues into a new round instead of
// gating - but a decline silently erasing a real selection would be a data
// loss, so the invariant is enforced in the statement rather than assumed by
// the caller.
func (d *DB) SetStepRoundDeclined(id string) error {
	declined := DeclinedSelectionJSON
	if _, err := d.sql.Exec(
		`UPDATE step_rounds SET selected_finding_ids = ?, selection_source = ?
		  WHERE id = ? AND selection_source IS NULL`,
		declined, RoundSelectionSourceUserDeclined, id,
	); err != nil {
		return fmt.Errorf("set step round declined: %w", err)
	}
	return nil
}

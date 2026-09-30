package db

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"net/url"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// PRContextCandidate is the exact PR identity and comparison observed by a
// caller. The PR URL and forge head are empty only before a PR exists.
type PRContextCandidate struct {
	PRURL        string
	SourceRepo   string
	SourceBranch string
	ForgeHeadSHA string
	LocalHeadSHA string
	TargetBranch string
	TargetSHA    string
	MergeBaseSHA string
	DiffDigest   string
}

type PRContext struct {
	PRContextCandidate
	ObservedAt int64
	Generation int64
}

type PRContextBindResult struct {
	Changed    bool
	Generation int64
}

// GetRunPRContext returns nil for an unbound run. No legacy run fields are
// interpreted as a receipt.
func (d *DB) GetRunPRContext(runID string) (*PRContext, error) {
	context := &PRContext{}
	var prURL, sourceRepo, sourceBranch, forgeHead sql.NullString
	err := d.sql.QueryRow(`SELECT pr_url, source_repo, source_branch, forge_head_sha,
		local_head_sha, target_branch, target_sha, merge_base_sha, diff_digest,
		observed_at, generation FROM run_pr_contexts WHERE run_id = ?`, runID).Scan(
		&prURL, &sourceRepo, &sourceBranch, &forgeHead,
		&context.LocalHeadSHA, &context.TargetBranch, &context.TargetSHA,
		&context.MergeBaseSHA, &context.DiffDigest, &context.ObservedAt, &context.Generation)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get run PR context: %w", err)
	}
	context.PRURL = prURL.String
	context.SourceRepo = sourceRepo.String
	context.SourceBranch = sourceBranch.String
	context.ForgeHeadSHA = forgeHead.String
	return context, nil
}

// BindRunPRContext records an exact observed comparison. Repeating identical
// content has no side effect. A changed comparison invalidates review authority
// and step results in the same transaction. Attaching the first PR identity to
// an identical prospective comparison preserves completed steps. Rebase is the
// boundary when the target moved; Review is the boundary after Rebase itself
// changed local HEAD.
func (d *DB) BindRunPRContext(runID string, candidate PRContextCandidate, resetFrom types.StepName) (PRContextBindResult, error) {
	return d.bindRunPRContext(runID, candidate, resetFrom, false)
}

// AdvanceRunPRContext records a commit made by the current forward pipeline
// step. Earlier completed steps keep their own evidence and review anchor.
func (d *DB) AdvanceRunPRContext(runID string, candidate PRContextCandidate) (PRContextBindResult, error) {
	return d.bindRunPRContext(runID, candidate, types.StepReview, true)
}

func (d *DB) bindRunPRContext(runID string, candidate PRContextCandidate, resetFrom types.StepName, forward bool) (PRContextBindResult, error) {
	if err := validatePRContextCandidate(candidate); err != nil {
		return PRContextBindResult{}, err
	}
	if resetFrom != types.StepRebase && resetFrom != types.StepReview {
		return PRContextBindResult{}, fmt.Errorf("bind run PR context: reset boundary must be rebase or review")
	}
	tx, err := d.sql.Begin()
	if err != nil {
		return PRContextBindResult{}, fmt.Errorf("bind run PR context: begin: %w", err)
	}
	defer tx.Rollback()
	var recordedPRURL sql.NullString
	if err := tx.QueryRow(`SELECT pr_url FROM runs WHERE id = ?`, runID).Scan(&recordedPRURL); err != nil {
		return PRContextBindResult{}, fmt.Errorf("bind run PR context: run lookup: %w", err)
	}
	if recordedPRURL.Valid && recordedPRURL.String != "" && recordedPRURL.String != candidate.PRURL {
		return PRContextBindResult{}, fmt.Errorf("bind run PR context: PR identity conflicts with run")
	}
	previous := &PRContext{}
	var prURL, sourceRepo, sourceBranch, forgeHead sql.NullString
	err = tx.QueryRow(`SELECT pr_url, source_repo, source_branch, forge_head_sha,
		local_head_sha, target_branch, target_sha, merge_base_sha, diff_digest,
		observed_at, generation FROM run_pr_contexts WHERE run_id = ?`, runID).Scan(
		&prURL, &sourceRepo, &sourceBranch, &forgeHead,
		&previous.LocalHeadSHA, &previous.TargetBranch, &previous.TargetSHA,
		&previous.MergeBaseSHA, &previous.DiffDigest, &previous.ObservedAt, &previous.Generation)
	initial := errors.Is(err, sql.ErrNoRows)
	if err != nil && !initial {
		return PRContextBindResult{}, fmt.Errorf("bind run PR context: read prior receipt: %w", err)
	}
	if !initial {
		previous.PRURL = prURL.String
		previous.SourceRepo = sourceRepo.String
		previous.SourceBranch = sourceBranch.String
		previous.ForgeHeadSHA = forgeHead.String
		if previous.SourceRepo != "" && (candidate.SourceRepo != previous.SourceRepo || candidate.SourceBranch != previous.SourceBranch) {
			return PRContextBindResult{}, fmt.Errorf("bind run PR context: source identity conflicts with receipt")
		}
		if previous.PRURL != "" && candidate.PRURL != previous.PRURL {
			return PRContextBindResult{}, fmt.Errorf("bind run PR context: PR identity conflict")
		}
		if previous.PRContextCandidate == candidate {
			return PRContextBindResult{Generation: previous.Generation}, nil
		}
		if previous.Generation == math.MaxInt64 {
			return PRContextBindResult{}, fmt.Errorf("bind run PR context: generation exhausted")
		}
	}
	identityOnlyAttach := !initial && previous.PRURL == "" && candidate.PRURL != "" &&
		candidate.ForgeHeadSHA == candidate.LocalHeadSHA &&
		previous.SourceRepo == candidate.SourceRepo && previous.SourceBranch == candidate.SourceBranch &&
		previous.LocalHeadSHA == candidate.LocalHeadSHA &&
		previous.TargetBranch == candidate.TargetBranch &&
		previous.TargetSHA == candidate.TargetSHA &&
		previous.MergeBaseSHA == candidate.MergeBaseSHA &&
		previous.DiffDigest == candidate.DiffDigest
	generation := int64(1)
	if !initial {
		generation = previous.Generation + 1
	}
	ts := now()
	if _, err := tx.Exec(`INSERT INTO run_pr_contexts (
		run_id, pr_url, source_repo, source_branch, forge_head_sha,
		local_head_sha, target_branch, target_sha, merge_base_sha, diff_digest,
		observed_at, generation) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(run_id) DO UPDATE SET
		pr_url=excluded.pr_url, source_repo=excluded.source_repo,
		source_branch=excluded.source_branch, forge_head_sha=excluded.forge_head_sha,
		local_head_sha=excluded.local_head_sha, target_branch=excluded.target_branch,
		target_sha=excluded.target_sha, merge_base_sha=excluded.merge_base_sha,
		diff_digest=excluded.diff_digest, observed_at=excluded.observed_at,
		generation=excluded.generation`,
		runID, optionalPRField(candidate.PRURL), optionalPRField(candidate.SourceRepo),
		optionalPRField(candidate.SourceBranch), optionalPRField(candidate.ForgeHeadSHA),
		candidate.LocalHeadSHA, candidate.TargetBranch, candidate.TargetSHA,
		candidate.MergeBaseSHA, candidate.DiffDigest, ts, generation); err != nil {
		return PRContextBindResult{}, fmt.Errorf("bind run PR context: write receipt: %w", err)
	}
	if candidate.PRURL != "" && recordedPRURL.String == "" {
		if _, err := tx.Exec(`UPDATE runs SET pr_url=?, pr_state='open', pr_state_observed_at=?, updated_at=? WHERE id=? AND (pr_url IS NULL OR pr_url='')`, candidate.PRURL, ts, ts, runID); err != nil {
			return PRContextBindResult{}, fmt.Errorf("bind run PR context: attach discovered PR: %w", err)
		}
	}
	if !initial && !identityOnlyAttach && forward {
		if _, err := tx.Exec(`UPDATE runs SET ci_ready_at=NULL, ci_ready_no_ci=0,
			terminal_head_verified_at=NULL, updated_at=? WHERE id=?`, ts, runID); err != nil {
			return PRContextBindResult{}, fmt.Errorf("advance run PR context: revoke terminal evidence: %w", err)
		}
	}
	if !initial && !identityOnlyAttach && !forward {
		if _, err := tx.Exec(`DELETE FROM step_rounds WHERE step_result_id IN (
			SELECT id FROM step_results WHERE run_id=? AND step_order>=?)`, runID, resetFrom.Order()); err != nil {
			return PRContextBindResult{}, fmt.Errorf("bind run PR context: discard superseded rounds: %w", err)
		}
		if _, err := tx.Exec(`UPDATE runs SET review_approved_head_sha=NULL,
			ci_ready_at=NULL, ci_ready_no_ci=0, terminal_head_verified_at=NULL,
			awaiting_agent_since=NULL, updated_at=? WHERE id=?`, ts, runID); err != nil {
			return PRContextBindResult{}, fmt.Errorf("bind run PR context: revoke run authority: %w", err)
		}
		if _, err := tx.Exec(`UPDATE step_results SET status=?, exit_code=NULL,
			duration_ms=NULL, log_path=NULL, findings_json=NULL, error=NULL,
			started_at=NULL, round_started_at=NULL, completed_at=NULL,
			last_activity_at=NULL, last_activity=NULL, agent_pid=NULL,
			auto_fix_limit=NULL, ci_fix_attempts=0, override_reason=NULL,
			approval_reason=NULL, skip_reason=NULL
			WHERE run_id=? AND step_order>=?`, types.StepStatusPending, runID, resetFrom.Order()); err != nil {
			return PRContextBindResult{}, fmt.Errorf("bind run PR context: reset dependent steps: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return PRContextBindResult{}, fmt.Errorf("bind run PR context: commit: %w", err)
	}
	return PRContextBindResult{Changed: true, Generation: generation}, nil
}

func optionalPRField(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func validatePRContextCandidate(c PRContextCandidate) error {
	for name, value := range map[string]string{
		"local head SHA": c.LocalHeadSHA, "target branch": c.TargetBranch,
		"target SHA": c.TargetSHA, "merge-base SHA": c.MergeBaseSHA,
		"diff digest": c.DiffDigest,
	} {
		if value == "" || strings.TrimSpace(value) != value || strings.ContainsAny(value, "\r\n\t") {
			return fmt.Errorf("bind run PR context: invalid %s", name)
		}
	}
	if !validGitSHA(c.LocalHeadSHA) || !validGitSHA(c.TargetSHA) || !validGitSHA(c.MergeBaseSHA) || !validHex(c.DiffDigest, 64) {
		return fmt.Errorf("bind run PR context: invalid SHA or digest")
	}
	if (c.SourceRepo == "") != (c.SourceBranch == "") ||
		(c.SourceRepo != "" && (c.SourceRepo != strings.TrimSpace(c.SourceRepo) || strings.ContainsAny(c.SourceRepo, "\r\n\t") || c.SourceBranch != strings.TrimSpace(c.SourceBranch) || strings.ContainsAny(c.SourceBranch, "\r\n\t"))) {
		return fmt.Errorf("bind run PR context: incomplete source identity")
	}
	if c.PRURL == "" {
		if c.ForgeHeadSHA != "" {
			return fmt.Errorf("bind run PR context: incomplete PR identity")
		}
		return nil
	}
	u, err := url.Parse(c.PRURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.Fragment != "" || c.PRURL != strings.TrimSpace(c.PRURL) {
		return fmt.Errorf("bind run PR context: invalid PR URL")
	}
	if c.SourceRepo == "" || !validGitSHA(c.ForgeHeadSHA) {
		return fmt.Errorf("bind run PR context: incomplete or invalid PR identity")
	}
	return nil
}

func validGitSHA(s string) bool { return validHex(s, 40) || validHex(s, 64) }

func validHex(s string, length int) bool {
	if len(s) != length {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			if c < 'a' || c > 'f' {
				return false
			}
		}
	}
	return true
}

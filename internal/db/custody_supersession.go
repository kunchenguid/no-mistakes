package db

import (
	"database/sql"
	"errors"
	"fmt"
)

const (
	StaleCustodySupersessionPrepared   = "prepared"
	StaleCustodySupersessionLocalMoved = "local_moved"
)

// StaleCustodySupersession is the durable journal for replacing one stale
// terminal owner with a later exact pipeline lineage. It binds both runs, the
// run that reconnects the stale preserved head to the later lineage, every Git
// head observed by the transition, the configured target, and both authority
// generations before the checked-out branch can move.
type StaleCustodySupersession struct {
	OldRunID            string
	LaterRunID          string
	LineageRunID        string
	RepoID              string
	Branch              string
	OldHead             string
	LocalHead           string
	LaterSubmittedHead  string
	LaterPushedHead     string
	LineagePushedHead   string
	RemoteHead          string
	GateHead            string
	TargetKind          string
	TargetFingerprint   string
	TargetRef           string
	OwnershipGeneration int64
	RepoGeneration      int64
	Phase               string
	CreatedAt           int64
	UpdatedAt           int64
}

// GetStaleCustodySupersession returns the exact old run's durable journal.
func (d *DB) GetStaleCustodySupersession(oldRunID string) (*StaleCustodySupersession, error) {
	a := &StaleCustodySupersession{}
	err := d.sql.QueryRow(`SELECT old_run_id, later_run_id, lineage_run_id, repo_id, branch,
		old_head, local_head, later_submitted_head, later_pushed_head, lineage_pushed_head,
		remote_head, gate_head, target_kind, target_fingerprint, target_ref,
		ownership_generation, repo_generation, phase, created_at, updated_at
		FROM stale_custody_supersessions WHERE old_run_id = ?`, oldRunID).
		Scan(&a.OldRunID, &a.LaterRunID, &a.LineageRunID, &a.RepoID, &a.Branch,
			&a.OldHead, &a.LocalHead, &a.LaterSubmittedHead, &a.LaterPushedHead, &a.LineagePushedHead,
			&a.RemoteHead, &a.GateHead, &a.TargetKind, &a.TargetFingerprint, &a.TargetRef,
			&a.OwnershipGeneration, &a.RepoGeneration, &a.Phase, &a.CreatedAt, &a.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get stale custody supersession: %w", err)
	}
	return a, nil
}

// PrepareStaleCustodySupersession creates the immutable journal only while all
// three run identities, the repository registration, and authority generations
// still match the caller's independently verified plan.
func (d *DB) PrepareStaleCustodySupersession(old, later, lineage *Run, repo *Repo, proposed StaleCustodySupersession) (*StaleCustodySupersession, error) {
	if old == nil || later == nil || lineage == nil || repo == nil ||
		proposed.OldRunID != old.ID || proposed.LaterRunID != later.ID || proposed.LineageRunID != lineage.ID ||
		proposed.RepoID != old.RepoID || proposed.RepoID != later.RepoID || proposed.RepoID != lineage.RepoID ||
		proposed.Branch != old.Branch || proposed.Branch != later.Branch || proposed.Branch != lineage.Branch {
		return nil, ErrRunCustodyChanged
	}
	ts := now()
	result, err := d.sql.Exec(`INSERT INTO stale_custody_supersessions (
		old_run_id, later_run_id, lineage_run_id, repo_id, branch,
		old_head, local_head, later_submitted_head, later_pushed_head, lineage_pushed_head,
		remote_head, gate_head, target_kind, target_fingerprint, target_ref,
		ownership_generation, repo_generation, phase, created_at, updated_at)
		SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
		FROM runs AS old
		JOIN runs AS later ON later.id = ?
		JOIN runs AS lineage ON lineage.id = ?
		JOIN repos ON repos.id = old.repo_id
		JOIN branch_ownership_generations AS ownership
		  ON ownership.repo_id = old.repo_id AND ownership.branch = old.branch
		WHERE old.id = ? AND old.repo_id = ? AND old.branch = ? AND old.head_sha = ?
		  AND old.submitted_head_sha IS ? AND old.status = ? AND old.status IN ('completed', 'failed', 'cancelled')
		  AND old.last_pushed_sha IS ? AND old.push_target_kind IS ? AND old.push_target_fingerprint IS ?
		  AND old.push_ref IS ? AND old.push_generation IS ? AND old.push_active = 0
		  AND old.terminal_head_verified_at IS ? AND old.custody_returned_at IS NULL
		  AND later.repo_id = ? AND later.branch = ? AND later.head_sha = ?
		  AND later.submitted_head_sha IS ? AND later.status = ? AND later.status IN ('completed', 'failed', 'cancelled')
		  AND later.last_pushed_sha IS ? AND later.push_target_kind IS ? AND later.push_target_fingerprint IS ?
		  AND later.push_ref IS ? AND later.push_generation IS ? AND later.push_active = 0
		  AND later.terminal_head_verified_at IS ? AND later.pr_state IS ? AND later.custody_returned_at IS NULL
		  AND lineage.repo_id = ? AND lineage.branch = ? AND lineage.head_sha = ?
		  AND lineage.submitted_head_sha IS ? AND lineage.status = ? AND lineage.status IN ('completed', 'failed', 'cancelled')
		  AND lineage.last_pushed_sha IS ? AND lineage.push_target_kind IS ? AND lineage.push_target_fingerprint IS ?
		  AND lineage.push_ref IS ? AND lineage.push_generation IS ? AND lineage.push_active = 0
		  AND lineage.terminal_head_verified_at IS ? AND lineage.pr_state IS ? AND lineage.custody_returned_at IS NULL
		  AND (old.created_at < lineage.created_at OR (old.created_at = lineage.created_at AND old.id < lineage.id))
		  AND (lineage.created_at < later.created_at OR (lineage.created_at = later.created_at AND lineage.id <= later.id))
		  AND repos.working_path = ? AND repos.upstream_url = ? AND COALESCE(repos.fork_url, '') = ?
		  AND repos.default_branch = ? AND repos.metadata_generation = ? AND ownership.generation = ?
		ON CONFLICT(old_run_id) DO NOTHING`,
		proposed.OldRunID, proposed.LaterRunID, proposed.LineageRunID, proposed.RepoID, proposed.Branch,
		proposed.OldHead, proposed.LocalHead, proposed.LaterSubmittedHead, proposed.LaterPushedHead, proposed.LineagePushedHead,
		proposed.RemoteHead, proposed.GateHead, proposed.TargetKind, proposed.TargetFingerprint, proposed.TargetRef,
		proposed.OwnershipGeneration, proposed.RepoGeneration, StaleCustodySupersessionPrepared, ts, ts,
		later.ID, lineage.ID,
		old.ID, old.RepoID, old.Branch, old.HeadSHA, old.SubmittedHeadSHA, old.Status,
		old.LastPushedSHA, old.PushTargetKind, old.PushTargetFingerprint, old.PushRef, old.PushGeneration, old.TerminalHeadVerifiedAt,
		later.RepoID, later.Branch, later.HeadSHA, later.SubmittedHeadSHA, later.Status,
		later.LastPushedSHA, later.PushTargetKind, later.PushTargetFingerprint, later.PushRef, later.PushGeneration, later.TerminalHeadVerifiedAt, later.PRState,
		lineage.RepoID, lineage.Branch, lineage.HeadSHA, lineage.SubmittedHeadSHA, lineage.Status,
		lineage.LastPushedSHA, lineage.PushTargetKind, lineage.PushTargetFingerprint, lineage.PushRef, lineage.PushGeneration, lineage.TerminalHeadVerifiedAt, lineage.PRState,
		repo.WorkingPath, repo.UpstreamURL, repo.ForkURL, repo.DefaultBranch, proposed.RepoGeneration, proposed.OwnershipGeneration,
	)
	if err != nil {
		return nil, fmt.Errorf("prepare stale custody supersession: %w", err)
	}
	if affected, rowsErr := result.RowsAffected(); rowsErr != nil {
		return nil, fmt.Errorf("prepare stale custody supersession: rows affected: %w", rowsErr)
	} else if affected == 1 {
		return d.GetStaleCustodySupersession(old.ID)
	}
	existing, err := d.GetStaleCustodySupersession(old.ID)
	if err != nil {
		return nil, err
	}
	if !sameStaleCustodySupersession(existing, &proposed) {
		return nil, ErrRunCustodyChanged
	}
	return existing, nil
}

func sameStaleCustodySupersession(a, b *StaleCustodySupersession) bool {
	return a != nil && b != nil &&
		a.OldRunID == b.OldRunID && a.LaterRunID == b.LaterRunID && a.LineageRunID == b.LineageRunID &&
		a.RepoID == b.RepoID && a.Branch == b.Branch && a.OldHead == b.OldHead && a.LocalHead == b.LocalHead &&
		a.LaterSubmittedHead == b.LaterSubmittedHead && a.LaterPushedHead == b.LaterPushedHead &&
		a.LineagePushedHead == b.LineagePushedHead && a.RemoteHead == b.RemoteHead && a.GateHead == b.GateHead &&
		a.TargetKind == b.TargetKind && a.TargetFingerprint == b.TargetFingerprint && a.TargetRef == b.TargetRef &&
		a.OwnershipGeneration == b.OwnershipGeneration && a.RepoGeneration == b.RepoGeneration
}

// RebindStaleCustodySupersessionAuthority moves an otherwise identical retry
// onto freshly revalidated generations. Generations bind one attempt's final
// window, not permanent attempt identity, so an unrelated completed write
// cannot strand a crash-retry forever. The exact observed row, live requested
// generations, and still-held old-run custody are all required.
func (d *DB) RebindStaleCustodySupersessionAuthority(oldRunID string, observed *StaleCustodySupersession, authority CustodyReleaseAuthority) (*StaleCustodySupersession, error) {
	if observed == nil || observed.OldRunID != oldRunID {
		return nil, ErrRunCustodyChanged
	}
	ts := now()
	result, err := d.sql.Exec(`UPDATE stale_custody_supersessions AS supersession
		SET ownership_generation = ?, repo_generation = ?, updated_at = ?
		WHERE old_run_id = ? AND later_run_id = ? AND lineage_run_id = ?
		  AND repo_id = ? AND branch = ? AND old_head = ? AND local_head = ?
		  AND later_submitted_head = ? AND later_pushed_head = ? AND lineage_pushed_head = ?
		  AND remote_head = ? AND gate_head = ? AND target_kind = ? AND target_fingerprint = ?
		  AND target_ref = ? AND ownership_generation = ? AND repo_generation = ? AND phase = ?
		  AND EXISTS (
		      SELECT 1 FROM branch_ownership_generations AS ownership
		       WHERE ownership.repo_id = supersession.repo_id AND ownership.branch = supersession.branch
		         AND ownership.generation = ?
		  )
		  AND EXISTS (
		      SELECT 1 FROM repos
		       WHERE repos.id = supersession.repo_id AND repos.metadata_generation = ?
		  )
		  AND EXISTS (
		      SELECT 1 FROM runs
		       WHERE runs.id = supersession.old_run_id AND runs.custody_returned_at IS NULL
		  )`,
		authority.OwnershipGeneration, authority.RepoGeneration, ts,
		observed.OldRunID, observed.LaterRunID, observed.LineageRunID,
		observed.RepoID, observed.Branch, observed.OldHead, observed.LocalHead,
		observed.LaterSubmittedHead, observed.LaterPushedHead, observed.LineagePushedHead,
		observed.RemoteHead, observed.GateHead, observed.TargetKind, observed.TargetFingerprint,
		observed.TargetRef, observed.OwnershipGeneration, observed.RepoGeneration, observed.Phase,
		authority.OwnershipGeneration, authority.RepoGeneration)
	if err != nil {
		return nil, fmt.Errorf("rebind stale custody supersession authority: %w", err)
	}
	if affected, rowsErr := result.RowsAffected(); rowsErr != nil {
		return nil, fmt.Errorf("rebind stale custody supersession authority: rows affected: %w", rowsErr)
	} else if affected != 1 {
		return nil, ErrRunCustodyChanged
	}
	return d.GetStaleCustodySupersession(oldRunID)
}

// MarkStaleCustodySupersessionLocalMoved records that the exact local
// compare-and-swap and worktree update completed. The authority snapshot must
// still be current; an already marked journal is idempotent.
func (d *DB) MarkStaleCustodySupersessionLocalMoved(oldRunID string) error {
	ts := now()
	result, err := d.sql.Exec(`UPDATE stale_custody_supersessions AS supersession
		SET phase = ?, updated_at = ?
		WHERE old_run_id = ? AND phase = ?
		  AND EXISTS (
		      SELECT 1 FROM branch_ownership_generations AS ownership
		       WHERE ownership.repo_id = supersession.repo_id AND ownership.branch = supersession.branch
		         AND ownership.generation = supersession.ownership_generation
		  )
		  AND EXISTS (
		      SELECT 1 FROM repos
		       WHERE repos.id = supersession.repo_id AND repos.metadata_generation = supersession.repo_generation
		  )`,
		StaleCustodySupersessionLocalMoved, ts, oldRunID, StaleCustodySupersessionPrepared)
	if err != nil {
		return fmt.Errorf("mark stale custody supersession local moved: %w", err)
	}
	if affected, rowsErr := result.RowsAffected(); rowsErr != nil {
		return fmt.Errorf("mark stale custody supersession local moved: rows affected: %w", rowsErr)
	} else if affected == 1 {
		return nil
	}
	attempt, err := d.GetStaleCustodySupersession(oldRunID)
	if err != nil {
		return err
	}
	if attempt != nil && attempt.Phase == StaleCustodySupersessionLocalMoved {
		return nil
	}
	return ErrRunCustodyChanged
}

// CommitStaleCustodySupersession releases only the old run while the complete
// journal, all three run rows, repository identity, and authority generation
// remain exact. The later run's push provenance is evidence, never mutated.
func (d *DB) CommitStaleCustodySupersession(old, later, lineage *Run, repo *Repo, attempt *StaleCustodySupersession) (bool, error) {
	if old == nil || later == nil || lineage == nil || repo == nil || attempt == nil ||
		attempt.OldRunID != old.ID || attempt.LaterRunID != later.ID || attempt.LineageRunID != lineage.ID || attempt.RepoID != repo.ID {
		return false, ErrRunCustodyChanged
	}
	ts := now()
	result, err := d.sql.Exec(`UPDATE runs AS old SET
		custody_returned_at = ?, custody_return_reason = ?, updated_at = ?
		WHERE old.id = ? AND old.repo_id = ? AND old.branch = ? AND old.head_sha = ?
		  AND old.submitted_head_sha IS ? AND old.status = ? AND old.status IN ('completed', 'failed', 'cancelled')
		  AND old.last_pushed_sha IS ? AND old.push_target_kind IS ? AND old.push_target_fingerprint IS ?
		  AND old.push_ref IS ? AND old.push_generation IS ? AND old.push_active = 0
		  AND old.terminal_head_verified_at IS ? AND old.custody_returned_at IS NULL
		  AND EXISTS (
		      SELECT 1 FROM stale_custody_supersessions AS supersession
		       WHERE supersession.old_run_id = old.id AND supersession.later_run_id = ?
		         AND supersession.lineage_run_id = ? AND supersession.repo_id = old.repo_id
		         AND supersession.branch = old.branch AND supersession.phase = ?
		         AND supersession.old_head = ? AND supersession.local_head = ?
		         AND supersession.later_submitted_head = ? AND supersession.later_pushed_head = ?
		         AND supersession.lineage_pushed_head = ? AND supersession.remote_head = ?
		         AND supersession.gate_head = ? AND supersession.target_kind = ?
		         AND supersession.target_fingerprint = ? AND supersession.target_ref = ?
		         AND supersession.ownership_generation = ? AND supersession.repo_generation = ?
		  )
		  AND EXISTS (
		      SELECT 1 FROM runs AS later
		       WHERE later.id = ? AND later.repo_id = ? AND later.branch = ? AND later.head_sha = ?
		         AND later.submitted_head_sha IS ? AND later.status = ? AND later.status IN ('completed', 'failed', 'cancelled')
		         AND later.last_pushed_sha IS ? AND later.push_target_kind IS ? AND later.push_target_fingerprint IS ?
		         AND later.push_ref IS ? AND later.push_generation IS ? AND later.push_active = 0
		         AND later.terminal_head_verified_at IS ? AND later.pr_state IS ? AND later.custody_returned_at IS NULL
		  )
		  AND EXISTS (
		      SELECT 1 FROM runs AS lineage
		       WHERE lineage.id = ? AND lineage.repo_id = ? AND lineage.branch = ? AND lineage.head_sha = ?
		         AND lineage.submitted_head_sha IS ? AND lineage.status = ? AND lineage.status IN ('completed', 'failed', 'cancelled')
		         AND lineage.last_pushed_sha IS ? AND lineage.push_target_kind IS ? AND lineage.push_target_fingerprint IS ?
		         AND lineage.push_ref IS ? AND lineage.push_generation IS ? AND lineage.push_active = 0
		         AND lineage.terminal_head_verified_at IS ? AND lineage.pr_state IS ? AND lineage.custody_returned_at IS NULL
		  )
		  AND EXISTS (
		      SELECT 1 FROM branch_ownership_generations AS ownership
		       WHERE ownership.repo_id = old.repo_id AND ownership.branch = old.branch
		         AND ownership.generation = ?
		  )
		  AND EXISTS (
		      SELECT 1 FROM repos
		       WHERE repos.id = old.repo_id AND repos.working_path = ? AND repos.upstream_url = ?
		         AND COALESCE(repos.fork_url, '') = ? AND repos.default_branch = ? AND repos.metadata_generation = ?
		  )`,
		ts, CustodyReturnReasonStaleOwnerSuperseded, ts,
		old.ID, old.RepoID, old.Branch, old.HeadSHA, old.SubmittedHeadSHA, old.Status,
		old.LastPushedSHA, old.PushTargetKind, old.PushTargetFingerprint, old.PushRef, old.PushGeneration, old.TerminalHeadVerifiedAt,
		attempt.LaterRunID, attempt.LineageRunID, StaleCustodySupersessionLocalMoved,
		attempt.OldHead, attempt.LocalHead, attempt.LaterSubmittedHead, attempt.LaterPushedHead,
		attempt.LineagePushedHead, attempt.RemoteHead, attempt.GateHead, attempt.TargetKind,
		attempt.TargetFingerprint, attempt.TargetRef, attempt.OwnershipGeneration, attempt.RepoGeneration,
		later.ID, later.RepoID, later.Branch, later.HeadSHA, later.SubmittedHeadSHA, later.Status,
		later.LastPushedSHA, later.PushTargetKind, later.PushTargetFingerprint, later.PushRef, later.PushGeneration, later.TerminalHeadVerifiedAt, later.PRState,
		lineage.ID, lineage.RepoID, lineage.Branch, lineage.HeadSHA, lineage.SubmittedHeadSHA, lineage.Status,
		lineage.LastPushedSHA, lineage.PushTargetKind, lineage.PushTargetFingerprint, lineage.PushRef, lineage.PushGeneration, lineage.TerminalHeadVerifiedAt, lineage.PRState,
		attempt.OwnershipGeneration,
		repo.WorkingPath, repo.UpstreamURL, repo.ForkURL, repo.DefaultBranch, attempt.RepoGeneration,
	)
	if err != nil {
		return false, fmt.Errorf("commit stale custody supersession: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("commit stale custody supersession: rows affected: %w", err)
	}
	if affected == 1 {
		return true, nil
	}
	current, err := d.GetRun(old.ID)
	if err != nil {
		return false, err
	}
	if current != nil && current.RepoID == old.RepoID && current.Branch == old.Branch && current.HeadSHA == old.HeadSHA &&
		current.CustodyReturnedAt != nil && current.CustodyReturnReason != nil && *current.CustodyReturnReason == CustodyReturnReasonStaleOwnerSuperseded {
		return false, nil
	}
	return false, ErrRunCustodyChanged
}

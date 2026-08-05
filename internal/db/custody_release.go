package db

import (
	"database/sql"
	"errors"
	"fmt"
)

const (
	UnavailableCustodyReleasePrepared  = "prepared"
	UnavailableCustodyReleaseGateMoved = "gate_moved"
)

// CustodyReleaseAuthority binds an exceptional custody release to one exact
// repository metadata generation and one exact branch-ownership generation.
// Every supported run ownership mutation advances the branch generation, and
// every supported repository metadata mutation advances the repo generation.
type CustodyReleaseAuthority struct {
	OwnershipGeneration int64
	RepoGeneration      int64
}

// UnavailableCustodyRelease is the durable pre-commit journal for an
// unavailable-head release. It makes the gate compare-and-swap retryable even
// when the gate's original head differs from the local/remote replacement.
type UnavailableCustodyRelease struct {
	RunID               string
	RepoID              string
	Branch              string
	PreservedHead       string
	LocalHead           string
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

// SnapshotCustodyReleaseAuthority returns the generations that a caller must
// carry through its final Git recheck and guarded journal creation.
func (d *DB) SnapshotCustodyReleaseAuthority(repoID, branch string) (CustodyReleaseAuthority, error) {
	var authority CustodyReleaseAuthority
	err := d.sql.QueryRow(`SELECT ownership.generation, repos.metadata_generation
		FROM branch_ownership_generations AS ownership
		JOIN repos ON repos.id = ownership.repo_id
		WHERE ownership.repo_id = ? AND ownership.branch = ?`, repoID, branch).
		Scan(&authority.OwnershipGeneration, &authority.RepoGeneration)
	if err != nil {
		return CustodyReleaseAuthority{}, fmt.Errorf("snapshot custody release authority: %w", err)
	}
	return authority, nil
}

// GetUnavailableCustodyRelease returns a run's durable release journal.
func (d *DB) GetUnavailableCustodyRelease(runID string) (*UnavailableCustodyRelease, error) {
	return getUnavailableCustodyRelease(d.sql, runID)
}

type custodyReleaseQuerier interface {
	QueryRow(query string, args ...any) *sql.Row
}

func getUnavailableCustodyRelease(q custodyReleaseQuerier, runID string) (*UnavailableCustodyRelease, error) {
	a := &UnavailableCustodyRelease{}
	err := q.QueryRow(`SELECT run_id, repo_id, branch, preserved_head, local_head, remote_head, gate_head,
		target_kind, target_fingerprint, target_ref, ownership_generation, repo_generation,
		phase, created_at, updated_at
		FROM unavailable_custody_releases WHERE run_id = ?`, runID).
		Scan(&a.RunID, &a.RepoID, &a.Branch, &a.PreservedHead, &a.LocalHead, &a.RemoteHead, &a.GateHead,
			&a.TargetKind, &a.TargetFingerprint, &a.TargetRef, &a.OwnershipGeneration, &a.RepoGeneration,
			&a.Phase, &a.CreatedAt, &a.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get unavailable custody release: %w", err)
	}
	return a, nil
}

// PrepareUnavailableCustodyRelease creates the immutable release journal only
// while the caller's run, repository, and authority generations still match.
// A matching existing row is returned idempotently; a collision fails closed.
func (d *DB) PrepareUnavailableCustodyRelease(expected *Run, repo *Repo, proposed UnavailableCustodyRelease) (*UnavailableCustodyRelease, error) {
	if expected == nil || repo == nil || proposed.RunID != expected.ID || proposed.RepoID != expected.RepoID || proposed.Branch != expected.Branch {
		return nil, ErrRunCustodyChanged
	}
	ts := now()
	result, err := d.sql.Exec(`INSERT INTO unavailable_custody_releases (
		run_id, repo_id, branch, preserved_head, local_head, remote_head, gate_head,
		target_kind, target_fingerprint, target_ref, ownership_generation, repo_generation,
		phase, created_at, updated_at)
		SELECT ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?
		FROM runs AS selected
		JOIN repos ON repos.id = selected.repo_id
		JOIN branch_ownership_generations AS ownership
		  ON ownership.repo_id = selected.repo_id AND ownership.branch = selected.branch
		WHERE selected.id = ? AND selected.repo_id = ? AND selected.branch = ? AND selected.head_sha = ?
		  AND selected.submitted_head_sha IS ? AND selected.status = ? AND selected.status IN ('completed', 'failed', 'cancelled')
		  AND selected.last_pushed_sha IS ? AND selected.push_target_kind IS ? AND selected.push_target_fingerprint IS ?
		  AND selected.push_ref IS ? AND selected.push_generation IS ? AND selected.push_active = 0
		  AND selected.terminal_head_verified_at IS ? AND selected.custody_returned_at IS NULL
		  AND repos.working_path = ? AND repos.upstream_url = ? AND COALESCE(repos.fork_url, '') = ?
		  AND repos.default_branch = ? AND repos.metadata_generation = ?
		  AND ownership.generation = ?
		ON CONFLICT(run_id) DO NOTHING`,
		proposed.RunID, proposed.RepoID, proposed.Branch, proposed.PreservedHead,
		proposed.LocalHead, proposed.RemoteHead, proposed.GateHead, proposed.TargetKind,
		proposed.TargetFingerprint, proposed.TargetRef, proposed.OwnershipGeneration,
		proposed.RepoGeneration, UnavailableCustodyReleasePrepared, ts, ts,
		expected.ID, expected.RepoID, expected.Branch, expected.HeadSHA,
		expected.SubmittedHeadSHA, expected.Status, expected.LastPushedSHA,
		expected.PushTargetKind, expected.PushTargetFingerprint, expected.PushRef,
		expected.PushGeneration, expected.TerminalHeadVerifiedAt,
		repo.WorkingPath, repo.UpstreamURL, repo.ForkURL, repo.DefaultBranch,
		proposed.RepoGeneration, proposed.OwnershipGeneration,
	)
	if err != nil {
		return nil, fmt.Errorf("prepare unavailable custody release: %w", err)
	}
	if affected, rowsErr := result.RowsAffected(); rowsErr != nil {
		return nil, fmt.Errorf("prepare unavailable custody release: rows affected: %w", rowsErr)
	} else if affected == 1 {
		return d.GetUnavailableCustodyRelease(expected.ID)
	}
	existing, err := d.GetUnavailableCustodyRelease(expected.ID)
	if err != nil {
		return nil, err
	}
	if !sameUnavailableCustodyRelease(existing, &proposed) {
		return nil, ErrRunCustodyChanged
	}
	return existing, nil
}

func sameUnavailableCustodyRelease(a, b *UnavailableCustodyRelease) bool {
	return a != nil && b != nil &&
		a.RunID == b.RunID && a.RepoID == b.RepoID && a.Branch == b.Branch &&
		a.PreservedHead == b.PreservedHead && a.LocalHead == b.LocalHead &&
		a.RemoteHead == b.RemoteHead && a.GateHead == b.GateHead &&
		a.TargetKind == b.TargetKind && a.TargetFingerprint == b.TargetFingerprint &&
		a.TargetRef == b.TargetRef && a.OwnershipGeneration == b.OwnershipGeneration &&
		a.RepoGeneration == b.RepoGeneration
}

// SupersedeUnavailableCustodyRelease removes a journaled attempt that no longer
// describes its caller's revalidated facts, so the run can be journaled again
// from scratch. It deletes only the exact row the caller observed and only
// while custody is still held, so a concurrent writer's newer attempt and an
// already committed release are both untouchable.
func (d *DB) SupersedeUnavailableCustodyRelease(runID string, observed *UnavailableCustodyRelease) error {
	if observed == nil || observed.RunID != runID {
		return ErrRunCustodyChanged
	}
	result, err := d.sql.Exec(`DELETE FROM unavailable_custody_releases
		WHERE run_id = ? AND repo_id = ? AND branch = ? AND preserved_head = ? AND local_head = ?
		  AND remote_head = ? AND gate_head = ? AND target_kind = ? AND target_fingerprint = ?
		  AND target_ref = ? AND ownership_generation = ? AND repo_generation = ? AND phase = ?
		  AND EXISTS (
		      SELECT 1 FROM runs
		       WHERE runs.id = unavailable_custody_releases.run_id AND runs.custody_returned_at IS NULL
		  )`,
		observed.RunID, observed.RepoID, observed.Branch, observed.PreservedHead, observed.LocalHead,
		observed.RemoteHead, observed.GateHead, observed.TargetKind, observed.TargetFingerprint,
		observed.TargetRef, observed.OwnershipGeneration, observed.RepoGeneration, observed.Phase)
	if err != nil {
		return fmt.Errorf("supersede unavailable custody release: %w", err)
	}
	affected, rowsErr := result.RowsAffected()
	if rowsErr != nil {
		return fmt.Errorf("supersede unavailable custody release: rows affected: %w", rowsErr)
	}
	if affected != 1 {
		return ErrRunCustodyChanged
	}
	return nil
}

// RebindUnavailableCustodyReleaseAuthority moves a journaled attempt onto the
// generations a retry just snapshotted. Generations bind one attempt's recheck,
// gate move, and stamp into a single atomic window; they are not attempt
// identity, so a retry must be able to adopt current values instead of being
// pinned to a superseded generation no live row can satisfy. The rebind still
// requires the exact observed row, the requested generations to be the live
// ones, and custody to still be held.
func (d *DB) RebindUnavailableCustodyReleaseAuthority(runID string, observed *UnavailableCustodyRelease, authority CustodyReleaseAuthority) (*UnavailableCustodyRelease, error) {
	if observed == nil || observed.RunID != runID {
		return nil, ErrRunCustodyChanged
	}
	ts := now()
	result, err := d.sql.Exec(`UPDATE unavailable_custody_releases AS release
		SET ownership_generation = ?, repo_generation = ?, updated_at = ?
		WHERE run_id = ? AND ownership_generation = ? AND repo_generation = ? AND phase = ?
		  AND EXISTS (
		      SELECT 1 FROM branch_ownership_generations AS ownership
		       WHERE ownership.repo_id = release.repo_id AND ownership.branch = release.branch
		         AND ownership.generation = ?
		  )
		  AND EXISTS (
		      SELECT 1 FROM repos
		       WHERE repos.id = release.repo_id AND repos.metadata_generation = ?
		  )
		  AND EXISTS (
		      SELECT 1 FROM runs
		       WHERE runs.id = release.run_id AND runs.custody_returned_at IS NULL
		  )`,
		authority.OwnershipGeneration, authority.RepoGeneration, ts,
		observed.RunID, observed.OwnershipGeneration, observed.RepoGeneration, observed.Phase,
		authority.OwnershipGeneration, authority.RepoGeneration)
	if err != nil {
		return nil, fmt.Errorf("rebind unavailable custody release authority: %w", err)
	}
	if affected, rowsErr := result.RowsAffected(); rowsErr != nil {
		return nil, fmt.Errorf("rebind unavailable custody release authority: rows affected: %w", rowsErr)
	} else if affected != 1 {
		return nil, ErrRunCustodyChanged
	}
	return d.GetUnavailableCustodyRelease(runID)
}

// MarkUnavailableCustodyReleaseGateMoved durably records that the journaled
// gate compare-and-swap completed. Authority changes make the transition fail
// closed; an already-marked exact attempt is idempotent.
func (d *DB) MarkUnavailableCustodyReleaseGateMoved(runID string) error {
	ts := now()
	result, err := d.sql.Exec(`UPDATE unavailable_custody_releases AS release
		SET phase = ?, updated_at = ?
		WHERE run_id = ? AND phase = ?
		  AND EXISTS (
		      SELECT 1 FROM branch_ownership_generations AS ownership
		       WHERE ownership.repo_id = release.repo_id AND ownership.branch = release.branch
		         AND ownership.generation = release.ownership_generation
		  )
		  AND EXISTS (
		      SELECT 1 FROM repos
		       WHERE repos.id = release.repo_id AND repos.metadata_generation = release.repo_generation
		  )`,
		UnavailableCustodyReleaseGateMoved, ts, runID, UnavailableCustodyReleasePrepared)
	if err != nil {
		return fmt.Errorf("mark unavailable custody release gate moved: %w", err)
	}
	if affected, rowsErr := result.RowsAffected(); rowsErr != nil {
		return fmt.Errorf("mark unavailable custody release gate moved: rows affected: %w", rowsErr)
	} else if affected == 1 {
		return nil
	}
	attempt, err := d.GetUnavailableCustodyRelease(runID)
	if err != nil {
		return err
	}
	if attempt != nil && attempt.Phase == UnavailableCustodyReleaseGateMoved {
		return nil
	}
	return ErrRunCustodyChanged
}

// CommitUnavailableRunCustody stamps the exceptional release only while the
// immutable journal, exact run, repository metadata generation, and complete
// branch ownership generation remain unchanged.
func (d *DB) CommitUnavailableRunCustody(expected *Run, repo *Repo, attempt *UnavailableCustodyRelease) (bool, error) {
	if expected == nil || repo == nil || attempt == nil || attempt.RunID != expected.ID || attempt.RepoID != repo.ID {
		return false, ErrRunCustodyChanged
	}
	ts := now()
	result, err := d.sql.Exec(`UPDATE runs AS selected SET
		custody_returned_at = ?, custody_return_reason = ?, updated_at = ?
		WHERE selected.id = ? AND selected.repo_id = ? AND selected.branch = ? AND selected.head_sha = ?
		  AND selected.submitted_head_sha IS ? AND selected.status = ? AND selected.status IN ('completed', 'failed', 'cancelled')
		  AND selected.last_pushed_sha IS ? AND selected.push_target_kind IS ? AND selected.push_target_fingerprint IS ?
		  AND selected.push_ref IS ? AND selected.push_generation IS ? AND selected.push_active = 0
		  AND selected.terminal_head_verified_at IS ? AND selected.custody_returned_at IS NULL
		  AND EXISTS (
		      SELECT 1 FROM unavailable_custody_releases AS release
		       WHERE release.run_id = selected.id AND release.repo_id = selected.repo_id
		         AND release.branch = selected.branch AND release.phase = ?
		         AND release.preserved_head = ? AND release.local_head = ? AND release.remote_head = ?
		         AND release.gate_head = ? AND release.target_kind = ?
		         AND release.target_fingerprint = ? AND release.target_ref = ?
		         AND release.ownership_generation = ? AND release.repo_generation = ?
		  )
		  AND EXISTS (
		      SELECT 1 FROM branch_ownership_generations AS ownership
		       WHERE ownership.repo_id = selected.repo_id AND ownership.branch = selected.branch
		         AND ownership.generation = ?
		  )
		  AND EXISTS (
		      SELECT 1 FROM repos
		       WHERE repos.id = selected.repo_id AND repos.working_path = ?
		         AND repos.upstream_url = ? AND COALESCE(repos.fork_url, '') = ?
		         AND repos.default_branch = ? AND repos.metadata_generation = ?
		  )`,
		ts, CustodyReturnReasonPreservedHeadUnavailable, ts,
		expected.ID, expected.RepoID, expected.Branch, expected.HeadSHA,
		expected.SubmittedHeadSHA, expected.Status, expected.LastPushedSHA,
		expected.PushTargetKind, expected.PushTargetFingerprint, expected.PushRef,
		expected.PushGeneration, expected.TerminalHeadVerifiedAt,
		UnavailableCustodyReleaseGateMoved, attempt.PreservedHead, attempt.LocalHead,
		attempt.RemoteHead, attempt.GateHead, attempt.TargetKind, attempt.TargetFingerprint,
		attempt.TargetRef, attempt.OwnershipGeneration, attempt.RepoGeneration,
		attempt.OwnershipGeneration,
		repo.WorkingPath, repo.UpstreamURL, repo.ForkURL, repo.DefaultBranch, attempt.RepoGeneration,
	)
	if err != nil {
		return false, fmt.Errorf("commit unavailable run custody: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("commit unavailable run custody: rows affected: %w", err)
	}
	if affected == 1 {
		return true, nil
	}
	current, err := d.GetRun(expected.ID)
	if err != nil {
		return false, err
	}
	if current != nil && current.RepoID == expected.RepoID && current.Branch == expected.Branch && current.HeadSHA == expected.HeadSHA &&
		current.CustodyReturnedAt != nil && current.CustodyReturnReason != nil && *current.CustodyReturnReason == CustodyReturnReasonPreservedHeadUnavailable {
		return false, nil
	}
	return false, ErrRunCustodyChanged
}

package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/kunchenguid/no-mistakes/internal/branchsync"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type preparedHeadAdvanceRecoveryStats struct {
	Prepared   int
	Reconciled int
	Skipped    int
	Failed     int
}

// reconcilePreparedHeadAdvances closes the only live-adoption state that a
// process restart may safely finish: the exact immutable transition was
// prepared while the run was active, its worktree is still clean at the strict
// forward candidate, and the gate CAS already moved from the recorded old head
// to that candidate. It runs under the same repo+branch mutex as admission and
// ordinary adoption, before generic stale-run failure and worktree cleanup.
//
// A prepared transition whose gate is still at the old head is deliberately
// not adopted; generic crash recovery may fail that run while the immutable
// candidate anchor remains. A different readable gate head is also left to
// ordinary stale-run recovery. Once the gate exactly equals the candidate,
// any failed proof aborts startup before stale-run recovery can destroy the
// evidence needed for a safe retry or explicit repair.
func (m *RunManager) reconcilePreparedHeadAdvances(ctx context.Context) (preparedHeadAdvanceRecoveryStats, error) {
	var stats preparedHeadAdvanceRecoveryStats
	advances, err := m.db.GetPreparedActiveRunHeadAdvances()
	if err != nil {
		stats.Failed = 1
		return stats, fmt.Errorf("list prepared run head advances: %w", err)
	}
	stats.Prepared = len(advances)
	for _, advance := range advances {
		mu := m.branchMutex(advance.RepoID, advance.Branch)
		mu.Lock()
		reconciled, reconcileErr := m.reconcilePreparedHeadAdvance(ctx, advance)
		mu.Unlock()
		if reconcileErr != nil {
			stats.Failed++
			return stats, fmt.Errorf("reconcile prepared run head advance for run %s: %w", advance.RunID, reconcileErr)
		}
		if !reconciled {
			stats.Skipped++
			continue
		}
		stats.Reconciled++
		slog.Info("reconciled prepared run head advance",
			"run_id", advance.RunID, "repo_id", advance.RepoID, "branch", advance.Branch,
			"candidate", advance.Candidate)
	}
	return stats, nil
}

func (m *RunManager) reconcilePreparedHeadAdvance(ctx context.Context, advance db.ActiveRunHeadAdvance) (bool, error) {
	if !branchsync.IsExactRunID(advance.RunID) {
		return false, fmt.Errorf("run identity is not one canonical complete ID")
	}
	if !branchsync.IsExactFullObjectID(advance.ExpectedHead) || !branchsync.IsExactFullObjectID(advance.Candidate) || advance.ExpectedHead == advance.Candidate {
		return false, fmt.Errorf("head transition is not one exact strict-forward object pair")
	}
	expectedAnchor := "refs/no-mistakes/run-head-candidates/" + advance.RunID + "/" + advance.Candidate
	if advance.AnchorRef != expectedAnchor {
		return false, fmt.Errorf("candidate anchor does not match the exact run and candidate")
	}

	run, err := m.db.GetRun(advance.RunID)
	if err != nil {
		return false, fmt.Errorf("reload exact run: %w", err)
	}
	if run == nil || run.RepoID != advance.RepoID || run.Branch != advance.Branch || run.HeadSHA != advance.ExpectedHead || run.Status != types.RunRunning {
		return false, fmt.Errorf("exact active run authority changed before startup reconciliation")
	}
	repo, err := m.db.GetRepo(advance.RepoID)
	if err != nil {
		return false, fmt.Errorf("reload exact repository: %w", err)
	}
	if repo == nil || repo.ID != run.RepoID {
		return false, fmt.Errorf("exact registered repository is unavailable")
	}

	gateDir := m.paths.RepoDir(repo.ID)
	workDir := m.paths.WorktreeDir(repo.ID, run.ID)
	bare, err := git.Run(ctx, gateDir, "rev-parse", "--is-bare-repository")
	if err != nil || bare != "true" {
		return false, fmt.Errorf("registered gate identity is unavailable or not bare")
	}
	branchRef, err := preparedHeadBranchRef(ctx, gateDir, run.Branch)
	if err != nil {
		return false, err
	}
	gateHead, err := git.Run(ctx, gateDir, "rev-parse", "--verify", branchRef+"^{commit}")
	if err != nil {
		return false, fmt.Errorf("read exact gate branch: %w", err)
	}
	if gateHead == advance.ExpectedHead {
		return false, nil
	}
	if gateHead != advance.Candidate {
		// Another exact gate writer won. There is no authority to adopt this
		// candidate, and generic stale-run recovery may safely fail the run
		// while leaving both the concurrent gate head and immutable journal.
		return false, nil
	}

	switch types.StepName(advance.StepName) {
	case types.StepReview, types.StepTest, types.StepDocument, types.StepLint:
	default:
		return false, fmt.Errorf("step %q cannot author a live head transition", advance.StepName)
	}

	anchor, err := git.Run(ctx, gateDir, "rev-parse", "--verify", advance.AnchorRef+"^{commit}")
	if err != nil || anchor != advance.Candidate {
		return false, fmt.Errorf("immutable candidate anchor is missing or changed")
	}
	oldObject, err := git.Run(ctx, gateDir, "rev-parse", "--verify", advance.ExpectedHead+"^{commit}")
	if err != nil || oldObject != advance.ExpectedHead {
		return false, fmt.Errorf("exact expected old commit is unavailable")
	}
	candidateObject, err := git.Run(ctx, gateDir, "rev-parse", "--verify", advance.Candidate+"^{commit}")
	if err != nil || candidateObject != advance.Candidate {
		return false, fmt.Errorf("exact candidate commit is unavailable")
	}
	if _, err := git.Run(ctx, gateDir, "merge-base", "--is-ancestor", advance.ExpectedHead, advance.Candidate); err != nil {
		return false, fmt.Errorf("candidate is not a strict descendant of the expected old head")
	}

	commonDir, err := git.Run(ctx, workDir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return false, fmt.Errorf("resolve prepared worktree gate identity: %w", err)
	}
	if !filepath.IsAbs(commonDir) {
		commonDir = filepath.Join(workDir, commonDir)
	}
	if !samePath(commonDir, gateDir) {
		return false, fmt.Errorf("prepared worktree belongs to a different gate")
	}
	worktreeHead, err := git.HeadSHA(ctx, workDir)
	if err != nil || worktreeHead != advance.Candidate {
		return false, fmt.Errorf("prepared worktree HEAD is not the exact candidate")
	}
	status, err := git.Run(ctx, workDir, "status", "--porcelain")
	if err != nil {
		return false, fmt.Errorf("read prepared worktree status: %w", err)
	}
	if strings.TrimSpace(status) != "" {
		return false, fmt.Errorf("prepared candidate worktree is not clean")
	}

	// AdvanceActiveRunHeadCAS repeats the exact run, publication, competing-run,
	// expected-head and immutable-journal predicates. Discovery and
	// Git preflight are never treated as authority.
	if err := m.db.AdvanceActiveRunHeadCAS(advance); err != nil {
		return false, err
	}

	finalRun, err := m.db.GetRun(advance.RunID)
	if err != nil || finalRun == nil || finalRun.HeadSHA != advance.Candidate || finalRun.Status != types.RunRunning {
		return false, fmt.Errorf("durable head advanced but final run proof failed")
	}
	if finalGate, gateErr := git.Run(ctx, gateDir, "rev-parse", "--verify", branchRef+"^{commit}"); gateErr != nil || finalGate != advance.Candidate {
		return false, fmt.Errorf("durable head advanced but final gate proof failed")
	}
	if finalAnchor, anchorErr := git.Run(ctx, gateDir, "rev-parse", "--verify", advance.AnchorRef+"^{commit}"); anchorErr != nil || finalAnchor != advance.Candidate {
		return false, fmt.Errorf("durable head advanced but final anchor proof failed")
	}
	return true, nil
}

func preparedHeadBranchRef(ctx context.Context, gateDir, branch string) (string, error) {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return "", fmt.Errorf("prepared transition has no exact branch")
	}
	ref := "refs/heads/" + branch
	if strings.HasPrefix(branch, "refs/heads/") {
		ref = branch
	} else if strings.HasPrefix(branch, "refs/") {
		return "", fmt.Errorf("prepared transition branch is outside refs/heads")
	}
	if _, err := git.Run(ctx, gateDir, "check-ref-format", ref); err != nil {
		return "", fmt.Errorf("prepared transition branch is invalid: %w", err)
	}
	return ref, nil
}

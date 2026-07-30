package branchsync

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const (
	ForwardRecoveryOperatorAuthorized = "operator_authorized_forward_head"
	ForwardRecoveryPhaseInspected     = "inspected"
	ForwardRecoveryPhaseAnchored      = "candidate_anchored"
	ForwardRecoveryPhaseAdopted       = "head_adopted"
	ForwardRecoveryPhaseAdvanced      = "local_advanced"
	ForwardRecoveryPhaseComplete      = "custody_returned"
)

// ForwardRecoveryResult is deliberately explicit about partial durable state.
// An anchor or DB adoption is never rolled back after a later refusal.
type ForwardRecoveryResult struct {
	Mode         string
	State        string
	Safety       string
	Phase        string
	RunID        string
	RepoID       string
	Branch       string
	LocalHead    string
	RecordedHead string
	Candidate    string
	AnchorRef    string
	Changed      bool
	Recovered    bool
	Error        string
}

type forwardRecoveryHooks struct {
	AfterAnchor       func() error
	BeforeHeadCAS     func() error
	AfterHeadCAS      func() error
	BeforeFastForward func() error
	AfterFastForward  func() error
	BeforeFinalProof  func() error
	BeforeCustodyCAS  func() error
}

// RecoverAuthorizedForwardHead repairs only the captain-approved historical
// incident shape. Candidate is explicit operator authorization; ancestry and a
// gate match preserve history but do not infer that the old run produced it.
// The caller must hold the daemon's repo+branch lock for this entire call.
func (s *Service) RecoverAuthorizedForwardHead(ctx context.Context, runID, candidate string) ForwardRecoveryResult {
	result := ForwardRecoveryResult{
		Mode: ForwardRecoveryOperatorAuthorized, State: StatePipelineOwned,
		Safety: "blocked_forward_recovery", Phase: ForwardRecoveryPhaseInspected,
		RunID: strings.TrimSpace(runID), Candidate: strings.TrimSpace(candidate),
	}
	deny := func(safety, message string) ForwardRecoveryResult {
		result.Safety = safety
		result.Error = message
		return result
	}
	if !IsExactFullObjectID(result.Candidate) {
		return deny("blocked_forward_candidate_not_exact", "the operator-authorized candidate must be one canonical full 40- or 64-hex commit object ID; abbreviations, refs, tags, and revision expressions are refused")
	}
	if !IsExactRunID(result.RunID) {
		return deny("blocked_forward_run_not_exact", "an exact run ID is required")
	}

	run, recovery, refusal := s.loadForwardRecoveryAuthority(result.RunID, result.Candidate)
	if refusal != "" {
		return deny("blocked_forward_run_ineligible", refusal)
	}
	result.RepoID = run.RepoID
	result.Branch = run.Branch
	result.RecordedHead = run.HeadSHA
	if run.RepoID != s.Repo.ID {
		return deny("blocked_forward_wrong_repo", "the exact run does not belong to the registered invoking repository")
	}
	if recovery == nil {
		result.AnchorRef = recoveryCandidateAnchorRef(run.ID, result.Candidate)
	} else {
		result.AnchorRef = recovery.AnchorRef
	}

	local, refusal := s.readForwardLocalState(ctx, run.Branch)
	result.LocalHead = local.head
	if refusal != "" {
		return deny("blocked_forward_worktree", refusal)
	}
	if refusal := s.validateForwardRunAndSteps(ctx, run, recovery, local.head, result.Candidate); refusal != "" {
		return deny("blocked_forward_run_ineligible", refusal)
	}

	expected := run.HeadSHA
	authorityLocal := local.head
	if recovery != nil {
		expected = recovery.ExpectedHeadSHA
		authorityLocal = recovery.LocalHeadSHA
		result.Phase = ForwardRecoveryPhaseAdopted
		if local.head == result.Candidate {
			result.Phase = ForwardRecoveryPhaseAdvanced
		}
	}

	gateHead, err := s.exactGateHead(ctx, run.Branch)
	if err != nil {
		return deny("blocked_forward_gate_unavailable", err.Error())
	}
	if gateHead != result.Candidate {
		return deny("blocked_forward_gate_mismatch", fmt.Sprintf("the gate branch is at %s, not the exact operator-authorized candidate %s", gateHead, result.Candidate))
	}
	anchoredBefore, anchorReadErr := git.Run(ctx, s.workDir(), "rev-parse", "--verify", result.AnchorRef+"^{commit}")
	anchorAlreadyExact := anchorReadErr == nil && anchoredBefore == result.Candidate
	if refusal := s.fetchAndAnchorForwardCandidate(ctx, run, result.Candidate, result.AnchorRef); refusal != "" {
		return deny("blocked_forward_anchor", refusal)
	}
	result.Changed = result.Changed || !anchorAlreadyExact
	result.Phase = ForwardRecoveryPhaseAnchored
	if recovery != nil {
		result.Phase = ForwardRecoveryPhaseAdopted
		if local.head == result.Candidate {
			result.Phase = ForwardRecoveryPhaseAdvanced
		}
	}
	if s.forwardRecoveryHooks.AfterAnchor != nil {
		if err := s.forwardRecoveryHooks.AfterAnchor(); err != nil {
			return deny("interrupted_after_forward_anchor", err.Error())
		}
	}
	if !isAncestor(ctx, s.workDir(), expected, result.Candidate) || expected == result.Candidate {
		return deny("blocked_forward_candidate_ancestry", "the recorded head is not a strict ancestor of the exact candidate")
	}
	if !isAncestor(ctx, s.workDir(), authorityLocal, expected) || authorityLocal == expected {
		return deny("blocked_forward_submitted_ancestry", "the required strict local == submitted < recorded relationship is not present")
	}
	if run.ReviewApprovedHeadSHA != nil && !isAncestor(ctx, s.workDir(), *run.ReviewApprovedHeadSHA, result.Candidate) {
		return deny("blocked_forward_review_ancestry", "the recorded review-approved head is not an ancestor of the exact candidate")
	}

	// Re-read every local, gate, run, step and competing-run assumption at the
	// immediate database boundary. An immutable anchor may honestly remain.
	run, recovery, refusal = s.loadForwardRecoveryAuthority(result.RunID, result.Candidate)
	if refusal != "" {
		return deny("blocked_forward_pre_cas_race", refusal)
	}
	local, refusal = s.readForwardLocalState(ctx, run.Branch)
	result.LocalHead = local.head
	validLocalHead := local.head == authorityLocal
	if recovery != nil {
		validLocalHead = validLocalHead || local.head == result.Candidate
	}
	if refusal != "" || !validLocalHead {
		return deny("blocked_forward_pre_cas_race", "the local branch, HEAD, cleanliness, sequencer state, or unique checkout changed before the head CAS")
	}
	if refusal := s.validateForwardRunAndSteps(ctx, run, recovery, local.head, result.Candidate); refusal != "" {
		return deny("blocked_forward_pre_cas_race", refusal)
	}
	if gateHead, err = s.exactGateHead(ctx, run.Branch); err != nil || gateHead != result.Candidate {
		return deny("blocked_forward_gate_race", "the gate branch changed before the head CAS")
	}
	if anchored, err := git.Run(ctx, s.workDir(), "rev-parse", "--verify", result.AnchorRef+"^{commit}"); err != nil || anchored != result.Candidate {
		return deny("blocked_forward_anchor_race", "the immutable candidate anchor is missing or changed")
	}
	if s.forwardRecoveryHooks.BeforeHeadCAS != nil {
		if err := s.forwardRecoveryHooks.BeforeHeadCAS(); err != nil {
			return deny("interrupted_before_forward_head_cas", err.Error())
		}
	}

	audit := db.RunHeadRecovery{
		RunID: run.ID, RepoID: run.RepoID, Branch: run.Branch, BaseSHA: run.BaseSHA,
		ExpectedHeadSHA: expected, CandidateHeadSHA: result.Candidate,
		LocalHeadSHA: authorityLocal, AnchorRef: result.AnchorRef,
		ReviewApprovedSHA: run.ReviewApprovedHeadSHA,
	}
	if recovery == nil {
		if err := s.DB.AdoptCompletedRunHeadCAS(audit); err != nil {
			return deny("blocked_forward_head_cas", fmt.Sprintf("the exact terminal head CAS failed: %v", err))
		}
		result.Changed = true
	} else if run.HeadSHA != result.Candidate {
		return deny("blocked_forward_audit_mismatch", "the recovery audit exists but the run head no longer equals its exact candidate")
	}
	result.RecordedHead = result.Candidate
	result.Phase = ForwardRecoveryPhaseAdopted
	if s.forwardRecoveryHooks.AfterHeadCAS != nil {
		if err := s.forwardRecoveryHooks.AfterHeadCAS(); err != nil {
			return deny("interrupted_after_forward_head_cas", err.Error())
		}
	}

	// Retry after the DB transaction arrives here with local at either the
	// original submitted head or the exact candidate. No other local shape is
	// ever accepted.
	if s.forwardRecoveryHooks.BeforeFastForward != nil {
		if err := s.forwardRecoveryHooks.BeforeFastForward(); err != nil {
			return deny("interrupted_before_forward_fast_forward", err.Error())
		}
	}
	run, recovery, refusal = s.loadForwardRecoveryAuthority(result.RunID, result.Candidate)
	if refusal != "" || recovery == nil {
		return deny("blocked_forward_pre_apply_race", "the exact recovery audit or run eligibility changed before the strict fast-forward")
	}
	local, refusal = s.readForwardLocalState(ctx, run.Branch)
	result.LocalHead = local.head
	if refusal != "" {
		return deny("blocked_forward_pre_apply_race", refusal)
	}
	if refusal := s.validateForwardRunAndSteps(ctx, run, recovery, local.head, result.Candidate); refusal != "" {
		return deny("blocked_forward_pre_apply_race", refusal)
	}
	if gateHead, err = s.exactGateHead(ctx, run.Branch); err != nil || gateHead != result.Candidate {
		return deny("blocked_forward_gate_race", "the gate branch changed before the strict fast-forward")
	}
	switch local.head {
	case result.Candidate:
		// Retry after the local fast-forward.
	case authorityLocal:
		if _, err := git.Run(ctx, s.workDir(), "merge", "--ff-only", "--no-edit", result.Candidate); err != nil {
			result.LocalHead, _ = git.HeadSHA(ctx, s.workDir())
			return deny("blocked_forward_fast_forward", fmt.Sprintf("strict local fast-forward failed; final HEAD is %s: %v", result.LocalHead, err))
		}
		result.Changed = true
	default:
		return deny("blocked_forward_local_mismatch", "the local HEAD is neither the exact submitted head nor the adopted candidate")
	}
	result.LocalHead, _ = git.HeadSHA(ctx, s.workDir())
	result.Phase = ForwardRecoveryPhaseAdvanced
	if s.forwardRecoveryHooks.AfterFastForward != nil {
		if err := s.forwardRecoveryHooks.AfterFastForward(); err != nil {
			return deny("interrupted_after_forward_fast_forward", err.Error())
		}
	}

	if s.forwardRecoveryHooks.BeforeFinalProof != nil {
		if err := s.forwardRecoveryHooks.BeforeFinalProof(); err != nil {
			return deny("interrupted_before_forward_final_proof", err.Error())
		}
	}
	// Final Git proofs immediately precede the full-predicate custody CAS.
	local, refusal = s.readForwardLocalState(ctx, run.Branch)
	result.LocalHead = local.head
	if refusal != "" || local.head != result.Candidate {
		return deny("blocked_forward_final_local", "the final local branch is not uniquely checked out, clean, and exactly at the candidate; custody was not stamped")
	}
	if gateHead, err = s.exactGateHead(ctx, run.Branch); err != nil || gateHead != result.Candidate {
		return deny("blocked_forward_final_gate", "the final gate branch does not equal the exact candidate; custody was not stamped")
	}
	if anchored, err := git.Run(ctx, s.workDir(), "rev-parse", "--verify", result.AnchorRef+"^{commit}"); err != nil || anchored != result.Candidate {
		return deny("blocked_forward_final_anchor", "the immutable candidate anchor is missing or changed; custody was not stamped")
	}
	run, recovery, refusal = s.loadForwardRecoveryAuthority(result.RunID, result.Candidate)
	if refusal != "" || recovery == nil {
		return deny("blocked_forward_final_predicate", "the exact run or recovery audit changed before the custody CAS")
	}
	if refusal := s.validateForwardRunAndSteps(ctx, run, recovery, local.head, result.Candidate); refusal != "" {
		return deny("blocked_forward_final_predicate", refusal)
	}
	if s.forwardRecoveryHooks.BeforeCustodyCAS != nil {
		if err := s.forwardRecoveryHooks.BeforeCustodyCAS(); err != nil {
			return deny("interrupted_before_forward_custody_cas", err.Error())
		}
	}
	stamped, err := s.DB.CompleteRunHeadRecoveryCAS(audit)
	if err != nil {
		return deny("blocked_forward_custody_cas", fmt.Sprintf("the final full-predicate custody CAS failed: %v", err))
	}
	result.Changed = result.Changed || stamped
	result.Recovered = true
	result.State = StateCustodyReturned
	result.Safety = "custody_returned"
	result.Phase = ForwardRecoveryPhaseComplete
	result.Error = ""
	return result
}

type forwardLocalState struct {
	root   string
	branch string
	head   string
}

func (s *Service) readForwardLocalState(ctx context.Context, expectedBranch string) (forwardLocalState, string) {
	var state forwardLocalState
	root, err := git.FindGitRoot(s.workDir())
	if err != nil {
		return state, "the invoking path is not a non-bare registered Git worktree"
	}
	state.root = root
	mainRoot, err := git.FindMainRepoRoot(root)
	if err != nil || !samePath(mainRoot, s.Repo.WorkingPath) {
		return state, "the invoking worktree does not belong to the exact registered repository"
	}
	localCommon, err := resolvedGitCommonDir(ctx, root)
	if err != nil {
		return state, "the invoking worktree common Git directory could not be resolved"
	}
	registeredCommon, err := resolvedGitCommonDir(ctx, s.Repo.WorkingPath)
	if err != nil || !samePath(localCommon, registeredCommon) || samePath(localCommon, s.GateDir) {
		return state, "the invoking worktree has the wrong Git common directory"
	}
	state.branch, err = git.CurrentBranch(ctx, root)
	if err != nil || state.branch == "" || state.branch == "HEAD" {
		return state, "recovery requires the exact checked-out branch, not detached HEAD"
	}
	if state.branch != expectedBranch {
		return state, fmt.Sprintf("the invoking branch %s does not equal the exact run branch %s", state.branch, expectedBranch)
	}
	state.head, err = git.HeadSHA(ctx, root)
	if err != nil {
		return state, "the invoking HEAD could not be resolved"
	}
	clean, reason := worktreeClean(ctx, root)
	if !clean {
		return state, "the invoking worktree is not completely clean: " + reason
	}
	if duplicateBranchCheckout(ctx, root, state.branch) {
		return state, "the exact run branch is checked out in zero or multiple worktrees"
	}
	return state, ""
}

func (s *Service) loadForwardRecoveryAuthority(runID, candidate string) (*db.Run, *db.RunHeadRecovery, string) {
	run, err := s.DB.GetRun(runID)
	if err != nil {
		return nil, nil, "the exact run row could not be read"
	}
	if run == nil {
		return nil, nil, "the exact run ID was not found; prefixes are never resolved"
	}
	recovery, err := s.DB.GetRunHeadRecovery(runID)
	if err != nil {
		return nil, nil, "the recovery audit row could not be read"
	}
	if recovery != nil {
		if recovery.RunID != run.ID || recovery.RepoID != run.RepoID || recovery.Branch != run.Branch ||
			recovery.BaseSHA != run.BaseSHA || recovery.CandidateHeadSHA != candidate ||
			!sameOptionalHead(recovery.ReviewApprovedSHA, run.ReviewApprovedHeadSHA) ||
			recovery.AnchorRef != recoveryCandidateAnchorRef(run.ID, candidate) {
			return nil, nil, "a conflicting recovery audit record already exists for this run"
		}
	}
	return run, recovery, ""
}

func (s *Service) validateForwardRunAndSteps(ctx context.Context, run *db.Run, recovery *db.RunHeadRecovery, localHead, candidate string) string {
	if run.Status != types.RunCompleted {
		return "only an exactly completed run is eligible; pending, running, failed, and cancelled runs are excluded"
	}
	if run.Error != nil || run.AwaitingAgentSince != nil || run.PushActive {
		return "the run has an error, awaiting-agent marker, or active push authority"
	}
	if run.LastPushedSHA != nil || run.PushTargetKind != nil || run.PushTargetFingerprint != nil || run.PushRef != nil || run.LastPushedAt != nil || run.PushGeneration != nil ||
		run.PRURL != nil || run.PRStateObservedAt != nil || run.CIReadyAt != nil || (run.PRState != nil && *run.PRState != "" && *run.PRState != "none") {
		return "the run has push, PR, or CI publication authority and is not the completed local-only incident shape"
	}
	if run.SubmittedHeadSHA == nil || strings.TrimSpace(*run.SubmittedHeadSHA) == "" {
		return "the run has no exact submitted head"
	}
	if recovery == nil {
		if run.CustodyReturnedAt != nil {
			return "custody was already returned without the exact recovery audit witness"
		}
		if localHead != *run.SubmittedHeadSHA {
			return "narrow recovery requires local HEAD to equal the recorded submitted head"
		}
		if run.HeadSHA == candidate {
			return "the candidate is already recorded without the exact recovery audit witness"
		}
	} else {
		if recovery.LocalHeadSHA != *run.SubmittedHeadSHA || recovery.ExpectedHeadSHA == candidate || run.HeadSHA != candidate {
			return "the recovery audit, submitted head, expected old head, and current run head do not match"
		}
		if localHead != recovery.LocalHeadSHA && localHead != candidate {
			return "retry requires local HEAD at the exact original submitted head or exact candidate"
		}
	}

	runs, err := s.DB.GetRunsByRepo(run.RepoID)
	if err != nil {
		return "same-branch run ordering could not be read"
	}
	foundLatest := false
	for _, other := range runs {
		if other.Branch != run.Branch {
			continue
		}
		if !foundLatest {
			if other.ID != run.ID {
				return "another same-branch run is newer, including same-second ULID ordering"
			}
			foundLatest = true
			continue
		}
		if other.ID != run.ID && (other.Status == types.RunPending || other.Status == types.RunRunning) {
			return "another active same-branch run exists"
		}
	}
	if !foundLatest {
		return "the exact run is not the latest same-branch row"
	}

	steps, err := s.DB.GetStepsByRun(run.ID)
	if err != nil {
		return "the exact run steps could not be read"
	}
	if refusal := exactLocalOnlyStepRefusal(steps); refusal != "" {
		return refusal
	}
	if recovery != nil && run.CustodyReturnedAt != nil && localHead != candidate {
		return "a completed retry requires local HEAD at the exact candidate"
	}
	_ = ctx
	return ""
}

func exactLocalOnlyStepRefusal(steps []*db.StepResult) string {
	if len(steps) != len(types.AllSteps()) {
		return "the run must have exactly nine step rows"
	}
	seen := make(map[types.StepName]bool, len(steps))
	for _, step := range steps {
		if step == nil || seen[step.StepName] || step.StepName.Order() == 0 {
			return "the run has a missing, duplicate, or unknown step row"
		}
		seen[step.StepName] = true
		if step.ExitCode == nil || *step.ExitCode != 0 || step.CompletedAt == nil || step.Error != nil {
			return "every step must have exit code zero, a completion timestamp, and no error"
		}
		if step.StepName.Order() <= types.StepLint.Order() {
			if step.Status != types.StepStatusCompleted || step.StartedAt == nil {
				return "Intent through Lint must be exactly completed with start and completion timestamps"
			}
		} else if step.Status != types.StepStatusSkipped {
			return "Push, PR, and CI must be exactly skipped"
		}
	}
	return ""
}

func (s *Service) fetchAndAnchorForwardCandidate(ctx context.Context, run *db.Run, candidate, anchorRef string) string {
	stagingRef := "refs/no-mistakes/recovery-fetch/" + run.ID + "/" + candidate
	for _, ref := range []string{stagingRef, anchorRef} {
		if _, err := git.Run(ctx, s.workDir(), "check-ref-format", ref); err != nil {
			return "a candidate-specific recovery ref is malformed"
		}
	}
	if existing, err := git.Run(ctx, s.workDir(), "rev-parse", "--verify", stagingRef+"^{commit}"); err == nil && existing != candidate {
		return "a conflicting candidate fetch ref already exists"
	}
	if existing, err := git.Run(ctx, s.workDir(), "rev-parse", "--verify", stagingRef+"^{commit}"); err != nil || existing != candidate {
		refspec := "refs/heads/" + run.Branch + ":" + stagingRef
		if _, err := git.Run(ctx, s.workDir(), "fetch", "--no-tags", "--no-write-fetch-head", s.GateDir, refspec); err != nil {
			return "the exact gate branch could not be fetched into a private candidate ref"
		}
	}
	fetched, err := git.Run(ctx, s.workDir(), "rev-parse", "--verify", stagingRef+"^{commit}")
	if err != nil || fetched != candidate {
		return "the fetched gate branch did not resolve to the exact candidate"
	}
	if gateHead, err := s.exactGateHead(ctx, run.Branch); err != nil || gateHead != candidate {
		return "the gate branch changed while the exact candidate was fetched"
	}
	if err := createOrVerifyImmutableCandidateRef(ctx, s.workDir(), anchorRef, candidate); err != nil {
		return err.Error()
	}
	_, _ = git.Run(ctx, s.workDir(), "update-ref", "-d", stagingRef, candidate)
	return ""
}

func createOrVerifyImmutableCandidateRef(ctx context.Context, dir, ref, candidate string) error {
	if existing, err := git.Run(ctx, dir, "rev-parse", "--verify", ref+"^{commit}"); err == nil {
		if existing != candidate {
			return fmt.Errorf("immutable candidate anchor conflicts with commit %s", existing)
		}
		return nil
	}
	if _, err := git.Run(ctx, dir, "update-ref", ref, candidate, strings.Repeat("0", len(candidate))); err != nil {
		if existing, readErr := git.Run(ctx, dir, "rev-parse", "--verify", ref+"^{commit}"); readErr == nil && existing == candidate {
			return nil
		}
		return fmt.Errorf("create immutable candidate anchor: %w", err)
	}
	anchored, err := git.Run(ctx, dir, "rev-parse", "--verify", ref+"^{commit}")
	if err != nil || anchored != candidate {
		return fmt.Errorf("immutable candidate anchor verification failed")
	}
	return nil
}

func (s *Service) exactGateHead(ctx context.Context, branch string) (string, error) {
	if strings.TrimSpace(s.GateDir) == "" {
		return "", fmt.Errorf("the registered gate directory is unavailable")
	}
	head, err := git.Run(ctx, s.GateDir, "rev-parse", "--verify", "refs/heads/"+branch+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("the exact gate branch could not be resolved: %w", err)
	}
	return head, nil
}

func resolvedGitCommonDir(ctx context.Context, dir string) (string, error) {
	common, err := git.Run(ctx, dir, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(dir, common)
	}
	return filepath.Clean(common), nil
}

func recoveryCandidateAnchorRef(runID, candidate string) string {
	return "refs/no-mistakes/recovery-candidates/" + runID + "/" + candidate
}

// IsExactFullObjectID accepts only canonical SHA-1/SHA-256 object IDs. It
// intentionally rejects uppercase, abbreviations, refs, and rev expressions.
func IsExactFullObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	if strings.Trim(value, "0") == "" {
		return false
	}
	for _, r := range value {
		if unicode.IsDigit(r) || (r >= 'a' && r <= 'f') {
			continue
		}
		return false
	}
	return true
}

// IsExactRunID accepts only a canonical complete ULID. Recovery never resolves
// prefixes, lowercase variants, or a latest/current fallback.
func IsExactRunID(value string) bool {
	if len(value) != 26 {
		return false
	}
	const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	for _, r := range value {
		if !strings.ContainsRune(crockford, r) {
			return false
		}
	}
	return true
}

func sameOptionalHead(a, b *string) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

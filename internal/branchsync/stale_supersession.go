package branchsync

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// staleSupersessionEvidence is the complete immutable plan for replacing one
// stale terminal owner with a later exact pipeline lineage.
type staleSupersessionEvidence struct {
	state      State
	old        *db.Run
	later      *db.Run
	lineage    *db.Run
	repo       *db.Repo
	authority  db.CustodyReleaseAuthority
	targetURL  string
	targetRef  string
	localHead  string
	remoteHead string
	gateHead   string
}

// PlanStaleCustody performs the new read-only custody plan. Outside the exact
// impossible-custody shape it returns ordinary branch-sync truth unchanged.
// A positive plan names both run IDs in the only supported transition command.
func (s *Service) PlanStaleCustody(ctx context.Context) State {
	state, selected, _ := s.inspect(ctx)
	if selected == nil || state.State != StatePipelineOwned || state.Safety != "blocked_pipeline_owned_recoverable" {
		return state
	}
	evidence, _ := s.buildStaleSupersessionEvidence(ctx, state, selected, "", nil)
	if evidence == nil {
		return state
	}
	transition := staleSupersessionTransition(evidence)
	evidence.state.CustodyTransition = transition
	evidence.state.Safety = "safe_stale_custody_supersession"
	evidence.state.Error = "an older terminal run still owns recoverable custody, but a later exact pipeline lineage binds this clean local submission to the exact gate and remote pushed head; run only the identity-bound transition returned below"
	evidence.state.NextAction = &NextAction{
		Code:    "supersede_stale_custody",
		Command: fmt.Sprintf("no-mistakes axi sync --supersede-stale --run %s --later-run %s", evidence.old.ID, evidence.later.ID),
	}
	return evidence.state
}

// SupersedeStaleCustody returns custody from one stale terminal run and adopts
// a later run's exact submitted-head-to-pushed-head transition. It never uses a
// patch ID, tree equivalence, or an inferred rewrite. Authority comes only from
// durable run identities plus exact Git and configured-target facts.
//
// The old preserved head must remain recoverable in both owned object stores.
// A later exact-pushed run must be connected to it by another exact run whose
// submitted head is the old preserved head; that run's pushed head must be an
// ancestor of the later submission. The invoking branch must be either the
// later run's exact submission, or the old run's exact submission when the old
// output is itself the later submission. The second form lets this transition
// recover its own dogfood branch without weakening the direct PR 3838 path.
//
// Before mutation, direct head-scoped refs anchor the old preserved head in
// both object stores, the pre-adoption local head, and the exact remote and gate
// heads. A durable journal binds all three runs, every head, the target, and the
// repository and branch-ownership generations. The checked-out branch moves by
// update-ref compare-and-swap plus read-tree, and a no-op gate CAS is the final
// Git boundary before the transactional old-run custody stamp. A crash after
// the local CAS resumes from the journal and never guesses from current state.
func (s *Service) SupersedeStaleCustody(ctx context.Context, oldRunID, laterRunID string) State {
	if refusal, blocked := s.gateContextRefusal(ctx); blocked {
		return refusal
	}
	oldRunID = strings.TrimSpace(oldRunID)
	laterRunID = strings.TrimSpace(laterRunID)
	state, selected, _ := s.inspect(ctx)
	old, oldErr := s.DB.GetRun(oldRunID)
	later, laterErr := s.DB.GetRun(laterRunID)
	attempt, attemptErr := s.DB.GetStaleCustodySupersession(oldRunID)
	if oldErr != nil || laterErr != nil || attemptErr != nil || old == nil || later == nil {
		return blockedPlan(state, state.State, "blocked_supersede_run_identity", "the exact old and later run ids could not be resolved in this repository; no files or refs were changed")
	}
	if old.CustodyReturnedAt != nil {
		return s.completedStaleSupersession(ctx, state, old, later, attempt)
	}
	if selected == nil || selected.ID != old.ID {
		if !terminalRunStatus(old.Status) || !terminalRunStatus(later.Status) {
			return blockedPlan(state, state.State, "blocked_supersede_run_active", "an active run still participates in this branch ownership; no files or refs were changed")
		}
		return blockedPlan(state, state.State, "blocked_supersede_run_identity", "the requested old run is not the exact authoritative owner of this repository and checked-out branch; no files or refs were changed")
	}
	evidence, blocked := s.buildStaleSupersessionEvidence(ctx, state, old, later.ID, attempt)
	if evidence == nil {
		return blocked
	}
	transition := staleSupersessionTransition(evidence)
	state = evidence.state
	state.CustodyTransition = transition

	base := staleSupersessionRef(old.ID)
	oldLocalRef := staleSupersessionAnchorRef(base, "preserved-local", old.HeadSHA)
	if blocked, ok := anchorExactCommit(ctx, state, transition, s.workDir(), oldLocalRef, old.HeadSHA, &transition.PreservedLocalAnchor); !ok {
		return blocked
	}
	oldGateRef := staleSupersessionAnchorRef(base, "preserved-gate", old.HeadSHA)
	if blocked, ok := anchorExactCommit(ctx, state, transition, s.GateDir, oldGateRef, old.HeadSHA, &transition.PreservedGateAnchor); !ok {
		return blocked
	}
	localRef := staleSupersessionAnchorRef(base, "local", evidence.localHead)
	if blocked, ok := anchorExactCommit(ctx, state, transition, s.workDir(), localRef, evidence.localHead, &transition.LocalAnchor); !ok {
		return blocked
	}

	remoteStagingRef := base + "/remote-staging"
	branch := evidence.old.Branch
	stagingCtx, cancelStaging := context.WithTimeout(ctx, s.networkBudget())
	stagingErr := git.FetchRemoteBranchToPrivateRef(stagingCtx, s.workDir(), evidence.targetURL, branch, remoteStagingRef)
	cancelStaging()
	if stagingErr != nil {
		return blockedSupersession(state, transition, "blocked_supersede_preserve_failed", "the exact remote head could not be fetched before creating its safety anchor; existing anchors remain intact")
	}
	fetched, fetchErr := git.Run(ctx, s.workDir(), "rev-parse", remoteStagingRef+"^{commit}")
	if fetchErr != nil || fetched != evidence.remoteHead {
		_, _ = git.Run(ctx, s.workDir(), "update-ref", "-d", remoteStagingRef)
		return blockedSupersession(state, transition, "blocked_supersede_remote_changed", "the configured remote branch changed while its safety anchor was being prepared; existing anchors remain intact")
	}
	remoteRef := staleSupersessionAnchorRef(base, "remote", evidence.remoteHead)
	if blocked, ok := anchorExactCommit(ctx, state, transition, s.workDir(), remoteRef, fetched, &transition.RemoteAnchor); !ok {
		_, _ = git.Run(ctx, s.workDir(), "update-ref", "-d", remoteStagingRef)
		return blocked
	}
	_, _ = git.Run(ctx, s.workDir(), "update-ref", "-d", remoteStagingRef)

	gateRef := staleSupersessionAnchorRef(base, "gate", evidence.gateHead)
	if blocked, ok := anchorExactCommit(ctx, state, transition, s.GateDir, gateRef, evidence.gateHead, &transition.GateAnchor); !ok {
		return blocked
	}

	if attempt == nil {
		var err error
		attempt, err = s.DB.PrepareStaleCustodySupersession(old, evidence.later, evidence.lineage, evidence.repo, db.StaleCustodySupersession{
			OldRunID: old.ID, LaterRunID: evidence.later.ID, LineageRunID: evidence.lineage.ID,
			RepoID: old.RepoID, Branch: old.Branch, OldHead: old.HeadSHA,
			LocalHead: evidence.localHead, LaterSubmittedHead: ptr(evidence.later.SubmittedHeadSHA),
			LaterPushedHead: ptr(evidence.later.LastPushedSHA), LineagePushedHead: ptr(evidence.lineage.LastPushedSHA),
			RemoteHead: evidence.remoteHead, GateHead: evidence.gateHead,
			TargetKind: targetKind(evidence.repo), TargetFingerprint: TargetFingerprint(evidence.repo.PushURL()), TargetRef: evidence.targetRef,
			OwnershipGeneration: evidence.authority.OwnershipGeneration, RepoGeneration: evidence.authority.RepoGeneration,
		})
		if err != nil {
			return blockedSupersession(state, transition, "blocked_supersede_assumptions_changed", "the exact repository, run lineage, or branch ownership generation changed before the transition was journaled; safety anchors remain intact")
		}
	} else if !sameStaleSupersessionAttempt(attempt, evidence) {
		return blockedSupersession(state, transition, "blocked_supersede_assumptions_changed", "the durable stale-custody journal does not match the exact requested lineage; safety anchors remain intact")
	}
	if attempt.OwnershipGeneration != evidence.authority.OwnershipGeneration || attempt.RepoGeneration != evidence.authority.RepoGeneration {
		rebound, err := s.DB.RebindStaleCustodySupersessionAuthority(old.ID, attempt, evidence.authority)
		if err != nil {
			return blockedSupersession(state, transition, "blocked_supersede_assumptions_changed", "the durable stale-custody journal could not be rebound to the freshly verified authority generations; safety anchors remain intact")
		}
		attempt = rebound
	}

	currentHead, headErr := git.HeadSHA(ctx, s.workDir())
	if headErr != nil || (currentHead != attempt.LocalHead && currentHead != attempt.LaterPushedHead) {
		return blockedSupersession(state, transition, "blocked_supersede_submission_mismatch", "the local branch no longer equals either side of the journaled exact adoption; safety anchors remain intact")
	}
	if s.beforeStaleSupersessionLocalCAS != nil {
		s.beforeStaleSupersessionLocalCAS()
	}
	requireClean := currentHead == attempt.LocalHead
	if refusal := s.recheckStaleSupersession(ctx, evidence, attempt, currentHead, requireClean); refusal != nil {
		refusal.CustodyTransition = transition
		return *refusal
	}

	changed := false
	branchRef := "refs/heads/" + attempt.Branch
	if currentHead == attempt.LocalHead {
		if _, err := git.Run(ctx, s.workDir(), "update-ref", branchRef, attempt.LaterPushedHead, attempt.LocalHead); err != nil {
			return blockedSupersession(state, transition, "blocked_supersede_assumptions_changed", "the local branch moved at the exact adoption boundary; no worktree files were changed")
		}
		changed = true
		if s.afterStaleSupersessionLocalCAS != nil {
			s.afterStaleSupersessionLocalCAS()
		}
	}
	if _, err := git.Run(ctx, s.workDir(), "read-tree", "-m", "-u", attempt.LocalHead, attempt.LaterPushedHead); err != nil {
		rollback := ""
		if _, rollbackErr := git.Run(ctx, s.workDir(), "update-ref", branchRef, attempt.LocalHead, attempt.LaterPushedHead); rollbackErr != nil {
			rollback = fmt.Sprintf("; the branch could not be restored and remains at anchored head %s", attempt.LaterPushedHead)
		}
		blocked := blockedSupersession(state, transition, "blocked_supersede_worktree_busy", "the worktree changed at the exact adoption boundary, so Git refused to overwrite it"+rollback+"; custody was not released")
		blocked.NextAction = &NextAction{Code: "inspect_worktree", Command: "git status"}
		return blocked
	}
	finalHead, _ := git.HeadSHA(ctx, s.workDir())
	finalClean, finalReason := worktreeClean(ctx, s.workDir())
	if finalHead != attempt.LaterPushedHead || !finalClean {
		blocked := blockedSupersession(state, transition, "blocked_supersede_apply_failed", fmt.Sprintf("the exact adoption did not finish cleanly at %s (%s); every pre-transition head remains anchored and custody was not released", finalHead, finalReason))
		blocked.NextAction = &NextAction{Code: "inspect_worktree", Command: "git status"}
		return blocked
	}
	if err := s.DB.MarkStaleCustodySupersessionLocalMoved(old.ID); err != nil {
		return blockedSupersession(state, transition, "blocked_supersede_assumptions_changed", "the branch ownership generation changed after exact adoption; every head remains anchored and custody was not released")
	}
	attempt.Phase = db.StaleCustodySupersessionLocalMoved

	if refusal := s.recheckStaleSupersession(ctx, evidence, attempt, attempt.LaterPushedHead, true); refusal != nil {
		refusal.CustodyTransition = transition
		return *refusal
	}
	if s.beforeStaleSupersessionCommit != nil {
		s.beforeStaleSupersessionCommit()
	}
	// Remote refs have no compare-and-swap read primitive. This final fresh read
	// is their linearization observation; a later remote move is new external
	// state and cannot make adopting the already anchored exact push lose work.
	remoteCtx, cancelRemote := context.WithTimeout(ctx, s.networkBudget())
	liveRemote, remoteErr := git.LsRemote(remoteCtx, s.workDir(), evidence.targetURL, evidence.targetRef)
	cancelRemote()
	if remoteErr != nil || liveRemote != attempt.RemoteHead {
		return blockedSupersession(state, transition, "blocked_supersede_remote_changed", "the configured remote branch changed at the final transition boundary; custody was not released")
	}
	branch, branchErr := git.CurrentBranch(ctx, s.workDir())
	clean, _ := worktreeClean(ctx, s.workDir())
	if branchErr != nil || branch != attempt.Branch || !clean || duplicateBranchCheckout(ctx, s.workDir(), branch) {
		return blockedSupersession(state, transition, "blocked_supersede_assumptions_changed", "the checked-out branch or worktree changed at the final transition boundary; custody was not released")
	}
	if _, err := git.Run(ctx, s.workDir(), "update-ref", branchRef, attempt.LaterPushedHead, attempt.LaterPushedHead); err != nil {
		return blockedSupersession(state, transition, "blocked_supersede_assumptions_changed", "the local branch changed at the final transition boundary; custody was not released")
	}
	if _, err := git.Run(ctx, s.GateDir, "update-ref", attempt.TargetRef, attempt.GateHead, attempt.GateHead); err != nil {
		return blockedSupersession(state, transition, "blocked_supersede_gate_race", "the gate branch changed at the final transition boundary; custody was not released")
	}

	applied, err := s.DB.CommitStaleCustodySupersession(old, evidence.later, evidence.lineage, evidence.repo, attempt)
	if err != nil {
		message := "the exact run lineage or authority generation changed before the transactional custody stamp; custody was not released"
		if !errors.Is(err, db.ErrRunCustodyChanged) {
			message = "the transactional stale-custody stamp failed; every head remains anchored and the operation is safe to retry"
		}
		return blockedSupersession(state, transition, "blocked_supersede_assumptions_changed", message)
	}
	transition.Idempotent = !applied
	final, _, _ := s.inspect(ctx)
	final.Released = true
	final.Changed = changed
	final.CustodyTransition = transition
	return final
}

func (s *Service) buildStaleSupersessionEvidence(ctx context.Context, state State, old *db.Run, requestedLaterID string, attempt *db.StaleCustodySupersession) (*staleSupersessionEvidence, State) {
	transition := &CustodyTransition{Action: "supersede_stale", Reason: db.CustodyReturnReasonStaleOwnerSuperseded}
	if old != nil {
		transition.RunID = old.ID
		transition.PreservedHead = old.HeadSHA
	}
	blocked := func(safety, message string) (*staleSupersessionEvidence, State) {
		return nil, blockedSupersession(state, transition, safety, message)
	}
	if old == nil || old.RepoID != s.Repo.ID || old.Branch != state.Local.Branch || !terminalRunStatus(old.Status) || !unpublishedPipelineHead(old) {
		return blocked("blocked_supersede_run_identity", "the selected run is not an inactive terminal unpublished owner of this exact repository and branch; no files or refs were changed")
	}
	if !state.Local.Clean && (attempt == nil || state.Local.Head != attempt.LaterPushedHead) {
		return blocked("blocked_supersede_dirty", fmt.Sprintf("the invoking worktree is not clean (%s); no files or refs were changed", state.Local.Reason))
	}
	if duplicateBranchCheckout(ctx, s.workDir(), state.Local.Branch) {
		return blocked("blocked_supersede_branch_ambiguous", "the checked-out branch is attached to more than one worktree; no files or refs were changed")
	}
	gateDir := strings.TrimSpace(s.GateDir)
	if gateDir == "" || git.ValidateBareRepository(ctx, gateDir) != nil {
		return blocked("blocked_supersede_gate_unavailable", "the registered local gate could not be verified as the exact bare repository; no files or refs were changed")
	}
	if !objectExists(ctx, s.workDir(), old.HeadSHA) || !objectExists(ctx, gateDir, old.HeadSHA) {
		return blocked("blocked_supersede_preserved_unavailable", "the old preserved head is not recoverable in both owned object stores; use only the separately guarded unavailable-head release when it is genuinely absent")
	}
	repoSnapshot, err := s.DB.GetRepo(s.Repo.ID)
	if err != nil || !sameRepoRegistration(s.Repo, repoSnapshot) {
		return blocked("blocked_supersede_run_identity", "the registered repository identity changed before stale custody could be planned; no files or refs were changed")
	}
	runs, err := s.DB.GetRunsByRepo(s.Repo.ID)
	if err != nil {
		return blocked("blocked_supersede_assumptions_changed", "the repository run lineage could not be read; no files or refs were changed")
	}
	for _, run := range runs {
		if run.Branch == old.Branch && (run.Status == types.RunPending || run.Status == types.RunRunning) {
			return blocked("blocked_supersede_run_active", "an active run still participates in this branch ownership; no files or refs were changed")
		}
	}

	var laterCandidates []*db.Run
	for _, candidate := range runs {
		if candidate.ID == old.ID || candidate.Branch != old.Branch || candidate.RepoID != old.RepoID ||
			!runNewerThan(candidate, old) || !terminalRunStatus(candidate.Status) || candidate.CustodyReturnedAt != nil ||
			normalizePRState(candidate.PRState) == "merged" || normalizePRState(candidate.PRState) == "closed" ||
			!exactPushedBinding(repoSnapshot, candidate, old.Branch) || candidate.SubmittedHeadSHA == nil {
			continue
		}
		if staleAdoptionStartsAt(old, candidate, state.Local.Head, attempt) {
			laterCandidates = append(laterCandidates, candidate)
		}
	}
	if len(laterCandidates) == 0 {
		if requestedLaterID != "" {
			requested, _ := s.DB.GetRun(requestedLaterID)
			if requested != nil && (!terminalRunStatus(requested.Status) || requested.Status == types.RunPending || requested.Status == types.RunRunning) {
				return blocked("blocked_supersede_run_active", "the requested later run is still active; no files or refs were changed")
			}
			if requested != nil && requested.ID == requestedLaterID && requested.RepoID == old.RepoID && requested.Branch == old.Branch && exactPushedBinding(repoSnapshot, requested, old.Branch) {
				return blocked("blocked_supersede_submission_mismatch", "the local branch is not the exact adoption source recorded by the requested later lineage; no files or refs were changed")
			}
		}
		return blocked("blocked_supersede_run_identity", "no unique later exact pushed run binds this local head to the stale owner; no files or refs were changed")
	}
	if len(laterCandidates) != 1 {
		return blocked("blocked_supersede_owner_ambiguous", "more than one later exact run could claim this adoption; no files or refs were changed")
	}
	later := laterCandidates[0]
	if requestedLaterID != "" && later.ID != requestedLaterID {
		return blocked("blocked_supersede_run_identity", "the requested later run is not the one unique exact lineage for this adoption; no files or refs were changed")
	}

	var lineageCandidates []*db.Run
	for _, candidate := range runs {
		if candidate.RepoID != old.RepoID || candidate.Branch != old.Branch || candidate.SubmittedHeadSHA == nil ||
			ptr(candidate.SubmittedHeadSHA) != old.HeadSHA || !runNewerThan(candidate, old) || runNewerThan(candidate, later) ||
			!terminalRunStatus(candidate.Status) || candidate.CustodyReturnedAt != nil || !exactPushedBinding(repoSnapshot, candidate, old.Branch) ||
			!samePushTargetBinding(candidate, later) {
			continue
		}
		pushed := ptr(candidate.LastPushedSHA)
		if candidate.ID == later.ID || pushed == ptr(later.SubmittedHeadSHA) || isAncestor(ctx, gateDir, pushed, ptr(later.SubmittedHeadSHA)) {
			lineageCandidates = append(lineageCandidates, candidate)
		}
	}
	if len(lineageCandidates) != 1 {
		return blocked("blocked_supersede_lineage_missing", "the old preserved head is not connected to the requested later submission by one unique exact pushed run lineage; no files or refs were changed")
	}
	lineage := lineageCandidates[0]
	if attempt != nil && (attempt.LaterRunID != later.ID || attempt.LineageRunID != lineage.ID) {
		return blocked("blocked_supersede_assumptions_changed", "the durable stale-custody journal names a different exact run lineage; no ownership state was changed")
	}

	targetRef := "refs/heads/" + old.Branch
	targetURL, err := s.verifiedConfiguredPushURL(ctx, repoSnapshot)
	if err != nil {
		return blocked("blocked_supersede_target_ambiguous", "the invoking worktree does not have exactly one configured remote matching this repository's push target; no files or refs were changed")
	}
	remoteCtx, cancelRemote := context.WithTimeout(ctx, s.networkBudget())
	remoteHead, remoteErr := git.LsRemote(remoteCtx, s.workDir(), targetURL, targetRef)
	cancelRemote()
	if remoteErr != nil {
		return blocked("blocked_supersede_remote_unavailable", "the configured push target could not be read; no files or refs were changed")
	}
	if remoteHead == "" || remoteHead != ptr(later.LastPushedSHA) {
		return blocked("blocked_supersede_remote_mismatch", "the configured remote branch does not equal the requested later run's exact pushed head; no files or refs were changed")
	}
	gateHead, gateErr := git.Run(ctx, gateDir, "rev-parse", targetRef+"^{commit}")
	if gateErr != nil || gateHead != ptr(later.LastPushedSHA) {
		return blocked("blocked_supersede_gate_mismatch", "the gate branch does not equal the requested later run's exact pushed head; no files or refs were changed")
	}
	authority, err := s.DB.SnapshotCustodyReleaseAuthority(old.RepoID, old.Branch)
	if err != nil {
		return blocked("blocked_supersede_assumptions_changed", "the repository and branch ownership generations could not be read; no files or refs were changed")
	}
	localHead := state.Local.Head
	if attempt != nil {
		localHead = attempt.LocalHead
	}
	state.Target = TargetState{Kind: targetKind(repoSnapshot), Remote: s.remoteName(ctx), URL: displayTarget(repoSnapshot.PushURL()), Ref: targetRef}
	state.Remote = RemoteState{ObservedHead: remoteHead, Freshness: "live", ObservedAt: time.Now().Unix()}
	return &staleSupersessionEvidence{
		state: state, old: old, later: later, lineage: lineage, repo: repoSnapshot, authority: authority,
		targetURL: targetURL, targetRef: targetRef, localHead: localHead, remoteHead: remoteHead, gateHead: gateHead,
	}, State{}
}

func staleAdoptionStartsAt(old, later *db.Run, local string, attempt *db.StaleCustodySupersession) bool {
	if old == nil || later == nil || later.SubmittedHeadSHA == nil {
		return false
	}
	if attempt != nil {
		return later.ID == attempt.LaterRunID && (local == attempt.LocalHead || local == attempt.LaterPushedHead)
	}
	if local == ptr(later.SubmittedHeadSHA) {
		return true
	}
	return old.SubmittedHeadSHA != nil && local == ptr(old.SubmittedHeadSHA) && old.HeadSHA == ptr(later.SubmittedHeadSHA)
}

func runNewerThan(candidate, older *db.Run) bool {
	return candidate != nil && older != nil && (candidate.CreatedAt > older.CreatedAt || (candidate.CreatedAt == older.CreatedAt && candidate.ID > older.ID))
}

func staleSupersessionTransition(e *staleSupersessionEvidence) *CustodyTransition {
	return &CustodyTransition{
		Action: "supersede_stale", Reason: db.CustodyReturnReasonStaleOwnerSuperseded,
		RunID: e.old.ID, SupersedingRunID: e.later.ID, LineageRunID: e.lineage.ID,
		PreservedHead: e.old.HeadSHA, SubmittedHead: ptr(e.later.SubmittedHeadSHA), PushedHead: ptr(e.later.LastPushedSHA),
		LocalHead: e.localHead, RemoteHead: e.remoteHead, GateHead: e.gateHead,
	}
}

func sameStaleSupersessionAttempt(attempt *db.StaleCustodySupersession, e *staleSupersessionEvidence) bool {
	return attempt != nil && e != nil && attempt.OldRunID == e.old.ID && attempt.LaterRunID == e.later.ID &&
		attempt.LineageRunID == e.lineage.ID && attempt.RepoID == e.old.RepoID && attempt.Branch == e.old.Branch &&
		attempt.OldHead == e.old.HeadSHA && attempt.LocalHead == e.localHead &&
		attempt.LaterSubmittedHead == ptr(e.later.SubmittedHeadSHA) && attempt.LaterPushedHead == ptr(e.later.LastPushedSHA) &&
		attempt.LineagePushedHead == ptr(e.lineage.LastPushedSHA) && attempt.RemoteHead == e.remoteHead && attempt.GateHead == e.gateHead &&
		attempt.TargetKind == targetKind(e.repo) && attempt.TargetFingerprint == TargetFingerprint(e.repo.PushURL()) &&
		attempt.TargetRef == e.targetRef
}

func (s *Service) recheckStaleSupersession(ctx context.Context, e *staleSupersessionEvidence, attempt *db.StaleCustodySupersession, expectedLocal string, requireClean bool) *State {
	branch, branchErr := git.CurrentBranch(ctx, s.workDir())
	head, headErr := git.HeadSHA(ctx, s.workDir())
	clean, _ := worktreeClean(ctx, s.workDir())
	if branchErr != nil || branch != attempt.Branch || headErr != nil || head != expectedLocal || (requireClean && !clean) || duplicateBranchCheckout(ctx, s.workDir(), branch) {
		blocked := blockedSupersession(e.state, staleSupersessionTransition(e), "blocked_supersede_assumptions_changed", "the local branch, HEAD, or worktree changed while stale custody was being prepared; safety anchors remain intact")
		return &blocked
	}
	freshRepo, repoErr := s.DB.GetRepo(e.old.RepoID)
	freshAuthority, authorityErr := s.DB.SnapshotCustodyReleaseAuthority(e.old.RepoID, e.old.Branch)
	freshOld, oldErr := s.DB.GetRun(e.old.ID)
	freshLater, laterErr := s.DB.GetRun(e.later.ID)
	freshLineage, lineageErr := s.DB.GetRun(e.lineage.ID)
	if repoErr != nil || !sameRepoRegistration(e.repo, freshRepo) || authorityErr != nil || freshAuthority != e.authority ||
		oldErr != nil || !sameUnavailableReleaseRun(e.old, freshOld) || laterErr != nil || !sameExactPushedRun(e.later, freshLater) ||
		lineageErr != nil || !sameExactPushedRun(e.lineage, freshLineage) {
		blocked := blockedSupersession(e.state, staleSupersessionTransition(e), "blocked_supersede_assumptions_changed", "the exact repository, run lineage, or branch ownership generation changed while stale custody was being prepared; safety anchors remain intact")
		return &blocked
	}
	_, selected, _ := s.inspect(ctx)
	if selected == nil || selected.ID != e.old.ID {
		blocked := blockedSupersession(e.state, staleSupersessionTransition(e), "blocked_supersede_assumptions_changed", "a different run became the authoritative branch owner while stale custody was being prepared; safety anchors remain intact")
		return &blocked
	}
	remoteCtx, cancelRemote := context.WithTimeout(ctx, s.networkBudget())
	liveRemote, remoteErr := git.LsRemote(remoteCtx, s.workDir(), e.targetURL, e.targetRef)
	cancelRemote()
	if remoteErr != nil || liveRemote != attempt.RemoteHead {
		blocked := blockedSupersession(e.state, staleSupersessionTransition(e), "blocked_supersede_remote_changed", "the configured remote branch changed while stale custody was being prepared; safety anchors remain intact")
		return &blocked
	}
	gateHead, gateErr := git.Run(ctx, s.GateDir, "rev-parse", e.targetRef+"^{commit}")
	if gateErr != nil || gateHead != attempt.GateHead {
		blocked := blockedSupersession(e.state, staleSupersessionTransition(e), "blocked_supersede_gate_race", "the gate branch changed while stale custody was being prepared; safety anchors remain intact")
		return &blocked
	}
	return nil
}

func sameExactPushedRun(expected, current *db.Run) bool {
	return expected != nil && current != nil && expected.ID == current.ID && expected.RepoID == current.RepoID && expected.Branch == current.Branch &&
		expected.HeadSHA == current.HeadSHA && sameStringPointer(expected.SubmittedHeadSHA, current.SubmittedHeadSHA) && expected.Status == current.Status &&
		sameStringPointer(expected.LastPushedSHA, current.LastPushedSHA) && sameStringPointer(expected.PushTargetKind, current.PushTargetKind) &&
		sameStringPointer(expected.PushTargetFingerprint, current.PushTargetFingerprint) && sameStringPointer(expected.PushRef, current.PushRef) &&
		sameInt64Pointer(expected.PushGeneration, current.PushGeneration) && expected.PushActive == current.PushActive &&
		sameInt64Pointer(expected.TerminalHeadVerifiedAt, current.TerminalHeadVerifiedAt) &&
		sameStringPointer(expected.PRState, current.PRState) && current.CustodyReturnedAt == nil
}

func (s *Service) completedStaleSupersession(ctx context.Context, state State, old, later *db.Run, attempt *db.StaleCustodySupersession) State {
	transition := &CustodyTransition{Action: "supersede_stale", Reason: db.CustodyReturnReasonStaleOwnerSuperseded, RunID: old.ID, SupersedingRunID: later.ID, Idempotent: true}
	blocked := func(safety, message string) State { return blockedSupersession(state, transition, safety, message) }
	if old.CustodyReturnReason == nil || *old.CustodyReturnReason != db.CustodyReturnReasonStaleOwnerSuperseded || attempt == nil ||
		attempt.OldRunID != old.ID || attempt.LaterRunID != later.ID || attempt.Phase != db.StaleCustodySupersessionLocalMoved ||
		old.RepoID != attempt.RepoID || old.Branch != attempt.Branch || old.HeadSHA != attempt.OldHead || !terminalRunStatus(old.Status) {
		return blocked("blocked_supersede_not_applicable", "custody was already returned through a different supported path or lineage; no files or refs were changed")
	}
	branch, branchErr := git.CurrentBranch(ctx, s.workDir())
	head, headErr := git.HeadSHA(ctx, s.workDir())
	clean, _ := worktreeClean(ctx, s.workDir())
	if branchErr != nil || branch != attempt.Branch || headErr != nil || head != attempt.LaterPushedHead || !clean || duplicateBranchCheckout(ctx, s.workDir(), branch) {
		return blocked("blocked_supersede_assumptions_changed", "the completed transition no longer matches this exact clean checked-out branch; no files or refs were changed")
	}
	repoSnapshot, repoErr := s.DB.GetRepo(attempt.RepoID)
	lineage, lineageErr := s.DB.GetRun(attempt.LineageRunID)
	if repoErr != nil || !sameRepoRegistration(s.Repo, repoSnapshot) || lineageErr != nil ||
		!runMatchesCompletedSupersession(later, attempt, false) || !runMatchesCompletedSupersession(lineage, attempt, true) ||
		attempt.TargetKind != targetKind(repoSnapshot) || attempt.TargetFingerprint != TargetFingerprint(repoSnapshot.PushURL()) ||
		attempt.TargetRef != "refs/heads/"+attempt.Branch {
		return blocked("blocked_supersede_run_identity", "the completed transition no longer matches this repository and exact run lineage; no files or refs were changed")
	}
	targetURL, targetErr := s.verifiedConfiguredPushURL(ctx, repoSnapshot)
	if targetErr != nil {
		return blocked("blocked_supersede_target_ambiguous", "the configured target identity is ambiguous; no files or refs were changed")
	}
	remoteCtx, cancelRemote := context.WithTimeout(ctx, s.networkBudget())
	remoteHead, remoteErr := git.LsRemote(remoteCtx, s.workDir(), targetURL, attempt.TargetRef)
	cancelRemote()
	gateHead, gateErr := git.Run(ctx, s.GateDir, "rev-parse", attempt.TargetRef+"^{commit}")
	if remoteErr != nil || remoteHead != attempt.RemoteHead || gateErr != nil || gateHead != attempt.GateHead {
		return blocked("blocked_supersede_assumptions_changed", "the completed transition's exact gate or remote identity changed; no files or refs were changed")
	}
	transition.LineageRunID = attempt.LineageRunID
	transition.PreservedHead = attempt.OldHead
	transition.SubmittedHead = attempt.LaterSubmittedHead
	transition.PushedHead = attempt.LaterPushedHead
	transition.LocalHead = attempt.LocalHead
	transition.RemoteHead = attempt.RemoteHead
	transition.GateHead = attempt.GateHead
	populateStaleSupersessionAudit(ctx, s.workDir(), s.GateDir, staleSupersessionRef(old.ID), attempt, transition)
	state.Target = TargetState{Kind: attempt.TargetKind, Remote: s.remoteName(ctx), URL: displayTarget(repoSnapshot.PushURL()), Ref: attempt.TargetRef}
	state.Remote = RemoteState{ObservedHead: remoteHead, Freshness: "live", ObservedAt: time.Now().Unix()}
	state.Released = true
	state.Changed = false
	state.CustodyTransition = transition
	return state
}

func runMatchesCompletedSupersession(run *db.Run, attempt *db.StaleCustodySupersession, lineage bool) bool {
	if run == nil || attempt == nil || run.RepoID != attempt.RepoID || run.Branch != attempt.Branch ||
		!terminalRunStatus(run.Status) || run.CustodyReturnedAt != nil || run.PushActive ||
		normalizePRState(run.PRState) == "merged" || normalizePRState(run.PRState) == "closed" ||
		run.LastPushedSHA == nil || run.HeadSHA != ptr(run.LastPushedSHA) ||
		ptr(run.PushTargetKind) != attempt.TargetKind || ptr(run.PushTargetFingerprint) != attempt.TargetFingerprint ||
		ptr(run.PushRef) != attempt.TargetRef || run.PushGeneration == nil {
		return false
	}
	if lineage {
		return run.ID == attempt.LineageRunID && ptr(run.SubmittedHeadSHA) == attempt.OldHead && run.HeadSHA == attempt.LineagePushedHead
	}
	return run.ID == attempt.LaterRunID && ptr(run.SubmittedHeadSHA) == attempt.LaterSubmittedHead && run.HeadSHA == attempt.LaterPushedHead
}

func populateStaleSupersessionAudit(ctx context.Context, workDir, gateDir, base string, attempt *db.StaleCustodySupersession, transition *CustodyTransition) {
	if attempt == nil || transition == nil {
		return
	}
	checks := []struct {
		dir, kind, head string
		dest            *string
	}{
		{workDir, "preserved-local", attempt.OldHead, &transition.PreservedLocalAnchor},
		{gateDir, "preserved-gate", attempt.OldHead, &transition.PreservedGateAnchor},
		{workDir, "local", attempt.LocalHead, &transition.LocalAnchor},
		{workDir, "remote", attempt.RemoteHead, &transition.RemoteAnchor},
		{gateDir, "gate", attempt.GateHead, &transition.GateAnchor},
	}
	for _, check := range checks {
		ref := staleSupersessionAnchorRef(base, check.kind, check.head)
		if anchored, ok := directAnchorHead(ctx, check.dir, ref); ok && anchored == check.head {
			*check.dest = ref
		}
	}
}

func staleSupersessionRef(oldRunID string) string {
	return "refs/no-mistakes/custody-supersede/" + oldRunID
}

func staleSupersessionAnchorRef(base, kind, head string) string {
	return base + "/" + kind + "/" + head
}

func blockedSupersession(state State, transition *CustodyTransition, safety, message string) State {
	blocked := blockedPlan(state, state.State, safety, message)
	blocked.CustodyTransition = transition
	return blocked
}

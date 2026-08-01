package branchsync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/gatecontext"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/safeurl"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const refreshTimeout = 15 * time.Second

const (
	StatePipelineOwned        = "pipeline_owned"
	StatePushInProgress       = "push_in_progress"
	StateBehind               = "behind"
	StateSynchronized         = "synchronized"
	StateLocalAhead           = "local_ahead"
	StateDiverged             = "diverged"
	StateDirty                = "dirty"
	StateRemoteAdvanced       = "remote_advanced"
	StateRemoteRewritten      = "remote_rewritten"
	StateRemoteMissing        = "remote_missing"
	StateMergedRemoteRetained = "merged_remote_retained"
	StateMergedRemoteRemoved  = "merged_remote_removed"
	StateClosed               = "closed"
	StateOffline              = "offline"
	StateTargetChanged        = "target_changed"
	StateAmbiguousContext     = "ambiguous_context"
	StateLegacyUnbound        = "legacy_unbound"
	StateCustodyReturned      = "custody_returned"
	// StateUserOwned reports a branch released by its terminal outcome: the
	// run ended before the pipeline changed the submitted head, so no
	// pipeline-created content exists to recover and the exact branch and head
	// are the operator's, immediately usable with no sync action.
	StateUserOwned = "user_owned"
)

const (
	RelationEqual    = "equal"
	RelationBehind   = "behind"
	RelationAhead    = "ahead"
	RelationDiverged = "diverged"
	RelationUnknown  = "unknown"
)

const (
	SafetySafeFastForward       = "safe_fast_forward"
	SafetySafeEquivalentAdvance = "safe_equivalent_advance"
)

// State is the shared branch synchronization contract rendered by CLI, AXI,
// and TUI presenters. Cached inspection never contacts a remote.
type State struct {
	State    string
	Changed  bool
	Local    LocalState
	Pipeline PipelineState
	Target   TargetState
	Remote   RemoteState
	Relation string
	Safety   string
	PRState  string
	// Recovered is set only by Recover and reports that the operator owns the
	// branch when the call returns: custody of a stranded terminal run was
	// returned (by this call or an earlier, idempotent one), or the terminal
	// outcome had already released the branch (user_owned), making recovery an
	// idempotent no-op.
	Recovered  bool
	NextAction *NextAction
	Error      string
}

type LocalState struct {
	Branch string
	Head   string
	Clean  bool
	Reason string
}

type PipelineState struct {
	RunID          string
	Status         string
	Phase          string
	SubmittedHead  string
	CurrentHead    string
	PushedHead     string
	PushedAt       int64
	PushGeneration int64
}

type TargetState struct {
	Kind   string
	Remote string
	URL    string
	Ref    string
}

type RemoteState struct {
	ObservedHead string
	Freshness    string
	ObservedAt   int64
}

type NextAction struct {
	Code    string
	Command string
}

// CanApply reports whether Apply may advance the clean checked-out branch for
// a freshly verified plan. It includes strict fast-forwards and the narrower
// equivalent-diverged advance that first anchors the pre-sync head.
func CanApply(state State) bool {
	return state.Safety == SafetySafeFastForward || state.Safety == SafetySafeEquivalentAdvance
}

// Service synchronizes only the invoking worktree. Repo is the registered
// repository record, while WorkDir may be its main or a linked worktree.
// GateDir is the repo's local bare gate; selection may read its exact branch
// head and ancestry as provenance evidence, while Recover is the only method
// that mutates it.
type Service struct {
	DB                       *db.DB
	Repo                     *db.Repo
	WorkDir                  string
	GateDir                  string
	Paths                    *paths.Paths
	GateConfigCurrent        func() bool
	InternalMutationConsumed func(string) error

	beforeApply              func()
	beforeGateReset          func()
	beforeCustodyLock        func()
	afterCustodyLockAttempt  func(error)
	beforeRecoverAnchorFetch func()
	beforeRecoverStamp       func()
	beforeRecoverFastForward func()
	beforeGateRefReconcile   func()
}

// OpenCurrent opens a service for the invoking registered worktree. The caller
// owns the returned close function.
func OpenCurrent() (*Service, func(), error) {
	p, err := paths.New()
	if err != nil {
		return nil, nil, err
	}
	database, err := db.Open(p.DB())
	if err != nil {
		return nil, nil, err
	}
	root, err := git.FindGitRoot(".")
	if err != nil {
		database.Close()
		return nil, nil, fmt.Errorf("not in a git repository")
	}
	repo, err := database.GetRepoByPath(root)
	if err != nil {
		database.Close()
		return nil, nil, err
	}
	if repo == nil {
		mainRoot, mainErr := git.FindMainRepoRoot(root)
		if mainErr == nil {
			repo, err = database.GetRepoByPath(mainRoot)
		}
	}
	if err != nil || repo == nil {
		database.Close()
		return nil, nil, fmt.Errorf("repo not initialized")
	}
	return &Service{DB: database, Repo: repo, WorkDir: root, GateDir: p.RepoDir(repo.ID), Paths: p}, func() { _ = database.Close() }, nil
}

// TargetFingerprint returns a stable one-way identity for a credential-free,
// canonical target. No URL is persisted by callers.
func TargetFingerprint(raw string) string {
	sum := sha256.Sum256([]byte(canonicalTarget(raw)))
	return hex.EncodeToString(sum[:])
}

func canonicalTarget(raw string) string {
	raw = strings.TrimSpace(raw)
	parsed, err := url.Parse(raw)
	if err == nil && parsed.Scheme != "" {
		if parsed.Scheme == "http" || parsed.Scheme == "https" {
			parsed.User = nil
			parsed.Scheme = strings.ToLower(parsed.Scheme)
			parsed.Host = strings.ToLower(parsed.Host)
		}
		parsed.Fragment = ""
		return strings.TrimSuffix(parsed.String(), "/")
	}
	return strings.TrimSuffix(raw, "/")
}

func displayTarget(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") {
		parsed.User = nil
		return parsed.String()
	}
	return safeurl.Redact(raw)
}

// InspectCached reads local Git, persisted provenance, and read-only gate
// ancestry evidence without fetching or mutating refs, the index, or the
// worktree.
func (s *Service) InspectCached(ctx context.Context) State {
	state, _, _ := s.inspect(ctx)
	return state
}

// Refresh explicitly verifies the exact configured push ref into a private
// no-mistakes ref. It never updates an ordinary remote-tracking ref.
func (s *Service) Refresh(ctx context.Context) State {
	state, run, ok := s.inspect(ctx)
	if !ok || !refreshable(state) {
		return state
	}
	freshRun, runErr := s.DB.GetRun(run.ID)
	freshRepo, repoErr := s.DB.GetRepo(s.Repo.ID)
	if runErr != nil || repoErr != nil || freshRun == nil || freshRepo == nil || freshRun.PushActive ||
		value(freshRun.PushGeneration) != state.Pipeline.PushGeneration || ptr(freshRun.LastPushedSHA) != state.Pipeline.PushedHead ||
		ptr(freshRun.PushTargetFingerprint) != TargetFingerprint(freshRepo.PushURL()) || ptr(freshRun.PushTargetKind) != targetKind(freshRepo) || ptr(freshRun.PushRef) != state.Target.Ref {
		if state.PRState == "merged" || state.PRState == "closed" {
			return state
		}
		return blockedPlan(state, StateTargetChanged, "blocked_binding_changed", "the push binding or configured target changed before refresh; no files or refs were changed")
	}
	pushURL := freshRepo.PushURL()

	refreshCtx, cancel := context.WithTimeout(ctx, refreshTimeout)
	defer cancel()
	live, err := git.LsRemote(refreshCtx, s.workDir(), pushURL, state.Target.Ref)
	if err != nil {
		state.State = StateOffline
		state.Safety = "blocked_offline"
		state.Error = "could not refresh the configured push target; no files or refs were changed"
		state.NextAction = &NextAction{Code: "retry", Command: "no-mistakes sync --check"}
		return state
	}
	state.Remote.Freshness = "live"
	state.Remote.ObservedAt = time.Now().Unix()
	state.Remote.ObservedHead = live

	if live == "" {
		state.Relation = RelationUnknown
		state.NextAction = nil
		if state.PRState == "merged" {
			state.State = StateMergedRemoteRemoved
			state.Safety = "already_retired"
			state.Error = ""
			return state
		}
		if state.PRState == "closed" {
			state.State = StateClosed
			state.Safety = "blocked_closed"
			return state
		}
		state.State = StateRemoteMissing
		state.Safety = "blocked_remote_missing"
		state.Error = "the pipeline-bound remote branch no longer exists; no files or refs were changed"
		return state
	}

	privateRef := "refs/no-mistakes/sync/" + run.ID
	branch := strings.TrimPrefix(state.Target.Ref, "refs/heads/")
	if err := git.FetchRemoteBranchToPrivateRef(refreshCtx, s.workDir(), pushURL, branch, privateRef); err != nil {
		state.State = StateOffline
		state.Safety = "blocked_offline"
		state.Error = "could not fetch the configured push target; no files or worktree refs were changed"
		return state
	}
	fetched, err := git.Run(ctx, s.workDir(), "rev-parse", privateRef)
	if err != nil || fetched != live {
		state.State = StateRemoteRewritten
		state.Safety = "blocked_remote_changed_during_refresh"
		state.Error = "the remote branch changed while it was being refreshed; no files or worktree refs were changed"
		return state
	}

	bound := ptr(run.LastPushedSHA)
	if live != bound {
		state.NextAction = nil
		if isAncestor(ctx, s.workDir(), bound, live) {
			state.State = StateRemoteAdvanced
			state.Safety = "blocked_remote_advanced"
			state.Relation = RelationUnknown
			state.Error = "the live remote contains commits outside the persisted pipeline push binding; no files or refs were changed"
		} else {
			state.State = StateRemoteRewritten
			state.Safety = "blocked_remote_rewritten"
			state.Relation = RelationUnknown
			state.Error = "the live remote no longer equals the persisted pipeline push binding; no files or refs were changed"
		}
		return state
	}

	if state.PRState == "merged" {
		state.State = StateMergedRemoteRetained
		state.Safety = "blocked_merged"
		state.NextAction = nil
		return state
	}
	if state.PRState == "closed" {
		state.State = StateClosed
		state.Safety = "blocked_closed"
		state.NextAction = nil
		return state
	}

	s.classifyRelation(ctx, &state, bound, run.BaseSHA, true)
	return state
}

func (s *Service) gateContextRefusal(ctx context.Context) (State, bool) {
	p := s.Paths
	if p == nil && strings.TrimSpace(s.GateDir) != "" {
		p = paths.WithRoot(filepath.Dir(filepath.Dir(filepath.Clean(s.GateDir))))
	}
	if p == nil {
		// Manually constructed services without a gate path are used by pure
		// branch-sync callers and tests. Production entrypoints always provide
		// Paths/GateDir and are classified before mutation.
		return State{}, false
	}
	result, err := (gatecontext.Inspector{DB: s.DB, Paths: p}).Inspect(ctx, gatecontext.Request{CWD: s.workDir(), MarkerPresent: gatecontext.MarkerPresent()})
	if err != nil {
		return State{State: StateAmbiguousContext, Safety: "blocked_gate_context_unknown", Error: "could not verify gate execution context; no files or refs were changed"}, true
	}
	if !result.Nested {
		return State{}, false
	}
	return State{State: StateAmbiguousContext, Safety: gatecontext.ErrorCode, Error: gatecontext.RefusalMessage(result)}, true
}

// Apply repeats remote and mutable-precondition checks, then advances the clean
// checked-out branch to the exact pipeline-bound SHA. Ordinary behind branches
// use a strict fast-forward. Equivalent-diverged branches first anchor the
// pre-sync head, then move to the verified equivalent pipeline head.
func (s *Service) Apply(ctx context.Context) State {
	if refusal, blocked := s.gateContextRefusal(ctx); blocked {
		return refusal
	}
	plan := s.Refresh(ctx)
	if plan.State == StateSynchronized || plan.State == StateMergedRemoteRemoved {
		plan.Changed = false
		return plan
	}
	if !CanApply(plan) {
		return plan
	}
	if s.beforeApply != nil {
		s.beforeApply()
	}

	freshRun, err := s.DB.GetRun(plan.Pipeline.RunID)
	freshRepo, repoErr := s.DB.GetRepo(s.Repo.ID)
	if err != nil || repoErr != nil || freshRepo == nil || freshRun == nil || freshRun.PushActive || ptr(freshRun.LastPushedSHA) != plan.Pipeline.PushedHead ||
		value(freshRun.PushGeneration) != plan.Pipeline.PushGeneration || ptr(freshRun.PushRef) != plan.Target.Ref ||
		ptr(freshRun.PushTargetFingerprint) != TargetFingerprint(freshRepo.PushURL()) || ptr(freshRun.PushTargetKind) != targetKind(freshRepo) {
		return blockedPlan(plan, "pipeline_owned", "blocked_generation_changed", "the pipeline push binding changed before synchronization; no files or refs were changed")
	}

	recheck, _, ok := s.inspect(ctx)
	if !ok || recheck.Local.Head != plan.Local.Head || !recheck.Local.Clean || recheck.Local.Branch != plan.Local.Branch {
		return blockedPlan(recheck, StateAmbiguousContext, "blocked_assumptions_changed", "the local branch or worktree changed before synchronization; no files or refs were changed")
	}
	if recheck.State == StatePushInProgress || recheck.State == StatePipelineOwned || recheck.State == StateDirty {
		return recheck
	}

	checkCtx, cancel := context.WithTimeout(ctx, refreshTimeout)
	defer cancel()
	live, err := git.LsRemote(checkCtx, s.workDir(), s.Repo.PushURL(), plan.Target.Ref)
	if err != nil || live != plan.Pipeline.PushedHead {
		return blockedPlan(plan, StateRemoteRewritten, "blocked_remote_changed_before_apply", "the live remote changed before synchronization; no files or refs were changed")
	}
	finalPrecondition, finalRun, finalOK := s.inspect(ctx)
	finalRepo, finalRepoErr := s.DB.GetRepo(s.Repo.ID)
	if !finalOK || finalRun == nil || finalRepoErr != nil || finalRepo == nil || finalRun.PushActive ||
		value(finalRun.PushGeneration) != plan.Pipeline.PushGeneration || ptr(finalRun.PushTargetFingerprint) != TargetFingerprint(finalRepo.PushURL()) || ptr(finalRun.PushTargetKind) != targetKind(finalRepo) ||
		finalPrecondition.Local.Branch != plan.Local.Branch || finalPrecondition.Local.Head != plan.Local.Head || !finalPrecondition.Local.Clean {
		return blockedPlan(finalPrecondition, StateAmbiguousContext, "blocked_assumptions_changed", "the push binding, branch, HEAD, or worktree changed immediately before synchronization; no files or refs were changed")
	}
	equivalentAdvance := plan.Safety == SafetySafeEquivalentAdvance
	if equivalentAdvance {
		if !equivalentDivergence(ctx, s.workDir(), plan.Local.Head, plan.Pipeline.PushedHead, finalRun.BaseSHA) {
			return blockedPlan(plan, StateDiverged, "blocked_diverged", "the equivalent-diverged proof changed before synchronization; no files or refs were changed")
		}
	} else if !isAncestor(ctx, s.workDir(), plan.Local.Head, plan.Pipeline.PushedHead) || plan.Local.Head == plan.Pipeline.PushedHead {
		return blockedPlan(plan, StateAmbiguousContext, "blocked_assumptions_changed", "the strict fast-forward assumptions changed before synchronization; no files or refs were changed")
	}

	var applyErr error
	if equivalentAdvance {
		anchorRef := syncAnchorRef(plan.Pipeline.RunID)
		if _, err := git.Run(ctx, s.workDir(), "update-ref", anchorRef, plan.Local.Head); err != nil {
			return blockedPlan(plan, StateAmbiguousContext, "blocked_preserve_failed", "the pre-sync local head could not be anchored; no files or refs were changed")
		}
		if anchored, err := git.Run(ctx, s.workDir(), "rev-parse", anchorRef+"^{commit}"); err != nil || anchored != plan.Local.Head {
			return blockedPlan(plan, StateAmbiguousContext, "blocked_preserve_failed", "the pre-sync local head could not be verified after anchoring; no files or worktree refs were changed")
		}
		_, applyErr = git.Run(ctx, s.workDir(), "reset", "--hard", plan.Pipeline.PushedHead)
	} else {
		_, applyErr = git.Run(ctx, s.workDir(), "merge", "--ff-only", "--no-edit", plan.Pipeline.PushedHead)
	}
	finalHead, _ := git.HeadSHA(ctx, s.workDir())
	finalClean, finalReason := worktreeClean(ctx, s.workDir())
	plan.Local.Head = finalHead
	plan.Local.Clean = finalClean
	plan.Local.Reason = finalReason
	plan.Changed = finalHead == plan.Pipeline.PushedHead && finalHead != recheck.Local.Head
	if applyErr != nil || finalHead != plan.Pipeline.PushedHead {
		plan.State = StateAmbiguousContext
		plan.Safety = "blocked_apply_failed"
		plan.Error = fmt.Sprintf("synchronization failed; final HEAD is %s and no destructive recovery was attempted", finalHead)
		return plan
	}
	if !finalClean {
		plan.State = StateDirty
		plan.Relation = RelationEqual
		plan.Safety = "blocked_post_apply_" + finalReason
		plan.Error = "HEAD reached the exact pipeline-pushed commit, but a Git hook left the worktree non-clean; no recovery was attempted"
		return plan
	}
	plan.State = StateSynchronized
	plan.Relation = RelationEqual
	plan.Safety = "already_synchronized"
	plan.NextAction = nil
	plan.Error = ""
	return plan
}

// Recover returns custody of a branch stranded by a TERMINAL run whose MOVED
// pipeline head was never published: cancelled or failed before the push with
// pipeline commits in the gate, or terminal after a push with additional
// unpublished commits. While such a run was active the pipeline_owned block
// was correct; once it is terminal nothing will ever publish the head, so an
// explicit guarded exit must exist. A terminal run whose verified worktree
// head never changed from the submitted head needs no recovery at all, so
// Recover treats that user_owned state as an idempotent no-op success.
//
// The decision matrix, by worktree relation to the preserved pipeline head P
// recorded in runs.head_sha:
//
//	relation   worktree  default                        --keep-local
//	equal      any       anchor locally; return custody same
//	ahead      any       anchor locally; return custody same
//	behind     clean     strict fast-forward to P,      custody at local head;
//	                     then return custody            gate reset to it (CAS)
//	behind     dirty     refuse (commit/stash first)    custody at local head;
//	                                                    gate reset to it (CAS)
//	diverged   any       refuse (anchor named, manual   custody at local head;
//	                     reconcile / rerun offered)     gate reset to it (CAS)
//	P missing  any       refuse                         refuse
//
// Fail-safe rules, in the same spirit as Refresh/Apply:
//   - An active run always refuses: only terminal runs are recoverable.
//   - The preserved commits must be provably safe before custody moves: when
//     already reachable from the local branch (equal/ahead), recovery pins the
//     private anchor ref refs/no-mistakes/recover/<runID> locally without gate
//     access; otherwise the preserved head is verified at the gate branch head
//     and fetched into that anchor. A terminal run whose gate branch moved back
//     to the submitted/local head may also anchor P by exact object ID from the
//     configured local gate before either refusing actionably or honoring
//     --keep-local, but only for the exact newest unpublished run with local and
//     gate both at its submitted head. The anchor keeps P reachable locally no
//     matter what later happens to the gate.
//   - The only possible worktree mutation stays a strict fast-forward of a
//     clean checked-out branch. When the operator explicitly keeps a behind or
//     diverged local head instead of taking P, --keep-local never touches the
//     worktree and moves the gate branch to the kept head with an atomic
//     compare-and-swap, so a concurrent gate push wins and recovery refuses. A
//     private run-owned marker makes a crash after that CAS retryable.
//   - Anything unverifiable (missing gate where required, unrelated moved gate
//     branch, publication provenance, newer owner, conflicting anchor, failed
//     anchor write or fetch, changed assumptions) refuses without a custody
//     stamp. A verified immutable anchor or prepared private marker may remain.
//
// Recovery ends with a persisted custody-return stamp on an eligible
// never-published run. Publication-bearing runs remain blocked until their
// publication owner reconciles them. `no-mistakes rerun` starts from the
// ordinary gate branch, which may differ from the recovery anchor.
func (s *Service) Recover(ctx context.Context, keepLocal bool) State {
	if refusal, blocked := s.gateContextRefusal(ctx); blocked {
		return refusal
	}
	state, run, _ := s.inspect(ctx)
	if pending, err := s.DB.GetPendingReceiveReservationsForBranch(s.Repo.ID, state.Local.Branch); err != nil {
		return blockedPlan(state, state.State, "blocked_recover_receive_reservation", "a managed gate receive is still being reconciled; custody was not returned and no files or refs were changed")
	} else if len(pending) > 0 {
		return blockedPlan(state, state.State, "blocked_recover_receive_reservation", "a managed gate receive is still being reconciled; custody was not returned and no files or refs were changed")
	}
	if run != nil && publicationAttemptPresent(run) {
		if err := s.DB.ReconcileRunPublicationAttempt(run.ID); err != nil {
			return blockedPlan(state, StatePipelineOwned, "blocked_recover_publication", "a publication attempt has no exact durable push binding; custody was not returned and no files or refs were changed")
		}
		state, run, _ = s.inspect(ctx)
	}
	if run != nil && run.CustodyReturnedAt != nil {
		lock, err := s.acquireRecoveryLock(run)
		if err != nil {
			return custodyLockFailure(state, err)
		}
		defer lock.Release()
		if err := s.reclaimStampedGateRefLock(lock, run); err != nil {
			state.Recovered = false
			state.Changed = false
			state.Safety = "blocked_recover_gate_race"
			state.Error = fmt.Sprintf("the stamped custody recovery left an ambiguous ordinary gate lock (%v); custody remains returned but the lock was not reclaimed; re-run recovery after the lock owner is gone", err)
			state.NextAction = nil
			return state
		}
		s.cleanupRecoverMarkers(ctx, lock, run)
		if token := run.CustodyTransitionToken; token != nil {
			_ = s.DB.ClearRunCustodyTransition(ctx, run.ID, *token)
		}
		state.Recovered = true
		state.Changed = false
		return state
	}
	// A branch released by its terminal outcome is already the operator's:
	// nothing pipeline-created exists to recover, so recovery is an idempotent
	// no-op that mutates no file, ref, or database row.
	if state.State == StateUserOwned {
		state.Recovered = true
		state.Changed = false
		return state
	}
	if state.State != StatePipelineOwned || run == nil {
		return blockedPlan(state, state.State, "blocked_recover_not_applicable", "nothing to recover: the branch is not held by a terminal run with unpublished pipeline commits; no files or refs were changed")
	}
	if !terminalRunStatus(run.Status) {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_run_active", "the run that owns this branch is still active; drive it to completion or abort it first; no files or refs were changed")
	}
	quarantine, quarantineErr := s.DB.GetGateRefQuarantine(s.Repo.ID, s.GateDir, "refs/heads/"+run.Branch)
	if quarantineErr != nil {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_gate_race", "the managed gate quarantine journal could not be read; custody was not returned and no files or refs were changed")
	}
	if quarantine != nil {
		currentQuarantinedHead, currentQuarantinedErr := git.Run(ctx, s.GateDir, "rev-parse", "refs/heads/"+run.Branch+"^{commit}")
		if currentQuarantinedErr != nil || db.NormalizeManagedGateHead(currentQuarantinedHead) != db.NormalizeManagedGateHead(quarantine.ExpectedHead) {
			return blockedPlan(state, StatePipelineOwned, "blocked_recover_gate_quarantined", fmt.Sprintf("the ordinary gate ref is quarantined after an unbound transition from %s to %s; reconcile it before retrying recovery", quarantine.ExpectedHead, quarantine.ObservedHead))
		}
		if err := s.DB.ClearGateRefQuarantine(s.Repo.ID, s.GateDir, "refs/heads/"+run.Branch); err != nil {
			return blockedPlan(state, StatePipelineOwned, "blocked_recover_gate_race", "the reconciled gate quarantine could not be cleared; custody was not returned and no files or refs were changed")
		}
	}
	if run.TerminalHeadVerifiedAt == nil {
		branch := state.Local.Branch
		if strings.TrimSpace(s.GateDir) == "" {
			return blockedPlan(state, StatePipelineOwned, "blocked_recover_unverified_head", "the terminal run has no verified head and no gate is available to prove preserved custody; no files or refs were changed")
		}
		gateHead, err := git.Run(ctx, s.GateDir, "rev-parse", "refs/heads/"+branch+"^{commit}")
		if err != nil {
			return blockedPlan(state, StatePipelineOwned, "blocked_recover_unverified_head", "the terminal run has no verified head and the preserved gate head could not be read; no files or refs were changed")
		}
		if gateHead != run.HeadSHA {
			if !isAncestor(ctx, s.GateDir, run.HeadSHA, gateHead) {
				return blockedPlan(state, StatePipelineOwned, "blocked_recover_unverified_head", "the terminal run has no verified head and the gate head does not descend from the recorded head; no files or refs were changed")
			}
			if err := s.DB.UpdateRunHeadSHA(run.ID, gateHead); err != nil {
				return blockedPlan(state, StatePipelineOwned, "blocked_recover_unverified_head", "the verified gate head could not be preserved; no files or refs were changed")
			}
			run.HeadSHA = gateHead
			state.Pipeline.CurrentHead = gateHead
			state.Relation = relationBetween(ctx, s.workDir(), state.Local.Head, gateHead)
		}
	}

	wd := s.workDir()
	branch := state.Local.Branch
	local := state.Local.Head
	preserved := run.HeadSHA
	anchorRef := recoverAnchorRef(run.ID)
	if !isExactFullObjectID(preserved) {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_invalid_head", "the recorded pipeline head is not one canonical full 40- or 64-hex object ID; no files or refs were changed")
	}
	if latest, ok := s.latestRunForBranch(run.Branch); !ok || latest.ID != run.ID {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_newer_run", "a newer run owns this repository branch; custody was not returned and no files or refs were changed")
	}
	lock, lockErr := s.acquireRecoveryLock(run)
	if lockErr != nil {
		return custodyLockFailure(state, lockErr)
	}
	defer lock.Release()
	if strings.TrimSpace(s.GateDir) == "" || !s.gateConfigCurrent() {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_gate_unavailable", "the managed gate fencing configuration is missing or tampered; custody was not returned and no files or refs were changed")
	}
	if pending, err := s.DB.GetPendingReceiveReservationsForBranch(s.Repo.ID, run.Branch); err != nil {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_receive_reservation", "a managed gate receive is still being reconciled; custody was not returned and no files or refs were changed")
	} else if len(pending) > 0 {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_receive_reservation", "a managed gate receive is still being reconciled; custody was not returned and no files or refs were changed")
	}
	currentRepo, repoErr := s.DB.GetRepo(run.RepoID)
	if repoErr != nil || currentRepo == nil {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_publication", "the registered repository target could not be reloaded under the custody lock; custody was not returned and no files or refs were changed")
	}
	if run.LastPushedSHA != nil && run.HeadSHA != ptr(run.LastPushedSHA) {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_publication", "the mutable run head differs from its exact published head; the publication owner must reconcile the run before custody can be returned; no files or refs were changed")
	}
	if runHasPublication(run) {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_publication", "the run has publication provenance; the publication owner must reconcile it before custody can be returned; no files or refs were changed")
	}
	if safety := s.verifyLegacyRunUnpublished(ctx, run, branch, currentRepo); safety != "" {
		return blockedPlan(state, StatePipelineOwned, safety, "the run has no exact authoritative unpublished-publication journal for its original target; custody was not returned and no files or refs were changed")
	}
	anchored, anchorConflict := recoveryAnchorState(ctx, wd, anchorRef, preserved)
	if anchorConflict {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_anchor_conflict", fmt.Sprintf("the recovery anchor %s already names a different object; custody was not returned and no refs were changed", anchorRef))
	}

	if objectExists(ctx, wd, preserved) && (local == preserved || isAncestor(ctx, wd, preserved, local)) {
		if !anchored {
			if blocked, ok := s.anchorReachablePreserved(ctx, state, anchorRef, preserved); !ok {
				return blocked
			}
		}
		if latest, ok := s.latestRunForBranch(run.Branch); !ok || latest.ID != run.ID {
			return blockedPlan(state, StatePipelineOwned, "blocked_recover_newer_run", "a newer run owns this repository branch; custody was not returned; the exact recovery anchor remains available for inspection")
		}
		return s.finishRecover(ctx, run, false, lock)
	}

	gateDir := strings.TrimSpace(s.GateDir)
	if gateDir == "" {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_gate_unavailable", "no local gate is configured for this repository, so the preserved pipeline head cannot be verified; no files or refs were changed")
	}
	gateHead, err := git.Run(ctx, gateDir, "rev-parse", "refs/heads/"+branch+"^{commit}")
	if err != nil {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_gate_unavailable", fmt.Sprintf("the local gate no longer has branch %s, so the preserved pipeline head %s cannot be verified; no files or refs were changed", branch, preserved))
	}
	custodyRef := custodyReturnRef(run.ID)
	custodyHead, _ := git.Run(ctx, gateDir, "rev-parse", custodyRef+"^{commit}")
	// A keep-local recovery that reset the gate but crashed before stamping
	// custody resumes here: the gate either equals the current kept local head,
	// or equals the durable run-owned custody marker from the prior CAS.
	resumedKeepLocal := keepLocal && anchored && (gateHead == local || (custodyHead != "" && gateHead == custodyHead))
	movedGateAtSubmitted := run.SubmittedHeadSHA != nil && gateHead == local && local == *run.SubmittedHeadSHA
	if gateHead != preserved && !resumedKeepLocal && !movedGateAtSubmitted {
		if quarantineErr := s.DB.QuarantineGateRef(s.Repo.ID, s.GateDir, "refs/heads/"+branch, preserved, gateHead, "unbound-or-unexpected-gate-ref"); quarantineErr != nil {
			return blockedPlan(state, StatePipelineOwned, "blocked_recover_gate_race", fmt.Sprintf("the gate branch is at %s instead of %s and the quarantine journal could not be persisted; custody was not returned and no files or refs were changed", gateHead, preserved))
		}
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_gate_diverged", fmt.Sprintf("the gate branch is at %s, not the preserved pipeline head %s recorded for this run; no files or refs were changed", gateHead, preserved))
	}
	if !resumedKeepLocal {
		managedGateRef, managedGateErr := s.DB.GetManagedGateRef(s.Repo.ID, s.GateDir, "refs/heads/"+branch)
		if managedGateErr != nil {
			return blockedPlan(state, StatePipelineOwned, "blocked_recover_gate_race", "the managed gate head journal could not be read; custody was not returned and no files or refs were changed")
		}
		if managedGateRef == nil {
			if !s.verifyLegacyGateBaseline(ctx, state, run, branch, currentRepo, gateHead) {
				if err := s.DB.QuarantineGateRef(s.Repo.ID, s.GateDir, "refs/heads/"+branch, ptr(run.SubmittedHeadSHA), gateHead, "unbound-or-unexpected-gate-ref"); err != nil {
					return blockedPlan(state, StatePipelineOwned, "blocked_recover_gate_race", "the unjournaled gate mismatch could not be quarantined; custody was not returned and no files or refs were changed")
				}
				return blockedPlan(state, StatePipelineOwned, "blocked_recover_gate_quarantined", "the ordinary gate ref has no authoritative managed-head journal and does not match the submitted head; reconcile it before retrying recovery")
			}
			if err := s.DB.SetManagedGateRefHead(s.Repo.ID, s.GateDir, "refs/heads/"+branch, ptr(run.SubmittedHeadSHA)); err != nil {
				return blockedPlan(state, StatePipelineOwned, "blocked_recover_gate_race", "the managed gate head journal could not be initialized; custody was not returned and no files or refs were changed")
			}
		} else if db.NormalizeManagedGateHead(managedGateRef.Head) != db.NormalizeManagedGateHead(gateHead) {
			if err := s.DB.QuarantineGateRef(s.Repo.ID, s.GateDir, "refs/heads/"+branch, managedGateRef.Head, gateHead, "unbound-or-unexpected-gate-ref"); err != nil {
				return blockedPlan(state, StatePipelineOwned, "blocked_recover_gate_race", "the managed gate head mismatch could not be quarantined; custody was not returned and no files or refs were changed")
			}
			return blockedPlan(state, StatePipelineOwned, "blocked_recover_gate_quarantined", fmt.Sprintf("the ordinary gate ref changed from journaled head %s to %s; reconcile the quarantined ref before retrying recovery", managedGateRef.Head, gateHead))
		}
	}
	if movedGateAtSubmitted && runHasPublication(run) {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_publication", "exact-object moved-gate recovery is limited to a terminal run with no push, PR, or CI publication provenance; custody was not returned and no refs were changed")
	}
	if !anchored {
		if safety := s.fetchExactRecoveryAnchor(ctx, lock, run, preserved, anchorRef); safety != "" {
			message := fmt.Sprintf("the preserved pipeline head %s could not be immutably anchored from the local gate by exact object ID; no ordinary refs were changed", preserved)
			if safety == "blocked_recover_anchor_conflict" {
				message = fmt.Sprintf("the recovery anchor %s changed or names a different object; custody was not returned and the conflicting anchor was not overwritten", anchorRef)
			}
			return blockedPlan(state, StatePipelineOwned, safety, message)
		}
		anchored = true
	}
	if gateHead != preserved && movedGateAtSubmitted {
		if keepLocal {
			return s.recoverKeepLocal(ctx, run, state, gateHead, lock)
		}
		state.Relation = RelationDiverged
		blocked := blockedPlan(state, StatePipelineOwned, "blocked_recover_diverged", fmt.Sprintf("the gate branch has moved to the submitted local head while the preserved pipeline head is anchored at %s; re-run with `no-mistakes axi sync --recover --keep-local` to return custody at the current head, or inspect with `git log --oneline --left-right HEAD...%s`; `no-mistakes rerun` is available, but rerun starts from the current ordinary gate branch, not the recovery anchor; no files or refs were changed except the recovery anchor", anchorRef, anchorRef))
		blocked.NextAction = &NextAction{Code: "inspect_and_reconcile_manually", Command: "git log --oneline --left-right HEAD..." + anchorRef}
		return blocked
	}

	switch {
	case local == preserved, isAncestor(ctx, wd, preserved, local):
		// Equal or ahead, discovered only after anchoring made the preserved
		// head comparable locally.
		return s.finishRecover(ctx, run, false, lock)
	case isAncestor(ctx, wd, local, preserved):
		if keepLocal {
			return s.recoverKeepLocal(ctx, run, state, gateHead, lock)
		}
		if !state.Local.Clean {
			state.Relation = RelationBehind
			blocked := blockedPlan(state, StatePipelineOwned, "blocked_recover_dirty", fmt.Sprintf("the invoking worktree is not clean (%s); commit or stash first and re-run the recovery, or use --keep-local to return custody at the current head without moving the worktree; no files or refs were changed", state.Local.Reason))
			blocked.NextAction = &NextAction{Code: "inspect_worktree", Command: "git status"}
			return blocked
		}
		return s.recoverFastForward(ctx, run, state, preserved, lock)
	default:
		if keepLocal {
			return s.recoverKeepLocal(ctx, run, state, gateHead, lock)
		}
		state.Relation = RelationDiverged
		blocked := blockedPlan(state, StatePipelineOwned, "blocked_recover_diverged", fmt.Sprintf("the local branch and the preserved pipeline head have diverged; the preserved commits are anchored at %s - reconcile manually and re-run the recovery; `no-mistakes rerun` is available, but rerun starts from the current ordinary gate branch, not the recovery anchor; use --keep-local to keep the current head; no files or refs were changed", anchorRef))
		blocked.NextAction = &NextAction{Code: "inspect_and_reconcile_manually", Command: "git log --oneline --left-right HEAD..." + anchorRef}
		return blocked
	}
}

// recoverKeepLocal performs the explicit keep-local custody return: the
// worktree is never touched; the gate branch is verified at the kept local head
// or moved to it with an atomic compare-and-swap when needed, so a concurrent
// gate push refuses instead of being clobbered. The kept head's objects reach
// the gate through a gate-side fetch - never a push, which would fire the
// gate's receive hooks and start a pipeline run. The preserved head stays
// reachable through the anchor ref.
func (s *Service) recoverKeepLocal(ctx context.Context, run *db.Run, state State, gateHead string, lock *custodyLock) State {
	if s.beforeGateReset != nil {
		s.beforeGateReset()
	}
	precheck, currentGateHead, ok := s.recheckRecoverKeepLocal(ctx, state, gateHead)
	if !ok {
		return precheck
	}
	gateHead = currentGateHead
	originalGateHead, originalExists := custodyOriginalHead(ctx, s.GateDir, run.ID)
	if originalExists && originalGateHead == state.Local.Head {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_gate_race", "the custody transition marker conflicts with the kept local head; re-run the recovery after reconciling the gate; no local files or ordinary refs were changed")
	}
	if originalExists && gateHead != state.Local.Head && originalGateHead != gateHead {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_gate_race", "the custody transition marker no longer matches the gate branch; re-run the recovery after reconciling the gate; no local files or ordinary refs were changed")
	}
	var owner *db.CustodyTransition
	if gateHead != state.Local.Head || originalExists {
		var err error
		owner, err = s.beginOrResumeCustodyTransition(ctx, run)
		if err != nil {
			return blockedPlan(state, StatePipelineOwned, "blocked_recover_custody_race", "the custody transition is owned by a changed or concurrent run; no files or ordinary refs were changed")
		}
	}
	phase := ""
	if owner != nil {
		var err error
		phase, err = owner.Phase(ctx)
		if err != nil {
			return releaseCustodyOwner(blockedPlan(state, StatePipelineOwned, "blocked_recover_custody_race", "the durable custody transition phase could not be read; no ordinary refs were changed"), owner)
		}
		if phase == db.CustodyPhaseRestoring {
			return s.reconcileCustodyRestore(ctx, state, run, owner, originalGateHead, lock)
		}
		if phase != db.CustodyPhasePreparing && phase != db.CustodyPhaseStaged && phase != db.CustodyPhaseGateMoved {
			return releaseCustodyOwner(blockedPlan(state, StatePipelineOwned, "blocked_recover_custody_race", "the durable custody transition is in an unknown phase; no custody was stamped"), owner)
		}
		if phase == db.CustodyPhaseGateMoved && !originalExists {
			return blockedPlan(state, StatePipelineOwned, "blocked_recover_gate_race", "the durable custody transition has no verified original gate head; no custody was stamped")
		}
	}
	if owner != nil && phase != db.CustodyPhaseGateMoved {
		if !originalExists {
			var err error
			originalGateHead, err = s.prepareCustodyOriginal(ctx, lock, state.Local.Branch, run.ID, gateHead)
			if err != nil {
				return releaseCustodyOwner(blockedPlan(state, StatePipelineOwned, "blocked_recover_gate_race", "the gate transition could not be durably prepared; no local files or ordinary refs were changed"), owner)
			}
			originalExists = true
		}
		if phase == db.CustodyPhasePreparing {
			source, err := filepath.Abs(s.workDir())
			if err != nil {
				return releaseCustodyOwner(blockedPlan(state, StatePipelineOwned, "blocked_recover_assumptions_changed", "the invoking worktree path could not be resolved; no files or ordinary refs were changed"), owner)
			}
			stagingRef := custodyReturnRef(run.ID)
			if err := s.fetchGatePrivateRef(ctx, lock, state.Local.Branch, source, stagingRef, "", state.Local.Head); err != nil {
				return releaseCustodyOwner(blockedPlan(state, StatePipelineOwned, "blocked_recover_assumptions_changed", "the kept local head could not be staged into the gate; no ordinary refs were changed"), owner)
			}
			staged, err := git.Run(ctx, s.GateDir, "rev-parse", stagingRef+"^{commit}")
			if err != nil || staged != state.Local.Head {
				return releaseCustodyOwner(blockedPlan(state, StatePipelineOwned, "blocked_recover_assumptions_changed", "the local branch head changed while custody was being returned; no ordinary refs were changed"), owner)
			}
			if err := owner.Advance(ctx, db.CustodyPhasePreparing, db.CustodyPhaseStaged); err != nil {
				return releaseCustodyOwner(blockedPlan(state, StatePipelineOwned, "blocked_recover_custody_race", "the durable custody transition changed while the kept head was being staged; no ordinary refs were changed"), owner)
			}
			phase = db.CustodyPhaseStaged
		}
		staged, err := git.Run(ctx, s.GateDir, "rev-parse", custodyReturnRef(run.ID)+"^{commit}")
		if err != nil || staged != state.Local.Head {
			return releaseCustodyOwner(blockedPlan(state, StatePipelineOwned, "blocked_recover_assumptions_changed", "the staged local head could not be verified; no ordinary refs were changed"), owner)
		}
		currentGateHead, err := git.Run(ctx, s.GateDir, "rev-parse", "refs/heads/"+state.Local.Branch+"^{commit}")
		if err != nil {
			return releaseCustodyOwner(blockedPlan(state, StatePipelineOwned, "blocked_recover_gate_race", "the ordinary gate ref disappeared while custody was being returned; no ordinary refs were changed"), owner)
		}
		if currentGateHead == originalGateHead {
			if casErr := s.updateGateRef(ctx, lock, state.Local.Branch, "refs/heads/"+state.Local.Branch, originalGateHead, state.Local.Head); casErr != nil {
				return releaseCustodyOwner(blockedPlan(state, StatePipelineOwned, "blocked_recover_gate_race", "the gate branch could not be authorized by the live branch-lock authority; custody was not stamped; re-run recovery"), owner)
			}
		} else if currentGateHead != state.Local.Head {
			return releaseCustodyOwner(blockedPlan(state, StatePipelineOwned, "blocked_recover_gate_race", "the ordinary gate ref changed while custody was being returned; re-run after reconciling the gate; no custody was stamped"), owner)
		}
		if err := owner.Advance(ctx, db.CustodyPhaseStaged, db.CustodyPhaseGateMoved); err != nil {
			phase, phaseErr := owner.Phase(ctx)
			if phaseErr != nil || phase != db.CustodyPhaseGateMoved {
				return blockedPlan(state, StatePipelineOwned, "blocked_recover_custody_race", "the gate was moved but its durable custody phase was claimed by another transition; no custody was stamped")
			}
		}
	}
	var transition *custodyGateTransition
	if originalExists {
		transition = &custodyGateTransition{branch: state.Local.Branch, original: originalGateHead, current: state.Local.Head, owner: owner, lock: lock}
	}
	if s.beforeRecoverStamp != nil {
		s.beforeRecoverStamp()
	}
	precheck, _, ok = s.recheckRecoverKeepLocal(ctx, state, state.Local.Head)
	if !ok {
		return s.recoverKeepLocalFailure(ctx, precheck, run, transition, lock)
	}
	expectedGateHead := run.HeadSHA
	if transition != nil {
		expectedGateHead = transition.current
	} else {
		expectedGateHead = state.Local.Head
	}
	return s.finishRecoverAtGateHead(ctx, run, false, transition, lock, expectedGateHead)
}

func (s *Service) acquireRecoveryLock(run *db.Run) (*custodyLock, error) {
	if s.beforeCustodyLock != nil {
		s.beforeCustodyLock()
	}
	lock, err := acquireCustodyLock(s, run)
	if s.afterCustodyLockAttempt != nil {
		s.afterCustodyLockAttempt(err)
	}
	return lock, err
}

func (s *Service) internalGateContext(ctx context.Context, lock *custodyLock, branch, ref, oldSHA, newSHA, operation string) (context.Context, string, error) {
	if lock == nil || s == nil || s.DB == nil || s.Repo == nil {
		return nil, "", fmt.Errorf("managed gate mutation requires an active branch lock")
	}
	scope := db.InternalRefMutationScopePrivate
	if strings.HasPrefix(ref, "refs/heads/") {
		scope = db.InternalRefMutationScopeOrdinary
	}
	capability, endpoint, err := IssueInternalRefMutation(s.DB, lock, db.InternalRefMutationSpec{
		RepoID: s.Repo.ID, GatePath: s.GateDir, Branch: branch, Ref: ref,
		OldSHA: oldSHA, NewSHA: newSHA, Operation: operation, Scope: scope,
	})
	if err != nil {
		return nil, "", err
	}
	mutationCtx := git.WithInternalMutationCapability(ctx, capability)
	mutationCtx = git.WithInternalMutationOperation(mutationCtx, operation)
	mutationCtx = git.WithInternalMutationBranch(mutationCtx, branch)
	mutationCtx = git.WithInternalMutationAuthority(mutationCtx, endpoint)
	return mutationCtx, capability, nil
}

func internalZeroObjectID(sha string) string {
	if len(strings.TrimSpace(sha)) == 64 {
		return strings.Repeat("0", 64)
	}
	return strings.Repeat("0", 40)
}

func (s *Service) updateGateRef(ctx context.Context, lock *custodyLock, branch, ref, oldSHA, newSHA string) error {
	if !git.LooksLikeBareRepository(s.GateDir) || !s.gateConfigCurrent() {
		return fmt.Errorf("managed gate fencing configuration is missing or tampered")
	}
	oldSHA = strings.TrimSpace(oldSHA)
	newSHA = strings.TrimSpace(newSHA)
	if oldSHA == "" {
		oldSHA = internalZeroObjectID(newSHA)
	}
	if newSHA == "" {
		newSHA = internalZeroObjectID(oldSHA)
	}
	if strings.HasPrefix(ref, "refs/heads/") {
		return s.updateOrdinaryGateRef(ctx, lock, branch, ref, oldSHA, newSHA)
	}
	mutationCtx, capability, err := s.internalGateContext(ctx, lock, branch, ref, oldSHA, newSHA, "update-ref")
	if err != nil {
		return err
	}
	mutationCtx = git.WithSanitizedGateConfig(mutationCtx)
	hookPath, err := filepath.Abs(filepath.Join(s.GateDir, "hooks"))
	if err != nil {
		return fmt.Errorf("resolve managed gate hooks path: %w", err)
	}
	args := []string{"update-ref"}
	if git.IsZeroSHA(newSHA) || (len(newSHA) == 64 && newSHA == strings.Repeat("0", 64)) {
		args = append(args, "-d", ref)
	} else {
		args = append(args, ref, newSHA)
	}
	if oldSHA != "" {
		args = append(args, oldSHA)
	}
	args = append([]string{"-c", "core.hooksPath=" + hookPath, "-c", "extensions.worktreeConfig=true"}, args...)
	_, err = git.Run(mutationCtx, s.GateDir, args...)
	if err != nil {
		return err
	}
	return s.verifyInternalMutationConsumed(capability)
}

func (s *Service) updateOrdinaryGateRef(ctx context.Context, lock *custodyLock, branch, ref, oldSHA, newSHA string) error {
	if lock == nil || lock.runID == "" {
		return fmt.Errorf("managed ordinary ref mutation requires a run-owned branch lock")
	}
	authority, err := lock.ensureInternalMutationAuthority(s.DB)
	if err != nil {
		return err
	}
	endpoint, generation, err := lock.authorityIdentity()
	if err != nil {
		return err
	}
	if err := HandoffManagedGateRefAuthority(s.GateDir, ref); err != nil {
		return err
	}
	if completed, err := s.reconcileCompletedOrdinaryGateRefMutation(lock.runID, branch, ref, oldSHA, newSHA, endpoint); err != nil {
		return err
	} else if completed {
		return nil
	}
	gateLock, err := acquireGateRefLock(s.GateDir, ref, authority)
	if err != nil {
		return err
	}
	defer func() {
		if gateLock != nil && !gateLock.released {
			gateLock.closeKeepJournal()
		}
	}()
	mutationCtx, capability, err := s.internalGateContext(ctx, lock, branch, ref, oldSHA, newSHA, "update-ref")
	if err != nil {
		return err
	}
	gateLock.owner = gateRefLockOwner{RunID: lock.runID, RepoID: s.Repo.ID, GatePath: s.GateDir, Branch: branch, Ref: ref, OwnerGeneration: generation, AuthorityEndpoint: endpoint, ExpectedHead: oldSHA}
	if err := gateLock.setOwner(gateLock.owner); err != nil {
		return err
	}
	gateLock.database = s.DB
	if err := s.DB.PrepareGateRefLock(db.GateRefLockJournal{RunID: lock.runID, RepoID: s.Repo.ID, GatePath: s.GateDir, Branch: branch, Ref: ref, LockPath: gateLock.path, OwnerGeneration: generation, AuthorityEndpoint: endpoint, ExpectedHead: oldSHA, NewHead: newSHA, FileIdentity: gateLock.identity}); err != nil {
		return err
	}
	request := InternalRefMutationAuthorization{Capability: capability, Phase: "prepared", GatePath: s.GateDir, Branch: branch, Ref: ref, OldSHA: oldSHA, NewSHA: newSHA, Operation: "update-ref", Scope: db.InternalRefMutationScopeOrdinary}
	if s.InternalMutationConsumed != nil {
		if err := s.InternalMutationConsumed(capability); err != nil {
			return err
		}
	} else if err := AuthorizeInternalRefMutation(authority, request); err != nil {
		return err
	}
	if err := gateLock.commitRef(mutationCtx, s.GateDir, ref, oldSHA, newSHA); err != nil {
		return err
	}
	if s.InternalMutationConsumed == nil {
		request.Phase = "committed"
		if err := AuthorizeInternalRefMutation(authority, request); err != nil {
			return err
		}
	}
	if err := s.DB.SetManagedGateRefHead(s.Repo.ID, s.GateDir, ref, newSHA); err != nil {
		return err
	}
	if err := gateLock.Release(); err != nil {
		return err
	}
	gateLock = nil
	return nil
}

func (s *Service) reconcileCompletedOrdinaryGateRefMutation(runID, branch, ref, oldSHA, newSHA, authorityEndpoint string) (bool, error) {
	journal, err := s.DB.GetGateRefLock(runID)
	if err != nil || journal == nil {
		return false, err
	}
	if journal.RepoID != s.Repo.ID || journal.GatePath != s.GateDir || journal.Branch != branch || journal.Ref != ref || journal.ExpectedHead != oldSHA || journal.NewHead != newSHA || journal.AuthorityEndpoint == "" {
		return false, nil
	}
	if journal.AuthorityEndpoint != authorityEndpoint {
		return false, nil
	}
	if err := HandoffManagedGateRefAuthority(s.GateDir, ref); err != nil {
		return false, err
	}
	owner := gateRefLockOwner{RunID: journal.RunID, RepoID: journal.RepoID, GatePath: journal.GatePath, Branch: journal.Branch, Ref: journal.Ref, OwnerGeneration: journal.OwnerGeneration, AuthorityEndpoint: journal.AuthorityEndpoint, ExpectedHead: journal.ExpectedHead}
	var gateLock *gateRefLock
	if _, statErr := os.Stat(journal.LockPath); os.IsNotExist(statErr) {
		gateLock, err = acquireOwnedGateRefLock(s.GateDir, ref, owner)
		if err != nil {
			return false, err
		}
		if err := s.DB.UpdateGateRefLockIdentity(journal.RunID, journal.OwnerGeneration, gateLock.identity); err != nil {
			gateLock.closeKeepJournal()
			return false, err
		}
	} else if statErr != nil {
		return false, statErr
	} else {
		onDisk, ownerErr := readOwnedGateRefLock(journal.LockPath)
		if ownerErr != nil {
			return false, ownerErr
		}
		if onDisk != owner {
			return false, nil
		}
		identity, identityErr := gateRefFileIdentity(journal.LockPath)
		if identityErr != nil || identity != journal.FileIdentity {
			return false, fmt.Errorf("reconciliation gate ref lock identity changed")
		}
		file, openErr := os.OpenFile(journal.LockPath, os.O_RDWR, 0o644)
		if openErr != nil {
			return false, openErr
		}
		gateLock = &gateRefLock{file: file, path: journal.LockPath, owner: owner, identity: identity}
	}
	defer gateLock.closeKeepJournal()
	if err := acquireGateRefOSLock(gateLock.file); err != nil {
		return false, err
	}
	gateLock.osLocked = true
	if s.beforeGateRefReconcile != nil {
		s.beforeGateRefReconcile()
	}
	current, err := readManagedGateRef(s.GateDir, ref)
	if err != nil {
		return false, err
	}
	if db.NormalizeManagedGateHead(current) != db.NormalizeManagedGateHead(newSHA) {
		return false, nil
	}
	consumed, err := s.DB.ConsumedInternalRefMutationExists(db.InternalRefMutationSpec{RepoID: s.Repo.ID, GatePath: s.GateDir, Branch: branch, Ref: ref, OldSHA: oldSHA, NewSHA: newSHA, Operation: "update-ref", Scope: db.InternalRefMutationScopeOrdinary}, journal.AuthorityEndpoint)
	if err != nil {
		return false, err
	}
	if !consumed {
		return false, nil
	}
	if err := s.DB.SetManagedGateRefHead(s.Repo.ID, s.GateDir, ref, newSHA); err != nil {
		return false, err
	}
	final, finalErr := readManagedGateRef(s.GateDir, ref)
	if finalErr != nil || db.NormalizeManagedGateHead(final) != db.NormalizeManagedGateHead(newSHA) {
		_ = s.DB.PrepareGateRefLock(*journal)
		if finalErr != nil {
			return false, finalErr
		}
		return false, fmt.Errorf("managed gate ref changed during reconciliation")
	}
	if err := s.DB.ClearGateRefLock(journal.RunID, journal.OwnerGeneration); err != nil {
		return false, err
	}
	gateLock.database = nil
	if err := gateLock.Release(); err != nil {
		_ = s.DB.PrepareGateRefLock(*journal)
		return false, err
	}
	gateLock = nil
	return true, nil
}

func (s *Service) fetchGatePrivateRef(ctx context.Context, lock *custodyLock, branch, source, destination, oldSHA, newSHA string) error {
	if !git.LooksLikeBareRepository(s.GateDir) || !s.gateConfigCurrent() {
		return fmt.Errorf("managed gate fencing configuration is missing or tampered")
	}
	oldSHA = strings.TrimSpace(oldSHA)
	newSHA = strings.TrimSpace(newSHA)
	mutationCtx := git.WithSanitizedGateConfig(ctx)
	stageRunID, err := custodyStageRunID(destination)
	if err != nil {
		return err
	}
	run, err := s.DB.GetRun(stageRunID)
	if err != nil || run == nil || run.RepoID != s.Repo.ID || run.Branch != branch || run.CustodyReturnedAt != nil || run.CustodyTransitionToken == nil || run.CustodyTransitionPhase == nil || *run.CustodyTransitionPhase != db.CustodyPhasePreparing {
		return fmt.Errorf("managed private staging requires the active custody transition journal")
	}
	currentBranch, branchErr := git.CurrentBranch(mutationCtx, source)
	currentHead, headErr := git.HeadSHA(mutationCtx, source)
	if branchErr != nil || headErr != nil || currentBranch != branch || currentHead != newSHA {
		return fmt.Errorf("managed private staging provenance no longer matches the active local head")
	}
	if _, err := git.Run(mutationCtx, s.GateDir, "show-ref", "--verify", "--hash", custodyOriginalRef(stageRunID)); err != nil {
		return fmt.Errorf("managed private staging provenance has no custody-original marker")
	}
	stage, err := s.DB.GetCustodyRefStage(stageRunID)
	if err != nil {
		return err
	}
	if stage != nil && (stage.RepoID != s.Repo.ID || stage.GatePath != s.GateDir || stage.Branch != branch || stage.Ref != destination || stage.NewSHA != newSHA || stage.OldSHA == "" || stage.OwnerGeneration == "" || stage.AuthorityEndpoint == "") {
		return fmt.Errorf("managed private staging journal does not match the exact transition")
	}
	if stage != nil && stage.State != db.CustodyRefStagePrepared && stage.State != db.CustodyRefStageStaged {
		return fmt.Errorf("managed private staging journal has an unknown state")
	}
	existingSHA, directErr := readDirectLooseGateRef(s.GateDir, destination)
	existing := directErr == nil
	if directErr != nil && !errors.Is(directErr, errGateRefAbsent) {
		return directErr
	}
	if existing {
		if stage == nil || existingSHA != newSHA {
			return fmt.Errorf("managed private ref %s is an unjournaled or conflicting ref", destination)
		}
		consumed, consumedErr := s.DB.ConsumedInternalRefMutationExists(db.InternalRefMutationSpec{RepoID: s.Repo.ID, GatePath: s.GateDir, Branch: branch, Ref: destination, OldSHA: stage.OldSHA, NewSHA: stage.NewSHA, Operation: "fetch-private-ref", Scope: db.InternalRefMutationScopePrivate}, stage.AuthorityEndpoint)
		if consumedErr != nil {
			return consumedErr
		}
		if !consumed {
			return fmt.Errorf("managed private ref %s lacks a consumed writer capability", destination)
		}
		if stage.State == db.CustodyRefStagePrepared {
			if err := s.DB.MarkCustodyRefStageStaged(stageRunID, stage.OwnerGeneration); err != nil {
				return err
			}
		}
		return nil
	}
	if stage != nil && stage.State == db.CustodyRefStageStaged {
		return fmt.Errorf("managed private staging journal says the destination was staged, but the exact ref is absent")
	}
	if oldSHA == "" {
		oldSHA, _ = git.Run(mutationCtx, s.GateDir, "rev-parse", destination+"^{commit}")
		if oldSHA == "" {
			oldSHA = internalZeroObjectID(newSHA)
		}
	}
	if stage != nil && stage.OldSHA != oldSHA {
		return fmt.Errorf("managed private staging old head changed")
	}
	if _, err := lock.ensureInternalMutationAuthority(s.DB); err != nil {
		return err
	}
	endpoint, generation, err := lock.authorityIdentity()
	if err != nil {
		return err
	}
	if err := s.DB.PrepareCustodyRefStage(db.CustodyRefStage{RunID: stageRunID, RepoID: s.Repo.ID, GatePath: s.GateDir, Branch: branch, Ref: destination, OldSHA: oldSHA, NewSHA: newSHA, OwnerGeneration: generation, AuthorityEndpoint: endpoint}); err != nil {
		return err
	}
	mutationCtx, capability, err := s.internalGateContext(ctx, lock, branch, destination, oldSHA, newSHA, "fetch-private-ref")
	if err != nil {
		return err
	}
	mutationCtx = git.WithSanitizedGateConfig(mutationCtx)
	hookPath, err := filepath.Abs(filepath.Join(s.GateDir, "hooks"))
	if err != nil {
		return fmt.Errorf("resolve managed gate hooks path: %w", err)
	}
	_, err = git.Run(mutationCtx, s.GateDir, "-c", "core.hooksPath="+hookPath, "-c", "extensions.worktreeConfig=true", "fetch", "--no-tags", "--no-write-fetch-head", source, "+refs/heads/"+branch+":"+destination)
	if err != nil {
		return err
	}
	if err := s.verifyInternalMutationConsumed(capability); err != nil {
		return err
	}
	return s.DB.MarkCustodyRefStageStaged(stageRunID, generation)
}

func custodyStageRunID(ref string) (string, error) {
	const prefix = "refs/no-mistakes/custody-return/"
	if !strings.HasPrefix(ref, prefix) {
		return "", fmt.Errorf("managed private staging requires a run-scoped custody-return ref")
	}
	runID := strings.TrimPrefix(ref, prefix)
	if runID == "" || strings.Contains(runID, "/") {
		return "", fmt.Errorf("managed private staging ref has an invalid run identity")
	}
	return runID, nil
}

func (s *Service) verifyInternalMutationConsumed(capability string) error {
	if s.InternalMutationConsumed != nil {
		return s.InternalMutationConsumed(capability)
	}
	mutation, err := s.DB.GetInternalRefMutation(capability)
	if err != nil {
		return err
	}
	if mutation.State != db.InternalRefMutationStateConsumed {
		return fmt.Errorf("managed gate mutation was not authorized by its live branch-lock authority")
	}
	return nil
}

func (s *Service) gateConfigCurrent() bool {
	if s.GateConfigCurrent != nil {
		return s.GateConfigCurrent()
	}
	return git.GateConfigCurrent(s.GateDir)
}

func custodyLockFailure(state State, err error) State {
	if errors.Is(err, ErrCustodyLockHeld) {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_custody_race", "another custody recovery is active for this repository branch; no files or ordinary refs were changed")
	}
	return blockedPlan(state, StatePipelineOwned, "blocked_recover_custody_lock", fmt.Sprintf("the custody recovery lock could not be established (%v); no files or ordinary refs were changed", err))
}

func (s *Service) beginOrResumeCustodyTransition(ctx context.Context, run *db.Run) (*db.CustodyTransition, error) {
	owner, err := s.DB.BeginRunCustodyTransition(ctx, run)
	if err == nil {
		return owner, nil
	}
	current, getErr := s.DB.GetRun(run.ID)
	if getErr != nil || current == nil || current.CustodyTransitionToken == nil || current.CustodyTransitionPhase == nil {
		return nil, err
	}
	return s.DB.ResumeRunCustodyTransition(ctx, current)
}

func (s *Service) reconcileCustodyRestore(ctx context.Context, state State, run *db.Run, owner *db.CustodyTransition, original string, lock *custodyLock) State {
	if original == "" {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_gate_race", "the durable custody rollback has no verified original gate head; no custody was stamped")
	}
	current, err := git.Run(ctx, s.GateDir, "rev-parse", "refs/heads/"+state.Local.Branch+"^{commit}")
	if err != nil {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_gate_race", "the ordinary gate ref disappeared during custody rollback; re-run after reconciling the gate; no custody was stamped")
	}
	transition := &custodyGateTransition{branch: state.Local.Branch, original: original, current: state.Local.Head, owner: owner, lock: lock}
	if current == state.Local.Head {
		if err := s.restoreCustodyGate(ctx, transition); err != nil {
			return blockedPlan(state, StatePipelineOwned, "blocked_recover_gate_race", "the ordinary gate ref changed during custody rollback; no custody was stamped")
		}
		current = original
	}
	if current != original {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_gate_race", "the ordinary gate ref does not match the durable custody rollback; no custody was stamped")
	}
	if err := owner.FinishRestore(ctx); err != nil {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_custody_race", "the durable custody rollback could not be completed; no custody was stamped")
	}
	s.cleanupRecoverMarkers(ctx, lock, run)
	return blockedPlan(state, StatePipelineOwned, "blocked_recover_custody_race", "a previous custody transition was rolled back; re-run recovery from freshly inspected state; no custody was stamped")
}

func (s *Service) recheckRecoverKeepLocal(ctx context.Context, state State, expectedGateHead string) (State, string, bool) {
	currentGateHead, err := git.Run(ctx, s.GateDir, "rev-parse", "refs/heads/"+state.Local.Branch+"^{commit}")
	if err != nil || currentGateHead != expectedGateHead {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_gate_race", "the gate branch changed while custody was being returned; re-run the recovery; no local files or refs were changed"), "", false
	}
	branch, branchErr := git.CurrentBranch(ctx, s.workDir())
	head, headErr := git.HeadSHA(ctx, s.workDir())
	if branchErr != nil || branch != state.Local.Branch || headErr != nil || head != state.Local.Head {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_assumptions_changed", "the local branch head changed while custody was being returned; no files or refs were changed"), "", false
	}
	return State{}, currentGateHead, true
}

// recoverFastForward advances the clean checked-out branch to the preserved
// pipeline head with the same strict fast-forward and honesty rules as Apply.
func (s *Service) recoverFastForward(ctx context.Context, run *db.Run, state State, preserved string, lock *custodyLock) State {
	if s.beforeRecoverFastForward != nil {
		s.beforeRecoverFastForward()
	}
	branch, branchErr := git.CurrentBranch(ctx, s.workDir())
	head, headErr := git.HeadSHA(ctx, s.workDir())
	clean, _ := worktreeClean(ctx, s.workDir())
	if branchErr != nil || branch != state.Local.Branch || headErr != nil || head != state.Local.Head || !clean {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_assumptions_changed", "the local branch or worktree changed while custody was being returned; no files or refs were changed")
	}
	_, mergeErr := git.Run(ctx, s.workDir(), "merge", "--ff-only", "--no-edit", preserved)
	finalHead, _ := git.HeadSHA(ctx, s.workDir())
	finalClean, finalReason := worktreeClean(ctx, s.workDir())
	state.Local.Head = finalHead
	state.Local.Clean = finalClean
	state.Local.Reason = finalReason
	state.Changed = finalHead == preserved && finalHead != head
	if mergeErr != nil || finalHead != preserved {
		blocked := blockedPlan(state, StatePipelineOwned, "blocked_recover_apply_failed", fmt.Sprintf("strict fast-forward to the preserved pipeline head failed; final HEAD is %s and no destructive recovery was attempted", finalHead))
		return blocked
	}
	if !finalClean {
		state.State = StateDirty
		state.Relation = RelationEqual
		state.Safety = "blocked_post_recover_" + finalReason
		state.Error = "HEAD reached the preserved pipeline head, but a Git hook left the worktree non-clean; custody was not recorded"
		state.NextAction = &NextAction{Code: "inspect_worktree", Command: "git status"}
		return state
	}
	return s.finishRecover(ctx, run, true, lock)
}

func (s *Service) anchorReachablePreserved(ctx context.Context, state State, anchorRef, preserved string) (State, bool) {
	if _, err := git.Run(ctx, s.workDir(), "update-ref", anchorRef, preserved, ""); err != nil {
		anchored, conflict := recoveryAnchorState(ctx, s.workDir(), anchorRef, preserved)
		if conflict {
			return blockedPlan(state, StatePipelineOwned, "blocked_recover_anchor_conflict", fmt.Sprintf("the recovery anchor %s changed or names a different object; custody was not returned and the conflicting anchor was not overwritten", anchorRef)), false
		}
		if !anchored {
			return blockedPlan(state, StatePipelineOwned, "blocked_recover_preserve_failed", "the preserved pipeline commits could not be immutably anchored locally; no files or refs were changed"), false
		}
	}
	if anchored, err := git.Run(ctx, s.workDir(), "rev-parse", anchorRef+"^{commit}"); err != nil || anchored != preserved {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_preserve_failed", "the preserved pipeline commits could not be anchored locally; no files or refs were changed"), false
	}
	return State{}, true
}

type custodyGateTransition struct {
	branch   string
	original string
	current  string
	owner    *db.CustodyTransition
	lock     *custodyLock
}

func custodyOriginalHead(ctx context.Context, gateDir, runID string) (string, bool) {
	if strings.TrimSpace(gateDir) == "" {
		return "", false
	}
	head, err := git.Run(ctx, gateDir, "rev-parse", custodyOriginalRef(runID)+"^{commit}")
	return head, err == nil
}

func (s *Service) prepareCustodyOriginal(ctx context.Context, lock *custodyLock, branch, runID, gateHead string) (string, error) {
	originalRef := custodyOriginalRef(runID)
	if existing, err := git.Run(ctx, s.GateDir, "rev-parse", originalRef+"^{commit}"); err == nil {
		if existing != gateHead {
			return "", fmt.Errorf("custody original marker mismatch")
		}
		return existing, nil
	}
	if err := s.updateGateRef(ctx, lock, branch, originalRef, "", gateHead); err != nil {
		return "", err
	}
	existing, err := git.Run(ctx, s.GateDir, "rev-parse", originalRef+"^{commit}")
	if err != nil || existing != gateHead {
		return "", fmt.Errorf("custody original marker could not be verified")
	}
	return existing, nil
}

func (s *Service) restoreCustodyGate(ctx context.Context, transition *custodyGateTransition) error {
	if transition == nil || transition.original == transition.current {
		return nil
	}
	if err := s.updateGateRef(ctx, transition.lock, transition.branch, "refs/heads/"+transition.branch, transition.current, transition.original); err != nil {
		return err
	}
	restored, err := git.Run(ctx, s.GateDir, "rev-parse", "refs/heads/"+transition.branch+"^{commit}")
	if err != nil || restored != transition.original {
		return fmt.Errorf("restored gate head could not be verified")
	}
	return nil
}

func (s *Service) cleanupRecoverMarkers(ctx context.Context, lock *custodyLock, run *db.Run) {
	if lock == nil || run == nil || strings.TrimSpace(s.GateDir) == "" {
		return
	}
	for _, marker := range []struct {
		ref   string
		value string
	}{
		{custodyReturnRef(run.ID), ""},
		{custodyOriginalRef(run.ID), ""},
		{recoverySourceRef(run.ID), run.HeadSHA},
	} {
		if marker.value == "" {
			marker.value, _ = git.Run(ctx, s.GateDir, "rev-parse", marker.ref+"^{commit}")
		}
		if marker.value != "" {
			_ = s.updateGateRef(ctx, lock, run.Branch, marker.ref, marker.value, "")
		}
	}
}

func (s *Service) recoverKeepLocalFailure(ctx context.Context, state State, run *db.Run, transition *custodyGateTransition, lock *custodyLock) State {
	if transition == nil {
		return state
	}
	if err := s.rollbackCustodyTransition(ctx, transition); err != nil {
		if current, getErr := s.DB.GetRun(run.ID); getErr == nil && current != nil && current.CustodyReturnedAt != nil {
			s.cleanupRecoverMarkers(ctx, lock, current)
			if current.CustodyTransitionToken != nil {
				_ = s.DB.ClearRunCustodyTransition(ctx, current.ID, *current.CustodyTransitionToken)
			}
			return s.InspectCached(ctx)
		}
		state.Safety = "blocked_recover_gate_race"
		state.Error = "the gate changed before custody could be recorded and the ordinary gate ref could not be restored; custody was not stamped; re-run after reconciling the gate"
		return state
	}
	s.cleanupRecoverMarkers(ctx, lock, run)
	return state
}

func (s *Service) rollbackCustodyTransition(ctx context.Context, transition *custodyGateTransition) error {
	phase, err := transition.owner.Phase(ctx)
	if err != nil {
		return err
	}
	if phase == db.CustodyPhaseGateMoved {
		if err := transition.owner.BeginRestore(ctx); err != nil {
			var phaseErr error
			phase, phaseErr = transition.owner.Phase(ctx)
			if phaseErr != nil {
				return err
			}
		} else {
			phase = db.CustodyPhaseRestoring
		}
	}
	if phase != db.CustodyPhaseRestoring {
		return db.ErrRunCustodyCAS
	}
	if err := s.restoreCustodyGate(ctx, transition); err != nil {
		return err
	}
	return transition.owner.FinishRestore(ctx)
}

// finishRecover stamps custody returned and reports the fresh post-recovery
// truth. changed reports whether this call moved the worktree HEAD.
func (s *Service) finishRecover(ctx context.Context, run *db.Run, changed bool, lock *custodyLock) State {
	return s.finishRecoverWithTransition(ctx, run, changed, nil, lock)
}

func (s *Service) finishRecoverWithTransition(ctx context.Context, run *db.Run, changed bool, transition *custodyGateTransition, lock *custodyLock) State {
	expectedGateHead := run.HeadSHA
	if transition != nil {
		expectedGateHead = transition.current
	}
	return s.finishRecoverAtGateHead(ctx, run, changed, transition, lock, expectedGateHead)
}

func (s *Service) finishRecoverAtGateHead(ctx context.Context, run *db.Run, changed bool, transition *custodyGateTransition, lock *custodyLock, expectedGateHead string) (result State) {
	if lock == nil {
		state, _, _ := s.inspect(ctx)
		state.Changed = changed
		state.Recovered = false
		state.Safety = "blocked_recover_gate_race"
		state.Error = "the live branch-lock authority is missing for the final custody check; custody was not stamped; re-run recovery"
		state.NextAction = nil
		return state
	}
	if !git.LooksLikeBareRepository(s.GateDir) || !s.gateConfigCurrent() {
		state, _, _ := s.inspect(ctx)
		state.Changed = changed
		state.Recovered = false
		state.Safety = "blocked_recover_gate_unavailable"
		state.Error = "the managed gate is missing or its fencing configuration changed before the final custody check; custody was not stamped; re-run recovery"
		state.NextAction = nil
		return state
	}
	authorityEndpoint, authorityErr := lock.ensureInternalMutationAuthority(s.DB)
	if authorityErr != nil {
		state, _, _ := s.inspect(ctx)
		state.Changed = changed
		state.Recovered = false
		state.Safety = "blocked_recover_gate_race"
		state.Error = "the live branch-lock authority could not be established for the final custody check; custody was not stamped; re-run recovery"
		state.NextAction = nil
		return state
	}
	lockGeneration, generationErr := newGateRefLockGeneration()
	if generationErr != nil {
		state, _, _ := s.inspect(ctx)
		state.Changed = changed
		state.Recovered = false
		state.Safety = "blocked_recover_gate_race"
		state.Error = "the ordinary gate ref lock generation could not be created; custody was not stamped; re-run recovery"
		state.NextAction = nil
		return state
	}
	gateRef := "refs/heads/" + run.Branch
	owner := gateRefLockOwner{
		RunID: run.ID, RepoID: s.Repo.ID, GatePath: s.GateDir, Branch: run.Branch,
		Ref: gateRef, OwnerGeneration: lockGeneration, AuthorityEndpoint: authorityEndpoint,
		ExpectedHead: expectedGateHead,
	}
	gateLock, gateLockErr := acquireOwnedGateRefLock(s.GateDir, gateRef, owner)
	if gateLockErr != nil {
		state, _, _ := s.inspect(ctx)
		state.Changed = changed
		state.Recovered = false
		state.Safety = "blocked_recover_gate_race"
		state.Error = "the ordinary gate ref could not be exclusively locked for the final custody check; custody was not stamped; re-run recovery"
		state.NextAction = nil
		return state
	}
	if err := s.DB.PrepareGateRefLock(db.GateRefLockJournal{
		RunID: run.ID, RepoID: s.Repo.ID, GatePath: s.GateDir, Branch: run.Branch, Ref: gateRef,
		LockPath: gateLock.path, OwnerGeneration: owner.OwnerGeneration, AuthorityEndpoint: owner.AuthorityEndpoint,
		ExpectedHead: expectedGateHead, FileIdentity: gateLock.identity,
	}); err != nil {
		gateLock.Release()
		state, _, _ := s.inspect(ctx)
		state.Changed = changed
		state.Recovered = false
		state.Safety = "blocked_recover_gate_race"
		state.Error = "the ordinary gate lock ownership journal could not be persisted; custody was not stamped; re-run recovery"
		state.NextAction = nil
		return state
	}
	gateLock.database = s.DB
	releaseGateLock := func() error {
		if gateLock != nil {
			err := gateLock.Release()
			if err == nil {
				gateLock = nil
			}
			return err
		}
		return nil
	}
	defer func() {
		if err := releaseGateLock(); err != nil {
			result.Recovered = false
			result.Safety = "blocked_recover_gate_race"
			result.Error = fmt.Sprintf("the ordinary gate lock could not be released (%v); custody remains retryable and the ownership journal was retained", err)
			result.NextAction = nil
		}
	}()
	if !s.gateConfigCurrent() {
		state, _, _ := s.inspect(ctx)
		state.Changed = changed
		state.Recovered = false
		state.Safety = "blocked_recover_gate_unavailable"
		state.Error = "the managed gate fencing configuration changed before the final custody check; custody was not stamped; re-run recovery"
		state.NextAction = nil
		return state
	}
	gateHead, gateHeadErr := readLockedGateRef(s.GateDir, "refs/heads/"+run.Branch)
	if gateHeadErr != nil || gateHead != expectedGateHead {
		state, _, _ := s.inspect(ctx)
		state.Changed = changed
		state.Recovered = false
		state.Safety = "blocked_recover_gate_race"
		state.Error = "the ordinary gate ref changed before the final custody stamp; custody was not stamped; re-run recovery"
		state.NextAction = nil
		return state
	}
	if !s.gateConfigCurrent() {
		state, _, _ := s.inspect(ctx)
		state.Changed = changed
		state.Recovered = false
		state.Safety = "blocked_recover_gate_unavailable"
		state.Error = "the managed gate fencing configuration changed before the custody stamp; custody was not stamped; re-run recovery"
		state.NextAction = nil
		return state
	}
	var stampErr error
	if transition != nil {
		stampErr = transition.owner.Complete(ctx, run)
	} else {
		stampErr = s.DB.SetRunCustodyReturnedCAS(run)
	}
	if stampErr != nil {
		releaseGateLock()
		if transition != nil {
			current, currentErr := s.DB.GetRun(run.ID)
			if currentErr == nil && current != nil && current.CustodyReturnedAt != nil {
				s.cleanupRecoverMarkers(ctx, lock, current)
				if current.CustodyTransitionToken != nil {
					_ = s.DB.ClearRunCustodyTransition(ctx, current.ID, *current.CustodyTransitionToken)
				}
				state, _, _ := s.inspect(ctx)
				state.Recovered = true
				state.Changed = changed
				return state
			}
			if restoreErr := s.rollbackCustodyTransition(ctx, transition); restoreErr != nil {
				state, _, _ := s.inspect(ctx)
				state.Changed = changed
				state.Recovered = false
				state.Safety = "blocked_recover_gate_race"
				state.Error = "the exact run ownership or publication authority changed before custody could be recorded, and the ordinary gate ref changed again before it could be restored; custody was not stamped; re-run after reconciling the gate"
				state.NextAction = nil
				return state
			}
			s.cleanupRecoverMarkers(ctx, lock, run)
		}
		state, _, _ := s.inspect(ctx)
		state.Changed = changed
		state.Recovered = false
		state.Safety = "blocked_recover_custody_race"
		state.Error = "the exact run ownership or publication authority changed before custody could be recorded; custody was not stamped; re-run from freshly inspected state"
		state.NextAction = nil
		return state
	}
	if err := s.DB.MarkGateRefLockStamped(run.ID, owner.OwnerGeneration); err != nil {
		gateLock.database = nil
		state, _, _ := s.inspect(ctx)
		state.Changed = changed
		state.Recovered = true
		state.Safety = "blocked_recover_gate_race"
		state.Error = "custody was stamped but the ordinary gate lock ownership journal could not be finalized; re-run recovery to reclaim it"
		state.NextAction = nil
		return state
	}
	s.cleanupRecoverMarkers(ctx, lock, run)
	if transition != nil {
		_ = transition.owner.ReleaseStamped(ctx, run.ID)
	}
	state, _, _ := s.inspect(ctx)
	state.Recovered = true
	state.Changed = changed
	return state
}

func (s *Service) reclaimStampedGateRefLock(lock *custodyLock, run *db.Run) error {
	if s == nil || s.DB == nil || s.Repo == nil || lock == nil || lock.file == nil || run == nil {
		return fmt.Errorf("stamped gate lock reclaim requires active ownership")
	}
	if _, err := lock.file.Stat(); err != nil {
		return fmt.Errorf("branch ownership lock is no longer live")
	}
	if !git.LooksLikeBareRepository(s.GateDir) || !s.gateConfigCurrent() {
		return fmt.Errorf("managed gate fencing configuration is unavailable")
	}
	journal, err := s.DB.GetGateRefLock(run.ID)
	if err != nil {
		return err
	}
	if journal == nil {
		ref := "refs/heads/" + run.Branch
		lockPath := filepath.Join(s.GateDir, filepath.FromSlash(ref)+".lock")
		if _, statErr := os.Stat(lockPath); os.IsNotExist(statErr) {
			return nil
		} else if statErr != nil {
			return fmt.Errorf("inspect stamped gate lock: %w", statErr)
		}
		owner, ownerErr := readOwnedGateRefLock(lockPath)
		if ownerErr != nil {
			return ownerErr
		}
		if owner.RunID != run.ID || owner.RepoID != s.Repo.ID || owner.GatePath != s.GateDir || owner.Branch != run.Branch || owner.Ref != ref || owner.ExpectedHead == "" || owner.OwnerGeneration == "" || owner.AuthorityEndpoint == "" {
			return fmt.Errorf("stamped gate lock owner does not match the returned run")
		}
		identity, identityErr := gateRefFileIdentity(lockPath)
		if identityErr != nil {
			return identityErr
		}
		journal = &db.GateRefLockJournal{RunID: owner.RunID, RepoID: owner.RepoID, GatePath: owner.GatePath, Branch: owner.Branch, Ref: owner.Ref, LockPath: lockPath, OwnerGeneration: owner.OwnerGeneration, AuthorityEndpoint: owner.AuthorityEndpoint, ExpectedHead: owner.ExpectedHead, FileIdentity: identity, State: db.GateRefLockStateStamped}
	}
	if journal.State != db.GateRefLockStatePrepared && journal.State != db.GateRefLockStateStamped {
		return fmt.Errorf("gate lock ownership journal has an unknown state")
	}
	if journal.RepoID != s.Repo.ID || journal.Branch != run.Branch || journal.Ref != "refs/heads/"+run.Branch || journal.ExpectedHead == "" || journal.OwnerGeneration == "" || journal.AuthorityEndpoint == "" || journal.FileIdentity == "" {
		return fmt.Errorf("gate lock ownership journal does not match the stamped run")
	}
	if journal.GatePath != s.GateDir || journal.LockPath != filepath.Join(s.GateDir, filepath.FromSlash(journal.Ref)+".lock") {
		return fmt.Errorf("gate lock ownership journal path changed")
	}
	current, err := s.DB.GetRun(run.ID)
	if err != nil || current == nil || current.CustodyReturnedAt == nil {
		return fmt.Errorf("the exact custody stamp could not be re-read")
	}
	if conn, dialErr := dialInternalMutationAuthority(journal.AuthorityEndpoint); dialErr == nil {
		_ = conn.Close()
		return fmt.Errorf("the recorded gate lock authority is still live")
	}
	owner := gateRefLockOwner{RunID: journal.RunID, RepoID: journal.RepoID, GatePath: journal.GatePath, Branch: journal.Branch, Ref: journal.Ref, OwnerGeneration: journal.OwnerGeneration, AuthorityEndpoint: journal.AuthorityEndpoint, ExpectedHead: journal.ExpectedHead}
	var gateLock *gateRefLock
	if err := HandoffManagedGateRefAuthority(s.GateDir, journal.Ref); err != nil {
		return fmt.Errorf("handoff stamped gate authority: %w", err)
	}
	if _, statErr := os.Stat(journal.LockPath); os.IsNotExist(statErr) {
		gateLock, err = acquireOwnedGateRefLock(s.GateDir, journal.Ref, owner)
		if err != nil {
			return fmt.Errorf("reacquire stamped gate lock: %w", err)
		}
		if err := s.DB.UpdateGateRefLockIdentity(run.ID, journal.OwnerGeneration, gateLock.identity); err != nil {
			gateLock.closeKeepJournal()
			return err
		}
	} else if statErr != nil {
		return fmt.Errorf("inspect stamped gate lock: %w", statErr)
	} else {
		ownerOnDisk, ownerErr := readOwnedGateRefLock(journal.LockPath)
		if ownerErr != nil {
			return ownerErr
		}
		if ownerOnDisk != owner {
			return fmt.Errorf("stamped gate lock owner changed")
		}
		identity, identityErr := gateRefFileIdentity(journal.LockPath)
		if identityErr != nil || identity != journal.FileIdentity {
			return fmt.Errorf("stamped gate lock file identity changed")
		}
		file, openErr := os.OpenFile(journal.LockPath, os.O_RDWR, 0o644)
		if openErr != nil {
			return fmt.Errorf("open stamped gate lock: %w", openErr)
		}
		gateLock = &gateRefLock{file: file, path: journal.LockPath, owner: owner, identity: identity}
	}
	defer gateLock.closeKeepJournal()
	if err := acquireGateRefOSLock(gateLock.file); err != nil {
		return fmt.Errorf("acquire stamped gate lock: %w", err)
	}
	gateLock.osLocked = true
	if identity, identityErr := gateRefFileIdentity(journal.LockPath); identityErr != nil || identity != gateLock.identity {
		return fmt.Errorf("stamped gate lock file identity changed during reclaim")
	}
	gateHead, err := readLockedGateRef(s.GateDir, journal.Ref)
	if err != nil || gateHead != journal.ExpectedHead {
		return fmt.Errorf("ordinary gate ref changed before stamped lock reclaim")
	}
	if err := s.DB.ClearGateRefLock(run.ID, journal.OwnerGeneration); err != nil {
		return err
	}
	if gateHead, rereadErr := readLockedGateRef(s.GateDir, journal.Ref); rereadErr != nil || gateHead != journal.ExpectedHead {
		_ = s.DB.PrepareGateRefLock(*journal)
		return fmt.Errorf("ordinary gate ref changed while reclaiming stamped lock")
	}
	if err := removeHeldGateRefLock(gateLock); err != nil {
		_ = s.DB.PrepareGateRefLock(*journal)
		return fmt.Errorf("remove stamped gate lock: %w", err)
	}
	if _, statErr := os.Lstat(journal.LockPath); statErr == nil {
		_ = s.DB.PrepareGateRefLock(*journal)
		return fmt.Errorf("stamped gate lock remained after removal")
	} else if !os.IsNotExist(statErr) {
		_ = s.DB.PrepareGateRefLock(*journal)
		return fmt.Errorf("verify stamped gate lock removal: %w", statErr)
	}
	return nil
}

func releaseCustodyOwner(state State, owner *db.CustodyTransition) State {
	if owner == nil {
		return state
	}
	if err := owner.Release(); err != nil {
		state.Safety = "blocked_recover_custody_race"
		state.Error = "the custody transition could not be released; custody was not stamped; re-run from freshly inspected state"
	}
	return state
}

func recoverAnchorRef(runID string) string {
	return "refs/no-mistakes/recover/" + runID
}

func recoverySourceRef(runID string) string {
	return "refs/no-mistakes/recovery-source/" + runID
}

func custodyReturnRef(runID string) string {
	return "refs/no-mistakes/custody-return/" + runID
}

func custodyOriginalRef(runID string) string {
	return "refs/no-mistakes/custody-original/" + runID
}

func (s *Service) latestRunForBranch(branch string) (*db.Run, bool) {
	runs, err := s.DB.GetRunsByRepo(s.Repo.ID)
	if err != nil {
		return nil, false
	}
	for _, run := range runs {
		if run.Branch == branch {
			return run, true
		}
	}
	return nil, false
}

func isExactFullObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func recoveryAnchorState(ctx context.Context, dir, anchorRef, preserved string) (anchored, conflict bool) {
	existing, err := git.Run(ctx, dir, "show-ref", "--verify", "--hash", anchorRef)
	if err != nil {
		return false, false
	}
	return existing == preserved, existing != preserved
}

func (s *Service) fetchExactRecoveryAnchor(ctx context.Context, lock *custodyLock, run *db.Run, preserved, anchorRef string) string {
	sourceRef := recoverySourceRef(run.ID)
	source, err := git.Run(ctx, s.GateDir, "rev-parse", sourceRef+"^{commit}")
	if err == nil {
		if source != preserved {
			return "blocked_recover_anchor_conflict"
		}
	} else {
		if err := s.updateGateRef(ctx, lock, run.Branch, sourceRef, "", preserved); err != nil {
			return "blocked_recover_preserve_failed"
		}
	}
	if s.beforeRecoverAnchorFetch != nil {
		s.beforeRecoverAnchorFetch()
	}
	stagingRef := "refs/no-mistakes/recovery-fetch/" + run.ID
	defer func() { _, _ = git.Run(ctx, s.workDir(), "update-ref", "-d", stagingRef) }()
	if err := git.FetchRemoteRefToPrivateRef(ctx, s.workDir(), s.GateDir, sourceRef, stagingRef); err != nil {
		return "blocked_recover_preserve_failed"
	}
	if staged, err := git.Run(ctx, s.workDir(), "rev-parse", stagingRef+"^{commit}"); err != nil || staged != preserved {
		return "blocked_recover_preserve_failed"
	}
	if _, err := git.Run(ctx, s.workDir(), "update-ref", anchorRef, preserved, ""); err != nil {
		anchored, conflict := recoveryAnchorState(ctx, s.workDir(), anchorRef, preserved)
		if conflict {
			return "blocked_recover_anchor_conflict"
		}
		if !anchored {
			return "blocked_recover_preserve_failed"
		}
	}
	if anchored, err := git.Run(ctx, s.workDir(), "rev-parse", anchorRef+"^{commit}"); err != nil || anchored != preserved {
		return "blocked_recover_preserve_failed"
	}
	_ = s.updateGateRef(ctx, lock, run.Branch, sourceRef, preserved, "")
	return ""
}

func runHasPublication(run *db.Run) bool {
	if run == nil {
		return true
	}
	if run.LastPushedSHA != nil || run.PushTargetKind != nil || run.PushTargetFingerprint != nil ||
		run.PushRef != nil || positiveInt64(run.LastPushedAt) || positiveInt64(run.PushGeneration) || run.PushActive ||
		publicationAttemptPresent(run) ||
		run.PRURL != nil || run.PRStateObservedAt != nil || run.CIReadyAt != nil {
		return true
	}
	return run.PRState != nil && normalizePRState(run.PRState) != "none"
}

func publicationAttemptPresent(run *db.Run) bool {
	return run != nil && (run.PublicationAttemptHeadSHA != nil || run.PublicationAttemptTargetKind != nil || run.PublicationAttemptTargetFingerprint != nil || run.PublicationAttemptRef != nil)
}

func (s *Service) verifyLegacyRunUnpublished(ctx context.Context, run *db.Run, branch string, repo *db.Repo) string {
	if run == nil || run.PublicationJournalState == nil || *run.PublicationJournalState != db.PublicationJournalReady {
		return "blocked_recover_publication"
	}
	if run.SubmittedHeadSHA == nil || !isExactFullObjectID(ptr(run.SubmittedHeadSHA)) ||
		run.PublicationJournalTargetKind == nil || run.PublicationJournalTargetFingerprint == nil || run.PublicationJournalRef == nil {
		return "blocked_recover_publication"
	}
	target := s.recoveryPublicationTarget(ctx, repo)
	if target == "" || repo == nil || run.PublicationJournalTargetVersion == nil || *run.PublicationJournalTargetVersion != repo.URLVersion || ptr(run.PublicationJournalTargetKind) != targetKind(repo) ||
		ptr(run.PublicationJournalTargetFingerprint) != TargetFingerprint(target) ||
		ptr(run.PublicationJournalRef) != publicationRef(branch) {
		return "blocked_recover_publication"
	}
	return ""
}

func (s *Service) verifyLegacyGateBaseline(ctx context.Context, state State, run *db.Run, branch string, repo *db.Repo, gateHead string) bool {
	if s == nil || run == nil || run.SubmittedHeadSHA == nil || !isExactFullObjectID(ptr(run.SubmittedHeadSHA)) || state.Local.Head != ptr(run.SubmittedHeadSHA) || gateHead != ptr(run.SubmittedHeadSHA) {
		return false
	}
	remoteList, err := git.Run(ctx, s.workDir(), "remote")
	if err != nil {
		return false
	}
	remotes := strings.Fields(remoteList)
	if len(remotes) == 0 {
		return false
	}
	seenTargets := make(map[string]struct{}, len(remotes))
	for _, remote := range remotes {
		urls, urlErr := git.GetConfiguredRemoteURLs(ctx, s.workDir(), remote)
		if urlErr != nil || len(urls) != 1 || strings.TrimSpace(urls[0]) == "" {
			return false
		}
		remoteHead, headErr := git.LsRemote(ctx, s.workDir(), urls[0], publicationRef(branch))
		if headErr != nil || remoteHead != ptr(run.SubmittedHeadSHA) {
			return false
		}
		seenTargets[TargetFingerprint(urls[0])] = struct{}{}
	}
	for _, target := range []string{repo.UpstreamURL, repo.ForkURL} {
		if strings.TrimSpace(target) != "" {
			if _, ok := seenTargets[TargetFingerprint(target)]; !ok {
				return false
			}
		}
	}
	return true
}

func (s *Service) recoveryPublicationTarget(ctx context.Context, repo *db.Repo) string {
	if s == nil || repo == nil {
		return ""
	}
	target := strings.TrimSpace(repo.PushURL())
	origin, err := git.GetRemoteURL(ctx, s.workDir(), "origin")
	if err == nil && strings.TrimSpace(origin) != "" && TargetFingerprint(origin) == TargetFingerprint(target) {
		return strings.TrimSpace(origin)
	}
	return target
}

func publicationRef(branch string) string {
	return "refs/heads/" + strings.TrimPrefix(strings.TrimSpace(branch), "refs/heads/")
}

func positiveInt64(value *int64) bool {
	return value != nil && *value > 0
}

func (s *Service) inspect(ctx context.Context) (State, *db.Run, bool) {
	state := State{Relation: RelationUnknown, Safety: "blocked_ambiguous_context", Remote: RemoteState{Freshness: "unknown"}}
	root, err := git.FindGitRoot(s.workDir())
	if err != nil {
		state.State = StateAmbiguousContext
		state.Error = "the invoking directory is not a registered Git worktree"
		return state, nil, false
	}
	mainRoot, err := git.FindMainRepoRoot(root)
	if err != nil || !samePath(mainRoot, s.Repo.WorkingPath) {
		state.State = StateAmbiguousContext
		state.Error = "the invoking worktree does not belong to the registered repository"
		return state, nil, false
	}
	branch, err := git.CurrentBranch(ctx, root)
	if err != nil || branch == "" || branch == "HEAD" {
		state.State = StateAmbiguousContext
		state.Error = "synchronization requires an exact checked-out branch, not detached HEAD"
		return state, nil, false
	}
	head, err := git.HeadSHA(ctx, root)
	if err != nil {
		state.State = StateAmbiguousContext
		state.Error = "could not resolve the invoking worktree HEAD"
		return state, nil, false
	}
	state.Local = LocalState{Branch: branch, Head: head}
	clean, reason := worktreeClean(ctx, root)
	state.Local.Clean = clean
	state.Local.Reason = reason

	runs, err := s.DB.GetRunsByRepo(s.Repo.ID)
	if err != nil {
		state.State = StateAmbiguousContext
		state.Error = "could not load pipeline push provenance"
		return state, nil, false
	}
	var run *db.Run
	var newerPushed *db.Run
	for _, candidate := range runs {
		if candidate.Branch != branch {
			continue
		}
		if candidate.Status == types.RunPending || candidate.Status == types.RunRunning || unpublishedPipelineHead(candidate) {
			// A terminal unpublished run can be superseded only by a newer
			// exact binding whose pushed head is proven, in the local gate, to
			// contain the preserved head. Active ownership remains absolute.
			if unpublishedPipelineHead(candidate) && s.supersededUnpublishedRun(ctx, candidate, newerPushed, branch) {
				continue
			}
			run = candidate
			break
		}
		if newerPushed == nil && exactPushedBinding(s.Repo, candidate, branch) {
			newerPushed = candidate
		}
		// Custody-returned runs stay selectable so a recovered branch reports
		// custody_returned (or its ordinary post-push classification) instead
		// of falling back to an older binding or an ambiguous no-match. A
		// terminal run whose head never left the submitted head is equally
		// selectable: its terminal outcome released the branch, and skipping the
		// run misreported that release as wrong-branch ambiguity (v1.44.2
		// dogfood). The run==nil guard keeps every newer authoritative run winning.
		if run == nil && (candidate.LastPushedSHA != nil || candidate.CustodyReturnedAt != nil || releasedSubmittedHeadRun(candidate)) {
			run = candidate
		}
	}
	if run == nil {
		if len(runs) > 0 {
			state.State = StateAmbiguousContext
			state.Safety = "blocked_wrong_branch"
			state.Error = "the checked-out branch does not match any pipeline push binding"
		} else {
			state.State = StateLegacyUnbound
			state.Safety = "blocked_legacy_unbound"
			state.Error = "no exact successful pipeline push binding exists for the checked-out branch"
		}
		return state, nil, false
	}

	state.Pipeline = PipelineState{
		RunID: run.ID, Status: string(run.Status), SubmittedHead: ptr(run.SubmittedHeadSHA), CurrentHead: run.HeadSHA,
		PushedHead: ptr(run.LastPushedSHA), PushedAt: value(run.LastPushedAt), PushGeneration: value(run.PushGeneration),
	}
	state.PRState = normalizePRState(run.PRState)
	state.Target = TargetState{Kind: ptr(run.PushTargetKind), URL: displayTarget(s.Repo.PushURL()), Ref: ptr(run.PushRef)}
	state.Target.Remote = s.remoteName(ctx)
	state.Remote = RemoteState{ObservedHead: ptr(run.LastPushedSHA), Freshness: "pipeline_push", ObservedAt: value(run.LastPushedAt)}

	if run.PushActive || pushStepRunning(s.DB, run.ID) {
		state.State = StatePushInProgress
		state.Safety = "blocked_push_in_progress"
		state.Pipeline.Phase = "push"
		state.NextAction = &NextAction{Code: "continue_active_run", Command: "no-mistakes axi status"}
		return state, run, false
	}
	if run.LastPushedSHA == nil || run.PushTargetFingerprint == nil || run.PushRef == nil || run.PushGeneration == nil || run.SubmittedHeadSHA == nil {
		if run.SubmittedHeadSHA != nil && run.HeadSHA != ptr(run.SubmittedHeadSHA) {
			if run.CustodyReturnedAt != nil {
				s.classifyCustodyReturned(ctx, &state)
				return state, run, true
			}
			s.classifyPipelineOwned(ctx, &state, run, "the pipeline head has moved but has not been successfully pushed; do not make local follow-up commits yet")
			return state, run, false
		}
		// The head never left the submitted head and nothing was pushed. An
		// active run owns the branch and blocks with the plain pipeline-owned
		// reason. A terminal run releases it only when terminalization verified
		// the managed worktree head: no pipeline-created content exists to
		// recover, and the branch and head are immediately usable.
		if run.SubmittedHeadSHA != nil && run.LastPushedSHA == nil {
			if run.CustodyReturnedAt != nil {
				s.classifyCustodyReturned(ctx, &state)
				return state, run, true
			}
			if run.Status == types.RunPending || run.Status == types.RunRunning {
				s.classifyPipelineOwned(ctx, &state, run, "a validation run is active on this branch; do not make local follow-up commits until it finishes")
				return state, run, false
			}
			if terminalRunStatus(run.Status) && run.TerminalHeadVerifiedAt != nil && runHeadUnmoved(run) {
				s.classifyUserOwned(ctx, &state)
				return state, run, true
			}
			if terminalRunStatus(run.Status) {
				s.classifyPipelineOwned(ctx, &state, run, "the terminal run has no verified worktree-head evidence; recover custody before local follow-up work")
				return state, run, false
			}
		}
		state.State = StateLegacyUnbound
		state.Safety = "blocked_legacy_unbound"
		state.Error = "this run has no exact successful push provenance and cannot be synchronized safely"
		return state, run, false
	}
	if run.HeadSHA != ptr(run.LastPushedSHA) && run.CustodyReturnedAt == nil {
		s.classifyPipelineOwned(ctx, &state, run, "the pipeline head has not been successfully bound to the push target; do not make local follow-up commits yet")
		return state, run, false
	}
	// Terminal PR lifecycle retires the branch regardless of local dirtiness
	// or later target configuration. Refresh may classify retained versus
	// removed only while the exact original target binding still matches.
	if state.PRState == "merged" {
		state.State = StateMergedRemoteRetained
		state.Safety = "blocked_merged"
		return state, run, true
	}
	if state.PRState == "closed" {
		state.State = StateClosed
		state.Safety = "blocked_closed"
		return state, run, true
	}
	if ptr(run.PushRef) != "refs/heads/"+branch || ptr(run.PushTargetFingerprint) != TargetFingerprint(s.Repo.PushURL()) || ptr(run.PushTargetKind) != targetKind(s.Repo) {
		state.State = StateTargetChanged
		state.Safety = "blocked_target_changed"
		state.Error = "the configured push target or branch ref changed after the pipeline push"
		return state, run, false
	}
	if duplicateBranchCheckout(ctx, root, branch) {
		state.State = StateAmbiguousContext
		state.Safety = "blocked_branch_ambiguous"
		state.Error = "the checked-out branch is attached to more than one worktree"
		return state, run, false
	}
	if !clean {
		state.State = StateDirty
		state.Safety = "blocked_" + reason
		state.Error = "the invoking worktree is not completely clean; no network read or mutation was attempted"
		state.NextAction = &NextAction{Code: "inspect_worktree", Command: "git status"}
		return state, run, false
	}

	s.classifyRelation(ctx, &state, ptr(run.LastPushedSHA), run.BaseSHA, false)
	return state, run, true
}

func (s *Service) classifyRelation(ctx context.Context, state *State, pushed, base string, live bool) {
	if state.Local.Head == pushed {
		state.State = StateSynchronized
		state.Relation = RelationEqual
		state.Safety = "already_synchronized"
		state.NextAction = nil
		return
	}
	if objectExists(ctx, s.workDir(), pushed) {
		switch {
		case isAncestor(ctx, s.workDir(), state.Local.Head, pushed):
			state.State = StateBehind
			state.Relation = RelationBehind
		case isAncestor(ctx, s.workDir(), pushed, state.Local.Head):
			state.State = StateLocalAhead
			state.Relation = RelationAhead
			state.Safety = "blocked_local_ahead"
			state.NextAction = &NextAction{Code: "run_pipeline", Command: `no-mistakes axi run --intent "<what the user set out to accomplish>"`}
			return
		default:
			if equivalentDivergence(ctx, s.workDir(), state.Local.Head, pushed, base) {
				state.State = StateDiverged
				state.Relation = RelationDiverged
				if live {
					state.Safety = SafetySafeEquivalentAdvance
				} else {
					state.Safety = "refresh_required"
				}
				state.NextAction = &NextAction{Code: "sync", Command: "no-mistakes axi sync"}
				state.Error = ""
				return
			}
			state.State = StateDiverged
			state.Relation = RelationDiverged
			state.Safety = "blocked_diverged"
			state.NextAction = &NextAction{Code: "inspect_and_reconcile_manually", Command: "git log --oneline --left-right HEAD..." + pushed}
			state.Error = "local and pipeline-pushed histories have diverged; no files or refs were changed"
			return
		}
	} else if state.Local.Head == state.Pipeline.SubmittedHead && state.Pipeline.SubmittedHead != pushed {
		state.State = StateBehind
		state.Relation = RelationBehind
	} else {
		state.State = StateAmbiguousContext
		state.Relation = RelationUnknown
		state.Safety = "blocked_relation_unknown"
		state.Error = "the pipeline-pushed commit is not available locally; run an explicit synchronization check"
		state.NextAction = &NextAction{Code: "check_sync", Command: "no-mistakes sync --check"}
		return
	}
	if live {
		state.Safety = SafetySafeFastForward
	} else {
		state.Safety = "refresh_required"
	}
	state.NextAction = &NextAction{Code: "sync", Command: "no-mistakes axi sync"}
}

func syncAnchorRef(runID string) string {
	return "refs/no-mistakes/sync-anchor/" + runID
}

func equivalentDivergence(ctx context.Context, dir, local, pushed, base string) bool {
	if local == "" || pushed == "" || local == pushed {
		return false
	}
	base = usableEquivalenceBase(ctx, dir, local, pushed, base)
	if base == "" {
		return false
	}
	_, err := revList(ctx, dir, append([]string{"rev-list", "--right-only", pushed + "..." + local}, "^"+base)...)
	if err != nil {
		return false
	}
	return mergeTreePreservesFinalHead(ctx, dir, base, local, pushed)
}

func usableEquivalenceBase(ctx context.Context, dir, local, pushed, base string) string {
	if base != "" && !git.IsZeroSHA(base) && objectExists(ctx, dir, base) {
		return base
	}
	mergeBase, err := git.Run(ctx, dir, "merge-base", local, pushed)
	if err != nil {
		return ""
	}
	return mergeBase
}

func revList(ctx context.Context, dir string, args ...string) ([]string, error) {
	out, err := git.Run(ctx, dir, args...)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(out) == "" {
		return nil, nil
	}
	return strings.Fields(out), nil
}

func mergeTreePreservesFinalHead(ctx context.Context, dir, base, local, pushed string) bool {
	mergedTree, err := git.Run(ctx, dir, "merge-tree", "--write-tree", "--merge-base", base, pushed, local)
	if err != nil {
		return false
	}
	pushedTree, err := git.Run(ctx, dir, "rev-parse", pushed+"^{tree}")
	if err != nil {
		return false
	}
	return mergedTree == pushedTree
}

func (s *Service) remoteName(ctx context.Context) string {
	out, err := git.Run(ctx, s.workDir(), "remote")
	if err == nil {
		for _, name := range strings.Fields(out) {
			remoteURL, err := git.GetConfiguredRemoteURL(ctx, s.workDir(), name)
			if err == nil && TargetFingerprint(remoteURL) == TargetFingerprint(s.Repo.PushURL()) {
				return name
			}
		}
	}
	if strings.TrimSpace(s.Repo.ForkURL) != "" {
		return "fork"
	}
	return "origin"
}

func (s *Service) workDir() string {
	if strings.TrimSpace(s.WorkDir) == "" {
		return "."
	}
	return s.WorkDir
}

func refreshable(state State) bool {
	switch state.State {
	case StateBehind, StateSynchronized, StateLocalAhead, StateDiverged, StateMergedRemoteRetained, StateClosed, StateAmbiguousContext:
		return true
	default:
		return false
	}
}

func worktreeClean(ctx context.Context, dir string) (bool, string) {
	markers := []struct{ path, reason string }{
		{"MERGE_HEAD", "merge_in_progress"}, {"rebase-merge", "rebase_in_progress"}, {"rebase-apply", "rebase_in_progress"},
		{"CHERRY_PICK_HEAD", "cherry_pick_in_progress"}, {"REVERT_HEAD", "revert_in_progress"}, {"BISECT_LOG", "bisect_in_progress"}, {"sequencer", "sequencer_in_progress"},
	}
	for _, marker := range markers {
		path, err := git.Run(ctx, dir, "rev-parse", "--git-path", marker.path)
		if err == nil {
			if !filepath.IsAbs(path) {
				path = filepath.Join(dir, path)
			}
			if _, err := os.Stat(path); err == nil {
				return false, marker.reason
			}
		}
	}
	dirty, err := git.HasUncommittedChanges(ctx, dir)
	if err != nil {
		return false, "status_unavailable"
	}
	if dirty {
		return false, "dirty"
	}
	return true, ""
}

func duplicateBranchCheckout(ctx context.Context, dir, branch string) bool {
	out, err := git.Run(ctx, dir, "worktree", "list", "--porcelain")
	if err != nil {
		return true
	}
	needle := "branch refs/heads/" + branch
	count := 0
	for _, line := range strings.Split(out, "\n") {
		if line == needle {
			count++
		}
	}
	return count != 1
}

func unpublishedPipelineHead(run *db.Run) bool {
	if run == nil || run.SubmittedHeadSHA == nil || run.CustodyReturnedAt != nil {
		return false
	}
	if run.LastPushedSHA == nil {
		return run.HeadSHA != ptr(run.SubmittedHeadSHA) || (terminalRunStatus(run.Status) && run.TerminalHeadVerifiedAt == nil)
	}
	return run.HeadSHA != ptr(run.LastPushedSHA)
}

func exactPushedBinding(repo *db.Repo, run *db.Run, branch string) bool {
	return repo != nil && run != nil && run.Branch == branch && !run.PushActive && run.HeadSHA != "" &&
		run.LastPushedSHA != nil && run.HeadSHA == ptr(run.LastPushedSHA) &&
		run.PushTargetKind != nil && ptr(run.PushTargetKind) == targetKind(repo) &&
		run.PushTargetFingerprint != nil && ptr(run.PushTargetFingerprint) == TargetFingerprint(repo.PushURL()) &&
		run.PushRef != nil && ptr(run.PushRef) == "refs/heads/"+branch &&
		run.PushGeneration != nil
}

// supersededUnpublishedRun proves the narrow rerun relationship needed to
// ignore an older terminal unpublished head during branch selection. The gate
// is read-only evidence: its exact branch head must equal the newer push
// binding, and Git must prove the older preserved head is its ancestor. Any
// missing or conflicting evidence leaves the older run authoritative.
func (s *Service) supersededUnpublishedRun(ctx context.Context, older, newer *db.Run, branch string) bool {
	if older == nil || newer == nil || !terminalRunStatus(older.Status) || !unpublishedPipelineHead(older) ||
		!samePushTargetBinding(older, newer) || strings.TrimSpace(s.GateDir) == "" || older.HeadSHA == "" || newer.LastPushedSHA == nil {
		return false
	}
	pushed := ptr(newer.LastPushedSHA)
	gateHead, err := git.Run(ctx, s.GateDir, "rev-parse", "refs/heads/"+branch+"^{commit}")
	if err != nil || gateHead != pushed {
		return false
	}
	return isAncestor(ctx, s.GateDir, older.HeadSHA, pushed)
}

func samePushTargetBinding(older, newer *db.Run) bool {
	return older != nil && newer != nil &&
		older.PushTargetKind != nil && newer.PushTargetKind != nil && ptr(older.PushTargetKind) == ptr(newer.PushTargetKind) &&
		older.PushTargetFingerprint != nil && newer.PushTargetFingerprint != nil && ptr(older.PushTargetFingerprint) == ptr(newer.PushTargetFingerprint) &&
		older.PushRef != nil && newer.PushRef != nil && ptr(older.PushRef) == ptr(newer.PushRef)
}

func terminalRunStatus(status types.RunStatus) bool {
	switch status {
	case types.RunCompleted, types.RunFailed, types.RunCancelled:
		return true
	default:
		return false
	}
}

// classifyPipelineOwned reports a run that still holds branch custody without
// a successful push binding. While the run is active the block is absolute:
// the pipeline will publish or keep moving the head, so the worktree must
// wait. Once a run that MOVED the head is TERMINAL nothing will ever publish
// that head - the branch would be stranded in custody forever - so the same
// state becomes recoverable and points at the guarded custody-return action
// (issue: v1.38.1 dogfood, cancelled pre-push run with pipeline commits). A
// terminal run that never moved the head is classified user_owned instead:
// its terminal outcome releases ownership. The relation between the local
// head and the run's recorded head is exposed whenever it is computable
// locally, so the operator sees the exact ownership facts before acting.
func (s *Service) classifyPipelineOwned(ctx context.Context, state *State, run *db.Run, activeMessage string) {
	state.State = StatePipelineOwned
	state.Pipeline.Phase = "pre_push"
	state.Relation = relationBetween(ctx, s.workDir(), state.Local.Head, run.HeadSHA)
	if terminalRunStatus(run.Status) {
		state.Safety = "blocked_pipeline_owned_recoverable"
		state.Error = "the run finished " + string(run.Status) + " with unpublished pipeline commits preserved in the local gate; recover custody before any local follow-up commit"
		state.NextAction = &NextAction{Code: "recover_custody", Command: "no-mistakes axi sync --recover"}
		return
	}
	state.Safety = "blocked_pipeline_owned"
	state.Error = activeMessage
	state.NextAction = &NextAction{Code: "continue_active_run", Command: "no-mistakes axi status"}
}

// classifyUserOwned reports a branch released by its terminal outcome: the
// terminal run ended before the pipeline changed the submitted head, so no
// pipeline-created content exists to recover. The exact branch and head are
// the operator's and immediately usable - no sync action is required or
// offered, and a separately authorized direct push or PR is never blocked.
func (s *Service) classifyUserOwned(ctx context.Context, state *State) {
	state.State = StateUserOwned
	state.Safety = "user_owned"
	state.Error = ""
	state.NextAction = nil
	state.Relation = relationBetween(ctx, s.workDir(), state.Local.Head, state.Pipeline.CurrentHead)
}

// runHeadUnmoved reports whether the run's pipeline head still equals the
// submitted head, i.e. the run holds no pipeline-authored commits beyond what
// the operator submitted.
func runHeadUnmoved(run *db.Run) bool {
	return run != nil && run.SubmittedHeadSHA != nil && *run.SubmittedHeadSHA != "" && run.HeadSHA == *run.SubmittedHeadSHA
}

// releasedSubmittedHeadRun reports a terminal run whose outcome released
// the branch: no push provenance, no custody stamp, and positive terminal
// evidence that head_sha still equals submitted_head_sha.
func releasedSubmittedHeadRun(run *db.Run) bool {
	return run != nil && terminalRunStatus(run.Status) && run.CustodyReturnedAt == nil &&
		run.LastPushedSHA == nil && run.TerminalHeadVerifiedAt != nil && runHeadUnmoved(run)
}

// RunHeadUnmoved reports whether the classified run's pipeline head still
// equals the submitted head, i.e. the run holds no pipeline-authored commits
// whose loss a fresh gate push could cause.
func RunHeadUnmoved(state State) bool {
	return state.Pipeline.SubmittedHead != "" && state.Pipeline.CurrentHead == state.Pipeline.SubmittedHead
}

// classifyCustodyReturned reports a branch whose stranded terminal run was
// explicitly recovered and never had a push binding: the operator owns the
// branch again and the only remaining step is starting a fresh run. The
// relation against the preserved pipeline head is informative only.
func (s *Service) classifyCustodyReturned(ctx context.Context, state *State) {
	state.State = StateCustodyReturned
	state.Safety = "custody_returned"
	state.Error = ""
	state.NextAction = &NextAction{Code: "run_pipeline", Command: `no-mistakes axi run --intent "<what the user set out to accomplish>"`}
	state.Relation = relationBetween(ctx, s.workDir(), state.Local.Head, state.Pipeline.CurrentHead)
}

// relationBetween classifies the local head against a target commit using only
// local object evidence; a target that is missing locally stays unknown.
func relationBetween(ctx context.Context, dir, local, target string) string {
	if local == "" || target == "" || !objectExists(ctx, dir, target) {
		return RelationUnknown
	}
	switch {
	case local == target:
		return RelationEqual
	case isAncestor(ctx, dir, local, target):
		return RelationBehind
	case isAncestor(ctx, dir, target, local):
		return RelationAhead
	default:
		return RelationDiverged
	}
}

func pushStepRunning(database *db.DB, runID string) bool {
	steps, err := database.GetStepsByRun(runID)
	if err != nil {
		return true
	}
	for _, step := range steps {
		if step.StepName == types.StepPush && (step.Status == types.StepStatusRunning || step.Status == types.StepStatusFixing) {
			return true
		}
	}
	return false
}

func objectExists(ctx context.Context, dir, sha string) bool {
	_, err := git.Run(ctx, dir, "cat-file", "-e", sha+"^{commit}")
	return err == nil
}

func isAncestor(ctx context.Context, dir, ancestor, descendant string) bool {
	if ancestor == "" || descendant == "" {
		return false
	}
	_, err := git.Run(ctx, dir, "merge-base", "--is-ancestor", ancestor, descendant)
	return err == nil
}

func samePath(a, b string) bool {
	resolve := func(path string) string {
		abs, _ := filepath.Abs(path)
		if evaluated, err := filepath.EvalSymlinks(abs); err == nil {
			return evaluated
		}
		return abs
	}
	return resolve(a) == resolve(b)
}

func targetKind(repo *db.Repo) string {
	if repo != nil && strings.TrimSpace(repo.ForkURL) != "" {
		return "fork"
	}
	return "upstream"
}

func normalizePRState(state *string) string {
	if state == nil || strings.TrimSpace(*state) == "" {
		return "unknown"
	}
	return strings.ToLower(strings.TrimSpace(*state))
}

func ptr(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}

func value(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}

func blockedPlan(state State, resultState, safety, message string) State {
	state.State = resultState
	state.Safety = safety
	state.Changed = false
	state.NextAction = nil
	state.Error = message
	return state
}

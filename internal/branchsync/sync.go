package branchsync

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/gatecontext"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/safeurl"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

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
	Recovery   *RecoveryEvidence
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

// RecoveryEvidence describes the exact preservation proof behind a recovery
// action. Bound archive recovery is deliberately keep-local-only: the archive
// preserves the divergent later head while custody returns at RequiredHead.
type RecoveryEvidence struct {
	Source        string
	RepositoryID  string
	RunID         string
	Branch        string
	RequiredHead  string
	PreservedHead string
	ArchiveRef    string
	KeepLocal     bool
	Proof         string
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
	DB      *db.DB
	Repo    *db.Repo
	WorkDir string
	GateDir string
	Paths   *paths.Paths

	// RemoteTimeout bounds each individual remote Git operation (ls-remote,
	// fetch) performed by Refresh/Apply. Production callers set it from the
	// operator's global config (config.GlobalConfig.BranchSyncRemoteTimeout,
	// key branch_sync_remote_timeout); it is never sourced from a repo's
	// .no-mistakes.yaml - RepoConfig has no matching field, so a pushed
	// branch cannot widen or narrow how long this service waits before
	// failing closed. Zero (the common case for a Service built without
	// explicitly setting it, including every test) falls back to
	// config.DefaultBranchSyncRemoteTimeout.
	RemoteTimeout time.Duration

	// Remote operation seams are service-local so tests can substitute
	// controlled fakes without changing process-wide state observed by
	// parallel tests. Nil uses the production git helpers.
	lsRemote    func(context.Context, string, string, string) (string, error)
	fetchRemote func(context.Context, string, string, string, string) error

	beforeApply               func()
	beforeGateReset           func()
	beforeRecoverWorktreeMove func()
	beforeRecoverBranchMove   func()
	afterRecoverBranchMove    func()
}

// remoteTimeout returns the bounded deadline budget for one remote
// operation: the configured RemoteTimeout when set, otherwise the package
// default.
func (s *Service) remoteTimeout() time.Duration {
	if s.RemoteTimeout > 0 {
		return s.RemoteTimeout
	}
	return config.DefaultBranchSyncRemoteTimeout
}

func (s *Service) runLsRemote(ctx context.Context, dir, remote, ref string) (string, error) {
	if s.lsRemote != nil {
		return s.lsRemote(ctx, dir, remote, ref)
	}
	return git.LsRemote(ctx, dir, remote, ref)
}

func (s *Service) runFetchRemote(ctx context.Context, dir, remote, branch, localRef string) error {
	if s.fetchRemote != nil {
		return s.fetchRemote(ctx, dir, remote, branch, localRef)
	}
	return git.FetchRemoteBranchToPrivateRef(ctx, dir, remote, branch, localRef)
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
	globalCfg, cfgErr := config.LoadGlobal(p.ConfigFile())
	if cfgErr != nil {
		database.Close()
		return nil, nil, cfgErr
	}
	return &Service{DB: database, Repo: repo, WorkDir: root, GateDir: p.RepoDir(repo.ID), Paths: p, RemoteTimeout: globalCfg.BranchSyncRemoteTimeout}, func() { _ = database.Close() }, nil
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

	// Each remote operation below gets its own bounded deadline derived from
	// ctx, rather than sharing one context across both calls: a slow-but-
	// successful ls-remote must not consume the fetch's budget, since that
	// would falsely classify a reachable target as offline (see the doc
	// comment on remoteTimeout). Parent cancellation still short-circuits
	// both, because context.WithTimeout always derives from its parent.
	lsRemoteCtx, lsRemoteCancel := context.WithTimeout(ctx, s.remoteTimeout())
	defer lsRemoteCancel()
	live, err := s.runLsRemote(lsRemoteCtx, s.workDir(), pushURL, state.Target.Ref)
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
	fetchCtx, fetchCancel := context.WithTimeout(ctx, s.remoteTimeout())
	defer fetchCancel()
	if err := s.runFetchRemote(fetchCtx, s.workDir(), pushURL, branch, privateRef); err != nil {
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

	checkCtx, cancel := context.WithTimeout(ctx, s.remoteTimeout())
	defer cancel()
	live, err := s.runLsRemote(checkCtx, s.workDir(), s.Repo.PushURL(), plan.Target.Ref)
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

// BindRecoveryArchive records one exact existing archive ref as recovery
// evidence for the currently selected terminal run. It never creates, moves,
// or deletes a Git ref. The record is useful only for the narrow keep-local
// recovery where the clean worktree remains at an exact submitted, reviewed,
// or successfully pushed head and the archived later head is divergent.
func (s *Service) BindRecoveryArchive(ctx context.Context, archiveRef string) State {
	if refusal, blocked := s.gateContextRefusal(ctx); blocked {
		return refusal
	}
	state, run, _ := s.inspect(ctx)
	if run == nil || state.State != StatePipelineOwned || !terminalRunStatus(run.Status) || !unpublishedPipelineHead(run) || run.CustodyReturnedAt != nil {
		return blockedPlan(state, state.State, "blocked_recover_archive_not_applicable", "an archive can be bound only to the selected terminal run that still owns an unpublished pipeline head; no files or refs were changed")
	}
	if run.TerminalHeadVerifiedAt == nil {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_archive_unverified_head", "the terminal run has no verified final-head evidence, so an archive cannot be bound; no files or refs were changed")
	}
	required, ok := archiveRequiredHead(state, run)
	if !ok {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_archive_required_head_mismatch", "the clean working branch is not at an exact submitted, reviewed, or successfully pushed head for this run; no files or refs were changed")
	}
	if !state.Local.Clean {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_archive_dirty", "the working tree must be clean before archive recovery evidence can be bound; no files or refs were changed")
	}
	if state.Relation != RelationDiverged || required == run.HeadSHA {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_archive_not_divergent", "archive-backed custody restoration applies only when the verified later head diverges from the exact required working head; no files or refs were changed")
	}

	record := &db.RecoveryArchive{
		OwnerRunID: run.ID, RepoID: s.Repo.ID, RunID: run.ID, Branch: run.Branch,
		RequiredHeadSHA: required, PreservedHeadSHA: run.HeadSHA, ArchiveRef: archiveRef,
	}
	records, err := s.DB.GetRecoveryArchivesByRun(run.ID)
	if err != nil {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_archive_record_unreadable", "recovery archive records could not be read; no files or refs were changed")
	}
	if len(records) > 0 {
		if len(records) == 1 && sameRecoveryArchive(records[0], record) {
			verified, _, _ := s.inspect(ctx)
			return verified
		}
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_archive_ambiguous", fmt.Sprintf("run %s already has %d recovery archive record(s); refusing to add another candidate; no files or refs were changed", run.ID, len(records)))
	}
	proof := s.verifyRecoveryArchiveRecord(ctx, &state, run, record)
	if !proof.available {
		if proof.evidence != nil {
			proof.evidence.Source = "archive_candidate"
		}
		return proof.apply(state)
	}
	if _, err := s.DB.RecordRecoveryArchive(*record); err != nil {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_archive_record_failed", fmt.Sprintf("the verified archive ref %s could not be recorded: %v; no files or refs were changed", record.ArchiveRef, err))
	}
	verified, _, _ := s.inspect(ctx)
	return verified
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
// (the terminally verified run head, pinned by its private recovery ref):
//
//	relation   worktree  default                        --keep-local
//	equal      any       anchor locally; return custody same
//	ahead      any       anchor locally; return custody same
//	behind     clean     strict fast-forward to P,      custody at local head;
//	                     then return custody            gate reset to it (CAS)
//	behind     dirty     refuse (commit/stash first)    custody at local head;
//	                                                    gate reset to it (CAS)
//	diverged,  clean     anchor the pre-recovery local  custody at local head;
//	P contains           head, then move to P with      gate reset to it (CAS)
//	all local            fail-closed ops; return custody
//	work
//	diverged,  dirty     refuse (commit/stash first)    custody at local head;
//	P contains                                          gate reset to it (CAS)
//	all local
//	work
//	diverged   any       refuse (anchor named, manual   custody at local head;
//	                     reconcile / rerun offered)     gate reset to it (CAS)
//	P missing  any       refuse                         custody at local head;
//	                                                    gate reset to it (CAS)
//
// The containment row exists because a cancelled validation routinely leaves P
// as a REBASE of the local branch onto a newer base: the same logical commits
// with new SHAs, so equality and ancestry alone see only divergence and
// escalated a case where nothing could be lost. The row applies only where
// preservedContainsLocalWork proves, by executable three-way merge, that P
// already carries every local change. That proof is deliberately narrow, and
// everything it cannot decide - including a rebase whose fix rounds also
// rewrote the operator's lines - falls through to the plain diverged refusal.
// No-data-loss outranks convenience here: when nothing can distinguish a
// deliberate pipeline fix from a dropped change, the operator decides.
//
// Fail-safe rules, in the same spirit as Refresh/Apply:
//   - An active run always refuses: only terminal runs are recoverable.
//   - The preserved commits must be provably safe before custody moves: when
//     already reachable from the local branch (equal/ahead), recovery pins the
//     private anchor ref refs/no-mistakes/recover/<runID> locally without
//     requiring gate access, but rejects a conflicting recovery ref when the
//     gate is available; otherwise the preserved head is verified through the
//     gate's run-specific recovery ref and fetched into that anchor. Legacy terminal
//     heads that still exist as unreferenced gate objects are anchored before
//     recovery continues. The branch ref may independently lag or advance.
//   - The only possible worktree mutation is a guarded move of a clean checked-out
//     branch: a strict fast-forward, or an anchored move to a proven-containing
//     head performed by Git operations that refuse on their own rather than by a
//     preceding observation (see recoverAdoptPreserved). When the operator explicitly keeps a behind or diverged local
//     head instead of taking P, --keep-local never touches the worktree and moves
//     the gate branch to the kept head with an atomic compare-and-swap, so a
//     concurrent gate push wins and recovery refuses. An independently moved
//     gate head is pinned first so that CAS never discards it.
//   - A bound archive can make that keep-local choice discoverable for a
//     divergent later P without authorizing P as the working result. Exactly one
//     append-only record must revalidate its repository, run, branch, exact
//     required and preserved heads, raw non-symbolic archive ref, and the gate's
//     run-specific recovery ref. Plain --recover refuses this source before any
//     ref write; only the reported --recover --keep-local action can use it.
//   - Anything unverifiable (an unverified recorded head, missing gate where
//     required, conflicting evidence, failed anchor write or fetch, or changed
//     assumptions) refuses with a reason. The sole exception is a verified head
//     proven absent from both the worktree and an accessible gate when
//     --keep-local explicitly discards those unavailable pipeline commits.
//     When any run in a stranded stack has such a missing head, the same preflight
//     validates every non-superseded run, anchors each available head, and stamps
//     the whole stack atomically. Plain --recover still refuses rather than
//     moving a branch that cannot anchor a real preserved head.
//
// Recovery ends with persisted custody-return stamps on the recovered run or
// stranded stack; inspection then reports custody_returned (never-pushed runs)
// or the ordinary classification against the last push binding (pushed runs),
// both pointing at run_pipeline as the next step. `no-mistakes rerun` remains the alternative
// exit that resumes validating the preserved head instead of taking it back.
func (s *Service) Recover(ctx context.Context, keepLocal bool) State {
	if refusal, blocked := s.gateContextRefusal(ctx); blocked {
		return refusal
	}
	state, run, _ := s.inspect(ctx)
	if run != nil && run.CustodyReturnedAt != nil {
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
	if keepLocal {
		runIDs, candidateHeads, anyMissing, allEligible := s.missingHeadKeepLocalRuns(ctx, &state, run)
		if anyMissing && !allEligible {
			return blockedPlan(state, StatePipelineOwned, "blocked_recover_manual_reconciliation", "a stranded run has unverified or conflicting recovery evidence; reconcile it before returning branch custody")
		}
		if anyMissing {
			return s.recoverKeepLocalAtCurrentHead(ctx, run, state, runIDs, candidateHeads)
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
		if gateHead != run.HeadSHA && !isAncestor(ctx, s.GateDir, run.HeadSHA, gateHead) {
			return blockedPlan(state, StatePipelineOwned, "blocked_recover_unverified_head", "the terminal run has no verified head and the gate head does not descend from the recorded head; no files or refs were changed")
		}
		if err := s.DB.UpdateRunStatusWithVerifiedHead(run.ID, run.Status, gateHead); err != nil {
			return blockedPlan(state, StatePipelineOwned, "blocked_recover_unverified_head", "the verified gate head could not be preserved; no files or refs were changed")
		}
		run.HeadSHA = gateHead
		verifiedAt := time.Now().Unix()
		run.TerminalHeadVerifiedAt = &verifiedAt
		state.Pipeline.CurrentHead = gateHead
		state.Relation = relationBetween(ctx, s.workDir(), state.Local.Head, gateHead)
	}

	wd := s.workDir()
	branch := state.Local.Branch
	local := state.Local.Head
	preserved := run.HeadSHA
	anchorRef := custody.RecoveryRef(run.ID)
	localAnchor := custody.RecoveryLocalRef(run.ID)
	gateDir := strings.TrimSpace(s.GateDir)
	gateAvailable := false
	if gateDir != "" {
		_, statErr := os.Stat(gateDir)
		gateAvailable = statErr == nil
	}

	if compatible, err := recoveryAnchorCompatible(ctx, wd, run.ID, preserved); err != nil || !compatible {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_anchor_mismatch", "the invoking worktree recovery ref conflicts with the recorded pipeline head; inspect both objects before returning custody; no files or refs were changed")
	}

	if anchoredLocal, err := git.Run(ctx, wd, "rev-parse", "--verify", localAnchor+"^{commit}"); err == nil && anchoredLocal != preserved && local == preserved && !state.Local.Clean {
		blocked := blockedPlan(state, StatePipelineOwned, "blocked_recover_incomplete_adoption", fmt.Sprintf("the branch reached the preserved pipeline head, but its worktree still differs from that head; the pre-recovery head remains anchored at %s; reconcile the worktree and re-run recovery; custody was not recorded", localAnchor))
		blocked.NextAction = &NextAction{Code: "inspect_worktree", Command: "git status"}
		return blocked
	}

	// A verified bound archive proves the divergent later head remains durable,
	// but never authorizes taking that head. Its only recovery is the existing
	// keep-local path at the exact required head recorded in the proof.
	source := s.recoverySourceAvailable(ctx, &state, run)
	if source.archiveClaimed {
		if !source.available {
			return source.apply(state)
		}
		if !keepLocal {
			blocked := blockedPlan(state, StatePipelineOwned, "blocked_recover_archive_requires_keep_local", fmt.Sprintf("the later pipeline head %s is preserved at %s, but it diverges from required head %s; run only the offered keep-local custody recovery; no files or refs were changed", preserved, source.archive.ArchiveRef, source.archive.RequiredHeadSHA))
			blocked.Recovery = source.evidence
			blocked.NextAction = &NextAction{Code: "recover_custody", Command: "no-mistakes axi sync --recover --keep-local"}
			return blocked
		}
		if !gateAvailable {
			blocked := blockedPlan(state, StatePipelineOwned, "blocked_recover_gate_unavailable", "no local gate is configured for this repository, so custody cannot be returned at the required archived-recovery head; no files or refs were changed")
			blocked.Recovery = source.evidence
			return blocked
		}
		gateHead, err := git.Run(ctx, gateDir, "rev-parse", "refs/heads/"+branch+"^{commit}")
		if err != nil {
			blocked := blockedPlan(state, StatePipelineOwned, "blocked_recover_gate_unavailable", fmt.Sprintf("the local gate no longer has branch %s, so custody cannot be returned at required head %s; no files or refs were changed", branch, source.archive.RequiredHeadSHA))
			blocked.Recovery = source.evidence
			return blocked
		}
		return s.recoverKeepLocalFromArchive(ctx, run, state, gateHead, source.archive)
	}

	if objectExists(ctx, wd, preserved) && (local == preserved || isAncestor(ctx, wd, preserved, local)) {
		if blocked, ok := s.anchorReachablePreserved(ctx, state, run.ID, preserved); !ok {
			return blocked
		}
		if gateAvailable {
			compatible, err := recoveryAnchorCompatible(ctx, gateDir, run.ID, preserved)
			if err != nil || !compatible {
				return blockedPlan(state, StatePipelineOwned, "blocked_recover_anchor_mismatch", "the run recovery ref in the local gate conflicts with the recorded pipeline head; inspect both objects before returning custody; no files or refs were changed")
			}
		}
		if keepLocal {
			return s.finishKeepLocalRecover(ctx, state, []string{run.ID})
		}
		return s.finishRecover(ctx, run, false)
	}

	if !gateAvailable {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_gate_unavailable", "no local gate is configured for this repository, so the preserved pipeline head cannot be imported; no files or refs were changed")
	}
	gateAnchor := custody.RecoveryRef(run.ID)
	gateAnchorAvailable := false
	gateAnchorTarget, gateAnchorExists, targetErr := git.ExactRefTarget(ctx, gateDir, gateAnchor)
	if targetErr != nil {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_anchor_mismatch", "the run recovery ref could not be inspected; inspect the recorded and live heads before returning custody; no files or refs were changed")
	}
	if gateAnchorExists {
		gateAnchored, err := git.Run(ctx, gateDir, "rev-parse", gateAnchor+"^{commit}")
		if err != nil {
			return blockedPlan(state, StatePipelineOwned, "blocked_recover_anchor_mismatch", fmt.Sprintf("the run recovery ref points at non-commit object %s instead of the recorded pipeline head %s; inspect both objects before returning custody; no files or refs were changed", gateAnchorTarget, preserved))
		}
		if gateAnchored != preserved {
			return blockedPlan(state, StatePipelineOwned, "blocked_recover_anchor_mismatch", fmt.Sprintf("the run recovery ref points at %s instead of the recorded pipeline head %s; inspect both heads before returning custody; no files or refs were changed", gateAnchored, preserved))
		}
		gateAnchorAvailable = true
	}
	if !gateAnchorAvailable {
		if !objectExists(ctx, gateDir, preserved) {
			return blockedPlan(state, StatePipelineOwned, "blocked_recover_preserved_head_missing", fmt.Sprintf("the recorded pipeline head %s is missing from the local gate; inspect the recorded and live heads before returning custody; no files or refs were changed", preserved))
		}
		if err := custody.PreserveRecoveryHead(ctx, gateDir, run.ID, preserved); err != nil {
			return blockedPlan(state, StatePipelineOwned, "blocked_recover_preserve_failed", "the recorded pipeline head exists but could not be anchored in the local gate; no files or worktree refs were changed")
		}
	}

	anchored := false
	if existing, anchorErr := git.Run(ctx, wd, "rev-parse", anchorRef+"^{commit}"); anchorErr == nil && existing == preserved {
		anchored = true
	}
	if !anchored {
		if fetchErr := git.FetchRemoteRef(ctx, wd, gateDir, gateAnchor, preserved); fetchErr != nil {
			return blockedPlan(state, StatePipelineOwned, "blocked_recover_preserve_failed", "the preserved pipeline commits could not be fetched from the local gate; no files or refs were changed")
		}
		if preserveErr := custody.PreserveRecoveryAnchor(ctx, wd, anchorRef, preserved); preserveErr != nil {
			return blockedPlan(state, StatePipelineOwned, "blocked_recover_anchor_mismatch", "the invoking worktree recovery ref conflicts with the recorded pipeline head; inspect both objects before returning custody; no files or refs were changed")
		}
	}

	switch {
	case local == preserved, isAncestor(ctx, wd, preserved, local):
		// Equal or ahead, discovered only after anchoring made the preserved
		// head comparable locally.
		if keepLocal {
			return s.finishKeepLocalRecover(ctx, state, []string{run.ID})
		}
		return s.finishRecover(ctx, run, false)
	case isAncestor(ctx, wd, local, preserved):
		if keepLocal {
			return s.recoverKeepLocalAtCurrentHead(ctx, run, state, []string{run.ID}, []string{run.HeadSHA})
		}
		if !state.Local.Clean {
			state.Relation = RelationBehind
			blocked := blockedPlan(state, StatePipelineOwned, "blocked_recover_dirty", fmt.Sprintf("the invoking worktree is not clean (%s); commit or stash first and re-run the recovery, or use --keep-local to return custody at the current head without moving the worktree; no files or refs were changed", state.Local.Reason))
			blocked.NextAction = &NextAction{Code: "inspect_worktree", Command: "git status"}
			return blocked
		}
		return s.recoverFastForward(ctx, run, state, preserved)
	default:
		if keepLocal {
			return s.recoverKeepLocalAtCurrentHead(ctx, run, state, []string{run.ID}, []string{run.HeadSHA})
		}
		if preservedContainsLocalWork(ctx, wd, local, preserved) {
			if !state.Local.Clean {
				state.Relation = RelationDiverged
				blocked := blockedPlan(state, StatePipelineOwned, "blocked_recover_dirty", fmt.Sprintf("the invoking worktree is not clean (%s); commit or stash first and re-run the recovery, or use --keep-local to return custody at the current head without moving the worktree; no files or refs were changed", state.Local.Reason))
				blocked.NextAction = &NextAction{Code: "inspect_worktree", Command: "git status"}
				return blocked
			}
			return s.recoverAdoptPreserved(ctx, run, state, preserved)
		}
		state.Relation = RelationDiverged
		blocked := blockedPlan(state, StatePipelineOwned, "blocked_recover_diverged", fmt.Sprintf("the local branch and the preserved pipeline head have diverged; the preserved commits are anchored at %s - reconcile manually and re-run the recovery, run `no-mistakes rerun` to resume validating the preserved head, or use --keep-local to keep the current head; no files or refs were changed", anchorRef))
		blocked.NextAction = &NextAction{Code: "inspect_and_reconcile_manually", Command: "git log --oneline --left-right HEAD..." + anchorRef}
		return blocked
	}
}

// recoverKeepLocal performs the explicit keep-local custody return: the
// worktree is never touched; the gate branch moves to the kept local head with
// an atomic compare-and-swap so a concurrent gate push refuses instead of
// being clobbered. The kept head's objects reach the gate through a gate-side
// fetch - never a push, which would fire the gate's receive hooks and start a
// pipeline run. The preserved head stays reachable through the ordinary
// recovery anchor. The archive-backed keep-local path lives in
// recoverKeepLocalFromArchive so each signature's callers stay intact.
func (s *Service) recoverKeepLocal(ctx context.Context, run *db.Run, state State, gateHead string, runIDs, candidateHeads []string) State {
	if s.beforeGateReset != nil {
		s.beforeGateReset()
	}
	if _, err := os.Stat(s.GateDir); err != nil {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_gate_unavailable", "the local gate became unavailable while custody was being returned; custody was not recorded")
	}
	if err := s.preserveKeepLocalCandidates(ctx, runIDs, candidateHeads); err != nil {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_preserve_failed", "the stranded run evidence changed or could not be preserved; custody was not recorded")
	}
	if gateHead != state.Local.Head {
		candidateGateHead := false
		for _, candidateHead := range candidateHeads {
			if gateHead == candidateHead {
				candidateGateHead = true
				break
			}
		}
		if gateHead != "" && !candidateGateHead {
			if !objectExists(ctx, s.GateDir, gateHead) {
				return blockedPlan(state, StatePipelineOwned, "blocked_recover_preserve_failed", "the independently moved gate head is unavailable and cannot be preserved; no files or branch refs were changed")
			}
			gateAnchor := custody.RecoveryGateRef(run.ID)
			compatible, err := recoveryGateAnchorCompatible(ctx, s.GateDir, run.ID, gateHead)
			if err != nil || !compatible {
				return blockedPlan(state, StatePipelineOwned, "blocked_recover_preserve_failed", "the independently moved gate head conflicts with the existing run recovery anchor; inspect both refs before returning custody; no files or branch refs were changed")
			}
			_, exists, err := git.ExactRefTarget(ctx, s.GateDir, gateAnchor)
			if err == nil && !exists {
				err = custody.PreserveRecoveryAnchor(ctx, s.GateDir, gateAnchor, gateHead)
			}
			if err != nil {
				return blockedPlan(state, StatePipelineOwned, "blocked_recover_preserve_failed", "the independently moved gate head could not be anchored before returning custody; no files or branch refs were changed")
			}
		}
		head, err := git.HeadSHA(ctx, s.workDir())
		if err != nil || head != state.Local.Head {
			return blockedPlan(state, StatePipelineOwned, "blocked_recover_assumptions_changed", "the local branch head changed while custody was being returned; no files or refs were changed")
		}
		stagingRef := "refs/no-mistakes/custody-return/" + run.ID
		if !objectExists(ctx, s.GateDir, state.Local.Head) {
			// The fetch source must be absolute: the command runs inside the gate
			// directory, where a relative invoking-worktree path would resolve to
			// the gate itself.
			source, err := filepath.Abs(s.workDir())
			if err != nil {
				return blockedPlan(state, StatePipelineOwned, "blocked_recover_assumptions_changed", "the invoking worktree path could not be resolved; no files or refs were changed")
			}
			if _, err := git.Run(ctx, s.GateDir, "fetch", "--no-tags", "--no-write-fetch-head", source, "+refs/heads/"+state.Local.Branch+":"+stagingRef); err != nil {
				return blockedPlan(state, StatePipelineOwned, "blocked_recover_assumptions_changed", "the kept local head could not be staged into the gate; no files or refs were changed")
			}
			staged, err := git.Run(ctx, s.GateDir, "rev-parse", stagingRef+"^{commit}")
			if err != nil || staged != state.Local.Head {
				_, _ = git.Run(ctx, s.GateDir, "update-ref", "-d", stagingRef)
				return blockedPlan(state, StatePipelineOwned, "blocked_recover_assumptions_changed", "the local branch head changed while custody was being returned; no files or refs were changed")
			}
		}
		oldValue := gateHead
		if oldValue == "" {
			oldValue = strings.Repeat("0", len(state.Local.Head))
		}
		_, casErr := git.Run(ctx, s.GateDir, "update-ref", "refs/heads/"+state.Local.Branch, state.Local.Head, oldValue)
		_, _ = git.Run(ctx, s.GateDir, "update-ref", "-d", stagingRef)
		if casErr != nil {
			return blockedPlan(state, StatePipelineOwned, "blocked_recover_gate_race", "the gate branch changed while custody was being returned; re-run the recovery; no local files or refs were changed")
		}
	}
	if err := s.preserveKeepLocalCandidates(ctx, runIDs, candidateHeads); err != nil {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_preserve_failed", "the stranded run evidence changed or could not be preserved; custody was not recorded")
	}
	branch, branchErr := git.CurrentBranch(ctx, s.workDir())
	head, headErr := git.HeadSHA(ctx, s.workDir())
	if branchErr != nil || branch != state.Local.Branch || headErr != nil || head != state.Local.Head {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_assumptions_changed", "the local branch or head changed while custody was being returned; custody was not recorded")
	}
	if _, err := os.Stat(s.GateDir); err != nil {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_gate_unavailable", "the local gate became unavailable while custody was being returned; custody was not recorded")
	}
	liveGateHead, exists, err := git.ExactRefTarget(ctx, s.GateDir, "refs/heads/"+state.Local.Branch)
	if err != nil || !exists || liveGateHead != state.Local.Head {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_gate_race", "the gate branch changed while custody was being returned; custody was not recorded")
	}
	return s.finishKeepLocalRecover(ctx, state, runIDs)
}

// recoverKeepLocalAtCurrentHead returns custody at the current local head
// without requiring a preserved pipeline object. --keep-local is the
// operator's explicit discard of unpublished pipeline commits, so an absent
// gate branch does not block it.
func (s *Service) recoverKeepLocalAtCurrentHead(ctx context.Context, run *db.Run, state State, runIDs, candidateHeads []string) State {
	gateDir := strings.TrimSpace(s.GateDir)
	if gateDir == "" {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_gate_unavailable", "no local gate is configured for this repository; custody was not recorded")
	}
	if _, err := os.Stat(gateDir); err != nil {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_gate_unavailable", "the local gate is unavailable; custody was not recorded")
	}
	gateHead, exists, err := git.ExactRefTarget(ctx, gateDir, "refs/heads/"+state.Local.Branch)
	if err != nil {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_gate_unavailable", fmt.Sprintf("the local gate branch %s could not be inspected; no files or refs were changed", state.Local.Branch))
	}
	if !exists {
		return s.recoverKeepLocal(ctx, run, state, "", runIDs, candidateHeads)
	}
	return s.recoverKeepLocal(ctx, run, state, gateHead, runIDs, candidateHeads)
}

// recoverKeepLocalFromArchive is the keep-local-only archive path: it
// revalidates the bound archive at the recovery boundary, never selects or
// replays the archived head, and rolls the gate branch back if custody
// stamping fails after the gate already moved.
func (s *Service) recoverKeepLocalFromArchive(ctx context.Context, run *db.Run, state State, gateHead string, archive *db.RecoveryArchive) State {
	if s.beforeGateReset != nil {
		s.beforeGateReset()
	}
	freshRun, err := s.DB.GetRun(run.ID)
	if err != nil || freshRun == nil {
		blocked := blockedPlan(state, StatePipelineOwned, "blocked_recover_assumptions_changed", "the terminal run could not be reloaded while archived custody was being returned; no files or refs were changed")
		blocked.Recovery = state.Recovery
		return blocked
	}
	proof := s.verifyBoundRecoveryArchive(ctx, &state, freshRun)
	if !proof.available || proof.archive == nil || archive == nil || proof.archive.ID != archive.ID {
		return proof.apply(state)
	}
	run = freshRun
	branch, branchErr := git.CurrentBranch(ctx, s.workDir())
	head, headErr := git.HeadSHA(ctx, s.workDir())
	clean, _ := worktreeClean(ctx, s.workDir())
	if branchErr != nil || branch != archive.Branch || headErr != nil || head != archive.RequiredHeadSHA || !clean {
		blocked := blockedPlan(state, StatePipelineOwned, "blocked_recover_assumptions_changed", "the required local branch, head, or clean-worktree proof changed while archived custody was being returned; no files or refs were changed")
		blocked.Recovery = proof.evidence
		return blocked
	}
	state.Recovery = proof.evidence
	gateMoved := false
	if gateHead != state.Local.Head {
		if gateHead != run.HeadSHA {
			gateAnchor := custody.RecoveryGateRef(run.ID)
			existing, exists, err := git.ExactRefTarget(ctx, s.GateDir, gateAnchor)
			if err != nil || (exists && existing != gateHead) {
				return blockedPlan(state, StatePipelineOwned, "blocked_recover_preserve_failed", "the independently moved gate head conflicts with the existing run recovery anchor; inspect both refs before returning custody; no files or branch refs were changed")
			}
			if !exists {
				err = custody.PreserveRecoveryAnchor(ctx, s.GateDir, gateAnchor, gateHead)
			}
			if err != nil {
				return blockedPlan(state, StatePipelineOwned, "blocked_recover_preserve_failed", "the independently moved gate head could not be anchored before returning custody; no files or branch refs were changed")
			}
		}
		head, err := git.HeadSHA(ctx, s.workDir())
		if err != nil || head != state.Local.Head {
			return blockedPlan(state, StatePipelineOwned, "blocked_recover_assumptions_changed", "the local branch head changed while custody was being returned; no files or refs were changed")
		}
		source, err := filepath.Abs(s.workDir())
		if err != nil {
			return blockedPlan(state, StatePipelineOwned, "blocked_recover_assumptions_changed", "the invoking worktree path could not be resolved; no files or refs were changed")
		}
		stagingRef := "refs/no-mistakes/custody-return/" + run.ID
		if _, err := git.Run(ctx, s.GateDir, "fetch", "--no-tags", "--no-write-fetch-head", source, "+refs/heads/"+state.Local.Branch+":"+stagingRef); err != nil {
			return blockedPlan(state, StatePipelineOwned, "blocked_recover_assumptions_changed", "the kept local head could not be staged into the gate; no files or refs were changed")
		}
		staged, err := git.Run(ctx, s.GateDir, "rev-parse", stagingRef+"^{commit}")
		if err != nil || staged != state.Local.Head {
			_, _ = git.Run(ctx, s.GateDir, "update-ref", "-d", stagingRef)
			return blockedPlan(state, StatePipelineOwned, "blocked_recover_assumptions_changed", "the local branch head changed while custody was being returned; no files or refs were changed")
		}
		_, casErr := git.Run(ctx, s.GateDir, "update-ref", "refs/heads/"+state.Local.Branch, state.Local.Head, gateHead)
		_, _ = git.Run(ctx, s.GateDir, "update-ref", "-d", stagingRef)
		if casErr != nil {
			return blockedPlan(state, StatePipelineOwned, "blocked_recover_gate_race", "the gate branch changed while custody was being returned; re-run the recovery; no local files or refs were changed")
		}
		gateMoved = true
	}
	result := s.finishRecover(ctx, run, false)
	if gateMoved && !result.Recovered {
		branchRef := "refs/heads/" + state.Local.Branch
		if _, err := git.Run(context.WithoutCancel(ctx), s.GateDir, "update-ref", branchRef, gateHead, state.Local.Head); err != nil {
			result.Error += fmt.Sprintf("; rollback could not restore gate branch %s from %s to %s", branchRef, state.Local.Head, gateHead)
		} else {
			result.Error += "; the gate branch was restored to its exact pre-recovery head"
		}
	}
	return result
}

// recoverFastForward advances the clean checked-out branch to the preserved
// pipeline head with the same strict fast-forward and honesty rules as Apply.
func (s *Service) recoverFastForward(ctx context.Context, run *db.Run, state State, preserved string) State {
	if s.beforeRecoverWorktreeMove != nil {
		s.beforeRecoverWorktreeMove()
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
	return s.finishRecover(ctx, run, true)
}

// preservedContainsLocalWork proves the preserved pipeline head already carries
// every change the local branch has, so adopting it discards no work.
//
// The proof is an executable three-way merge, never a patch-identity hash.
// Patch IDs discard hunk locations and whitespace, so they cannot tell a
// genuine replay from a same-shaped edit to a different identical block; a
// containment claim built on them is not a proof, and this path exists only to
// protect people's unlanded code. Merging the local branch into the preserved
// head and requiring the result to be exactly the preserved head's tree is
// decidable and content-exact: if the local branch had anything the preserved
// head lacks, the merged tree differs and the answer is no.
//
// The merge base is the only sound anchor. Only a commit provably reachable
// from BOTH heads makes the diff base..local mean exactly "the local branch's
// own work"; runs.base_sha cannot be used, because it is the previous gate head
// and for a re-pushed branch carries pipeline commits the local branch never
// had.
//
// The predicate is one-directional: it answers only "would adopting the
// preserved head lose local work", never "are the two heads interchangeable".
// It is deliberately narrow, and every unreadable, conflicting, or ambiguous
// case returns false so the plain diverged refusal escalates. That is the
// intended trade: an ordinary rebase whose content is carried forward intact
// recovers automatically, while a rebase that also rewrote the operator's lines
// - where nothing can distinguish a deliberate pipeline fix from a dropped
// change - stays a decision for the operator.
func preservedContainsLocalWork(ctx context.Context, dir, local, preserved string) bool {
	if local == "" || preserved == "" || local == preserved {
		return false
	}
	base, err := git.Run(ctx, dir, "merge-base", local, preserved)
	if err != nil || base == "" {
		return false
	}
	return mergeTreePreservesFinalHead(ctx, dir, base, local, preserved)
}

// recoverAdoptPreserved returns custody for a preserved pipeline head that
// already carries every local change. The local commits are represented in the
// preserved head, but their exact SHAs are not reachable from it, so the move is
// not a fast-forward and the pre-recovery local head is anchored first.
//
// The move itself must fail closed. An observation of branch, HEAD, and
// cleanliness followed by an unconditional `reset --hard` is check-then-act:
// anything landing in the gap is destroyed, and no amount of re-observation
// closes it, because the check and the mutation are separate commands. So the
// two Git operations that perform the move carry the guard in themselves, in
// the spirit of `merge --ff-only`:
//
//   - `update-ref <branch> <preserved> <observed>` is an atomic compare-and-swap.
//     A concurrent commit moved the branch, so the swap refuses and nothing at
//     all has been touched.
//   - `read-tree -m -u <observed> <preserved>` refuses to overwrite a modified
//     or untracked working-tree file. A concurrent edit to a file this move
//     would rewrite aborts it before any file changes; an edit to a file the
//     move does not touch is simply carried across. When it refuses, the branch
//     swap is rolled back by the same compare-and-swap in reverse.
//
// A crash between the two leaves the branch at the preserved head with the
// working tree still holding the pre-recovery content, which reads as ordinary
// uncommitted changes and loses nothing: containment was proven before the move
// and the pre-recovery head stays anchored. Custody is stamped only after the
// whole move is verified.
func (s *Service) recoverAdoptPreserved(ctx context.Context, run *db.Run, state State, preserved string) State {
	if s.beforeRecoverWorktreeMove != nil {
		s.beforeRecoverWorktreeMove()
	}
	wd := s.workDir()
	branch, branchErr := git.CurrentBranch(ctx, wd)
	head, headErr := git.HeadSHA(ctx, wd)
	clean, _ := worktreeClean(ctx, wd)
	if branchErr != nil || branch != state.Local.Branch || headErr != nil || head != state.Local.Head || !clean {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_assumptions_changed", "the local branch or worktree changed while custody was being returned; no files or refs were changed")
	}
	// The containment proof runs before the anchor and the move so that no
	// slow work sits between the last guard and the mutation.
	if !preservedContainsLocalWork(ctx, wd, head, preserved) {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_assumptions_changed", "the containment proof changed while custody was being returned; no files or refs were changed")
	}
	localAnchor := recoverLocalAnchorRef(run.ID)
	// Create-only: an empty old value requires the ref not to exist. A resumed
	// recovery legitimately finds its own anchor already at this head; an anchor
	// at any other commit is unexplained and refuses.
	existingAnchor, existingErr := git.Run(ctx, wd, "rev-parse", "--verify", localAnchor+"^{commit}")
	if existingErr == nil && existingAnchor != head {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_preserve_failed", "the pre-recovery local head could not be anchored; no files or refs were changed")
	}
	if existingErr != nil {
		if err := custody.PreserveRecoveryAnchor(ctx, wd, localAnchor, head); err != nil {
			return blockedPlan(state, StatePipelineOwned, "blocked_recover_preserve_failed", "the pre-recovery local head could not be anchored; no files or refs were changed")
		}
	}
	if anchored, err := git.Run(ctx, wd, "rev-parse", localAnchor+"^{commit}"); err != nil || anchored != head {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_preserve_failed", "the pre-recovery local head could not be verified after anchoring; no files or worktree refs were changed")
	}

	if s.beforeRecoverBranchMove != nil {
		s.beforeRecoverBranchMove()
	}
	branchRef := "refs/heads/" + state.Local.Branch
	boundaryBranch, boundaryErr := git.CurrentBranch(ctx, wd)
	if boundaryErr != nil || boundaryBranch != state.Local.Branch {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_assumptions_changed", "the checked-out branch changed while custody was being returned; no branch or worktree changes were made")
	}
	if _, err := git.Run(ctx, wd, "update-ref", branchRef, preserved, head); err != nil {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_assumptions_changed", "the local branch moved while custody was being returned; no files or refs were changed")
	}
	if s.afterRecoverBranchMove != nil {
		s.afterRecoverBranchMove()
	}
	// KNOWN BOUNDED FUNDAMENTAL-GIT LIMITATION: a concurrent git checkout
	// landing between this branch-identity verification and the read-tree
	// working-tree update can apply the preserved tree to another branch's
	// worktree. This is not data loss: containment is proven before the move,
	// the pre-recovery head stays anchored at
	// refs/no-mistakes/recover-local/<run>, custody is never stamped, and the
	// operation fails closed to a reported failure rather than a false success.
	// The window is sub-millisecond and inside a worktree the pipeline already
	// owns. It is irreducible because no single Git operation carries both
	// guards, and no lock git checkout honors can be held across the two
	// commands, so a further observation cannot close it. Keep this verification
	// even though it cannot make the two commands atomic.
	boundaryBranch, boundaryErr = git.CurrentBranch(ctx, wd)
	if boundaryErr != nil || boundaryBranch != state.Local.Branch {
		rollbackDetail := ""
		if _, rollbackErr := git.Run(ctx, wd, "update-ref", branchRef, head, preserved); rollbackErr != nil {
			rollbackDetail = fmt.Sprintf("; the branch could not be restored to %s and still requires manual reconciliation", head)
		}
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_assumptions_changed", fmt.Sprintf("the checked-out branch changed while custody was being returned%s; custody was not recorded", rollbackDetail))
	}
	if _, err := git.Run(ctx, wd, "read-tree", "-m", "-u", head, preserved); err != nil {
		rolledBack := ""
		if _, rollbackErr := git.Run(ctx, wd, "update-ref", branchRef, head, preserved); rollbackErr != nil {
			rolledBack = fmt.Sprintf("; the branch could not be restored to %s and now points at %s, whose content the pre-recovery head is contained in", head, preserved)
		}
		blocked := blockedPlan(state, StatePipelineOwned, "blocked_recover_worktree_busy", fmt.Sprintf("the working tree changed while custody was being returned, so no file was overwritten%s; re-run the recovery once the working tree is settled", rolledBack))
		blocked.NextAction = &NextAction{Code: "inspect_worktree", Command: "git status"}
		return blocked
	}

	finalHead, _ := git.HeadSHA(ctx, wd)
	finalClean, finalReason := worktreeClean(ctx, wd)
	state.Local.Head = finalHead
	state.Local.Clean = finalClean
	state.Local.Reason = finalReason
	state.Changed = finalHead == preserved && finalHead != head
	if finalHead != preserved {
		blocked := blockedPlan(state, StatePipelineOwned, "blocked_recover_apply_failed", fmt.Sprintf("adopting the preserved pipeline head did not reach it; final HEAD is %s and the pre-recovery head is anchored at %s; inspect the worktree before retrying", finalHead, localAnchor))
		blocked.NextAction = &NextAction{Code: "inspect_worktree", Command: "git status"}
		return blocked
	}
	if !finalClean {
		state.State = StateDirty
		state.Relation = RelationEqual
		state.Safety = "blocked_post_recover_" + finalReason
		state.Error = fmt.Sprintf("HEAD reached the preserved pipeline head, but the worktree is not clean; nothing was overwritten and the pre-recovery head is anchored at %s; custody was not recorded", localAnchor)
		state.NextAction = &NextAction{Code: "inspect_worktree", Command: "git status"}
		return state
	}
	return s.finishRecover(ctx, run, true)
}

func (s *Service) anchorReachablePreserved(ctx context.Context, state State, runID, preserved string) (State, bool) {
	if err := custody.PreserveRecoveryHead(ctx, s.workDir(), runID, preserved); err != nil {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_preserve_failed", "the preserved pipeline commits could not be anchored locally; no files or refs were changed"), false
	}
	anchorRef := custody.RecoveryRef(runID)
	if anchored, err := git.Run(ctx, s.workDir(), "rev-parse", anchorRef+"^{commit}"); err != nil || anchored != preserved {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_preserve_failed", "the preserved pipeline commits could not be anchored locally; no files or refs were changed"), false
	}
	return State{}, true
}

// finishRecover stamps custody returned and reports the fresh post-recovery
// truth. changed reports whether this call moved the worktree HEAD.
func (s *Service) finishRecover(ctx context.Context, run *db.Run, changed bool) State {
	if err := s.DB.SetRunCustodyReturned(run.ID); err != nil {
		state, _, _ := s.inspect(ctx)
		state.Changed = changed
		state.Safety = "blocked_recover_stamp_failed"
		state.Error = "the custody return could not be recorded; re-run the recovery"
		state.NextAction = nil
		return state
	}
	state, _, _ := s.inspect(ctx)
	state.Recovered = true
	state.Changed = changed
	return state
}

// finishKeepLocalRecover stamps custody returned on the preflighted runs.
func (s *Service) preserveKeepLocalCandidates(ctx context.Context, runIDs, candidateHeads []string) error {
	if len(runIDs) != len(candidateHeads) {
		return fmt.Errorf("invalid keep-local candidate set")
	}
	for i, runID := range runIDs {
		candidate, err := s.DB.GetRun(runID)
		if err != nil || candidate == nil || candidate.HeadSHA != candidateHeads[i] || candidate.TerminalHeadVerifiedAt == nil || !terminalRunStatus(candidate.Status) || !unpublishedPipelineHead(candidate) {
			return fmt.Errorf("run %s changed after preflight", runID)
		}
		if compatible, err := recoveryAnchorCompatible(ctx, s.workDir(), runID, candidate.HeadSHA); err != nil || !compatible {
			return fmt.Errorf("run %s has conflicting worktree recovery evidence", runID)
		}
		if compatible, err := recoveryAnchorCompatible(ctx, s.GateDir, runID, candidate.HeadSHA); err != nil || !compatible {
			return fmt.Errorf("run %s has conflicting gate recovery evidence", runID)
		}
		if objectExists(ctx, s.workDir(), candidate.HeadSHA) {
			if err := custody.PreserveRecoveryAnchor(ctx, s.workDir(), custody.RecoveryRef(runID), candidate.HeadSHA); err != nil {
				return err
			}
		}
		if objectExists(ctx, s.GateDir, candidate.HeadSHA) {
			if err := custody.PreserveRecoveryHead(ctx, s.GateDir, runID, candidate.HeadSHA); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Service) finishKeepLocalRecover(ctx context.Context, state State, runIDs []string) State {
	if err := s.DB.SetRunsCustodyReturned(runIDs); err != nil {
		fresh, _, _ := s.inspect(ctx)
		fresh.Changed = false
		fresh.Safety = "blocked_recover_stamp_failed"
		fresh.Error = "the custody return could not be recorded; re-run the recovery"
		fresh.NextAction = nil
		return fresh
	}
	fresh, _, _ := s.inspect(ctx)
	fresh.Recovered = true
	fresh.Changed = false
	return fresh
}

func recoverAnchorRef(runID string) string {
	return custody.RecoveryRef(runID)
}

// recoverLocalAnchorRef keeps the exact pre-recovery local commits reachable
// when custody is returned by adopting an equivalent preserved head, which
// leaves those SHAs unreferenced by the branch.
func recoverLocalAnchorRef(runID string) string {
	return custody.RecoveryLocalRef(runID)
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
	return status.Terminal()
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
		_, _, anyMissing, allEligible := s.missingHeadKeepLocalRuns(ctx, state, run)
		if anyMissing {
			if allEligible {
				state.Safety = "blocked_recover_preserved_head_missing"
				state.Error = "a stranded run's recorded pipeline head is not available in the invoking worktree or local gate; recover custody by keeping the current local head, which discards the missing preserved commits"
				state.NextAction = &NextAction{Code: "recover_custody", Command: "no-mistakes axi sync --recover --keep-local"}
				return
			}
			state.Safety = "blocked_recover_manual_reconciliation"
			state.Error = "a stranded run has missing, unverified, or conflicting recovery evidence; inspect and reconcile the recorded and live heads manually"
			state.NextAction = &NextAction{Code: "inspect_and_reconcile_manually", Command: "no-mistakes axi status"}
			return
		}
		source := s.recoverySourceAvailable(ctx, state, run)
		if !source.available {
			// A claimed archive owns its fail-closed proof codes. Ordinary
			// discovery failures stay #958's inspect-and-reconcile offer so a
			// dirty, diverged, unverified, or conflicting gate is never
			// advertised as a missing-head keep-local or archive recovery.
			if source.archiveClaimed {
				*state = source.apply(*state)
				return
			}
			state.Safety = "blocked_recover_manual_reconciliation"
			state.Error = "the run finished " + string(run.Status) + " but its preserved recovery evidence cannot be used safely; inspect and reconcile the recorded and live heads manually"
			state.NextAction = &NextAction{Code: "inspect_and_reconcile_manually", Command: "no-mistakes axi status"}
			return
		}
		state.Safety = "blocked_pipeline_owned_recoverable"
		state.Recovery = source.evidence
		if source.archive != nil {
			state.Error = fmt.Sprintf("the run finished %s with divergent later work preserved at verified archive %s; recover custody at exact required head %s before any local follow-up commit", run.Status, source.archive.ArchiveRef, source.archive.RequiredHeadSHA)
		} else {
			state.Error = "the run finished " + string(run.Status) + " with unpublished pipeline commits preserved in the local gate; recover custody before any local follow-up commit"
		}
		action := source.action
		state.NextAction = &action
		return
	}
	state.Safety = "blocked_pipeline_owned"
	state.Error = activeMessage
	state.NextAction = &NextAction{Code: "continue_active_run", Command: "no-mistakes axi status"}
}

type recoverySourceProof struct {
	available      bool
	archiveClaimed bool
	action         NextAction
	evidence       *RecoveryEvidence
	archive        *db.RecoveryArchive
	safety         string
	err            string
}

func (proof recoverySourceProof) apply(state State) State {
	state.State = StatePipelineOwned
	state.Safety = proof.safety
	if state.Safety == "" {
		state.Safety = "blocked_recover_manual_reconciliation"
	}
	state.Error = proof.err
	if state.Error == "" {
		state.Error = "the terminal run's recorded pipeline head has no verified recovery source; inspect and reconcile the recorded and live heads manually; no files or refs were changed"
	}
	state.Recovery = proof.evidence
	state.NextAction = &NextAction{Code: "inspect_and_reconcile_manually", Command: "no-mistakes axi status"}
	return state
}

func (s *Service) missingHeadKeepLocalRuns(ctx context.Context, state *State, run *db.Run) ([]string, []string, bool, bool) {
	if state == nil || run == nil {
		return nil, nil, false, false
	}
	runs, err := s.DB.GetRunsByRepo(s.Repo.ID)
	if err != nil {
		return nil, nil, false, false
	}
	gateDir := strings.TrimSpace(s.GateDir)
	if gateDir == "" {
		return nil, nil, false, false
	}
	if _, err := os.Stat(gateDir); err != nil {
		return nil, nil, false, false
	}
	gateHead, gateHeadExists, err := git.ExactRefTarget(ctx, gateDir, "refs/heads/"+state.Local.Branch)
	if err != nil {
		return nil, nil, false, false
	}
	var runIDs []string
	var candidateHeads []string
	anyMissing := false
	allEligible := true
	var newerPushed *db.Run
	for _, candidate := range runs {
		if candidate.Branch != state.Local.Branch {
			continue
		}
		if unpublishedPipelineHead(candidate) && s.supersededUnpublishedRun(ctx, candidate, newerPushed, state.Local.Branch) {
			continue
		}
		if newerPushed == nil && exactPushedBinding(s.Repo, candidate, state.Local.Branch) {
			newerPushed = candidate
		}
		if !terminalRunStatus(candidate.Status) || !unpublishedPipelineHead(candidate) {
			continue
		}
		eligible := candidate.TerminalHeadVerifiedAt != nil && strings.TrimSpace(candidate.HeadSHA) != ""
		missing := eligible && !objectExists(ctx, s.workDir(), candidate.HeadSHA) && !objectExists(ctx, gateDir, candidate.HeadSHA)
		if eligible {
			compatible, err := recoveryAnchorCompatible(ctx, s.workDir(), candidate.ID, candidate.HeadSHA)
			eligible = err == nil && compatible
		}
		if eligible {
			compatible, err := recoveryAnchorCompatible(ctx, gateDir, candidate.ID, candidate.HeadSHA)
			eligible = err == nil && compatible
		}
		anyMissing = anyMissing || missing
		allEligible = allEligible && eligible
		runIDs = append(runIDs, candidate.ID)
		candidateHeads = append(candidateHeads, candidate.HeadSHA)
	}
	if gateHeadExists && gateHead != state.Local.Head {
		candidateGateHead := false
		for _, candidateHead := range candidateHeads {
			if gateHead == candidateHead {
				candidateGateHead = true
				break
			}
		}
		if !candidateGateHead {
			allEligible = allEligible && objectExists(ctx, gateDir, gateHead)
			if allEligible {
				compatible, err := recoveryGateAnchorCompatible(ctx, gateDir, run.ID, gateHead)
				allEligible = err == nil && compatible
			}
		}
	}
	return runIDs, candidateHeads, anyMissing, allEligible
}

func unavailableRecoverySource(safety, message string) recoverySourceProof {
	return recoverySourceProof{safety: safety, err: message}
}

// recoverySourceAvailable is the single owner of preserved-head discovery and
// recovery classification. It first evaluates the ordinary run-specific
// recovery anchor path. A bound archive can add only one narrower result: keep
// the exact required local head while the divergent later head stays archived.
func (s *Service) recoverySourceAvailable(ctx context.Context, state *State, run *db.Run) recoverySourceProof {
	if state == nil || run == nil || strings.TrimSpace(run.HeadSHA) == "" {
		return unavailableRecoverySource("blocked_recover_unverified_head", "the terminal run has no recorded pipeline head to verify; no files or refs were changed")
	}

	archiveRecords, err := s.DB.GetRecoveryArchivesByRun(run.ID)
	if err != nil {
		proof := unavailableRecoverySource("blocked_recover_archive_record_unreadable", "recovery archive records could not be read; no files or refs were changed")
		proof.archiveClaimed = true
		return proof
	}
	var archiveProof recoverySourceProof
	if len(archiveRecords) > 0 {
		archiveProof = s.verifyBoundRecoveryArchiveRecords(ctx, state, run, archiveRecords)
		if !archiveProof.available {
			return archiveProof
		}
	}

	localAnchor := custody.RecoveryRef(run.ID)
	if symbolic, err := git.Run(ctx, s.workDir(), "symbolic-ref", "-q", localAnchor); err == nil && symbolic != "" {
		return unavailableRecoverySource("blocked_recover_anchor_mismatch", fmt.Sprintf("the invoking worktree recovery ref %s is symbolic to %s; no files or refs were changed", localAnchor, symbolic))
	}
	_, localAnchorExists, err := git.ExactRefTarget(ctx, s.workDir(), localAnchor)
	if err != nil {
		return unavailableRecoverySource("blocked_recover_anchor_mismatch", fmt.Sprintf("the invoking worktree recovery ref %s could not be inspected; no files or refs were changed", localAnchor))
	}
	if localAnchorExists {
		anchored, err := git.Run(ctx, s.workDir(), "rev-parse", localAnchor+"^{commit}")
		if err != nil || anchored != run.HeadSHA {
			return unavailableRecoverySource("blocked_recover_anchor_mismatch", fmt.Sprintf("the invoking worktree recovery ref %s does not point at recorded pipeline head %s; no files or refs were changed", localAnchor, run.HeadSHA))
		}
	}

	local := state.Local.Head
	preserved := run.HeadSHA
	localEligible := localRecoveryEligible(ctx, s.workDir(), state, run)
	if localEligible {
		localPreRecovery := custody.RecoveryLocalRef(run.ID)
		if anchored, err := git.Run(ctx, s.workDir(), "rev-parse", "--verify", localPreRecovery+"^{commit}"); err == nil && anchored != preserved && local == preserved && !state.Local.Clean {
			return unavailableRecoverySource("blocked_recover_incomplete_adoption", fmt.Sprintf("the branch reached preserved head %s with a conflicting pre-recovery anchor at %s; no files or refs were changed", preserved, localPreRecovery))
		}
	}

	gateDir := strings.TrimSpace(s.GateDir)
	gateAvailable := gateDir != ""
	if gateAvailable {
		_, err = os.Stat(gateDir)
		gateAvailable = err == nil
	}
	if gateAvailable {
		compatible, err := recoveryAnchorCompatible(ctx, gateDir, run.ID, preserved)
		if err != nil || !compatible {
			return unavailableRecoverySource("blocked_recover_anchor_mismatch", fmt.Sprintf("the local gate recovery ref %s conflicts with recorded pipeline head %s; no files or refs were changed", custody.RecoveryRef(run.ID), preserved))
		}
	}

	ordinaryAvailable := localEligible
	if !ordinaryAvailable && gateAvailable && objectExists(ctx, gateDir, preserved) && state.Local.Clean && objectExists(ctx, gateDir, local) {
		ordinaryAvailable = isAncestor(ctx, gateDir, local, preserved) || preservedContainsLocalWork(ctx, gateDir, local, preserved)
	}
	if ordinaryAvailable {
		return recoverySourceProof{
			available: true,
			action:    NextAction{Code: "recover_custody", Command: "no-mistakes axi sync --recover"},
		}
	}
	if archiveProof.available {
		return archiveProof
	}
	return unavailableRecoverySource(
		"blocked_recover_manual_reconciliation",
		"the run finished "+string(run.Status)+" but its preserved recovery evidence cannot be used safely; inspect and reconcile the recorded and live heads manually",
	)
}

func localRecoveryEligible(ctx context.Context, wd string, state *State, run *db.Run) bool {
	return objectExists(ctx, wd, run.HeadSHA) &&
		(state.Local.Head == run.HeadSHA || isAncestor(ctx, wd, run.HeadSHA, state.Local.Head))
}

func (s *Service) verifyBoundRecoveryArchive(ctx context.Context, state *State, run *db.Run) recoverySourceProof {
	records, err := s.DB.GetRecoveryArchivesByRun(run.ID)
	if err != nil {
		proof := unavailableRecoverySource("blocked_recover_archive_record_unreadable", "recovery archive records could not be read; no files or refs were changed")
		proof.archiveClaimed = true
		return proof
	}
	return s.verifyBoundRecoveryArchiveRecords(ctx, state, run, records)
}

func (s *Service) verifyBoundRecoveryArchiveRecords(ctx context.Context, state *State, run *db.Run, records []*db.RecoveryArchive) recoverySourceProof {
	if len(records) == 0 {
		proof := unavailableRecoverySource("blocked_recover_archive_record_missing", fmt.Sprintf("the recovery archive record for run %s is missing; no files or refs were changed", run.ID))
		proof.archiveClaimed = true
		return proof
	}
	if len(records) != 1 {
		proof := unavailableRecoverySource("blocked_recover_archive_ambiguous", fmt.Sprintf("run %s has %d recovery archive records; exactly one is required; no files or refs were changed", run.ID, len(records)))
		proof.archiveClaimed = true
		return proof
	}
	return s.verifyRecoveryArchiveRecord(ctx, state, run, records[0])
}

func (s *Service) verifyRecoveryArchiveRecord(ctx context.Context, state *State, run *db.Run, record *db.RecoveryArchive) recoverySourceProof {
	proof := recoverySourceProof{archiveClaimed: true, archive: record}
	if record != nil {
		proof.evidence = &RecoveryEvidence{
			Source: "bound_archive", RepositoryID: record.RepoID, RunID: record.RunID,
			Branch: record.Branch, RequiredHead: record.RequiredHeadSHA, PreservedHead: record.PreservedHeadSHA,
			ArchiveRef: record.ArchiveRef, KeepLocal: true,
		}
	}
	fail := func(safety, message string) recoverySourceProof {
		proof.safety = safety
		proof.err = message + "; no files or refs were changed"
		if proof.evidence != nil {
			proof.evidence.Proof = safety
		}
		return proof
	}
	if state == nil || run == nil || record == nil {
		return fail("blocked_recover_archive_malformed", "the recovery archive record is incomplete")
	}
	if record.OwnerRunID != run.ID {
		return fail("blocked_recover_archive_run_mismatch", fmt.Sprintf("archive record owner run %s does not match selected run %s", record.OwnerRunID, run.ID))
	}
	if s.Repo == nil || record.RepoID != s.Repo.ID || run.RepoID != s.Repo.ID {
		return fail("blocked_recover_archive_repository_mismatch", fmt.Sprintf("archive record repository %s does not match selected repository %s", record.RepoID, run.RepoID))
	}
	if record.RunID != run.ID {
		return fail("blocked_recover_archive_run_mismatch", fmt.Sprintf("archive evidence run %s does not match selected run %s", record.RunID, run.ID))
	}
	if record.Branch != run.Branch || record.Branch != state.Local.Branch {
		return fail("blocked_recover_archive_branch_mismatch", fmt.Sprintf("archive evidence branch %s does not match selected branch %s", record.Branch, state.Local.Branch))
	}
	required, requiredOK := archiveRequiredHead(*state, run)
	if !requiredOK || record.RequiredHeadSHA != required || state.Local.Head != record.RequiredHeadSHA {
		return fail("blocked_recover_archive_required_head_mismatch", fmt.Sprintf("archive required head %s does not match the selected exact working head %s", record.RequiredHeadSHA, state.Local.Head))
	}
	if record.PreservedHeadSHA != run.HeadSHA {
		return fail("blocked_recover_archive_preserved_head_mismatch", fmt.Sprintf("archive preserved head %s does not match recorded pipeline head %s", record.PreservedHeadSHA, run.HeadSHA))
	}
	if !terminalRunStatus(run.Status) || run.TerminalHeadVerifiedAt == nil || run.CustodyReturnedAt != nil {
		return fail("blocked_recover_archive_stale", fmt.Sprintf("archive provenance no longer describes an unrecovered terminal run %s", run.ID))
	}
	if !state.Local.Clean {
		return fail("blocked_recover_archive_dirty", "the working tree is not clean")
	}
	archiveRef := strings.TrimSpace(record.ArchiveRef)
	if archiveRef != record.ArchiveRef || !strings.HasPrefix(archiveRef, "refs/heads/archive/") {
		return fail("blocked_recover_archive_malformed", fmt.Sprintf("archive ref %q is outside refs/heads/archive", record.ArchiveRef))
	}
	if _, err := git.Run(ctx, s.workDir(), "check-ref-format", archiveRef); err != nil {
		return fail("blocked_recover_archive_malformed", fmt.Sprintf("archive ref %q is malformed", archiveRef))
	}
	if symbolic, err := git.Run(ctx, s.workDir(), "symbolic-ref", "-q", archiveRef); err == nil && symbolic != "" {
		return fail("blocked_recover_archive_symbolic", fmt.Sprintf("archive ref %s is symbolic to %s", archiveRef, symbolic))
	}
	target, exists, err := git.ExactRefTarget(ctx, s.workDir(), archiveRef)
	if err != nil {
		return fail("blocked_recover_archive_unreadable", fmt.Sprintf("archive ref %s could not be inspected", archiveRef))
	}
	if !exists {
		return fail("blocked_recover_archive_missing", fmt.Sprintf("archive ref %s is missing", archiveRef))
	}
	objectType, err := git.Run(ctx, s.workDir(), "cat-file", "-t", target)
	if err != nil || objectType != "commit" {
		return fail("blocked_recover_archive_replaced", fmt.Sprintf("archive ref %s points at non-commit object %s", archiveRef, target))
	}
	if target != record.PreservedHeadSHA {
		return fail("blocked_recover_archive_moved", fmt.Sprintf("archive ref %s moved to %s, expected %s", archiveRef, target, record.PreservedHeadSHA))
	}
	if relationBetween(ctx, s.workDir(), record.RequiredHeadSHA, record.PreservedHeadSHA) != RelationDiverged {
		return fail("blocked_recover_archive_not_divergent", fmt.Sprintf("archive head %s no longer diverges from required head %s", record.PreservedHeadSHA, record.RequiredHeadSHA))
	}
	if compatible, err := recoveryAnchorCompatible(ctx, s.workDir(), run.ID, run.HeadSHA); err != nil || !compatible {
		return fail("blocked_recover_anchor_mismatch", fmt.Sprintf("the invoking worktree recovery ref %s conflicts with recorded pipeline head %s", custody.RecoveryRef(run.ID), run.HeadSHA))
	}

	gateDir := strings.TrimSpace(s.GateDir)
	if gateDir == "" {
		return fail("blocked_recover_gate_unavailable", "no local gate is configured for this repository")
	}
	if _, err := os.Stat(gateDir); err != nil {
		return fail("blocked_recover_gate_unavailable", fmt.Sprintf("local gate %s is unavailable", gateDir))
	}
	gateRecoveryRef := custody.RecoveryRef(run.ID)
	if symbolic, err := git.Run(ctx, gateDir, "symbolic-ref", "-q", gateRecoveryRef); err == nil && symbolic != "" {
		return fail("blocked_recover_anchor_mismatch", fmt.Sprintf("the local gate recovery ref %s is symbolic to %s", gateRecoveryRef, symbolic))
	}
	gateRecoveryHead, exists, err := git.ExactRefTarget(ctx, gateDir, gateRecoveryRef)
	if err != nil || !exists {
		return fail("blocked_recover_anchor_mismatch", fmt.Sprintf("the local gate recovery ref %s is missing or unreadable", gateRecoveryRef))
	}
	if objectType, err := git.Run(ctx, gateDir, "cat-file", "-t", gateRecoveryHead); err != nil || objectType != "commit" || gateRecoveryHead != run.HeadSHA {
		return fail("blocked_recover_anchor_mismatch", fmt.Sprintf("the local gate recovery ref %s does not point at recorded pipeline head %s", gateRecoveryRef, run.HeadSHA))
	}
	gateBranchRef := "refs/heads/" + run.Branch
	if symbolic, err := git.Run(ctx, gateDir, "symbolic-ref", "-q", gateBranchRef); err == nil && symbolic != "" {
		return fail("blocked_recover_archive_gate_branch_invalid", fmt.Sprintf("local gate branch %s is symbolic to %s", gateBranchRef, symbolic))
	}
	gateHead, exists, err := git.ExactRefTarget(ctx, gateDir, gateBranchRef)
	if err != nil || !exists {
		return fail("blocked_recover_gate_unavailable", fmt.Sprintf("local gate branch %s is missing or unreadable", gateBranchRef))
	}
	if objectType, err := git.Run(ctx, gateDir, "cat-file", "-t", gateHead); err != nil || objectType != "commit" {
		return fail("blocked_recover_archive_gate_branch_invalid", fmt.Sprintf("local gate branch %s does not point at a commit", gateBranchRef))
	}
	if gateHead != record.RequiredHeadSHA && gateHead != record.PreservedHeadSHA {
		return fail("blocked_recover_archive_gate_head_mismatch", fmt.Sprintf("local gate branch %s is at %s, not required head %s or preserved head %s", gateBranchRef, gateHead, record.RequiredHeadSHA, record.PreservedHeadSHA))
	}

	proof.available = true
	proof.action = NextAction{Code: "recover_custody", Command: "no-mistakes axi sync --recover --keep-local"}
	proof.evidence.Proof = "verified"
	return proof
}

func archiveRequiredHead(state State, run *db.Run) (string, bool) {
	if run == nil || state.Local.Head == "" {
		return "", false
	}
	local := state.Local.Head
	return local, local == ptr(run.LastPushedSHA) || local == ptr(run.ReviewApprovedHeadSHA) || local == ptr(run.SubmittedHeadSHA)
}

func sameRecoveryArchive(a, b *db.RecoveryArchive) bool {
	return a != nil && b != nil &&
		a.OwnerRunID == b.OwnerRunID && a.RepoID == b.RepoID && a.RunID == b.RunID &&
		a.Branch == b.Branch && a.RequiredHeadSHA == b.RequiredHeadSHA &&
		a.PreservedHeadSHA == b.PreservedHeadSHA && a.ArchiveRef == b.ArchiveRef
}

// recoveryAnchorCompatible treats an absent ref as available for create-only
// preservation, but rejects every existing ref that is not exactly the
// recorded commit, including symbolic and non-commit evidence.
func recoveryAnchorCompatible(ctx context.Context, repoDir, runID, preserved string) (bool, error) {
	return exactCommitRefCompatible(ctx, repoDir, custody.RecoveryRef(runID), preserved)
}

func recoveryGateAnchorCompatible(ctx context.Context, repoDir, runID, gateHead string) (bool, error) {
	return exactCommitRefCompatible(ctx, repoDir, custody.RecoveryGateRef(runID), gateHead)
}

func exactCommitRefCompatible(ctx context.Context, repoDir, ref, expected string) (bool, error) {
	if symbolic, err := git.Run(ctx, repoDir, "symbolic-ref", "-q", ref); err == nil && symbolic != "" {
		return false, nil
	}
	_, exists, err := git.ExactRefTarget(ctx, repoDir, ref)
	if err != nil {
		return false, err
	}
	if !exists {
		return true, nil
	}
	anchored, err := git.Run(ctx, repoDir, "rev-parse", ref+"^{commit}")
	return err == nil && anchored == expected, nil
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

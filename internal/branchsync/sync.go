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

	"github.com/kunchenguid/no-mistakes/internal/db"
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
)

const (
	RelationEqual    = "equal"
	RelationBehind   = "behind"
	RelationAhead    = "ahead"
	RelationDiverged = "diverged"
	RelationUnknown  = "unknown"
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
	// Recovered is set only by Recover: custody of the stranded terminal run
	// was returned (either by this call or by an earlier, idempotent one).
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

// Service synchronizes only the invoking worktree. Repo is the registered
// repository record, while WorkDir may be its main or a linked worktree.
// GateDir is the repo's local bare gate; Recover reads preserved pipeline
// heads from it and is the only method that touches it.
type Service struct {
	DB      *db.DB
	Repo    *db.Repo
	WorkDir string
	GateDir string

	beforeApply     func()
	beforeGateReset func()
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
	return &Service{DB: database, Repo: repo, WorkDir: root, GateDir: p.RepoDir(repo.ID)}, func() { _ = database.Close() }, nil
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

// InspectCached reads local Git and persisted provenance without fetching or
// mutating refs, the index, or the worktree.
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

	s.classifyRelation(ctx, &state, bound, true)
	return state
}

// Apply repeats remote and mutable-precondition checks, then performs one
// strict fast-forward to the exact pipeline-bound SHA.
func (s *Service) Apply(ctx context.Context) State {
	plan := s.Refresh(ctx)
	if plan.State == StateSynchronized || plan.State == StateMergedRemoteRemoved {
		plan.Changed = false
		return plan
	}
	if plan.Safety != "safe_fast_forward" {
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
	if !isAncestor(ctx, s.workDir(), plan.Local.Head, plan.Pipeline.PushedHead) || plan.Local.Head == plan.Pipeline.PushedHead {
		return blockedPlan(plan, StateAmbiguousContext, "blocked_assumptions_changed", "the strict fast-forward assumptions changed before synchronization; no files or refs were changed")
	}

	_, mergeErr := git.Run(ctx, s.workDir(), "merge", "--ff-only", "--no-edit", plan.Pipeline.PushedHead)
	finalHead, _ := git.HeadSHA(ctx, s.workDir())
	finalClean, finalReason := worktreeClean(ctx, s.workDir())
	plan.Local.Head = finalHead
	plan.Local.Clean = finalClean
	plan.Local.Reason = finalReason
	plan.Changed = finalHead == plan.Pipeline.PushedHead && finalHead != recheck.Local.Head
	if mergeErr != nil || finalHead != plan.Pipeline.PushedHead {
		plan.State = StateAmbiguousContext
		plan.Safety = "blocked_apply_failed"
		plan.Error = fmt.Sprintf("strict fast-forward failed; final HEAD is %s and no destructive recovery was attempted", finalHead)
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

// Recover returns custody of a branch stranded by a TERMINAL run whose
// pipeline head was never published: cancelled or failed before the push, or
// terminal after a push with additional unpublished commits. While such a run
// was active the pipeline_owned block was correct; once it is terminal nothing
// will ever publish the head, so an explicit guarded exit must exist.
//
// The decision matrix, by worktree relation to the preserved pipeline head P
// (the gate branch head recorded as the run's head_sha):
//
//	relation   worktree  default                        --keep-local
//	equal      any       return custody; no mutation    same
//	ahead      any       return custody; no mutation    same
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
//   - The preserved commits must be provably safe before custody moves: either
//     already reachable from the local branch (equal/ahead), or verified at
//     the gate branch head and fetched into the private anchor ref
//     refs/no-mistakes/recover/<runID> in the invoking repository. The anchor
//     keeps them reachable locally no matter what later happens to the gate.
//   - The only possible worktree mutation stays a strict fast-forward of a
//     clean checked-out branch; --keep-local never touches the worktree and
//     instead moves the gate branch to the kept head with an atomic
//     compare-and-swap, so a concurrent gate push wins and recovery refuses.
//   - Anything unverifiable (missing gate, moved gate branch, failed anchor
//     fetch, changed assumptions) refuses with a reason and changes nothing.
//
// Recovery ends with a persisted custody-return stamp on the run; inspection
// then reports custody_returned (never-pushed runs) or the ordinary
// classification against the last push binding (pushed runs), both pointing at
// run_pipeline as the next step. `no-mistakes rerun` remains the alternative
// exit that resumes validating the preserved head instead of taking it back.
func (s *Service) Recover(ctx context.Context, keepLocal bool) State {
	state, run, _ := s.inspect(ctx)
	if run != nil && run.CustodyReturnedAt != nil {
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

	wd := s.workDir()
	branch := state.Local.Branch
	local := state.Local.Head
	preserved := run.HeadSHA
	anchorRef := recoverAnchorRef(run.ID)

	// When the preserved commits are already reachable from the local branch,
	// custody return needs no gate access and no anchor.
	if objectExists(ctx, wd, preserved) && (local == preserved || isAncestor(ctx, wd, preserved, local)) {
		return s.finishRecover(ctx, run, false)
	}

	gateDir := strings.TrimSpace(s.GateDir)
	if gateDir == "" {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_gate_unavailable", "no local gate is configured for this repository, so the preserved pipeline head cannot be verified; no files or refs were changed")
	}
	gateHead, err := git.Run(ctx, gateDir, "rev-parse", "refs/heads/"+branch+"^{commit}")
	if err != nil {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_gate_unavailable", fmt.Sprintf("the local gate no longer has branch %s, so the preserved pipeline head %s cannot be verified; no files or refs were changed", branch, preserved))
	}
	anchored := false
	if existing, anchorErr := git.Run(ctx, wd, "rev-parse", anchorRef+"^{commit}"); anchorErr == nil && existing == preserved {
		anchored = true
	}
	// A keep-local recovery that reset the gate but crashed before stamping
	// custody resumes here: the gate already equals the kept local head and
	// the preserved head is already anchored.
	resumedKeepLocal := keepLocal && anchored && gateHead == local
	if gateHead != preserved && !resumedKeepLocal {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_gate_diverged", fmt.Sprintf("the gate branch is at %s, not the preserved pipeline head %s recorded for this run; no files or refs were changed", gateHead, preserved))
	}
	if !anchored {
		if fetchErr := git.FetchRemoteBranchToPrivateRef(ctx, wd, gateDir, branch, anchorRef); fetchErr != nil {
			return blockedPlan(state, StatePipelineOwned, "blocked_recover_preserve_failed", "the preserved pipeline commits could not be fetched from the local gate; no files or refs were changed")
		}
		if fetched, fetchErr := git.Run(ctx, wd, "rev-parse", anchorRef+"^{commit}"); fetchErr != nil || fetched != preserved {
			return blockedPlan(state, StatePipelineOwned, "blocked_recover_preserve_failed", "the gate branch changed while the preserved pipeline commits were being anchored; no files or refs were changed")
		}
	}

	switch {
	case local == preserved, isAncestor(ctx, wd, preserved, local):
		// Equal or ahead, discovered only after anchoring made the preserved
		// head comparable locally.
		return s.finishRecover(ctx, run, false)
	case isAncestor(ctx, wd, local, preserved):
		if keepLocal {
			return s.recoverKeepLocal(ctx, run, state, gateHead)
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
			return s.recoverKeepLocal(ctx, run, state, gateHead)
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
// pipeline run. The preserved head stays reachable through the anchor ref.
func (s *Service) recoverKeepLocal(ctx context.Context, run *db.Run, state State, gateHead string) State {
	if s.beforeGateReset != nil {
		s.beforeGateReset()
	}
	if gateHead != state.Local.Head {
		head, err := git.HeadSHA(ctx, s.workDir())
		if err != nil || head != state.Local.Head {
			return blockedPlan(state, StatePipelineOwned, "blocked_recover_assumptions_changed", "the local branch head changed while custody was being returned; no files or refs were changed")
		}
		// The fetch source must be absolute: the command runs inside the gate
		// directory, where a relative invoking-worktree path would resolve to
		// the gate itself.
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
	}
	return s.finishRecover(ctx, run, false)
}

// recoverFastForward advances the clean checked-out branch to the preserved
// pipeline head with the same strict fast-forward and honesty rules as Apply.
func (s *Service) recoverFastForward(ctx context.Context, run *db.Run, state State, preserved string) State {
	head, headErr := git.HeadSHA(ctx, s.workDir())
	clean, _ := worktreeClean(ctx, s.workDir())
	if headErr != nil || head != state.Local.Head || !clean {
		return blockedPlan(state, StatePipelineOwned, "blocked_recover_assumptions_changed", "the local branch or worktree changed while custody was being returned; no files or refs were changed")
	}
	_, mergeErr := git.Run(ctx, s.workDir(), "merge", "--ff-only", "--no-edit", preserved)
	finalHead, _ := git.HeadSHA(ctx, s.workDir())
	if mergeErr != nil || finalHead != preserved {
		state.Local.Head = finalHead
		blocked := blockedPlan(state, StatePipelineOwned, "blocked_recover_apply_failed", fmt.Sprintf("strict fast-forward to the preserved pipeline head failed; final HEAD is %s and no destructive recovery was attempted", finalHead))
		blocked.Changed = finalHead != head
		return blocked
	}
	return s.finishRecover(ctx, run, true)
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

func recoverAnchorRef(runID string) string {
	return "refs/no-mistakes/recover/" + runID
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
	for _, candidate := range runs {
		if candidate.Branch != branch {
			continue
		}
		if candidate.Status == types.RunPending || candidate.Status == types.RunRunning || unpublishedPipelineHead(candidate) {
			run = candidate
			break
		}
		// Custody-returned runs stay selectable so a recovered branch reports
		// custody_returned (or its ordinary post-push classification) instead
		// of falling back to an older binding or an ambiguous no-match.
		if run == nil && (candidate.LastPushedSHA != nil || candidate.CustodyReturnedAt != nil) {
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
		return state, run, false
	}
	if run.LastPushedSHA == nil || run.PushTargetFingerprint == nil || run.PushRef == nil || run.PushGeneration == nil || run.SubmittedHeadSHA == nil {
		if run.SubmittedHeadSHA != nil && run.HeadSHA != ptr(run.SubmittedHeadSHA) {
			if run.CustodyReturnedAt != nil {
				s.classifyCustodyReturned(ctx, &state)
				return state, run, true
			}
			classifyPipelineOwned(&state, run, "the pipeline head has moved but has not been successfully pushed; do not make local follow-up commits yet")
			return state, run, false
		}
		state.State = StateLegacyUnbound
		state.Safety = "blocked_legacy_unbound"
		state.Error = "this run has no exact successful push provenance and cannot be synchronized safely"
		return state, run, false
	}
	if run.HeadSHA != ptr(run.LastPushedSHA) && run.CustodyReturnedAt == nil {
		classifyPipelineOwned(&state, run, "the pipeline head has not been successfully bound to the push target; do not make local follow-up commits yet")
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

	s.classifyRelation(ctx, &state, ptr(run.LastPushedSHA), false)
	return state, run, true
}

func (s *Service) classifyRelation(ctx context.Context, state *State, pushed string, live bool) {
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
		state.Safety = "safe_fast_forward"
	} else {
		state.Safety = "refresh_required"
	}
	state.NextAction = &NextAction{Code: "sync", Command: "no-mistakes axi sync"}
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
		return run.HeadSHA != ptr(run.SubmittedHeadSHA)
	}
	return run.HeadSHA != ptr(run.LastPushedSHA)
}

func terminalRunStatus(status types.RunStatus) bool {
	switch status {
	case types.RunCompleted, types.RunFailed, types.RunCancelled:
		return true
	default:
		return false
	}
}

// classifyPipelineOwned reports an unpublished pipeline head. While the run is
// active the block is absolute: the pipeline will publish or keep moving the
// head, so the worktree must wait. Once the run is TERMINAL nothing will ever
// publish that head - the branch would be stranded in custody forever - so the
// same state becomes recoverable and points at the guarded custody-return
// action (issue: v1.38.1 dogfood, cancelled pre-push run).
func classifyPipelineOwned(state *State, run *db.Run, activeMessage string) {
	state.State = StatePipelineOwned
	state.Pipeline.Phase = "pre_push"
	if terminalRunStatus(run.Status) {
		state.Safety = "blocked_pipeline_owned_recoverable"
		state.Error = "the run finished " + string(run.Status) + " with unpublished pipeline commits preserved in the local gate; recover custody before any local follow-up commit"
		state.NextAction = &NextAction{Code: "recover_custody", Command: "no-mistakes axi sync --recover"}
		return
	}
	state.Safety = "blocked_pipeline_owned"
	state.Error = activeMessage
}

// classifyCustodyReturned reports a branch whose stranded terminal run was
// explicitly recovered and never had a push binding: the operator owns the
// branch again and the only remaining step is starting a fresh run. The
// relation against the preserved pipeline head is informative only.
func (s *Service) classifyCustodyReturned(ctx context.Context, state *State) {
	state.State = StateCustodyReturned
	state.Safety = "custody_returned"
	state.Relation = RelationUnknown
	state.Error = ""
	state.NextAction = &NextAction{Code: "run_pipeline", Command: `no-mistakes axi run --intent "<what the user set out to accomplish>"`}
	preserved := state.Pipeline.CurrentHead
	if preserved == "" || !objectExists(ctx, s.workDir(), preserved) {
		return
	}
	switch {
	case state.Local.Head == preserved:
		state.Relation = RelationEqual
	case isAncestor(ctx, s.workDir(), state.Local.Head, preserved):
		state.Relation = RelationBehind
	case isAncestor(ctx, s.workDir(), preserved, state.Local.Head):
		state.Relation = RelationAhead
	default:
		state.Relation = RelationDiverged
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

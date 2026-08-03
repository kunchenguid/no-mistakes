package branchsync

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	gitpkg "github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	pipelinepkg "github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type cancellationRaceStep struct {
	committed chan string
}

type unreachedCancellationStep struct{}

type skippedDeliveryStep struct {
	name types.StepName
}

func (s *skippedDeliveryStep) Name() types.StepName { return s.name }

func (*skippedDeliveryStep) Execute(*pipelinepkg.StepContext) (*pipelinepkg.StepOutcome, error) {
	return &pipelinepkg.StepOutcome{}, nil
}

func (*unreachedCancellationStep) Name() types.StepName { return types.StepReview }

func (*unreachedCancellationStep) Execute(*pipelinepkg.StepContext) (*pipelinepkg.StepOutcome, error) {
	return &pipelinepkg.StepOutcome{}, nil
}

func (s *cancellationRaceStep) Name() types.StepName { return types.StepReview }

func (s *cancellationRaceStep) Execute(sctx *pipelinepkg.StepContext) (*pipelinepkg.StepOutcome, error) {
	if err := os.WriteFile(filepath.Join(sctx.WorkDir, "fix.txt"), []byte("pipeline fix\n"), 0o644); err != nil {
		return nil, err
	}
	if _, err := gitpkg.Run(sctx.Ctx, sctx.WorkDir, "add", "fix.txt"); err != nil {
		return nil, err
	}
	if _, err := gitpkg.Run(sctx.Ctx, sctx.WorkDir, "commit", "-m", "no-mistakes(review): fix"); err != nil {
		return nil, err
	}
	head, err := gitpkg.HeadSHA(sctx.Ctx, sctx.WorkDir)
	if err != nil {
		return nil, err
	}
	if _, err := gitpkg.Run(sctx.Ctx, sctx.WorkDir, "update-ref", "refs/heads/feature/recover", head); err != nil {
		return nil, err
	}
	s.committed <- head
	<-sctx.Ctx.Done()
	return nil, context.Cause(sctx.Ctx)
}

// recoverFixture reproduces the stranded custody state from the v1.38.1
// dogfood report: a run went terminal at the pre_push phase, so its pipeline
// fix commits exist only in the local gate's bare branch while the registered
// operator worktree still sits at the submitted head with no push binding.
type recoverFixture struct {
	t         *testing.T
	ctx       context.Context
	db        *db.DB
	repo      *db.Repo
	run       *db.Run
	service   *Service
	local     string
	gate      string
	remote    string
	base      string
	submitted string
	preserved string
}

// newRecoverFixture builds an operator repo on feature/recover at the
// submitted head, a bare gate whose feature/recover branch carries two extra
// pipeline fix commits (the preserved head), and a run row that is terminal
// with head_sha at the preserved head and no push provenance.
func newRecoverFixture(t *testing.T, status types.RunStatus) *recoverFixture {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	remote := filepath.Join(root, "upstream.git")
	mustRun(t, root, "init", "--bare", remote)

	local := filepath.Join(root, "operator")
	mustRun(t, root, "init", "-b", "main", local)
	configureIdentity(t, local)
	mustWrite(t, filepath.Join(local, "file.txt"), "base\n")
	mustRun(t, local, "add", "file.txt")
	mustRun(t, local, "commit", "-m", "base")
	base := mustRun(t, local, "rev-parse", "HEAD")
	mustRun(t, local, "checkout", "-b", "feature/recover")
	mustWrite(t, filepath.Join(local, "file.txt"), "feature\n")
	mustRun(t, local, "commit", "-am", "feature")
	submitted := mustRun(t, local, "rev-parse", "HEAD")

	// The gate receives the submitted branch, then the pipeline commits fixes
	// onto the gate branch; nothing is ever pushed to the upstream.
	gate := filepath.Join(root, "gate.git")
	mustRun(t, root, "init", "--bare", gate)
	mustRun(t, local, "push", gate, "refs/heads/feature/recover:refs/heads/feature/recover")
	pipeline := filepath.Join(root, "pipeline")
	mustRun(t, root, "-c", "core.autocrlf=false", "clone", gate, pipeline)
	configureIdentity(t, pipeline)
	mustRun(t, pipeline, "checkout", "feature/recover")
	mustWrite(t, filepath.Join(pipeline, "fix.txt"), "pipeline fix\n")
	mustRun(t, pipeline, "add", "fix.txt")
	mustRun(t, pipeline, "commit", "-m", "no-mistakes(review): fix")
	mustWrite(t, filepath.Join(pipeline, "fix.txt"), "pipeline fix 2\n")
	mustRun(t, pipeline, "commit", "-am", "no-mistakes(lint): fix")
	preserved := mustRun(t, pipeline, "rev-parse", "HEAD")
	mustRun(t, pipeline, "push", "origin", "HEAD:refs/heads/feature/recover")

	database, err := db.Open(filepath.Join(root, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	repo, err := database.InsertRepo(local, remote, "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "feature/recover", submitted, base)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunHeadSHA(run.ID, preserved); err != nil {
		t.Fatal(err)
	}
	var statusErr error
	if terminalRunStatus(status) {
		statusErr = database.UpdateRunStatusWithVerifiedHead(run.ID, status, preserved)
	} else {
		statusErr = database.UpdateRunStatus(run.ID, status)
	}
	if statusErr != nil {
		t.Fatal(statusErr)
	}
	run, _ = database.GetRun(run.ID)
	return &recoverFixture{
		t: t, ctx: ctx, db: database, repo: repo, run: run,
		service: &Service{DB: database, Repo: repo, WorkDir: local, GateDir: gate},
		local:   local, gate: gate, remote: remote,
		base: base, submitted: submitted, preserved: preserved,
	}
}

func (f *recoverFixture) anchorRef() string { return "refs/no-mistakes/recover/" + f.run.ID }

func (f *recoverFixture) custodyReturned() bool {
	f.t.Helper()
	run, err := f.db.GetRun(f.run.ID)
	if err != nil || run == nil {
		f.t.Fatalf("reload run: %#v, %v", run, err)
	}
	return run.CustodyReturnedAt != nil
}

// TestTerminalPrePushRunSurfacesGuardedCustodyRecovery is the regression test
// for the stranded state itself (dogfood run 01KXN8YJ6DWF8XPP582DWQC3HV): a
// terminal run at the pre_push phase must not be a dead end. The state stays
// pipeline_owned, but safety identifies it as recoverable, exposes the run's
// terminal status, and offers the guarded custody-return action.
func TestTerminalPrePushRunSurfacesGuardedCustodyRecovery(t *testing.T) {
	t.Parallel()

	for _, status := range []types.RunStatus{types.RunCancelled, types.RunFailed, types.RunCompleted} {
		t.Run(string(status), func(t *testing.T) {
			f := newRecoverFixture(t, status)
			state := f.service.InspectCached(f.ctx)
			if state.State != StatePipelineOwned || state.Safety != "blocked_pipeline_owned_recoverable" {
				t.Fatalf("state = %s safety = %s, want pipeline_owned/blocked_pipeline_owned_recoverable", state.State, state.Safety)
			}
			if state.Pipeline.Status != string(status) || state.Pipeline.Phase != "pre_push" {
				t.Fatalf("pipeline = %#v", state.Pipeline)
			}
			if state.NextAction == nil || state.NextAction.Code != "recover_custody" || !strings.Contains(state.NextAction.Command, "sync --recover") {
				t.Fatalf("next action = %#v", state.NextAction)
			}
			if !strings.Contains(state.Error, "preserved") {
				t.Fatalf("error does not explain preservation: %q", state.Error)
			}
		})
	}
}

// TestActivePrePushRunStaysBlockedWithoutRecovery pins the other half of the
// class split: while the run is still active the pre-push block is correct and
// no custody-return action may be offered.
func TestActivePrePushRunStaysBlockedWithoutRecovery(t *testing.T) {
	t.Parallel()

	f := newRecoverFixture(t, types.RunRunning)
	state := f.service.InspectCached(f.ctx)
	if state.State != StatePipelineOwned || state.Safety != "blocked_pipeline_owned" {
		t.Fatalf("active run state = %#v", state)
	}
	if state.NextAction == nil || state.NextAction.Code != "continue_active_run" || state.NextAction.Command != "no-mistakes axi status" {
		t.Fatalf("active run next action = %#v", state.NextAction)
	}
	if state.Pipeline.Status != "running" {
		t.Fatalf("pipeline status = %q", state.Pipeline.Status)
	}
	recovered := f.service.Recover(f.ctx, false)
	if recovered.Recovered || recovered.Safety != "blocked_recover_run_active" {
		t.Fatalf("recover on active run = %#v", recovered)
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.submitted {
		t.Fatal("recover on active run moved HEAD")
	}
	if f.custodyReturned() {
		t.Fatal("recover on active run stamped custody")
	}
}

// TestRecoverCleanBehindFastForwardsAndReturnsCustody is the primary recovery
// journey: terminal cancelled pre-push, clean worktree at the submitted head.
// Recovery must anchor the preserved commits locally, fast-forward the branch
// to the preserved head, stamp custody returned, and leave the branch free for
// a fresh run.
func TestRecoverCleanBehindFastForwardsAndReturnsCustody(t *testing.T) {
	t.Parallel()

	f := newRecoverFixture(t, types.RunCancelled)
	state := f.service.Recover(f.ctx, false)
	if !state.Recovered || !state.Changed {
		t.Fatalf("recover result = %#v", state)
	}
	if state.State != StateCustodyReturned || state.Safety != "custody_returned" || state.Relation != RelationEqual {
		t.Fatalf("post-recover state = %s/%s relation %s", state.State, state.Safety, state.Relation)
	}
	if state.NextAction == nil || state.NextAction.Code != "run_pipeline" {
		t.Fatalf("post-recover next action = %#v", state.NextAction)
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.preserved {
		t.Fatalf("HEAD = %s, want preserved %s", got, f.preserved)
	}
	if parents := strings.Fields(mustRun(t, f.local, "show", "-s", "--format=%P", f.preserved+"~1")); len(parents) != 1 || parents[0] != f.submitted {
		t.Fatalf("recovery rewrote history: %v", parents)
	}
	if got := mustRun(t, f.local, "rev-parse", f.anchorRef()); got != f.preserved {
		t.Fatalf("anchor ref = %s, want %s", got, f.preserved)
	}
	if !f.custodyReturned() {
		t.Fatal("custody not stamped")
	}

	// The branch is free again: cached inspection reports custody_returned
	// with a run_pipeline next action, and a brand-new run takes over cleanly.
	after := f.service.InspectCached(f.ctx)
	if after.State != StateCustodyReturned || after.NextAction == nil || after.NextAction.Code != "run_pipeline" {
		t.Fatalf("post-recover inspection = %#v", after)
	}
	fresh, err := f.db.InsertRun(f.repo.ID, "feature/recover", f.preserved, f.base)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.UpdateRunStatus(fresh.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	next := f.service.InspectCached(f.ctx)
	if next.Pipeline.RunID != fresh.ID {
		t.Fatalf("fresh run not selected after recovery: %#v", next.Pipeline)
	}
}

func TestRecoverFastForwardRechecksCurrentBranchBeforeMerge(t *testing.T) {
	t.Parallel()

	f := newRecoverFixture(t, types.RunCancelled)
	f.service.beforeRecoverWorktreeMove = func() {
		mustRun(t, f.local, "checkout", "-b", "other-clean-branch", f.submitted)
	}
	state := f.service.Recover(f.ctx, false)
	if state.Recovered || state.Changed || state.Safety != "blocked_recover_assumptions_changed" {
		t.Fatalf("recover after branch switch = %#v", state)
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.submitted {
		t.Fatalf("HEAD = %s, want submitted %s", got, f.submitted)
	}
	if got := strings.TrimSpace(mustRun(t, f.local, "branch", "--show-current")); got != "other-clean-branch" {
		t.Fatalf("current branch = %q", got)
	}
	if f.custodyReturned() {
		t.Fatal("branch-switch refusal stamped custody")
	}
}

func TestRecoverReportsDirtyFinalStateWhenPostMergeHookMutatesWorktree(t *testing.T) {
	t.Parallel()

	f := newRecoverFixture(t, types.RunCancelled)
	hooks := filepath.Join(f.local, ".git", "hooks")
	hook := filepath.Join(hooks, "post-merge")
	mustWrite(t, hook, "#!/bin/sh\nprintf hook > hook-output.txt\nexit 1\n")
	if err := os.Chmod(hook, 0o755); err != nil {
		t.Fatal(err)
	}
	state := f.service.Recover(f.ctx, false)
	if state.Recovered || !state.Changed || state.Local.Head != f.preserved || state.State != StateDirty || state.Local.Clean || !strings.HasPrefix(state.Safety, "blocked_post_recover_") {
		t.Fatalf("hook final state = %#v", state)
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.preserved {
		t.Fatalf("honest final HEAD = %s", got)
	}
	if f.custodyReturned() {
		t.Fatal("dirty post-recover state stamped custody")
	}
}

// TestRecoverIdempotentAfterSuccess proves a repeated recover is a safe no-op.
func TestRecoverIdempotentAfterSuccess(t *testing.T) {
	t.Parallel()

	f := newRecoverFixture(t, types.RunCancelled)
	if first := f.service.Recover(f.ctx, false); !first.Recovered {
		t.Fatalf("first recover = %#v", first)
	}
	second := f.service.Recover(f.ctx, false)
	if !second.Recovered || second.Changed || second.State != StateCustodyReturned {
		t.Fatalf("second recover = %#v", second)
	}
}

// TestRecoverWorktreeAlreadyAtPreservedHeadReturnsCustodyWithoutMutation
// covers the equal cell: nothing to reconcile, custody return only.
func TestRecoverWorktreeAlreadyAtPreservedHeadReturnsCustodyWithoutMutation(t *testing.T) {
	t.Parallel()

	f := newRecoverFixture(t, types.RunCancelled)
	mustRun(t, f.local, "fetch", f.gate, "refs/heads/feature/recover")
	mustRun(t, f.local, "merge", "--ff-only", f.preserved)
	if err := os.RemoveAll(f.gate); err != nil {
		t.Fatal(err)
	}
	state := f.service.Recover(f.ctx, false)
	if !state.Recovered || state.Changed || state.State != StateCustodyReturned || state.Relation != RelationEqual {
		t.Fatalf("recover equal = %#v", state)
	}
	if got := mustRun(t, f.local, "rev-parse", f.anchorRef()); got != f.preserved {
		t.Fatalf("anchor ref = %s, want %s", got, f.preserved)
	}
	if !f.custodyReturned() {
		t.Fatal("custody not stamped")
	}
}

// TestRecoverLocalAheadOfPreservedHeadReturnsCustodyWithoutMutation covers the
// ahead cell: the preserved commits are already incorporated locally.
func TestRecoverLocalAheadOfPreservedHeadReturnsCustodyWithoutMutation(t *testing.T) {
	t.Parallel()

	f := newRecoverFixture(t, types.RunFailed)
	mustRun(t, f.local, "fetch", f.gate, "refs/heads/feature/recover")
	mustRun(t, f.local, "merge", "--ff-only", f.preserved)
	mustWrite(t, filepath.Join(f.local, "followup.txt"), "followup\n")
	mustRun(t, f.local, "add", "followup.txt")
	mustRun(t, f.local, "commit", "-m", "followup")
	ahead := mustRun(t, f.local, "rev-parse", "HEAD")
	if err := os.RemoveAll(f.gate); err != nil {
		t.Fatal(err)
	}
	state := f.service.Recover(f.ctx, false)
	if !state.Recovered || state.Changed || state.State != StateCustodyReturned || state.Relation != RelationAhead {
		t.Fatalf("recover ahead = %#v", state)
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != ahead {
		t.Fatal("recover ahead moved HEAD")
	}
	if got := mustRun(t, f.local, "rev-parse", f.anchorRef()); got != f.preserved {
		t.Fatalf("anchor ref = %s, want %s", got, f.preserved)
	}
}

// TestRecoverDirtyWorktreeRefusesWithoutMutation covers the behind+dirty cell:
// never fast-forward over uncommitted changes; refuse with actionable options.
func TestRecoverDirtyWorktreeRefusesWithoutMutation(t *testing.T) {
	t.Parallel()

	f := newRecoverFixture(t, types.RunCancelled)
	mustWrite(t, filepath.Join(f.local, "file.txt"), "dirty\n")
	state := f.service.Recover(f.ctx, false)
	if state.Recovered || state.Changed || state.Safety != "blocked_recover_dirty" {
		t.Fatalf("recover dirty = %#v", state)
	}
	if !strings.Contains(state.Error, "--keep-local") {
		t.Fatalf("dirty refusal not actionable: %q", state.Error)
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.submitted {
		t.Fatal("dirty refusal moved HEAD")
	}
	if f.custodyReturned() {
		t.Fatal("dirty refusal stamped custody")
	}
}

// TestRecoverDivergedRefusesButKeepLocalReturnsCustody covers the diverged
// cells: the default refuses with the anchor named, and --keep-local performs
// the explicit choice - custody at the local head, gate reset to it atomically,
// preserved commits still reachable through the anchor ref.
func TestRecoverDivergedRefusesButKeepLocalReturnsCustody(t *testing.T) {
	t.Parallel()

	f := newRecoverFixture(t, types.RunCancelled)
	mustWrite(t, filepath.Join(f.local, "rescope.txt"), "rescope\n")
	mustRun(t, f.local, "add", "rescope.txt")
	mustRun(t, f.local, "commit", "-m", "diverging rescope")
	divergedHead := mustRun(t, f.local, "rev-parse", "HEAD")

	refused := f.service.Recover(f.ctx, false)
	if refused.Recovered || refused.Safety != "blocked_recover_diverged" || refused.Relation != RelationDiverged {
		t.Fatalf("recover diverged = %#v", refused)
	}
	if !strings.Contains(refused.Error, f.anchorRef()) || !strings.Contains(refused.Error, "--keep-local") {
		t.Fatalf("diverged refusal not actionable: %q", refused.Error)
	}
	if got := mustRun(t, f.local, "rev-parse", f.anchorRef()); got != f.preserved {
		t.Fatalf("diverged refusal did not anchor preserved commits: %s", got)
	}
	if f.custodyReturned() {
		t.Fatal("diverged refusal stamped custody")
	}

	kept := f.service.Recover(f.ctx, true)
	if !kept.Recovered || kept.Changed {
		t.Fatalf("keep-local recover = %#v", kept)
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != divergedHead {
		t.Fatal("keep-local moved the worktree")
	}
	if got := mustRun(t, f.gate, "rev-parse", "refs/heads/feature/recover"); got != divergedHead {
		t.Fatalf("gate branch = %s, want local head %s", got, divergedHead)
	}
	if got := mustRun(t, f.local, "rev-parse", f.anchorRef()); got != f.preserved {
		t.Fatal("keep-local lost the preserved anchor")
	}
	if !f.custodyReturned() {
		t.Fatal("keep-local did not stamp custody")
	}
}

// TestRecoverKeepLocalDirtyBehindReturnsCustodyWithoutTouchingWorktree covers
// the explicit keep-local choice on a dirty worktree: no worktree mutation is
// needed, so dirtiness must not block it, and the gate follows the kept head.
func TestRecoverKeepLocalDirtyBehindReturnsCustodyWithoutTouchingWorktree(t *testing.T) {
	t.Parallel()

	f := newRecoverFixture(t, types.RunCancelled)
	mustWrite(t, filepath.Join(f.local, "file.txt"), "dirty rescope\n")
	state := f.service.Recover(f.ctx, true)
	if !state.Recovered || state.Changed {
		t.Fatalf("keep-local dirty recover = %#v", state)
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.submitted {
		t.Fatal("keep-local dirty moved HEAD")
	}
	if got := readOptional(t, filepath.Join(f.local, "file.txt")); got != "dirty rescope\n" {
		t.Fatal("keep-local dirty touched worktree files")
	}
	if got := mustRun(t, f.gate, "rev-parse", "refs/heads/feature/recover"); got != f.submitted {
		t.Fatalf("gate branch = %s, want kept head %s", got, f.submitted)
	}
	if got := mustRun(t, f.local, "rev-parse", f.anchorRef()); got != f.preserved {
		t.Fatal("keep-local dirty lost the preserved anchor")
	}
}

// TestRecoverGateDivergenceAndUnavailabilityFailClosed: recovery must refuse
// whenever the preserved head cannot be verified and anchored - a moved gate
// branch, a deleted gate branch, or a missing gate.
func TestRecoverGateDivergenceAndUnavailabilityFailClosed(t *testing.T) {
	t.Parallel()

	t.Run("gate branch moved", func(t *testing.T) {
		f := newRecoverFixture(t, types.RunCancelled)
		writer := filepath.Join(t.TempDir(), "writer")
		mustRun(t, filepath.Dir(writer), "-c", "core.autocrlf=false", "clone", f.gate, writer)
		configureIdentity(t, writer)
		mustRun(t, writer, "checkout", "feature/recover")
		mustWrite(t, filepath.Join(writer, "other.txt"), "other\n")
		mustRun(t, writer, "add", "other.txt")
		mustRun(t, writer, "commit", "-m", "out of band gate commit")
		mustRun(t, writer, "push", "origin", "HEAD:refs/heads/feature/recover")
		state := f.service.Recover(f.ctx, false)
		if state.Recovered || state.Safety != "blocked_recover_gate_diverged" {
			t.Fatalf("recover with moved gate = %#v", state)
		}
		if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.submitted {
			t.Fatal("moved-gate refusal mutated HEAD")
		}
	})
	t.Run("gate branch deleted", func(t *testing.T) {
		f := newRecoverFixture(t, types.RunCancelled)
		mustRun(t, f.gate, "update-ref", "-d", "refs/heads/feature/recover")
		state := f.service.Recover(f.ctx, false)
		if state.Recovered || state.Safety != "blocked_recover_gate_unavailable" {
			t.Fatalf("recover with deleted gate branch = %#v", state)
		}
	})
	t.Run("gate missing", func(t *testing.T) {
		f := newRecoverFixture(t, types.RunCancelled)
		if err := os.RemoveAll(f.gate); err != nil {
			t.Fatal(err)
		}
		state := f.service.Recover(f.ctx, false)
		if state.Recovered || state.Safety != "blocked_recover_gate_unavailable" {
			t.Fatalf("recover with missing gate = %#v", state)
		}
		if f.custodyReturned() {
			t.Fatal("unverifiable preservation stamped custody")
		}
	})
}

// TestRecoverTerminalPostPushRunWithMovedHead covers the post-push class cell:
// a run that pushed successfully, then went terminal with additional
// unpublished pipeline commits. Recovery fast-forwards to the preserved head
// and the branch classifies as local_ahead against the pushed binding, whose
// existing run_pipeline guidance publishes the recovered commits.
func TestRecoverTerminalPostPushRunWithMovedHead(t *testing.T) {
	t.Parallel()

	f := newRecoverFixture(t, types.RunCancelled)
	// The run pushed the submitted head upstream, then the pipeline moved on.
	mustRun(t, f.local, "push", f.remote, "refs/heads/feature/recover:refs/heads/feature/recover")
	if err := f.db.UpdateRunPushBinding(f.run.ID, db.PushBinding{
		HeadSHA: f.submitted, TargetKind: "upstream", TargetFingerprint: TargetFingerprint(f.remote), Ref: "refs/heads/feature/recover",
	}); err != nil {
		t.Fatal(err)
	}

	state := f.service.InspectCached(f.ctx)
	if state.State != StatePipelineOwned || state.Safety != "blocked_pipeline_owned_recoverable" {
		t.Fatalf("post-push terminal state = %#v", state)
	}
	if state.NextAction == nil || state.NextAction.Code != "recover_custody" {
		t.Fatalf("post-push next action = %#v", state.NextAction)
	}

	recovered := f.service.Recover(f.ctx, false)
	if !recovered.Recovered || !recovered.Changed {
		t.Fatalf("post-push recover = %#v", recovered)
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.preserved {
		t.Fatalf("post-push recover HEAD = %s, want %s", got, f.preserved)
	}
	if recovered.State != StateLocalAhead || recovered.NextAction == nil || recovered.NextAction.Code != "run_pipeline" {
		t.Fatalf("post-push recovered classification = %#v", recovered)
	}
	if !f.custodyReturned() {
		t.Fatal("post-push recover did not stamp custody")
	}
}

// TestRecoverRefusesWhenNothingIsStranded pins the not-applicable guard: a
// healthy behind state (successful push binding) must not be recoverable.
func TestRecoverRefusesWhenNothingIsStranded(t *testing.T) {
	t.Parallel()

	f := newSyncFixture(t)
	state := f.service.Recover(f.ctx, false)
	if state.Recovered || state.Safety != "blocked_recover_not_applicable" {
		t.Fatalf("recover on healthy state = %#v", state)
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.old {
		t.Fatal("not-applicable refusal moved HEAD")
	}
}

// newUnmovedRecoverFixture models the released cancellation shape: a run
// cancelled through the supported abort before the pipeline changed anything
// (for example because delivery switched to a direct PR mid-validation). The
// gate branch and the run's head_sha still equal the submitted head, and the
// run carries no push provenance and no custody stamp. Cancellation releases
// ownership of this shape: it must classify user_owned, never wrong-branch
// ambiguity and never recoverable pipeline custody.
func newUnmovedRecoverFixture(t *testing.T, status types.RunStatus) *recoverFixture {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	remote := filepath.Join(root, "upstream.git")
	mustRun(t, root, "init", "--bare", remote)

	local := filepath.Join(root, "operator")
	mustRun(t, root, "init", "-b", "main", local)
	configureIdentity(t, local)
	mustWrite(t, filepath.Join(local, "file.txt"), "base\n")
	mustRun(t, local, "add", "file.txt")
	mustRun(t, local, "commit", "-m", "base")
	base := mustRun(t, local, "rev-parse", "HEAD")
	mustRun(t, local, "checkout", "-b", "feature/recover")
	mustWrite(t, filepath.Join(local, "file.txt"), "feature\n")
	mustRun(t, local, "commit", "-am", "feature")
	submitted := mustRun(t, local, "rev-parse", "HEAD")

	// The gate received the submitted branch and the run went terminal before
	// the pipeline committed anything: preserved head == submitted head.
	gate := filepath.Join(root, "gate.git")
	mustRun(t, root, "init", "--bare", gate)
	mustRun(t, local, "push", gate, "refs/heads/feature/recover:refs/heads/feature/recover")

	database, err := db.Open(filepath.Join(root, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	repo, err := database.InsertRepo(local, remote, "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "feature/recover", submitted, base)
	if err != nil {
		t.Fatal(err)
	}
	var statusErr error
	if terminalRunStatus(status) {
		statusErr = database.UpdateRunStatusWithVerifiedHead(run.ID, status, submitted)
	} else {
		statusErr = database.UpdateRunStatus(run.ID, status)
	}
	if statusErr != nil {
		t.Fatal(statusErr)
	}
	run, _ = database.GetRun(run.ID)
	return &recoverFixture{
		t: t, ctx: ctx, db: database, repo: repo, run: run,
		service: &Service{DB: database, Repo: repo, WorkDir: local, GateDir: gate},
		local:   local, gate: gate, remote: remote,
		base: base, submitted: submitted, preserved: submitted,
	}
}

func TestCancellationReconcilesCommittedWorktreeHeadBeforeReleaseClassification(t *testing.T) {
	t.Parallel()

	f := newUnmovedRecoverFixture(t, types.RunCancelled)
	if err := f.db.UpdateRunStatus(f.run.ID, types.RunPending); err != nil {
		t.Fatal(err)
	}
	f.run.Status = types.RunPending

	managed := filepath.Join(t.TempDir(), "managed")
	if err := gitpkg.WorktreeAdd(f.ctx, f.gate, managed, f.submitted); err != nil {
		t.Fatal(err)
	}
	configureIdentity(t, managed)

	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	step := &cancellationRaceStep{committed: make(chan string, 1)}
	executor := pipelinepkg.NewExecutor(f.db, p, nil, nil, []pipelinepkg.Step{step}, nil)
	ctx, cancel := context.WithCancelCause(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- executor.Execute(ctx, f.run, f.repo, managed)
	}()

	committed := <-step.committed
	cancel(errors.New(types.RunCancelReasonAbortedByUser))
	if err := <-done; err == nil {
		t.Fatal("cancelled executor returned nil")
	}

	terminal, err := f.db.GetRun(f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if terminal.Status != types.RunCancelled || terminal.HeadSHA != committed {
		t.Fatalf("terminal run = status %s head %s, want cancelled head %s", terminal.Status, terminal.HeadSHA, committed)
	}
	state := f.service.InspectCached(f.ctx)
	if state.State != StatePipelineOwned || state.Safety != "blocked_pipeline_owned_recoverable" {
		t.Fatalf("cancelled committed state = %#v", state)
	}
	if state.Pipeline.CurrentHead != committed || state.Pipeline.SubmittedHead != f.submitted {
		t.Fatalf("cancelled committed heads = %#v", state.Pipeline)
	}
	if state.NextAction == nil || state.NextAction.Code != "recover_custody" {
		t.Fatalf("cancelled committed next action = %#v", state.NextAction)
	}
}

func TestCancellationReleaseRequiresVerifiedManagedHead(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name       string
		worktree   bool
		wantState  string
		wantSafety string
		verified   bool
	}{
		{name: "verified equal head releases", worktree: true, wantState: StateUserOwned, wantSafety: "user_owned", verified: true},
		{name: "missing worktree keeps custody", wantState: StatePipelineOwned, wantSafety: "blocked_pipeline_owned_recoverable"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newUnmovedRecoverFixture(t, types.RunCancelled)
			if err := f.db.UpdateRunStatus(f.run.ID, types.RunPending); err != nil {
				t.Fatal(err)
			}
			f.run.Status = types.RunPending
			f.run.TerminalHeadVerifiedAt = nil

			workDir := filepath.Join(t.TempDir(), "missing-managed")
			if tt.worktree {
				workDir = filepath.Join(t.TempDir(), "managed")
				if err := gitpkg.WorktreeAdd(f.ctx, f.gate, workDir, f.submitted); err != nil {
					t.Fatal(err)
				}
			}
			p := paths.WithRoot(t.TempDir())
			if err := p.EnsureDirs(); err != nil {
				t.Fatal(err)
			}
			executor := pipelinepkg.NewExecutor(f.db, p, nil, nil, []pipelinepkg.Step{&unreachedCancellationStep{}}, nil)
			ctx, cancel := context.WithCancelCause(context.Background())
			cancel(errors.New(types.RunCancelReasonAbortedByUser))
			if err := executor.Execute(ctx, f.run, f.repo, workDir); err == nil {
				t.Fatal("cancelled executor returned nil")
			}

			terminal, err := f.db.GetRun(f.run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if (terminal.TerminalHeadVerifiedAt != nil) != tt.verified {
				t.Fatalf("terminal verification = %#v, want verified %t", terminal.TerminalHeadVerifiedAt, tt.verified)
			}
			state := f.service.InspectCached(f.ctx)
			if state.State != tt.wantState || state.Safety != tt.wantSafety {
				t.Fatalf("state = %#v, want %s/%s", state, tt.wantState, tt.wantSafety)
			}
		})
	}
}

func TestSuccessfulSkippedDeliveryReleasesVerifiedUnmovedHead(t *testing.T) {
	t.Parallel()

	f := newUnmovedRecoverFixture(t, types.RunCancelled)
	if err := f.db.UpdateRunStatus(f.run.ID, types.RunPending); err != nil {
		t.Fatal(err)
	}
	f.run.Status = types.RunPending
	f.run.TerminalHeadVerifiedAt = nil

	managed := filepath.Join(t.TempDir(), "managed")
	if err := gitpkg.WorktreeAdd(f.ctx, f.gate, managed, f.submitted); err != nil {
		t.Fatal(err)
	}
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	steps := []pipelinepkg.Step{
		&skippedDeliveryStep{name: types.StepPush},
		&skippedDeliveryStep{name: types.StepPR},
		&skippedDeliveryStep{name: types.StepCI},
	}
	executor := pipelinepkg.NewExecutor(f.db, p, nil, nil, steps, nil)
	executor.SetSkippedSteps([]types.StepName{types.StepPush, types.StepPR, types.StepCI})
	if err := executor.Execute(context.Background(), f.run, f.repo, managed); err != nil {
		t.Fatal(err)
	}

	terminal, err := f.db.GetRun(f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if terminal.Status != types.RunCompleted || terminal.TerminalHeadVerifiedAt == nil || terminal.HeadSHA != f.submitted {
		t.Fatalf("terminal run = status %s head %s verified %#v", terminal.Status, terminal.HeadSHA, terminal.TerminalHeadVerifiedAt)
	}
	state := f.service.InspectCached(f.ctx)
	if state.State != StateUserOwned || state.Safety != "user_owned" {
		t.Fatalf("completed skipped-delivery state = %#v", state)
	}
	if state.NextAction != nil {
		t.Fatalf("completed skipped-delivery next action = %#v", state.NextAction)
	}
}

func TestLegacyTerminalUnmovedRunKeepsRecoverableCustody(t *testing.T) {
	t.Parallel()

	f := newUnmovedRecoverFixture(t, types.RunCancelled)
	if err := f.db.UpdateRunStatus(f.run.ID, types.RunCancelled); err != nil {
		t.Fatal(err)
	}
	state := f.service.InspectCached(f.ctx)
	if state.State != StatePipelineOwned || state.Safety != "blocked_pipeline_owned_recoverable" {
		t.Fatalf("legacy terminal state = %#v", state)
	}
	if state.NextAction == nil || state.NextAction.Code != "recover_custody" {
		t.Fatalf("legacy terminal next action = %#v", state.NextAction)
	}
}

// TestTerminalUnmovedPrePushRunReportsUserOwnedRelease is the regression test
// for the pre-push abort taken to switch delivery away from the pipeline: a
// terminal run whose head never moved must be selected and reported as
// user-owned with the exact branch/head ownership facts - never wrong-branch
// ambiguity, and never recoverable pipeline custody (cancellation releases
// ownership; there is nothing pipeline-created to recover).
func TestTerminalUnmovedPrePushRunReportsUserOwnedRelease(t *testing.T) {
	t.Parallel()

	for _, status := range []types.RunStatus{types.RunCancelled, types.RunFailed} {
		t.Run(string(status), func(t *testing.T) {
			f := newUnmovedRecoverFixture(t, status)
			state := f.service.InspectCached(f.ctx)
			if state.State != StateUserOwned || state.Safety != "user_owned" {
				t.Fatalf("state = %s safety = %s error = %q, want user_owned/user_owned", state.State, state.Safety, state.Error)
			}
			if state.Pipeline.RunID != f.run.ID || state.Pipeline.Status != string(status) {
				t.Fatalf("pipeline = %#v", state.Pipeline)
			}
			if state.Pipeline.SubmittedHead != f.submitted || state.Pipeline.CurrentHead != f.submitted {
				t.Fatalf("pipeline heads = %#v, want submitted==current==%s", state.Pipeline, f.submitted)
			}
			if state.Local.Branch != "feature/recover" || state.Local.Head != f.submitted {
				t.Fatalf("local = %#v", state.Local)
			}
			if state.Relation != RelationEqual {
				t.Fatalf("relation = %s, want equal", state.Relation)
			}
			if state.NextAction != nil {
				t.Fatalf("released branch must need no sync action, got %#v", state.NextAction)
			}
			if state.Error != "" {
				t.Fatalf("released branch must not report an error, got %q", state.Error)
			}
		})
	}
}

// TestActiveUnmovedRunBlocksAsPipelineOwnedWithoutRecovery pins the active half
// of the unmoved shape: while the run is active the branch is pipeline-owned
// with a precise reason, recovery refuses as run-active, and nothing mutates.
func TestActiveUnmovedRunBlocksAsPipelineOwnedWithoutRecovery(t *testing.T) {
	t.Parallel()

	f := newUnmovedRecoverFixture(t, types.RunRunning)
	state := f.service.InspectCached(f.ctx)
	if state.State != StatePipelineOwned || state.Safety != "blocked_pipeline_owned" {
		t.Fatalf("active unmoved state = %s/%s error=%q", state.State, state.Safety, state.Error)
	}
	if state.NextAction == nil || state.NextAction.Code != "continue_active_run" || state.NextAction.Command != "no-mistakes axi status" {
		t.Fatalf("active unmoved next action = %#v", state.NextAction)
	}
	recovered := f.service.Recover(f.ctx, false)
	if recovered.Recovered || recovered.Safety != "blocked_recover_run_active" {
		t.Fatalf("recover on active unmoved run = %#v", recovered)
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.submitted {
		t.Fatal("recover on active unmoved run moved HEAD")
	}
	if f.custodyReturned() {
		t.Fatal("recover on active unmoved run stamped custody")
	}
}

// assertReleasedNoOpRecover runs Recover on a released (user_owned) branch and
// proves the contract: idempotent no-op success that mutates no file, ref, or
// database row - no worktree move, no anchor ref, no custody stamp, no gate
// rewrite.
func assertReleasedNoOpRecover(t *testing.T, f *recoverFixture, keepLocal bool, wantHead string) {
	t.Helper()
	gateHead, gateErr := gitpkg.Run(f.ctx, f.gate, "rev-parse", "refs/heads/feature/recover")
	state := f.service.Recover(f.ctx, keepLocal)
	if !state.Recovered || state.Changed || state.State != StateUserOwned {
		t.Fatalf("released recover = %#v", state)
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != wantHead {
		t.Fatalf("released recover moved HEAD to %s, want %s", got, wantHead)
	}
	if _, err := gitpkg.Run(f.ctx, f.local, "rev-parse", f.anchorRef()); err == nil {
		t.Fatal("released recover wrote an anchor ref")
	}
	if f.custodyReturned() {
		t.Fatal("released recover stamped custody")
	}
	if gateErr == nil {
		if got := mustRun(t, f.gate, "rev-parse", "refs/heads/feature/recover"); got != strings.TrimSpace(gateHead) {
			t.Fatalf("released recover moved the gate branch to %s", got)
		}
	}
}

// TestRecoverOnReleasedBranchIsIdempotentNoOp: cancellation already released
// the branch, so recovery has nothing to do and repeating it changes nothing.
func TestRecoverOnReleasedBranchIsIdempotentNoOp(t *testing.T) {
	t.Parallel()

	f := newUnmovedRecoverFixture(t, types.RunCancelled)
	assertReleasedNoOpRecover(t, f, false, f.submitted)
	assertReleasedNoOpRecover(t, f, false, f.submitted)
}

// TestReleasedBranchWithDirtyDirectPRWorkStaysUserOwnedUntouched: in-progress
// direct-PR edits are the operator's own work on their own branch; status
// stays user_owned with the dirt exposed as a fact, and recovery leaves every
// byte alone.
func TestReleasedBranchWithDirtyDirectPRWorkStaysUserOwnedUntouched(t *testing.T) {
	t.Parallel()

	f := newUnmovedRecoverFixture(t, types.RunCancelled)
	mustWrite(t, filepath.Join(f.local, "file.txt"), "direct-pr edit\n")
	state := f.service.InspectCached(f.ctx)
	if state.State != StateUserOwned || state.Local.Clean {
		t.Fatalf("dirty released state = %#v", state)
	}
	assertReleasedNoOpRecover(t, f, false, f.submitted)
	if got := readOptional(t, filepath.Join(f.local, "file.txt")); got != "direct-pr edit\n" {
		t.Fatalf("released recover touched worktree files: %q", got)
	}
}

// TestReleasedBranchIgnoresHiddenManagedCopyTip pins that the release binds to
// the run's recorded head, not whatever tip the managed gate copy happens to
// hold: an out-of-band gate mutation neither leaks into the worktree nor gets
// rewritten, and the branch stays user-owned.
func TestReleasedBranchIgnoresHiddenManagedCopyTip(t *testing.T) {
	t.Parallel()

	f := newUnmovedRecoverFixture(t, types.RunCancelled)
	writer := filepath.Join(t.TempDir(), "writer")
	mustRun(t, filepath.Dir(writer), "-c", "core.autocrlf=false", "clone", f.gate, writer)
	configureIdentity(t, writer)
	mustRun(t, writer, "checkout", "feature/recover")
	mustWrite(t, filepath.Join(writer, "hidden.txt"), "hidden\n")
	mustRun(t, writer, "add", "hidden.txt")
	mustRun(t, writer, "commit", "-m", "out of band gate commit")
	mustRun(t, writer, "push", "origin", "HEAD:refs/heads/feature/recover")
	hidden := mustRun(t, f.gate, "rev-parse", "refs/heads/feature/recover")

	state := f.service.InspectCached(f.ctx)
	if state.State != StateUserOwned {
		t.Fatalf("released state with hidden managed tip = %#v", state)
	}
	assertReleasedNoOpRecover(t, f, false, f.submitted)
	if got := mustRun(t, f.gate, "rev-parse", "refs/heads/feature/recover"); got != hidden {
		t.Fatalf("release rewrote the gate branch to %s", got)
	}
}

// TestReleasedBranchAfterUserResetOrDivergenceStaysUserOwned: once released,
// the branch is the operator's - resetting behind the submitted head or
// rewriting it is their own action, and no recovery path may "correct" it by
// moving the worktree or the gate, clean, dirty, or with --keep-local.
func TestReleasedBranchAfterUserResetOrDivergenceStaysUserOwned(t *testing.T) {
	t.Parallel()

	t.Run("reset behind clean", func(t *testing.T) {
		f := newUnmovedRecoverFixture(t, types.RunCancelled)
		mustRun(t, f.local, "reset", "--hard", f.base)
		state := f.service.InspectCached(f.ctx)
		if state.State != StateUserOwned || state.Relation != RelationBehind {
			t.Fatalf("released behind state = %s relation %s", state.State, state.Relation)
		}
		assertReleasedNoOpRecover(t, f, false, f.base)
	})
	t.Run("reset behind dirty", func(t *testing.T) {
		f := newUnmovedRecoverFixture(t, types.RunCancelled)
		mustRun(t, f.local, "reset", "--hard", f.base)
		mustWrite(t, filepath.Join(f.local, "file.txt"), "dirty\n")
		assertReleasedNoOpRecover(t, f, false, f.base)
	})
	t.Run("diverged with keep-local", func(t *testing.T) {
		f := newUnmovedRecoverFixture(t, types.RunCancelled)
		mustRun(t, f.local, "reset", "--hard", f.base)
		mustWrite(t, filepath.Join(f.local, "rescope.txt"), "rescope\n")
		mustRun(t, f.local, "add", "rescope.txt")
		mustRun(t, f.local, "commit", "-m", "diverging rescope")
		divergedHead := mustRun(t, f.local, "rev-parse", "HEAD")
		state := f.service.InspectCached(f.ctx)
		if state.State != StateUserOwned || state.Relation != RelationDiverged {
			t.Fatalf("released diverged state = %s relation %s", state.State, state.Relation)
		}
		assertReleasedNoOpRecover(t, f, true, divergedHead)
	})
}

// TestUnmovedRunSelectionPrefersNewerAuthoritativeRuns pins multi-run
// disambiguation on one branch: a newer active run or a newer exact pushed
// binding governs, and the stranded older run can never steal selection or be
// recovered underneath them.
func TestUnmovedRunSelectionPrefersNewerAuthoritativeRuns(t *testing.T) {
	t.Parallel()

	t.Run("newer active run wins", func(t *testing.T) {
		f := newUnmovedRecoverFixture(t, types.RunCancelled)
		time.Sleep(1100 * time.Millisecond)
		fresh, err := f.db.InsertRun(f.repo.ID, "feature/recover", f.submitted, f.base)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.db.UpdateRunStatus(fresh.ID, types.RunRunning); err != nil {
			t.Fatal(err)
		}
		state := f.service.InspectCached(f.ctx)
		if state.Pipeline.RunID != fresh.ID || state.Safety != "blocked_pipeline_owned" {
			t.Fatalf("selection with newer active run = %#v", state.Pipeline)
		}
		recovered := f.service.Recover(f.ctx, false)
		if recovered.Recovered || recovered.Safety != "blocked_recover_run_active" {
			t.Fatalf("recover under newer active run = %#v", recovered)
		}
		if f.custodyReturned() {
			t.Fatal("recover under newer active run stamped custody on the old run")
		}
	})
	t.Run("newer pushed binding wins", func(t *testing.T) {
		f := newUnmovedRecoverFixture(t, types.RunCancelled)
		time.Sleep(1100 * time.Millisecond)
		mustRun(t, f.local, "push", f.remote, "refs/heads/feature/recover:refs/heads/feature/recover")
		newer, err := f.db.InsertRun(f.repo.ID, "feature/recover", f.submitted, f.base)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.db.UpdateRunPushBinding(newer.ID, db.PushBinding{
			HeadSHA: f.submitted, TargetKind: "upstream", TargetFingerprint: TargetFingerprint(f.remote), Ref: "refs/heads/feature/recover",
		}); err != nil {
			t.Fatal(err)
		}
		if err := f.db.UpdateRunStatus(newer.ID, types.RunCompleted); err != nil {
			t.Fatal(err)
		}
		state := f.service.InspectCached(f.ctx)
		if state.Pipeline.RunID != newer.ID || state.State != StateSynchronized {
			t.Fatalf("selection with newer pushed binding = %s %#v", state.State, state.Pipeline)
		}
	})
}

// TestUnmovedRunWrongContextsStayRefusedWithoutStamp: a different checked-out
// branch and a detached HEAD keep their precise refusals, and neither path can
// stamp custody on the stranded run.
func TestUnmovedRunWrongContextsStayRefusedWithoutStamp(t *testing.T) {
	t.Parallel()

	t.Run("different branch", func(t *testing.T) {
		f := newUnmovedRecoverFixture(t, types.RunCancelled)
		mustRun(t, f.local, "checkout", "-b", "feature/other")
		state := f.service.InspectCached(f.ctx)
		if state.State != StateAmbiguousContext || state.Safety != "blocked_wrong_branch" {
			t.Fatalf("different-branch state = %#v", state)
		}
		recovered := f.service.Recover(f.ctx, false)
		if recovered.Recovered || recovered.Safety != "blocked_recover_not_applicable" {
			t.Fatalf("different-branch recover = %#v", recovered)
		}
		if f.custodyReturned() {
			t.Fatal("different-branch recover stamped custody")
		}
	})
	t.Run("detached head", func(t *testing.T) {
		f := newUnmovedRecoverFixture(t, types.RunCancelled)
		mustRun(t, f.local, "checkout", "--detach", f.submitted)
		state := f.service.InspectCached(f.ctx)
		if state.State != StateAmbiguousContext {
			t.Fatalf("detached state = %#v", state)
		}
		recovered := f.service.Recover(f.ctx, false)
		if recovered.Recovered || recovered.Safety != "blocked_recover_not_applicable" {
			t.Fatalf("detached recover = %#v", recovered)
		}
		if f.custodyReturned() {
			t.Fatal("detached recover stamped custody")
		}
	})
}

// TestRecoverConcurrentGatePushLosesCleanly: the keep-local gate reset is an
// atomic compare-and-swap, so a racing push to the gate wins and recovery
// refuses instead of clobbering the newer gate head.
func TestRecoverConcurrentGatePushLosesCleanly(t *testing.T) {
	t.Parallel()

	f := newRecoverFixture(t, types.RunCancelled)
	mustWrite(t, filepath.Join(f.local, "rescope.txt"), "rescope\n")
	mustRun(t, f.local, "add", "rescope.txt")
	mustRun(t, f.local, "commit", "-m", "diverging rescope")
	f.service.beforeGateReset = func() {
		writer := filepath.Join(t.TempDir(), "racer")
		mustRun(t, filepath.Dir(writer), "-c", "core.autocrlf=false", "clone", f.gate, writer)
		configureIdentity(t, writer)
		mustRun(t, writer, "checkout", "feature/recover")
		mustWrite(t, filepath.Join(writer, "race.txt"), "race\n")
		mustRun(t, writer, "add", "race.txt")
		mustRun(t, writer, "commit", "-m", "racing push")
		mustRun(t, writer, "push", "origin", "HEAD:refs/heads/feature/recover")
	}
	state := f.service.Recover(f.ctx, true)
	if state.Recovered || state.Safety != "blocked_recover_gate_race" {
		t.Fatalf("racing keep-local recover = %#v", state)
	}
	if f.custodyReturned() {
		t.Fatal("racing recover stamped custody")
	}
}

// newRebasedRecoverFixture reproduces the cancelled-validation custody state
// whose preserved pipeline head is a REBASE of the operator's local branch: the
// default branch advanced after the branch was submitted, so the pipeline
// replayed the same logical commits onto the newer base (new SHAs, identical
// content) and added a fix commit before the run was cancelled. No local commit
// is an ancestor of the preserved head and no preserved commit is an ancestor
// of the local head, so a plain equality/ancestry test sees only "diverged"
// even though the preserved head already contains every local change.
func newRebasedRecoverFixture(t *testing.T, status types.RunStatus) *recoverFixture {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	remote := filepath.Join(root, "upstream.git")
	mustRun(t, root, "init", "--bare", remote)

	local := filepath.Join(root, "operator")
	mustRun(t, root, "init", "-b", "main", local)
	configureIdentity(t, local)
	mustWrite(t, filepath.Join(local, "file.txt"), "base\n")
	mustRun(t, local, "add", "file.txt")
	mustRun(t, local, "commit", "-m", "base")
	base := mustRun(t, local, "rev-parse", "HEAD")

	mustRun(t, local, "checkout", "-b", "feature/recover")
	mustWrite(t, filepath.Join(local, "feature.txt"), "feature one\n")
	mustRun(t, local, "add", "feature.txt")
	mustRun(t, local, "commit", "-m", "feature one")
	mustWrite(t, filepath.Join(local, "feature.txt"), "feature one\nfeature two\n")
	mustRun(t, local, "commit", "-am", "feature two")
	submitted := mustRun(t, local, "rev-parse", "HEAD")

	// The default branch advances after submission; that is what makes the
	// pipeline rebase produce new SHAs for the same logical commits.
	mustRun(t, local, "checkout", "main")
	mustWrite(t, filepath.Join(local, "upstream.txt"), "upstream advance\n")
	mustRun(t, local, "add", "upstream.txt")
	mustRun(t, local, "commit", "-m", "upstream advance")
	mustRun(t, local, "checkout", "feature/recover")

	gate := filepath.Join(root, "gate.git")
	mustRun(t, root, "init", "--bare", gate)
	mustRun(t, local, "push", gate, "refs/heads/main:refs/heads/main", "refs/heads/feature/recover:refs/heads/feature/recover")

	pipeline := filepath.Join(root, "pipeline")
	mustRun(t, root, "-c", "core.autocrlf=false", "clone", gate, pipeline)
	configureIdentity(t, pipeline)
	mustRun(t, pipeline, "checkout", "feature/recover")
	mustRun(t, pipeline, "rebase", "origin/main")
	// The fix round supersedes the operator's own lines, which is what the
	// pipeline exists to do; the operator's commits stay patch-present in the
	// history underneath it.
	mustWrite(t, filepath.Join(pipeline, "feature.txt"), "feature one\nfeature two guarded\n")
	mustWrite(t, filepath.Join(pipeline, "fix.txt"), "pipeline fix\n")
	mustRun(t, pipeline, "add", "feature.txt", "fix.txt")
	mustRun(t, pipeline, "commit", "-m", "no-mistakes(review): fix")
	preserved := mustRun(t, pipeline, "rev-parse", "HEAD")
	mustRun(t, pipeline, "push", "--force", "origin", "HEAD:refs/heads/feature/recover")

	database, err := db.Open(filepath.Join(root, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	repo, err := database.InsertRepo(local, remote, "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "feature/recover", submitted, base)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunHeadSHA(run.ID, preserved); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatusWithVerifiedHead(run.ID, status, preserved); err != nil {
		t.Fatal(err)
	}
	run, _ = database.GetRun(run.ID)
	return &recoverFixture{
		t: t, ctx: ctx, db: database, repo: repo, run: run,
		service: &Service{DB: database, Repo: repo, WorkDir: local, GateDir: gate},
		local:   local, gate: gate, remote: remote,
		base: base, submitted: submitted, preserved: preserved,
	}
}

func (f *recoverFixture) localAnchorRef() string {
	return "refs/no-mistakes/recover-local/" + f.run.ID
}

// TestRecoverRebaseSupersetAdoptsPreservedHeadWithoutEscalating is the
// regression for the over-escalating custody return: a cancelled validation
// whose preserved pipeline head is a rebase-superset of the local branch loses
// nothing by adopting it, so recovery must succeed instead of refusing as
// diverged. The relationship is invisible to equality/ancestry alone, which is
// exactly what made the old decision escalate.
func TestRecoverRebaseSupersetAdoptsPreservedHeadWithoutEscalating(t *testing.T) {
	t.Parallel()

	f := newRebasedRecoverFixture(t, types.RunCancelled)
	if mustRun(t, f.local, "rev-parse", "HEAD") != f.submitted {
		t.Fatal("fixture did not leave the operator worktree at the submitted head")
	}
	// The bug's masking condition: neither head is an ancestor of the other, so
	// equality and ancestry alone cannot see that the preserved head already
	// carries every local commit.
	mustRun(t, f.local, "fetch", "--no-tags", f.gate, "+refs/heads/feature/recover:refs/no-mistakes/test/preserved")
	if isAncestor(f.ctx, f.local, f.submitted, f.preserved) || isAncestor(f.ctx, f.local, f.preserved, f.submitted) {
		t.Fatal("fixture is not a rebase divergence: one head is an ancestor of the other")
	}

	state := f.service.Recover(f.ctx, false)
	if !state.Recovered || !state.Changed {
		t.Fatalf("rebase-superset recovery escalated instead of returning custody: %#v", state)
	}
	if state.State != StateCustodyReturned || state.Safety != "custody_returned" {
		t.Fatalf("post-recover state = %s/%s", state.State, state.Safety)
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.preserved {
		t.Fatalf("HEAD = %s, want preserved %s", got, f.preserved)
	}
	// The operator's commits are patch-present under the preserved head, and
	// the fix round that superseded their content is the pipeline output the
	// operator is recovering.
	if got := readOptional(t, filepath.Join(f.local, "feature.txt")); got != "feature one\nfeature two guarded\n" {
		t.Fatalf("adopted content = %q", got)
	}
	if !strings.Contains(mustRun(t, f.local, "log", "--format=%s", f.preserved), "feature two") {
		t.Fatal("adopted head does not carry the operator's replayed commits")
	}
	if got := mustRun(t, f.local, "rev-parse", f.anchorRef()); got != f.preserved {
		t.Fatalf("preserved anchor = %s, want %s", got, f.preserved)
	}
	if got := mustRun(t, f.local, "rev-parse", f.localAnchorRef()); got != f.submitted {
		t.Fatalf("pre-recovery local head was not anchored: %s, want %s", got, f.submitted)
	}
	if !f.custodyReturned() {
		t.Fatal("custody not stamped")
	}
}

// TestRecoverRebasedPreservedHeadStillEscalatesForUniqueLocalWork is the
// disconfirming counterfactual for the same fixture: one genuinely unique local
// commit whose content the preserved head does not carry must keep escalating,
// because adopting the preserved head would silently discard unlanded work.
func TestRecoverRebasedPreservedHeadStillEscalatesForUniqueLocalWork(t *testing.T) {
	t.Parallel()

	f := newRebasedRecoverFixture(t, types.RunCancelled)
	mustWrite(t, filepath.Join(f.local, "unlanded.txt"), "unlanded work\n")
	mustRun(t, f.local, "add", "unlanded.txt")
	mustRun(t, f.local, "commit", "-m", "unlanded local work")
	uniqueHead := mustRun(t, f.local, "rev-parse", "HEAD")

	state := f.service.Recover(f.ctx, false)
	if state.Recovered || state.Changed {
		t.Fatalf("unique local work was auto-recovered: %#v", state)
	}
	if state.Safety != "blocked_recover_diverged" || state.Relation != RelationDiverged {
		t.Fatalf("recover with unique local work = %s/%s", state.Safety, state.Relation)
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != uniqueHead {
		t.Fatalf("HEAD moved to %s despite unique local work", got)
	}
	if got := readOptional(t, filepath.Join(f.local, "unlanded.txt")); got != "unlanded work\n" {
		t.Fatalf("unlanded work lost: %q", got)
	}
	if f.custodyReturned() {
		t.Fatal("escalation stamped custody")
	}
}

func TestRecoverRebasedPreservedHeadRequiresDistinctPatchCounterparts(t *testing.T) {
	t.Parallel()

	f := newRebasedRecoverFixture(t, types.RunCancelled)
	mustWrite(t, filepath.Join(f.local, "repeated.txt"), "repeated work\n")
	mustRun(t, f.local, "add", "repeated.txt")
	mustRun(t, f.local, "commit", "-m", "apply repeated work")
	firstApplication := mustRun(t, f.local, "rev-parse", "HEAD")
	mustRun(t, f.local, "revert", "--no-edit", firstApplication)
	mustRun(t, f.local, "cherry-pick", firstApplication)
	uniqueHead := mustRun(t, f.local, "rev-parse", "HEAD")

	pipeline := filepath.Join(filepath.Dir(f.local), "multiplicity")
	mustRun(t, filepath.Dir(f.local), "-c", "core.autocrlf=false", "clone", f.gate, pipeline)
	configureIdentity(t, pipeline)
	mustRun(t, pipeline, "checkout", "feature/recover")
	mustWrite(t, filepath.Join(pipeline, "repeated.txt"), "repeated work\n")
	mustRun(t, pipeline, "add", "repeated.txt")
	mustRun(t, pipeline, "commit", "-m", "apply repeated work")
	mustRun(t, pipeline, "revert", "--no-edit", "HEAD")
	preserved := mustRun(t, pipeline, "rev-parse", "HEAD")
	mustRun(t, pipeline, "push", "--force", "origin", "HEAD:refs/heads/feature/recover")
	if err := f.db.UpdateRunHeadSHA(f.run.ID, preserved); err != nil {
		t.Fatal(err)
	}
	if err := f.db.UpdateRunStatusWithVerifiedHead(f.run.ID, types.RunCancelled, preserved); err != nil {
		t.Fatal(err)
	}

	state := f.service.Recover(f.ctx, false)
	if state.Recovered || state.Changed || state.Safety != "blocked_recover_diverged" {
		t.Fatalf("unpaired repeated patch was auto-recovered: %#v", state)
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != uniqueHead {
		t.Fatalf("HEAD moved to %s despite an unpaired patch occurrence", got)
	}
	if got := readOptional(t, filepath.Join(f.local, "repeated.txt")); got != "repeated work\n" {
		t.Fatalf("repeated local work lost: %q", got)
	}
	if f.custodyReturned() {
		t.Fatal("unpaired-patch escalation stamped custody")
	}
}

func TestRecoverRebasedPreservedHeadRejectsPatchLocationCollision(t *testing.T) {
	t.Parallel()

	f := newRebasedRecoverFixture(t, types.RunCancelled)
	block := "block start\ncontext a\ncontext b\ncontext c\ntarget\ncontext d\ncontext e\ncontext f\nblock end\n"
	blocks := block + "separator one\nseparator two\nseparator three\nseparator four\nseparator five\nseparator six\nseparator seven\n" + block
	mustWrite(t, filepath.Join(f.local, "collision.txt"), blocks)
	mustRun(t, f.local, "add", "collision.txt")
	mustRun(t, f.local, "commit", "-m", "add identical blocks")
	localBlocks := strings.Replace(blocks, "target", "changed", 1)
	mustWrite(t, filepath.Join(f.local, "collision.txt"), localBlocks)
	mustRun(t, f.local, "commit", "-am", "change first block")
	uniqueHead := mustRun(t, f.local, "rev-parse", "HEAD")
	localPatchID, localSignature, err := gitpkg.PatchIdentity(f.ctx, f.local, uniqueHead)
	if err != nil {
		t.Fatal(err)
	}

	pipeline := filepath.Join(filepath.Dir(f.local), "patch-location")
	mustRun(t, filepath.Dir(f.local), "-c", "core.autocrlf=false", "clone", f.gate, pipeline)
	configureIdentity(t, pipeline)
	mustRun(t, pipeline, "checkout", "feature/recover")
	mustWrite(t, filepath.Join(pipeline, "collision.txt"), blocks)
	mustRun(t, pipeline, "add", "collision.txt")
	mustRun(t, pipeline, "commit", "-m", "add identical blocks")
	secondTarget := strings.LastIndex(blocks, "target")
	preservedBlocks := blocks[:secondTarget] + "changed" + blocks[secondTarget+len("target"):]
	mustWrite(t, filepath.Join(pipeline, "collision.txt"), preservedBlocks)
	mustRun(t, pipeline, "commit", "-am", "change second block")
	preserved := mustRun(t, pipeline, "rev-parse", "HEAD")
	preservedPatchID, preservedSignature, err := gitpkg.PatchIdentity(f.ctx, pipeline, preserved)
	if err != nil {
		t.Fatal(err)
	}
	if localPatchID != preservedPatchID || localSignature == preservedSignature {
		t.Fatalf("fixture is not a location collision: IDs %q/%q signatures equal=%v", localPatchID, preservedPatchID, localSignature == preservedSignature)
	}
	mustRun(t, pipeline, "push", "--force", "origin", "HEAD:refs/heads/feature/recover")
	if err := f.db.UpdateRunHeadSHA(f.run.ID, preserved); err != nil {
		t.Fatal(err)
	}
	if err := f.db.UpdateRunStatusWithVerifiedHead(f.run.ID, types.RunCancelled, preserved); err != nil {
		t.Fatal(err)
	}

	state := f.service.Recover(f.ctx, false)
	if state.Recovered || state.Changed || state.Safety != "blocked_recover_diverged" {
		t.Fatalf("patch location collision was auto-recovered: %#v", state)
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != uniqueHead {
		t.Fatalf("HEAD moved to %s despite a patch location collision", got)
	}
	if got := readOptional(t, filepath.Join(f.local, "collision.txt")); got != localBlocks {
		t.Fatalf("local collision edit lost: %q", got)
	}
	if f.custodyReturned() {
		t.Fatal("patch-collision escalation stamped custody")
	}
}

func TestRecoverRebaseSupersetRechecksAfterAnchoringLocalHead(t *testing.T) {
	t.Parallel()

	f := newRebasedRecoverFixture(t, types.RunCancelled)
	hook := filepath.Join(f.local, ".git", "hooks", "reference-transaction")
	mustWrite(t, hook, "#!/bin/sh\nif [ \"$1\" = prepared ]; then\n  while read old new ref; do\n    case \"$ref\" in\n      refs/no-mistakes/recover-local/*) printf 'hook work\\n' > hook-created.txt ;;\n    esac\n  done\nfi\n")
	if err := os.Chmod(hook, 0o755); err != nil {
		t.Fatal(err)
	}

	state := f.service.Recover(f.ctx, false)
	if state.Recovered || state.Changed || state.Safety != "blocked_recover_assumptions_changed" {
		t.Fatalf("recover after anchor hook mutation = %#v", state)
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.submitted {
		t.Fatalf("anchor hook mutation was reset to %s", got)
	}
	if got := readOptional(t, filepath.Join(f.local, "hook-created.txt")); got != "hook work\n" {
		t.Fatalf("anchor hook work was lost: %q", got)
	}
	if got := mustRun(t, f.local, "rev-parse", f.localAnchorRef()); got != f.submitted {
		t.Fatalf("atomic local anchor = %s, want %s", got, f.submitted)
	}
	if f.custodyReturned() {
		t.Fatal("anchor-race refusal stamped custody")
	}
}

func TestRecoverRebaseSupersetRechecksImmediatelyBeforeReset(t *testing.T) {
	t.Parallel()

	f := newRebasedRecoverFixture(t, types.RunCancelled)
	f.service.beforeRecoverReset = func() {
		mustWrite(t, filepath.Join(f.local, "last-moment.txt"), "last moment work\n")
		mustRun(t, f.local, "add", "last-moment.txt")
		mustRun(t, f.local, "commit", "-m", "last moment local work")
	}

	state := f.service.Recover(f.ctx, false)
	if state.Recovered || state.Changed || state.Safety != "blocked_recover_assumptions_changed" {
		t.Fatalf("recover after final-boundary commit = %#v", state)
	}
	lastMomentHead := mustRun(t, f.local, "rev-parse", "HEAD")
	if lastMomentHead == f.submitted || lastMomentHead == f.preserved {
		t.Fatalf("last-moment commit was not preserved at HEAD: %s", lastMomentHead)
	}
	if got := readOptional(t, filepath.Join(f.local, "last-moment.txt")); got != "last moment work\n" {
		t.Fatalf("last-moment work lost: %q", got)
	}
	if got := mustRun(t, f.local, "rev-parse", f.localAnchorRef()); got != f.submitted {
		t.Fatalf("pre-recovery anchor = %s, want %s", got, f.submitted)
	}
	if f.custodyReturned() {
		t.Fatal("final-boundary refusal stamped custody")
	}
}

func TestRecoverRebaseSupersetReportsAttemptedResetFailure(t *testing.T) {
	t.Parallel()

	f := newRebasedRecoverFixture(t, types.RunCancelled)
	hook := filepath.Join(f.local, ".git", "hooks", "reference-transaction")
	mustWrite(t, hook, "#!/bin/sh\nif [ \"$1\" = prepared ]; then\n  while read old new ref; do\n    case \"$ref\" in\n      refs/heads/feature/recover) exit 1 ;;\n    esac\n  done\nfi\n")
	if err := os.Chmod(hook, 0o755); err != nil {
		t.Fatal(err)
	}

	state := f.service.Recover(f.ctx, false)
	if state.Recovered || state.Changed || state.Safety != "blocked_recover_apply_failed" {
		t.Fatalf("recover after rejected reset = %#v", state)
	}
	if !strings.Contains(state.Error, "reset was attempted") || !strings.Contains(state.Error, f.localAnchorRef()) || !strings.Contains(state.Error, "inspect the worktree") {
		t.Fatalf("reset failure guidance = %q", state.Error)
	}
	if state.NextAction == nil || state.NextAction.Code != "inspect_worktree" || state.NextAction.Command != "git status" {
		t.Fatalf("reset failure next action = %#v", state.NextAction)
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.submitted {
		t.Fatalf("rejected reset moved HEAD to %s", got)
	}
	if got := mustRun(t, f.local, "rev-parse", f.localAnchorRef()); got != f.submitted {
		t.Fatalf("pre-reset local anchor = %s, want %s", got, f.submitted)
	}
	if f.custodyReturned() {
		t.Fatal("reset failure stamped custody")
	}
}

// TestRecoverRebaseSupersetRefusesDirtyWorktree keeps the uncommitted-work
// protection intact: adopting the preserved head is a hard reset, so a dirty
// worktree must refuse rather than overwrite files the operator has not
// committed. --keep-local remains the no-worktree-touch exit.
func TestRecoverRebaseSupersetRefusesDirtyWorktree(t *testing.T) {
	t.Parallel()

	f := newRebasedRecoverFixture(t, types.RunCancelled)
	mustWrite(t, filepath.Join(f.local, "feature.txt"), "uncommitted edit\n")

	state := f.service.Recover(f.ctx, false)
	if state.Recovered || state.Changed || state.Safety != "blocked_recover_dirty" {
		t.Fatalf("dirty rebase-superset recover = %#v", state)
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.submitted {
		t.Fatalf("dirty refusal moved HEAD to %s", got)
	}
	if got := readOptional(t, filepath.Join(f.local, "feature.txt")); got != "uncommitted edit\n" {
		t.Fatalf("dirty refusal overwrote uncommitted work: %q", got)
	}
	if f.custodyReturned() {
		t.Fatal("dirty refusal stamped custody")
	}
}

// TestRecoverRebaseSupersetKeepLocalStillKeepsTheLocalHead pins that the
// explicit keep-local choice still wins over the new adoption path.
func TestRecoverRebaseSupersetKeepLocalStillKeepsTheLocalHead(t *testing.T) {
	t.Parallel()

	f := newRebasedRecoverFixture(t, types.RunCancelled)
	state := f.service.Recover(f.ctx, true)
	if !state.Recovered || state.Changed {
		t.Fatalf("keep-local rebase-superset recover = %#v", state)
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.submitted {
		t.Fatalf("keep-local moved HEAD to %s", got)
	}
	if got := mustRun(t, f.gate, "rev-parse", "refs/heads/feature/recover"); got != f.submitted {
		t.Fatalf("gate branch = %s, want kept local head %s", got, f.submitted)
	}
	if got := mustRun(t, f.local, "rev-parse", f.anchorRef()); got != f.preserved {
		t.Fatal("keep-local lost the preserved anchor")
	}
}

// TestRecoverSquashedEquivalentPreservedHeadAdopts covers the second,
// independent containment proof: a fix round that AMENDS or squashes the
// operator's commits leaves no patch-identical counterpart, so per-commit
// replay cannot prove anything - but when the preserved head still carries the
// operator's exact content, adopting it loses nothing and must not escalate.
func TestRecoverSquashedEquivalentPreservedHeadAdopts(t *testing.T) {
	t.Parallel()

	f := newRebasedRecoverFixture(t, types.RunCancelled)
	// Rewrite the gate branch as one squashed commit carrying the operator's
	// exact content, exactly as a fix-round amend would leave it.
	pipeline := filepath.Join(filepath.Dir(f.local), "squash")
	mustRun(t, filepath.Dir(f.local), "-c", "core.autocrlf=false", "clone", f.gate, pipeline)
	configureIdentity(t, pipeline)
	mustRun(t, pipeline, "checkout", "feature/recover")
	mustRun(t, pipeline, "reset", "--hard", "HEAD~1")
	mustRun(t, pipeline, "reset", "--soft", "origin/main")
	mustRun(t, pipeline, "commit", "-m", "no-mistakes(rebase): squashed feature")
	squashed := mustRun(t, pipeline, "rev-parse", "HEAD")
	mustRun(t, pipeline, "push", "--force", "origin", "HEAD:refs/heads/feature/recover")
	if err := f.db.UpdateRunHeadSHA(f.run.ID, squashed); err != nil {
		t.Fatal(err)
	}
	if err := f.db.UpdateRunStatusWithVerifiedHead(f.run.ID, types.RunCancelled, squashed); err != nil {
		t.Fatal(err)
	}

	state := f.service.Recover(f.ctx, false)
	if !state.Recovered || !state.Changed || state.State != StateCustodyReturned {
		t.Fatalf("squashed-superset recovery = %#v", state)
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != squashed {
		t.Fatalf("HEAD = %s, want squashed preserved head %s", got, squashed)
	}
	if got := readOptional(t, filepath.Join(f.local, "feature.txt")); got != "feature one\nfeature two\n" {
		t.Fatalf("adopted content = %q", got)
	}
	if got := mustRun(t, f.local, "rev-parse", f.localAnchorRef()); got != f.submitted {
		t.Fatalf("pre-recovery local head was not anchored: %s", got)
	}
}

// TestRecoverSquashedPreservedHeadStillEscalatesForDroppedLocalWork is the
// counterfactual for that second proof, and pins the honest boundary of both:
// a squash that also changes the operator's lines destroys the per-commit
// provenance that would tell a deliberate fix apart from a dropped change, and
// the surviving content proof cannot cover it either. Neither proof holds, so
// recovery escalates rather than silently discarding the change.
func TestRecoverSquashedPreservedHeadStillEscalatesForDroppedLocalWork(t *testing.T) {
	t.Parallel()

	f := newRebasedRecoverFixture(t, types.RunCancelled)
	pipeline := filepath.Join(filepath.Dir(f.local), "squash-drop")
	mustRun(t, filepath.Dir(f.local), "-c", "core.autocrlf=false", "clone", f.gate, pipeline)
	configureIdentity(t, pipeline)
	mustRun(t, pipeline, "checkout", "feature/recover")
	mustRun(t, pipeline, "reset", "--soft", "origin/main")
	// The second of the operator's two lines never makes it into the squash.
	mustWrite(t, filepath.Join(pipeline, "feature.txt"), "feature one\n")
	mustRun(t, pipeline, "add", "feature.txt")
	mustRun(t, pipeline, "commit", "-m", "no-mistakes(review): squashed without the second line")
	dropped := mustRun(t, pipeline, "rev-parse", "HEAD")
	mustRun(t, pipeline, "push", "--force", "origin", "HEAD:refs/heads/feature/recover")
	if err := f.db.UpdateRunHeadSHA(f.run.ID, dropped); err != nil {
		t.Fatal(err)
	}
	if err := f.db.UpdateRunStatusWithVerifiedHead(f.run.ID, types.RunCancelled, dropped); err != nil {
		t.Fatal(err)
	}

	state := f.service.Recover(f.ctx, false)
	if state.Recovered || state.Changed || state.Safety != "blocked_recover_diverged" {
		t.Fatalf("dropped local work was auto-recovered: %#v", state)
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.submitted {
		t.Fatalf("HEAD moved to %s despite dropped local work", got)
	}
	if got := readOptional(t, filepath.Join(f.local, "feature.txt")); got != "feature one\nfeature two\n" {
		t.Fatalf("dropped-work escalation touched the worktree: %q", got)
	}
	if f.custodyReturned() {
		t.Fatal("dropped-work escalation stamped custody")
	}
}

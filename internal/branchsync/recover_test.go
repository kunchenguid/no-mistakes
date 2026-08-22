package branchsync

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/custody"
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
	// Pin hooks to the repo's own dir: an ambient global core.hooksPath
	// would silently hijack the hook installed below.
	mustRun(t, f.local, "config", "core.hooksPath", ".git/hooks")
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
	inspected := f.service.InspectCached(f.ctx)
	if inspected.NextAction == nil || inspected.NextAction.Code != "recover_custody" {
		t.Fatalf("equal status without gate did not advertise recovery = %#v", inspected)
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
	inspected := f.service.InspectCached(f.ctx)
	if inspected.NextAction == nil || inspected.NextAction.Code != "recover_custody" {
		t.Fatalf("ahead status without gate did not advertise recovery = %#v", inspected)
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
	mustRun(t, f.gate, "update-ref", f.anchorRef(), f.preserved)
	mustWrite(t, filepath.Join(f.local, "file.txt"), "dirty\n")
	inspected := f.service.InspectCached(f.ctx)
	if inspected.NextAction == nil || inspected.NextAction.Code == "recover_custody" {
		t.Fatalf("dirty behind status advertised recovery = %#v", inspected)
	}
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
	mustRun(t, f.gate, "update-ref", f.anchorRef(), f.preserved)
	mustWrite(t, filepath.Join(f.local, "rescope.txt"), "rescope\n")
	mustRun(t, f.local, "add", "rescope.txt")
	mustRun(t, f.local, "commit", "-m", "diverging rescope")
	divergedHead := mustRun(t, f.local, "rev-parse", "HEAD")
	inspected := f.service.InspectCached(f.ctx)
	if inspected.NextAction == nil || inspected.NextAction.Code == "recover_custody" {
		t.Fatalf("uncontained divergence advertised recovery = %#v", inspected)
	}

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

// TestRecoverGateDivergenceAndUnavailabilityFailClosed: an independently moved
// gate no longer hides a separately anchored preserved head, while deleted or
// unavailable preservation still refuses.
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
		movedGate := mustRun(t, f.gate, "rev-parse", "refs/heads/feature/recover")
		state := f.service.Recover(f.ctx, false)
		if !state.Recovered || !state.Changed {
			t.Fatalf("recover with moved gate = %#v", state)
		}
		if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.preserved {
			t.Fatalf("moved-gate recovery HEAD = %s, want %s", got, f.preserved)
		}
		if got := mustRun(t, f.gate, "rev-parse", "refs/heads/feature/recover"); got != movedGate {
			t.Fatalf("recovery rewrote independent gate head = %s, want %s", got, movedGate)
		}
	})
	t.Run("gate branch deleted with recovery ref", func(t *testing.T) {
		f := newRecoverFixture(t, types.RunCancelled)
		mustRun(t, f.gate, "update-ref", f.anchorRef(), f.preserved)
		mustRun(t, f.gate, "update-ref", "-d", "refs/heads/feature/recover")
		state := f.service.Recover(f.ctx, false)
		if !state.Recovered || !state.Changed {
			t.Fatalf("recover with deleted gate branch = %#v", state)
		}
		if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.preserved {
			t.Fatalf("recovered HEAD = %s, want %s", got, f.preserved)
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

func TestRecoverReachableHeadRejectsConflictingGateAnchor(t *testing.T) {
	t.Parallel()

	f := newRecoverFixture(t, types.RunCancelled)
	mustRun(t, f.gate, "update-ref", f.anchorRef(), f.submitted)
	mustRun(t, f.local, "fetch", f.gate, f.preserved)
	mustRun(t, f.local, "reset", "--hard", f.preserved)

	inspected := f.service.InspectCached(f.ctx)
	if inspected.NextAction == nil || inspected.NextAction.Code == "recover_custody" {
		t.Fatalf("inspect advertised recovery despite conflicting gate evidence = %#v", inspected)
	}

	state := f.service.Recover(f.ctx, false)
	if state.Recovered || state.Changed || state.Safety != "blocked_recover_anchor_mismatch" {
		t.Fatalf("recover with conflicting anchor = %#v", state)
	}
	if got := mustRun(t, f.local, "rev-parse", f.anchorRef()); got != f.preserved {
		t.Fatalf("local recovery anchor = %s, want %s", got, f.preserved)
	}
	if got := mustRun(t, f.gate, "rev-parse", f.anchorRef()); got != f.submitted {
		t.Fatalf("gate recovery anchor = %s, want preserved conflict %s", got, f.submitted)
	}
	if f.custodyReturned() {
		t.Fatal("conflicting recovery evidence stamped custody")
	}
}

func TestRecoverRejectsUnpeelableGateAnchorWithoutOverwritingIt(t *testing.T) {
	t.Parallel()

	f := newRecoverFixture(t, types.RunCancelled)
	blob := mustRun(t, f.gate, "hash-object", "-w", filepath.Join(f.local, "file.txt"))
	mustRun(t, f.gate, "update-ref", f.anchorRef(), blob)

	state := f.service.Recover(f.ctx, false)
	if state.Recovered || state.Safety != "blocked_recover_anchor_mismatch" {
		t.Fatalf("recover with unpeelable anchor = %#v", state)
	}
	if got := mustRun(t, f.gate, "rev-parse", f.anchorRef()); got != blob {
		t.Fatalf("recovery anchor = %s, want original blob %s", got, blob)
	}
	if f.custodyReturned() {
		t.Fatal("unpeelable recovery evidence stamped custody")
	}
}

func TestRecoverRejectsSymbolicGateAnchorWithoutOverwritingIt(t *testing.T) {
	t.Parallel()

	f := newRecoverFixture(t, types.RunCancelled)
	mustRun(t, f.gate, "symbolic-ref", f.anchorRef(), "refs/heads/feature/recover")
	mustRun(t, f.local, "fetch", f.gate, f.preserved)
	mustRun(t, f.local, "reset", "--hard", f.preserved)

	inspected := f.service.InspectCached(f.ctx)
	if inspected.NextAction == nil || inspected.NextAction.Code == "recover_custody" {
		t.Fatalf("inspect advertised recovery despite symbolic gate evidence = %#v", inspected)
	}
	state := f.service.Recover(f.ctx, false)
	if state.Recovered || state.Safety != "blocked_recover_anchor_mismatch" {
		t.Fatalf("recover with symbolic anchor = %#v", state)
	}
	if got := mustRun(t, f.gate, "symbolic-ref", f.anchorRef()); got != "refs/heads/feature/recover" {
		t.Fatalf("symbolic recovery anchor = %s, want refs/heads/feature/recover", got)
	}
}

func TestRecoverKeepLocalAnchorsIndependentlyMovedGateHead(t *testing.T) {
	t.Parallel()

	f := newRecoverFixture(t, types.RunCancelled)
	writer := filepath.Join(t.TempDir(), "writer")
	mustRun(t, filepath.Dir(writer), "-c", "core.autocrlf=false", "clone", f.gate, writer)
	configureIdentity(t, writer)
	mustRun(t, writer, "checkout", "feature/recover")
	mustWrite(t, filepath.Join(writer, "other.txt"), "independent gate work\n")
	mustRun(t, writer, "add", "other.txt")
	mustRun(t, writer, "commit", "-m", "independent gate work")
	mustRun(t, writer, "push", "origin", "HEAD:refs/heads/feature/recover")
	movedGate := mustRun(t, f.gate, "rev-parse", "refs/heads/feature/recover")

	recovered := f.service.Recover(f.ctx, true)
	if !recovered.Recovered || recovered.Changed {
		t.Fatalf("keep-local recovery = %#v", recovered)
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.submitted {
		t.Fatalf("keep-local moved HEAD = %s, want %s", got, f.submitted)
	}
	if got := mustRun(t, f.gate, "rev-parse", "refs/heads/feature/recover"); got != f.submitted {
		t.Fatalf("keep-local gate branch = %s, want %s", got, f.submitted)
	}
	if got := mustRun(t, f.gate, "rev-parse", "refs/no-mistakes/recover-gate/"+f.run.ID); got != movedGate {
		t.Fatalf("independent gate anchor = %s, want %s", got, movedGate)
	}
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
	if got := mustRun(t, f.gate, "rev-parse", f.anchorRef()+"^{commit}"); got != committed {
		t.Fatalf("terminal recovery anchor = %s, want %s", got, committed)
	}
	if got := mustRun(t, f.gate, "rev-parse", "refs/heads/feature/recover"); got != f.submitted {
		t.Fatalf("terminalization moved gate branch = %s, want submitted %s", got, f.submitted)
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

func TestRecoverUsesTerminalAnchorWhenGateBranchLags(t *testing.T) {
	t.Parallel()

	f := newRecoverFixture(t, types.RunCancelled)
	mustRun(t, f.gate, "update-ref", f.anchorRef(), f.preserved)
	mustRun(t, f.gate, "update-ref", "refs/heads/feature/recover", f.submitted, f.preserved)

	state := f.service.InspectCached(f.ctx)
	if state.NextAction == nil || state.NextAction.Code != "recover_custody" {
		t.Fatalf("anchored stale-gate state = %#v", state)
	}
	recovered := f.service.Recover(f.ctx, false)
	if !recovered.Recovered || !recovered.Changed {
		t.Fatalf("anchored stale-gate recovery = %#v", recovered)
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.preserved {
		t.Fatalf("recovered HEAD = %s, want %s", got, f.preserved)
	}
}

func TestInspectDoesNotAdvertiseRecoveryWhenRecordedHeadIsMissing(t *testing.T) {
	t.Parallel()

	f := newRecoverFixture(t, types.RunCancelled)
	missing := strings.Repeat("f", 40)
	if err := f.db.UpdateRunStatusWithVerifiedHead(f.run.ID, types.RunCancelled, missing); err != nil {
		t.Fatal(err)
	}

	state := f.service.InspectCached(f.ctx)
	if state.Safety != "blocked_recover_preserved_head_missing" {
		t.Fatalf("missing-head safety = %q, want blocked_recover_preserved_head_missing: %#v", state.Safety, state)
	}
	if state.NextAction == nil || state.NextAction.Code != "inspect_and_reconcile_manually" {
		t.Fatalf("missing-head next action = %#v", state.NextAction)
	}
	if state.NextAction.Code == "recover_custody" {
		t.Fatal("missing recorded head advertised an impossible recovery")
	}
}

func TestInspectDoesNotAdvertiseRecoveryWhenTerminalAnchorConflicts(t *testing.T) {
	t.Parallel()

	f := newRecoverFixture(t, types.RunCancelled)
	// The recorded preserved commit remains available in the gate, but the
	// run-specific evidence points elsewhere. Status must honor that conflict
	// instead of advertising a recovery command that Recover will refuse.
	mustRun(t, f.local, "fetch", f.gate, f.preserved)
	mustRun(t, f.gate, "update-ref", f.anchorRef(), f.submitted)

	state := f.service.InspectCached(f.ctx)
	if state.Safety != "blocked_recover_preserved_head_missing" {
		t.Fatalf("conflicting-anchor safety = %q, want blocked_recover_preserved_head_missing: %#v", state.Safety, state)
	}
	if state.NextAction == nil || state.NextAction.Code != "inspect_and_reconcile_manually" {
		t.Fatalf("conflicting-anchor next action = %#v", state.NextAction)
	}
	if state.NextAction.Code == "recover_custody" {
		t.Fatal("conflicting terminal anchor advertised a recovery that must fail")
	}
}

func TestInspectDoesNotAdvertiseRecoveryWhenTerminalAnchorIsNotACommit(t *testing.T) {
	t.Parallel()

	f := newRecoverFixture(t, types.RunCancelled)
	mustRun(t, f.local, "fetch", f.gate, f.preserved)
	blobPath := filepath.Join(f.local, "anchor-evidence.txt")
	mustWrite(t, blobPath, "conflicting evidence\n")
	blob := mustRun(t, f.gate, "hash-object", "-w", blobPath)
	mustRun(t, f.gate, "update-ref", f.anchorRef(), blob)

	state := f.service.InspectCached(f.ctx)
	if state.NextAction == nil || state.NextAction.Code != "inspect_and_reconcile_manually" {
		t.Fatalf("non-commit-anchor next action = %#v", state.NextAction)
	}
	if state.NextAction.Code == "recover_custody" {
		t.Fatal("non-commit terminal anchor advertised a recovery that must fail")
	}
	if got := mustRun(t, f.gate, "rev-parse", f.anchorRef()); got != blob {
		t.Fatalf("status inspection changed conflicting anchor: got %s, want %s", got, blob)
	}
}

func TestInspectDoesNotAdvertiseRecoveryFromLooseObjectWithoutUsableGate(t *testing.T) {
	t.Parallel()

	f := newRecoverFixture(t, types.RunCancelled)
	mustRun(t, f.local, "fetch", f.gate, f.preserved)
	f.service.GateDir = filepath.Join(t.TempDir(), "missing-gate.git")

	state := f.service.InspectCached(f.ctx)
	if state.NextAction == nil || state.NextAction.Code != "inspect_and_reconcile_manually" {
		t.Fatalf("loose-object-only next action = %#v", state.NextAction)
	}
	if state.NextAction.Code == "recover_custody" {
		t.Fatal("a locally present object advertised recovery even though the behind branch still requires gate evidence")
	}
}

func TestRecoverDoesNotOverwriteConflictingCheckoutAnchor(t *testing.T) {
	t.Parallel()

	f := newRecoverFixture(t, types.RunCancelled)
	mustRun(t, f.local, "update-ref", f.anchorRef(), f.submitted)

	state := f.service.Recover(f.ctx, false)
	if state.Recovered || state.Safety != "blocked_recover_anchor_mismatch" {
		t.Fatalf("recover with conflicting checkout anchor = %#v", state)
	}
	if got := mustRun(t, f.local, "rev-parse", f.anchorRef()+"^{commit}"); got != f.submitted {
		t.Fatalf("checkout recovery evidence was overwritten: got %s, want %s", got, f.submitted)
	}
	if f.custodyReturned() {
		t.Fatal("conflicting checkout recovery evidence stamped custody")
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

func TestRecoverRetryDoesNotOverwriteIndependentGateAnchor(t *testing.T) {
	t.Parallel()

	f := newRecoverFixture(t, types.RunCancelled)
	mustWrite(t, filepath.Join(f.local, "rescope.txt"), "rescope\n")
	mustRun(t, f.local, "add", "rescope.txt")
	mustRun(t, f.local, "commit", "-m", "diverging rescope")
	writer := filepath.Join(t.TempDir(), "writer")
	mustRun(t, filepath.Dir(writer), "-c", "core.autocrlf=false", "clone", f.gate, writer)
	configureIdentity(t, writer)
	mustRun(t, writer, "checkout", "feature/recover")
	mustWrite(t, filepath.Join(writer, "first.txt"), "first independent head\n")
	mustRun(t, writer, "add", "first.txt")
	mustRun(t, writer, "commit", "-m", "first independent head")
	mustRun(t, writer, "push", "origin", "HEAD:refs/heads/feature/recover")
	firstGate := mustRun(t, f.gate, "rev-parse", "refs/heads/feature/recover")
	f.service.beforeGateReset = func() {
		mustWrite(t, filepath.Join(writer, "second.txt"), "second independent head\n")
		mustRun(t, writer, "add", "second.txt")
		mustRun(t, writer, "commit", "-m", "second independent head")
		mustRun(t, writer, "push", "origin", "HEAD:refs/heads/feature/recover")
	}
	first := f.service.Recover(f.ctx, true)
	if first.Recovered || first.Safety != "blocked_recover_gate_race" {
		t.Fatalf("first recovery = %#v", first)
	}
	f.service.beforeGateReset = nil
	secondGate := mustRun(t, f.gate, "rev-parse", "refs/heads/feature/recover")
	second := f.service.Recover(f.ctx, true)
	if second.Recovered || second.Safety != "blocked_recover_preserve_failed" {
		t.Fatalf("retry recovery = %#v", second)
	}
	anchor := custody.RecoveryGateRef(f.run.ID)
	if got := mustRun(t, f.gate, "rev-parse", anchor); got != firstGate {
		t.Fatalf("independent gate anchor = %s, want original %s", got, firstGate)
	}
	if got := mustRun(t, f.gate, "rev-parse", "refs/heads/feature/recover"); got != secondGate {
		t.Fatalf("retry moved gate branch = %s, want %s", got, secondGate)
	}
}

// newRebasedRecoverFixture reproduces the reported cancelled-validation custody
// state: the default branch advanced after the branch was submitted, so the
// pipeline's rebase replayed the operator's commits onto the newer base (new
// SHAs, identical content) before the run was cancelled. No local commit is an
// ancestor of the preserved head and no preserved commit is an ancestor of the
// local head, so a plain equality/ancestry test sees only "diverged" even
// though the preserved head carries every local change.
func newRebasedRecoverFixture(t *testing.T, status types.RunStatus) *recoverFixture {
	t.Helper()
	return newRebasedRecoverFixtureWithPipelineWork(t, status, nil)
}

// newRebasedRecoverFixtureWithPipelineWork builds the same rebase state and
// then lets pipelineWork commit further pipeline changes on the gate branch,
// modelling the fix rounds a cancelled run may have produced.
func newRebasedRecoverFixtureWithPipelineWork(t *testing.T, status types.RunStatus, pipelineWork func(t *testing.T, pipelineDir string)) *recoverFixture {
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
	if pipelineWork != nil {
		pipelineWork(t, pipeline)
	}
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

// TestRecoverRebasedPreservedHeadAdoptsWithoutEscalating is the regression for
// the over-escalating custody return: a cancelled validation whose preserved
// pipeline head is the operator's own work rebased onto a newer base loses
// nothing by adopting it, so recovery must succeed instead of refusing as
// diverged. The relationship is invisible to equality and ancestry alone, which
// is exactly what made the old decision escalate.
func TestRecoverRebasedPreservedHeadAdoptsWithoutEscalating(t *testing.T) {
	t.Parallel()

	f := newRebasedRecoverFixture(t, types.RunCancelled)
	if mustRun(t, f.local, "rev-parse", "HEAD") != f.submitted {
		t.Fatal("fixture did not leave the operator worktree at the submitted head")
	}
	// The bug's masking condition: neither head is an ancestor of the other.
	mustRun(t, f.local, "fetch", "--no-tags", f.gate, "+refs/heads/feature/recover:refs/no-mistakes/test/preserved")
	if isAncestor(f.ctx, f.local, f.submitted, f.preserved) || isAncestor(f.ctx, f.local, f.preserved, f.submitted) {
		t.Fatal("fixture is not a rebase divergence: one head is an ancestor of the other")
	}

	state := f.service.Recover(f.ctx, false)
	if !state.Recovered || !state.Changed {
		t.Fatalf("rebased recovery escalated instead of returning custody: %#v", state)
	}
	if state.State != StateCustodyReturned || state.Safety != "custody_returned" {
		t.Fatalf("post-recover state = %s/%s", state.State, state.Safety)
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.preserved {
		t.Fatalf("HEAD = %s, want preserved %s", got, f.preserved)
	}
	if got := readOptional(t, filepath.Join(f.local, "feature.txt")); got != "feature one\nfeature two\n" {
		t.Fatalf("local work missing after adopting the preserved head: %q", got)
	}
	// The rebase carried the branch onto the advanced base, so the worktree
	// must now hold the newer base's files too.
	if got := readOptional(t, filepath.Join(f.local, "upstream.txt")); got != "upstream advance\n" {
		t.Fatalf("adopted head did not bring the advanced base into the worktree: %q", got)
	}
	if clean, reason := worktreeClean(f.ctx, f.local); !clean {
		t.Fatalf("worktree not clean after adoption: %s", reason)
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
// disconfirming counterfactual: one genuinely unique local commit whose content
// the preserved head does not carry must keep escalating, because adopting the
// preserved head would silently discard unlanded work.
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

// TestRecoverRebasedPreservedHeadEscalatesWhenFixRoundsRewroteOperatorLines
// pins the deliberate boundary of the narrowed contract. When the pipeline both
// rebased the branch and superseded the operator's own lines, the operator's
// content is genuinely absent from the preserved head and nothing available to
// recovery distinguishes a deliberate fix from a dropped change. No-data-loss
// wins: the ambiguous case escalates for an operator decision rather than being
// adopted on a patch-identity guess.
func TestRecoverRebasedPreservedHeadEscalatesWhenFixRoundsRewroteOperatorLines(t *testing.T) {
	t.Parallel()

	f := newRebasedRecoverFixtureWithPipelineWork(t, types.RunCancelled, func(t *testing.T, pipelineDir string) {
		mustWrite(t, filepath.Join(pipelineDir, "feature.txt"), "feature one\nfeature two guarded\n")
		mustRun(t, pipelineDir, "commit", "-am", "no-mistakes(review): guard the second line")
	})

	state := f.service.Recover(f.ctx, false)
	if state.Recovered || state.Changed {
		t.Fatalf("ambiguous rewritten-lines rebase was auto-recovered: %#v", state)
	}
	if state.Safety != "blocked_recover_diverged" {
		t.Fatalf("recover with rewritten operator lines = %s", state.Safety)
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.submitted {
		t.Fatalf("HEAD moved to %s despite an unprovable containment claim", got)
	}
	if got := readOptional(t, filepath.Join(f.local, "feature.txt")); got != "feature one\nfeature two\n" {
		t.Fatalf("escalation touched the worktree: %q", got)
	}
	// The preserved commits stay anchored so the operator can still reconcile.
	if got := mustRun(t, f.local, "rev-parse", f.anchorRef()); got != f.preserved {
		t.Fatalf("preserved anchor = %s, want %s", got, f.preserved)
	}
	if f.custodyReturned() {
		t.Fatal("escalation stamped custody")
	}
}

// TestRecoverRebasedPreservedHeadRefusesConcurrentCommitWithoutLosingIt is the
// no-data-loss-under-concurrency regression. A commit landing after every
// precondition was observed and after containment was proven must not be
// destroyed: the branch move is an atomic compare-and-swap against the observed
// head, so it refuses and the concurrent commit survives untouched.
func TestRecoverRebasedPreservedHeadRefusesConcurrentCommitWithoutLosingIt(t *testing.T) {
	t.Parallel()

	f := newRebasedRecoverFixture(t, types.RunCancelled)
	var concurrent string
	f.service.beforeRecoverBranchMove = func() {
		mustWrite(t, filepath.Join(f.local, "concurrent.txt"), "work committed mid-recovery\n")
		mustRun(t, f.local, "add", "concurrent.txt")
		mustRun(t, f.local, "commit", "-m", "concurrent local commit")
		concurrent = mustRun(t, f.local, "rev-parse", "HEAD")
	}

	state := f.service.Recover(f.ctx, false)
	if state.Recovered || state.Changed {
		t.Fatalf("recovery raced a concurrent commit: %#v", state)
	}
	if state.Safety != "blocked_recover_assumptions_changed" {
		t.Fatalf("concurrent-commit refusal = %s", state.Safety)
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != concurrent {
		t.Fatalf("HEAD = %s, want the concurrent commit %s preserved", got, concurrent)
	}
	if got := readOptional(t, filepath.Join(f.local, "concurrent.txt")); got != "work committed mid-recovery\n" {
		t.Fatalf("concurrent work lost: %q", got)
	}
	if f.custodyReturned() {
		t.Fatal("raced recovery stamped custody")
	}
}

func TestRecoverRebasedPreservedHeadRollsBackAfterConcurrentCheckout(t *testing.T) {
	t.Parallel()

	f := newRebasedRecoverFixture(t, types.RunCancelled)
	mustRun(t, f.local, "branch", "other-clean-branch", f.submitted)
	f.service.afterRecoverBranchMove = func() {
		mustRun(t, f.local, "checkout", "other-clean-branch")
	}

	state := f.service.Recover(f.ctx, false)
	if state.Recovered || state.Changed || state.Safety != "blocked_recover_assumptions_changed" {
		t.Fatalf("recovery raced a concurrent checkout: %#v", state)
	}
	if got := mustRun(t, f.local, "rev-parse", "refs/heads/feature/recover"); got != f.submitted {
		t.Fatalf("feature branch = %s, want rollback to %s", got, f.submitted)
	}
	if got := mustRun(t, f.local, "rev-parse", "refs/heads/other-clean-branch"); got != f.submitted {
		t.Fatalf("concurrently checked-out branch = %s, want %s", got, f.submitted)
	}
	if got := readOptional(t, filepath.Join(f.local, "feature.txt")); got != "feature one\nfeature two\n" {
		t.Fatalf("concurrent checkout worktree changed: %q", got)
	}
	if got := readOptional(t, filepath.Join(f.local, "upstream.txt")); got != "" {
		t.Fatalf("preserved tree leaked into concurrent checkout: %q", got)
	}
	if f.custodyReturned() {
		t.Fatal("raced recovery stamped custody")
	}
}

// TestRecoverRebasedPreservedHeadRefusesConcurrentWorktreeEditWithoutLosingIt
// is the other concurrency axis: an uncommitted edit to a file the move would
// rewrite must abort the move inside Git itself rather than being overwritten,
// and the branch must be restored to where it started.
func TestRecoverRebasedPreservedHeadRefusesConcurrentWorktreeEditWithoutLosingIt(t *testing.T) {
	t.Parallel()

	f := newRebasedRecoverFixture(t, types.RunCancelled)
	f.service.beforeRecoverBranchMove = func() {
		// upstream.txt exists only on the advanced base, so adopting the
		// preserved head must write it; an untracked copy blocks that write.
		mustWrite(t, filepath.Join(f.local, "upstream.txt"), "uncommitted local draft\n")
	}

	state := f.service.Recover(f.ctx, false)
	if state.Recovered || state.Changed {
		t.Fatalf("recovery overwrote a concurrent worktree edit: %#v", state)
	}
	if state.Safety != "blocked_recover_worktree_busy" {
		t.Fatalf("concurrent-edit refusal = %s", state.Safety)
	}
	if got := readOptional(t, filepath.Join(f.local, "upstream.txt")); got != "uncommitted local draft\n" {
		t.Fatalf("concurrent worktree edit lost: %q", got)
	}
	if got := mustRun(t, f.local, "rev-parse", "refs/heads/feature/recover"); got != f.submitted {
		t.Fatalf("branch = %s, want rollback to %s", got, f.submitted)
	}
	if f.custodyReturned() {
		t.Fatal("aborted move stamped custody")
	}
}

// TestRecoverRebasedPreservedHeadRefusesDirtyWorktree keeps the ordinary
// uncommitted-work protection intact: a worktree that is already dirty when
// recovery starts refuses outright, and --keep-local remains the exit that
// never touches the worktree.
func TestRecoverRebasedPreservedHeadRefusesDirtyWorktree(t *testing.T) {
	t.Parallel()

	f := newRebasedRecoverFixture(t, types.RunCancelled)
	mustWrite(t, filepath.Join(f.local, "feature.txt"), "uncommitted edit\n")

	state := f.service.Recover(f.ctx, false)
	if state.Recovered || state.Changed || state.Safety != "blocked_recover_dirty" {
		t.Fatalf("dirty rebased recover = %#v", state)
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

func TestRecoverIncompleteAdoptionDoesNotStampStaleWorktree(t *testing.T) {
	t.Parallel()

	f := newRebasedRecoverFixture(t, types.RunCancelled)
	mustRun(t, f.local, "fetch", "--no-tags", f.gate, "+refs/heads/feature/recover:"+f.anchorRef())
	mustRun(t, f.local, "update-ref", f.localAnchorRef(), f.submitted, "")
	mustRun(t, f.local, "update-ref", "refs/heads/feature/recover", f.preserved, f.submitted)

	state := f.service.Recover(f.ctx, false)
	if state.Recovered || state.Changed || state.Safety != "blocked_recover_incomplete_adoption" {
		t.Fatalf("incomplete adoption was stamped: %#v", state)
	}
	if !strings.Contains(state.Error, f.localAnchorRef()) {
		t.Fatalf("incomplete-adoption refusal does not name local anchor: %q", state.Error)
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.preserved {
		t.Fatalf("HEAD = %s, want preserved %s", got, f.preserved)
	}
	if got := readOptional(t, filepath.Join(f.local, "feature.txt")); got != "feature one\nfeature two\n" {
		t.Fatalf("pre-recovery worktree changed: %q", got)
	}
	if got := readOptional(t, filepath.Join(f.local, "upstream.txt")); got != "" {
		t.Fatalf("preserved tree was applied unexpectedly: %q", got)
	}
	if f.custodyReturned() {
		t.Fatal("incomplete adoption stamped custody")
	}
}

// TestRecoverRebasedPreservedHeadKeepLocalStillKeepsTheLocalHead pins that the
// explicit keep-local choice still wins over the new adoption path.
func TestRecoverRebasedPreservedHeadKeepLocalStillKeepsTheLocalHead(t *testing.T) {
	t.Parallel()

	f := newRebasedRecoverFixture(t, types.RunCancelled)
	state := f.service.Recover(f.ctx, true)
	if !state.Recovered || state.Changed {
		t.Fatalf("keep-local rebased recover = %#v", state)
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

// TestRecoverRebasedPreservedHeadRechecksAfterAnchoringLocalHead proves the
// pre-move guard still catches a branch switch that happens before the move.
func TestRecoverRebasedPreservedHeadRechecksAfterAnchoringLocalHead(t *testing.T) {
	t.Parallel()

	f := newRebasedRecoverFixture(t, types.RunCancelled)
	f.service.beforeRecoverWorktreeMove = func() {
		mustRun(t, f.local, "checkout", "-b", "other-clean-branch", f.submitted)
	}
	state := f.service.Recover(f.ctx, false)
	if state.Recovered || state.Changed || state.Safety != "blocked_recover_assumptions_changed" {
		t.Fatalf("recover after branch switch = %#v", state)
	}
	if got := strings.TrimSpace(mustRun(t, f.local, "branch", "--show-current")); got != "other-clean-branch" {
		t.Fatalf("current branch = %q", got)
	}
	if f.custodyReturned() {
		t.Fatal("branch-switch refusal stamped custody")
	}
}

// TestRecoverSquashedEquivalentPreservedHeadAdopts covers the same containment
// proof for a fix round that AMENDS or squashes the operator's commits: no
// commit-level correspondence survives, but the preserved head still carries
// the operator's exact content, so adopting it loses nothing.
func TestRecoverSquashedEquivalentPreservedHeadAdopts(t *testing.T) {
	t.Parallel()

	f := newRebasedRecoverFixture(t, types.RunCancelled)
	pipeline := filepath.Join(filepath.Dir(f.local), "squash")
	mustRun(t, filepath.Dir(f.local), "-c", "core.autocrlf=false", "clone", f.gate, pipeline)
	configureIdentity(t, pipeline)
	mustRun(t, pipeline, "checkout", "feature/recover")
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
		t.Fatalf("squashed-equivalent recovery = %#v", state)
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
// counterfactual for that proof: when the squash DROPS one of the operator's
// changes, the preserved head no longer contains it and recovery must escalate
// rather than silently discard it.
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

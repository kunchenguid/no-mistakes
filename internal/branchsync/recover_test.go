package branchsync

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	gitpkg "github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

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
	if err := database.UpdateRunStatus(run.ID, status); err != nil {
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

func (f *recoverFixture) anchorRef() string { return "refs/no-mistakes/recover/" + f.run.ID }

func (f *recoverFixture) moveGateBranchToSubmitted() {
	f.t.Helper()
	mustRun(f.t, f.gate, "update-ref", "refs/heads/feature/recover", f.submitted)
}

func (f *recoverFixture) custodyReturned() bool {
	f.t.Helper()
	run, err := f.db.GetRun(f.run.ID)
	if err != nil || run == nil {
		f.t.Fatalf("reload run: %#v, %v", run, err)
	}
	return run.CustodyReturnedAt != nil
}

func assertNoRecoverAnchor(t *testing.T, f *recoverFixture) {
	t.Helper()
	if got, err := gitpkg.Run(f.ctx, f.local, "rev-parse", "--verify", "--quiet", f.anchorRef()+"^{commit}"); err == nil {
		t.Fatalf("unexpected recovery anchor %s at %s", f.anchorRef(), got)
	}
}

// TestTerminalPrePushRunSurfacesGuardedCustodyRecovery is the regression test
// for the stranded state itself (dogfood run 01KXN8YJ6DWF8XPP582DWQC3HV): a
// terminal run at the pre_push phase must not be a dead end. The state stays
// pipeline_owned, but safety identifies it as recoverable, exposes the run's
// terminal status, and offers the guarded custody-return action.
func TestTerminalPrePushRunSurfacesGuardedCustodyRecovery(t *testing.T) {
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
	f := newRecoverFixture(t, types.RunCancelled)
	f.service.beforeRecoverFastForward = func() {
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

// TestRecoverGateAtLocalSubmittedHeadAnchorsRecordedHead covers the recovery
// gap diagnosed for Firstmate PR #983: the terminal run recorded preserved
// head P, the operator and gate branch have moved back to the submitted head
// A, and P still exists only in the local gate object database.
func TestRecoverGateAtLocalSubmittedHeadAnchorsRecordedHead(t *testing.T) {
	t.Run("default anchors and refuses with actionable custody choices", func(t *testing.T) {
		f := newRecoverFixture(t, types.RunFailed)
		f.moveGateBranchToSubmitted()
		beforeStatus := mustRun(t, f.local, "status", "--porcelain=v1")

		state := f.service.Recover(f.ctx, false)
		if state.Recovered || state.Changed || state.Safety != "blocked_recover_diverged" || state.Relation != RelationDiverged {
			t.Fatalf("moved-gate default recover = %#v", state)
		}
		for _, want := range []string{f.anchorRef(), "no-mistakes axi sync --recover --keep-local", "no-mistakes rerun", "git log --oneline --left-right HEAD..." + f.anchorRef()} {
			if !strings.Contains(state.Error, want) {
				t.Fatalf("moved-gate refusal missing %q: %q", want, state.Error)
			}
		}
		if state.NextAction == nil || state.NextAction.Code != "inspect_and_reconcile_manually" || !strings.Contains(state.NextAction.Command, f.anchorRef()) {
			t.Fatalf("moved-gate next action = %#v", state.NextAction)
		}
		if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.submitted {
			t.Fatalf("default recover moved HEAD to %s, want submitted %s", got, f.submitted)
		}
		if got := mustRun(t, f.gate, "rev-parse", "refs/heads/feature/recover"); got != f.submitted {
			t.Fatalf("default recover moved gate branch to %s, want submitted %s", got, f.submitted)
		}
		if got := mustRun(t, f.local, "rev-parse", f.anchorRef()); got != f.preserved {
			t.Fatalf("default recover anchor = %s, want preserved %s", got, f.preserved)
		}
		if got := mustRun(t, f.local, "status", "--porcelain=v1"); got != beforeStatus {
			t.Fatalf("default recover touched worktree status: before %q after %q", beforeStatus, got)
		}
		if f.custodyReturned() {
			t.Fatal("default moved-gate refusal stamped custody")
		}
	})

	t.Run("keep local stamps custody without moving worktree or gate", func(t *testing.T) {
		f := newRecoverFixture(t, types.RunFailed)
		f.moveGateBranchToSubmitted()

		state := f.service.Recover(f.ctx, true)
		if !state.Recovered || state.Changed {
			t.Fatalf("moved-gate keep-local recover = %#v", state)
		}
		if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.submitted {
			t.Fatalf("keep-local moved HEAD to %s, want submitted %s", got, f.submitted)
		}
		if got := mustRun(t, f.gate, "rev-parse", "refs/heads/feature/recover"); got != f.submitted {
			t.Fatalf("keep-local moved gate branch to %s, want submitted %s", got, f.submitted)
		}
		if got := mustRun(t, f.local, "rev-parse", f.anchorRef()); got != f.preserved {
			t.Fatalf("keep-local anchor = %s, want preserved %s", got, f.preserved)
		}
		if !f.custodyReturned() {
			t.Fatal("keep-local did not stamp custody")
		}
	})

	t.Run("keep local refuses concurrent moved gate update before stamp", func(t *testing.T) {
		f := newRecoverFixture(t, types.RunFailed)
		f.moveGateBranchToSubmitted()
		f.service.beforeGateReset = func() {
			writer := filepath.Join(t.TempDir(), "racer")
			mustRun(t, filepath.Dir(writer), "-c", "core.autocrlf=false", "clone", f.gate, writer)
			configureIdentity(t, writer)
			mustRun(t, writer, "checkout", "feature/recover")
			mustWrite(t, filepath.Join(writer, "race.txt"), "race\n")
			mustRun(t, writer, "add", "race.txt")
			mustRun(t, writer, "commit", "-m", "racing moved-gate push")
			mustRun(t, writer, "push", "origin", "HEAD:refs/heads/feature/recover")
		}

		state := f.service.Recover(f.ctx, true)
		if state.Recovered || state.Changed || state.Safety != "blocked_recover_gate_race" {
			t.Fatalf("racing moved-gate keep-local recover = %#v", state)
		}
		if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.submitted {
			t.Fatalf("racing moved-gate recover moved HEAD to %s, want submitted %s", got, f.submitted)
		}
		if got := mustRun(t, f.local, "rev-parse", f.anchorRef()); got != f.preserved {
			t.Fatalf("racing moved-gate recover anchor = %s, want preserved %s", got, f.preserved)
		}
		if f.custodyReturned() {
			t.Fatal("racing moved-gate recover stamped custody")
		}
	})

	t.Run("keep local refuses concurrent local commit before stamp", func(t *testing.T) {
		f := newRecoverFixture(t, types.RunFailed)
		f.moveGateBranchToSubmitted()
		f.service.beforeGateReset = func() {
			mustWrite(t, filepath.Join(f.local, "race.txt"), "race\n")
			mustRun(t, f.local, "add", "race.txt")
			mustRun(t, f.local, "commit", "-m", "racing local commit")
		}

		state := f.service.Recover(f.ctx, true)
		if state.Recovered || state.Changed || state.Safety != "blocked_recover_assumptions_changed" {
			t.Fatalf("racing local commit keep-local recover = %#v", state)
		}
		if got := mustRun(t, f.gate, "rev-parse", "refs/heads/feature/recover"); got != f.submitted {
			t.Fatalf("racing local commit recover moved gate branch to %s, want submitted %s", got, f.submitted)
		}
		if got := mustRun(t, f.local, "rev-parse", f.anchorRef()); got != f.preserved {
			t.Fatalf("racing local commit recover anchor = %s, want preserved %s", got, f.preserved)
		}
		if f.custodyReturned() {
			t.Fatal("racing local commit recover stamped custody")
		}
	})

	t.Run("keep local refuses concurrent branch switch before stamp", func(t *testing.T) {
		f := newRecoverFixture(t, types.RunFailed)
		f.moveGateBranchToSubmitted()
		f.service.beforeGateReset = func() {
			mustRun(t, f.local, "checkout", "main")
		}

		state := f.service.Recover(f.ctx, true)
		if state.Recovered || state.Changed || state.Safety != "blocked_recover_assumptions_changed" {
			t.Fatalf("racing branch switch keep-local recover = %#v", state)
		}
		if got := mustRun(t, f.gate, "rev-parse", "refs/heads/feature/recover"); got != f.submitted {
			t.Fatalf("racing branch switch recover moved gate branch to %s, want submitted %s", got, f.submitted)
		}
		if got := mustRun(t, f.local, "rev-parse", f.anchorRef()); got != f.preserved {
			t.Fatalf("racing branch switch recover anchor = %s, want preserved %s", got, f.preserved)
		}
		if f.custodyReturned() {
			t.Fatal("racing branch switch recover stamped custody")
		}
	})
}

func TestRecoverMovedGateExactAnchorDisconfirmingCases(t *testing.T) {
	t.Run("local differs from moved gate", func(t *testing.T) {
		f := newRecoverFixture(t, types.RunFailed)
		f.moveGateBranchToSubmitted()
		mustWrite(t, filepath.Join(f.local, "local.txt"), "local\n")
		mustRun(t, f.local, "add", "local.txt")
		mustRun(t, f.local, "commit", "-m", "local followup")

		state := f.service.Recover(f.ctx, true)
		if state.Recovered || state.Safety != "blocked_recover_gate_diverged" {
			t.Fatalf("local-differs moved gate recover = %#v", state)
		}
		assertNoRecoverAnchor(t, f)
		if f.custodyReturned() {
			t.Fatal("local-differs moved gate stamped custody")
		}
	})

	t.Run("recorded head absent from gate object database", func(t *testing.T) {
		f := newRecoverFixture(t, types.RunFailed)
		f.moveGateBranchToSubmitted()
		missing := strings.Repeat("1", 40)
		if err := f.db.UpdateRunHeadSHA(f.run.ID, missing); err != nil {
			t.Fatal(err)
		}

		state := f.service.Recover(f.ctx, true)
		if state.Recovered || state.Safety != "blocked_recover_preserve_failed" {
			t.Fatalf("missing-preserved moved gate recover = %#v", state)
		}
		assertNoRecoverAnchor(t, f)
		if f.custodyReturned() {
			t.Fatal("missing-preserved moved gate stamped custody")
		}
	})

	t.Run("wrong checked out branch", func(t *testing.T) {
		f := newRecoverFixture(t, types.RunFailed)
		f.moveGateBranchToSubmitted()
		mustRun(t, f.local, "checkout", "main")

		state := f.service.Recover(f.ctx, true)
		if state.Recovered || state.Safety != "blocked_recover_not_applicable" {
			t.Fatalf("wrong-branch moved gate recover = %#v", state)
		}
		assertNoRecoverAnchor(t, f)
		if f.custodyReturned() {
			t.Fatal("wrong-branch recover stamped custody")
		}
	})

	t.Run("nonterminal run", func(t *testing.T) {
		f := newRecoverFixture(t, types.RunRunning)
		f.moveGateBranchToSubmitted()

		state := f.service.Recover(f.ctx, true)
		if state.Recovered || state.Safety != "blocked_recover_run_active" {
			t.Fatalf("active moved gate recover = %#v", state)
		}
		assertNoRecoverAnchor(t, f)
		if f.custodyReturned() {
			t.Fatal("active moved gate stamped custody")
		}
	})

	t.Run("gate unavailable", func(t *testing.T) {
		f := newRecoverFixture(t, types.RunFailed)
		f.moveGateBranchToSubmitted()
		if err := os.RemoveAll(f.gate); err != nil {
			t.Fatal(err)
		}

		state := f.service.Recover(f.ctx, true)
		if state.Recovered || state.Safety != "blocked_recover_gate_unavailable" {
			t.Fatalf("unavailable moved gate recover = %#v", state)
		}
		assertNoRecoverAnchor(t, f)
		if f.custodyReturned() {
			t.Fatal("unavailable moved gate stamped custody")
		}
	})
}

// TestRecoverKeepLocalDirtyBehindReturnsCustodyWithoutTouchingWorktree covers
// the explicit keep-local choice on a dirty worktree: no worktree mutation is
// needed, so dirtiness must not block it, and the gate follows the kept head.
func TestRecoverKeepLocalDirtyBehindReturnsCustodyWithoutTouchingWorktree(t *testing.T) {
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
	f := newSyncFixture(t)
	state := f.service.Recover(f.ctx, false)
	if state.Recovered || state.Safety != "blocked_recover_not_applicable" {
		t.Fatalf("recover on healthy state = %#v", state)
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.old {
		t.Fatal("not-applicable refusal moved HEAD")
	}
}

// TestRecoverConcurrentGatePushLosesCleanly: the keep-local gate reset is an
// atomic compare-and-swap, so a racing push to the gate wins and recovery
// refuses instead of clobbering the newer gate head.
func TestRecoverConcurrentGatePushLosesCleanly(t *testing.T) {
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

func TestRecoverKeepLocalRechecksAfterGateCASBeforeStamp(t *testing.T) {
	t.Run("refuses gate update after CAS", func(t *testing.T) {
		f := newRecoverFixture(t, types.RunCancelled)
		mustWrite(t, filepath.Join(f.local, "rescope.txt"), "rescope\n")
		mustRun(t, f.local, "add", "rescope.txt")
		mustRun(t, f.local, "commit", "-m", "diverging rescope")
		kept := mustRun(t, f.local, "rev-parse", "HEAD")
		f.service.beforeRecoverStamp = func() {
			writer := filepath.Join(t.TempDir(), "racer")
			mustRun(t, filepath.Dir(writer), "-c", "core.autocrlf=false", "clone", f.gate, writer)
			configureIdentity(t, writer)
			mustRun(t, writer, "checkout", "feature/recover")
			mustWrite(t, filepath.Join(writer, "race.txt"), "race\n")
			mustRun(t, writer, "add", "race.txt")
			mustRun(t, writer, "commit", "-m", "post-cas racing push")
			mustRun(t, writer, "push", "origin", "HEAD:refs/heads/feature/recover")
		}

		state := f.service.Recover(f.ctx, true)
		if state.Recovered || state.Changed || state.Safety != "blocked_recover_gate_race" {
			t.Fatalf("post-cas gate race recover = %#v", state)
		}
		if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != kept {
			t.Fatalf("post-cas gate race moved HEAD to %s, want %s", got, kept)
		}
		if got := mustRun(t, f.local, "rev-parse", f.anchorRef()); got != f.preserved {
			t.Fatalf("post-cas gate race anchor = %s, want preserved %s", got, f.preserved)
		}
		if f.custodyReturned() {
			t.Fatal("post-cas gate race stamped custody")
		}
	})

	t.Run("refuses local commit after CAS", func(t *testing.T) {
		f := newRecoverFixture(t, types.RunCancelled)
		mustWrite(t, filepath.Join(f.local, "rescope.txt"), "rescope\n")
		mustRun(t, f.local, "add", "rescope.txt")
		mustRun(t, f.local, "commit", "-m", "diverging rescope")
		kept := mustRun(t, f.local, "rev-parse", "HEAD")
		f.service.beforeRecoverStamp = func() {
			mustWrite(t, filepath.Join(f.local, "race.txt"), "race\n")
			mustRun(t, f.local, "add", "race.txt")
			mustRun(t, f.local, "commit", "-m", "post-cas local commit")
		}

		state := f.service.Recover(f.ctx, true)
		if state.Recovered || state.Changed || state.Safety != "blocked_recover_assumptions_changed" {
			t.Fatalf("post-cas local commit recover = %#v", state)
		}
		if got := mustRun(t, f.gate, "rev-parse", "refs/heads/feature/recover"); got != kept {
			t.Fatalf("post-cas local commit gate = %s, want kept %s", got, kept)
		}
		if f.custodyReturned() {
			t.Fatal("post-cas local commit stamped custody")
		}
	})

	t.Run("refuses branch switch after CAS", func(t *testing.T) {
		f := newRecoverFixture(t, types.RunCancelled)
		mustWrite(t, filepath.Join(f.local, "rescope.txt"), "rescope\n")
		mustRun(t, f.local, "add", "rescope.txt")
		mustRun(t, f.local, "commit", "-m", "diverging rescope")
		kept := mustRun(t, f.local, "rev-parse", "HEAD")
		f.service.beforeRecoverStamp = func() {
			mustRun(t, f.local, "checkout", "main")
		}

		state := f.service.Recover(f.ctx, true)
		if state.Recovered || state.Changed || state.Safety != "blocked_recover_assumptions_changed" {
			t.Fatalf("post-cas branch switch recover = %#v", state)
		}
		if got := mustRun(t, f.gate, "rev-parse", "refs/heads/feature/recover"); got != kept {
			t.Fatalf("post-cas branch switch gate = %s, want kept %s", got, kept)
		}
		if f.custodyReturned() {
			t.Fatal("post-cas branch switch stamped custody")
		}
	})
}

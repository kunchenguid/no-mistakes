package branchsync

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRefresh_RemoteRewrittenOffersWorkingRecoveryNextAction is the DEFECT 2(b)
// regression: a rewritten remote used to leave the operator with a correctly
// blocked state but no route forward at all - no next_action, and --recover
// refused because the cached classification is never blocked_pipeline_owned
// for this case. The only escape hatch was a hand edit of state.sqlite. This
// proves the offered command actually resolves the block end to end, not
// merely that a string is present.
func TestRefresh_RemoteRewrittenOffersWorkingRecoveryNextAction(t *testing.T) {
	t.Parallel()
	f := newSyncFixture(t)
	attachGateHoldingPipelineHead(t, f)

	writer := cloneRemoteBranch(t, f.remote)
	rewriteRemote(t, writer, "rewrite", "rewrite.txt")
	rewrittenHead := mustRun(t, writer, "rev-parse", "HEAD")

	state := f.service.Refresh(f.ctx)
	if state.State != StateRemoteRewritten || state.Safety != "blocked_remote_rewritten" {
		t.Fatalf("setup: state = %#v", state)
	}
	if state.NextAction == nil || strings.TrimSpace(state.NextAction.Command) == "" {
		t.Fatalf("blocked_remote_rewritten offered no next_action, leaving no route but a hand edit of state.sqlite: %#v", state.NextAction)
	}
	if state.NextAction.Command != "no-mistakes axi sync --recover" {
		t.Fatalf("unexpected next_action command: %#v", state.NextAction)
	}

	// Prove the offered command actually works, not just that a string exists.
	recovered := f.service.Recover(f.ctx, false)
	if !recovered.Recovered {
		t.Fatalf("no-mistakes axi sync --recover did not resolve blocked_remote_rewritten: %#v", recovered)
	}
	if recovered.Changed {
		t.Fatalf("remote-rewritten recovery must never touch the worktree, got Changed=true: %#v", recovered)
	}

	// The block must not simply be dismissed: a subsequent check must no
	// longer report remote_rewritten, because the stale binding was actually
	// corrected.
	after := f.service.Refresh(f.ctx)
	if after.State == StateRemoteRewritten {
		t.Fatalf("still remote_rewritten after recovery: %#v", after)
	}

	run, err := f.db.GetRun(f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.LastPushedSHA == nil || *run.LastPushedSHA != rewrittenHead {
		t.Fatalf("pushed_head was not corrected to the verified remote head: got %v, want %s", run.LastPushedSHA, rewrittenHead)
	}
	if run.HeadSHA != rewrittenHead {
		t.Fatalf("run head was not advanced to match the corrected binding: got %s, want %s", run.HeadSHA, rewrittenHead)
	}
	if recovered.RecoveryKind != RecoveryPushBindingCorrected {
		t.Fatalf("a binding correction reported itself as %q; presenters would announce a custody return that never happened", recovered.RecoveryKind)
	}
	if run.CustodyReturnedAt != nil {
		t.Fatalf("the binding correction stamped custody it never returned")
	}
	if recovered.Error != "" {
		t.Fatalf("a successful correction still reported a blocked reason: %q", recovered.Error)
	}
	// The pipeline's own head is the only thing this correction supersedes, so
	// it must stay reachable through the tool rather than surviving only in the
	// gate's object store.
	anchored := mustRun(t, f.local, "rev-parse", "refs/no-mistakes/recover/"+f.run.ID+"^{commit}")
	if anchored != f.pushed {
		t.Fatalf("the superseded pipeline head was not anchored: got %s, want %s", anchored, f.pushed)
	}
}

// TestRecover_CustodyReturnedRunReachesRemoteRewrittenCorrection covers the run
// shape whose cached classification hides the hazard entirely: custody was
// already returned, so inspect() never reports pipeline_owned and Recover used
// to answer from the custody stamp alone - reporting success while the stale
// binding, and therefore the identical block, survived every retry.
func TestRecover_CustodyReturnedRunReachesRemoteRewrittenCorrection(t *testing.T) {
	t.Parallel()
	f := newSyncFixture(t)
	attachGateHoldingPipelineHead(t, f)
	if err := f.db.SetRunCustodyReturned(f.run.ID); err != nil {
		t.Fatal(err)
	}

	writer := cloneRemoteBranch(t, f.remote)
	rewriteRemote(t, writer, "rewrite", "rewrite.txt")
	rewrittenHead := mustRun(t, writer, "rev-parse", "HEAD")

	blocked := f.service.Refresh(f.ctx)
	if blocked.Safety != "blocked_remote_rewritten" {
		t.Fatalf("setup: a custody-returned run did not surface the rewritten remote: %#v", blocked)
	}
	if blocked.NextAction == nil || blocked.NextAction.Code != "recover_remote_rewritten" {
		t.Fatalf("setup: next_action = %#v", blocked.NextAction)
	}

	recovered := f.service.Recover(f.ctx, false)
	if !recovered.Recovered || recovered.RecoveryKind != RecoveryPushBindingCorrected {
		t.Fatalf("the offered recovery did not correct the binding for a custody-returned run: %#v", recovered)
	}

	run, err := f.db.GetRun(f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.LastPushedSHA == nil || *run.LastPushedSHA != rewrittenHead {
		t.Fatalf("pushed_head was not corrected: got %v, want %s", run.LastPushedSHA, rewrittenHead)
	}
	if after := f.service.Refresh(f.ctx); after.State == StateRemoteRewritten {
		t.Fatalf("the identical block survived the offered recovery: %#v", after)
	}
}

// TestRecover_RemoteRewrittenRefusesWhenSupersededHeadCannotBeAnchored proves
// the correction is never destructive: the push binding is the last record of
// the head this pipeline produced and pushed, so when that head cannot be made
// reachable in the worktree the correction refuses instead of overwriting it.
func TestRecover_RemoteRewrittenRefusesWhenSupersededHeadCannotBeAnchored(t *testing.T) {
	t.Parallel()
	f := newSyncFixture(t)

	writer := cloneRemoteBranch(t, f.remote)
	rewriteRemote(t, writer, "rewrite", "rewrite.txt")

	// No gate is attached, and the operator worktree never received the
	// pipeline head, so nothing can anchor it.
	recovered := f.service.Recover(f.ctx, false)
	if recovered.Recovered {
		t.Fatalf("the correction erased the last record of an unreachable pipeline head: %#v", recovered)
	}
	if recovered.Safety != "blocked_recover_preserve_failed" {
		t.Fatalf("safety = %q, want blocked_recover_preserve_failed: %#v", recovered.Safety, recovered)
	}
	if !strings.Contains(recovered.Error, f.pushed) {
		t.Fatalf("the refusal did not name the head it protected (%s): %q", f.pushed, recovered.Error)
	}

	run, err := f.db.GetRun(f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.LastPushedSHA == nil || *run.LastPushedSHA != f.pushed {
		t.Fatalf("push binding moved despite the refusal: got %v, want %s", run.LastPushedSHA, f.pushed)
	}
}

// TestRecover_NotApplicableNeverClaimsALiveCheckThatDidNotRun keeps the refusal
// honest: with no remote reachable at all, the "nothing to recover" answer must
// not assert that a fresh reading of the push target cleared the hazard.
func TestRecover_NotApplicableNeverClaimsALiveCheckThatDidNotRun(t *testing.T) {
	t.Parallel()
	f := newSyncFixture(t)
	if err := os.RemoveAll(f.remote); err != nil {
		t.Fatal(err)
	}

	recovered := f.service.Recover(f.ctx, false)
	if recovered.Safety != "blocked_recover_not_applicable" {
		t.Fatalf("safety = %q: %#v", recovered.Safety, recovered)
	}
	if strings.Contains(recovered.Error, "still matches") {
		t.Fatalf("the refusal claimed a live reading that never completed: %q", recovered.Error)
	}
	if !strings.Contains(recovered.Error, "no verified live reading") {
		t.Fatalf("the refusal did not disclose that no live reading was available: %q", recovered.Error)
	}
}

// attachGateHoldingPipelineHead gives the fixture the local bare gate every
// registered repository has, holding the branch at the pipeline's pushed head.
func attachGateHoldingPipelineHead(t *testing.T, f *syncFixture) string {
	t.Helper()
	root := filepath.Dir(f.local)
	gate := filepath.Join(root, "gate.git")
	mustRun(t, root, "init", "--bare", gate)
	mustRun(t, filepath.Join(root, "pipeline"), "push", gate, "HEAD:refs/heads/feature/sync")
	f.service.GateDir = gate
	return gate
}

// TestRefresh_RaceDuringRefreshDoesNotCarryForwardStaleNextAction is the
// DEFECT 2(a) regression. Before any live check, the cached classification
// (built from the persisted, possibly-stale binding) can compute an ordinary
// actionable next_action such as {Code: "sync"}. When the live remote then
// turns out to have changed again during the refresh itself, the method must
// halt on that fresh discovery, not continue on the stale guidance it
// computed a moment earlier: the returned state must not tell the caller to
// just run an ordinary sync.
func TestRefresh_RaceDuringRefreshDoesNotCarryForwardStaleNextAction(t *testing.T) {
	t.Parallel()
	f := newSyncFixture(t)

	// The fixture's cached classification, before any live check, is
	// StateBehind with an actionable next_action of {Code: "sync"} (the local
	// worktree is still at the pre-pipeline commit).
	writer := cloneRemoteBranch(t, f.remote)
	f.service.beforeRefreshFetch = func() {
		rewriteRemote(t, writer, "race", "race.txt")
	}

	state := f.service.Refresh(f.ctx)
	if state.State != StateRemoteRewritten || state.Safety != "blocked_remote_changed_during_refresh" {
		t.Fatalf("state = %#v", state)
	}
	if state.NextAction != nil && state.NextAction.Code == "sync" {
		t.Fatalf("a blocked mid-refresh race carried forward the stale pre-check next_action instead of halting on the fresh discovery: %#v", state.NextAction)
	}
	// The race never verified the head it observed, so it must not advertise
	// the reconciliation, which writes the observed head to the push binding.
	if state.NextAction != nil && state.NextAction.Code == "recover_remote_rewritten" {
		t.Fatalf("an unverified mid-refresh reading offered the binding-correcting recovery: %#v", state.NextAction)
	}
}

// TestRecover_MidRefreshRaceRefusesToPersistUnverifiedRemoteHead pins the
// distinction the recovery gate must draw: StateRemoteRewritten is reached both
// by the confirmed rewrite (the ls-remote head was fetched and rev-parsed back
// to the same commit) and by the mid-refresh race (that verification failed).
// Remote.ObservedHead is populated before the verification either way, so a gate
// that matches only the state family writes an unverified - here already stale -
// SHA into the run's push binding and reports success.
func TestRecover_MidRefreshRaceRefusesToPersistUnverifiedRemoteHead(t *testing.T) {
	t.Parallel()
	f := newSyncFixture(t)

	writer := cloneRemoteBranch(t, f.remote)
	rewriteRemote(t, writer, "first-rewrite", "first.txt")
	observed := mustRun(t, writer, "rev-parse", "HEAD")

	state := f.service.Refresh(f.ctx)
	if state.State != StateRemoteRewritten || state.Safety != "blocked_remote_rewritten" {
		t.Fatalf("setup: state = %#v", state)
	}
	if state.Remote.ObservedHead != observed {
		t.Fatalf("setup: observed head = %s, want %s", state.Remote.ObservedHead, observed)
	}

	// The remote moves again inside the recovery's own re-verification window,
	// so the head it observed by ls-remote is no longer the remote's head.
	raced := false
	f.service.beforeRefreshFetch = func() {
		if raced {
			return
		}
		raced = true
		rewriteRemote(t, writer, "second-rewrite", "second.txt")
	}

	recovered := f.service.Recover(f.ctx, false)
	if recovered.Recovered {
		t.Fatalf("recovery reported success from an unverified mid-refresh reading: %#v", recovered)
	}
	if recovered.Changed {
		t.Fatalf("recovery reported a change it never made: %#v", recovered)
	}

	run, err := f.db.GetRun(f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.LastPushedSHA == nil || *run.LastPushedSHA != f.pushed {
		t.Fatalf("push binding was rewritten from an unverified reading: got %v, want the untouched %s", run.LastPushedSHA, f.pushed)
	}
	if run.HeadSHA != f.pushed {
		t.Fatalf("run head was advanced from an unverified reading: got %s, want the untouched %s", run.HeadSHA, f.pushed)
	}
}

func rewriteRemote(t *testing.T, writer, branch, file string) {
	t.Helper()
	mustRun(t, writer, "checkout", "--orphan", branch)
	mustRun(t, writer, "rm", "-rf", ".")
	mustWrite(t, filepath.Join(writer, file), branch+"\n")
	mustRun(t, writer, "add", file)
	mustRun(t, writer, "commit", "-m", branch)
	mustRun(t, writer, "push", "--force", "origin", "HEAD:refs/heads/feature/sync")
}

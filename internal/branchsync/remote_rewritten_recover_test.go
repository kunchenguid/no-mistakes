package branchsync

import (
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

	writer := cloneRemoteBranch(t, f.remote)
	mustRun(t, writer, "checkout", "--orphan", "rewrite")
	mustRun(t, writer, "rm", "-rf", ".")
	mustWrite(t, filepath.Join(writer, "rewrite.txt"), "rewrite\n")
	mustRun(t, writer, "add", "rewrite.txt")
	mustRun(t, writer, "commit", "-m", "rewrite")
	rewrittenHead := mustRun(t, writer, "rev-parse", "HEAD")
	mustRun(t, writer, "push", "--force", "origin", "HEAD:refs/heads/feature/sync")

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
		mustRun(t, writer, "checkout", "--orphan", "race")
		mustRun(t, writer, "rm", "-rf", ".")
		mustWrite(t, filepath.Join(writer, "race.txt"), "race\n")
		mustRun(t, writer, "add", "race.txt")
		mustRun(t, writer, "commit", "-m", "race")
		mustRun(t, writer, "push", "--force", "origin", "HEAD:refs/heads/feature/sync")
	}

	state := f.service.Refresh(f.ctx)
	if state.State != StateRemoteRewritten || state.Safety != "blocked_remote_changed_during_refresh" {
		t.Fatalf("state = %#v", state)
	}
	if state.NextAction != nil && state.NextAction.Code == "sync" {
		t.Fatalf("a blocked mid-refresh race carried forward the stale pre-check next_action instead of halting on the fresh discovery: %#v", state.NextAction)
	}
}

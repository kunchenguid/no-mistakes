package branchsync

import (
	"path/filepath"
	"testing"

	gitpkg "github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// newRemoteRewrittenFixture models issue #652: a terminal run successfully
// pushed its pipeline head, the local gate still carries that head, and then
// someone outside the pipeline force-pushed an unrelated history over the
// configured push target. It returns the rewritten live head.
func newRemoteRewrittenFixture(t *testing.T) (*syncFixture, string) {
	t.Helper()
	f := newSyncFixture(t)
	gate := filepath.Join(filepath.Dir(f.local), "gate.git")
	mustRun(t, filepath.Dir(f.local), "clone", "--bare", f.remote, gate)
	f.service.GateDir = gate
	return f, forceRewriteRemote(t, f, "rewrite")
}

func forceRewriteRemote(t *testing.T, f *syncFixture, content string) string {
	t.Helper()
	writer := cloneRemoteBranch(t, f.remote)
	mustRun(t, writer, "checkout", "--orphan", "rewrite-"+content)
	mustRun(t, writer, "rm", "-rf", ".")
	mustWrite(t, filepath.Join(writer, "rewrite.txt"), content+"\n")
	mustRun(t, writer, "add", "rewrite.txt")
	mustRun(t, writer, "commit", "-m", content)
	mustRun(t, writer, "push", "--force", "origin", "HEAD:refs/heads/feature/sync")
	return mustRun(t, writer, "rev-parse", "HEAD")
}

func TestRefreshOffersRecoveryForTerminalRunWithRewrittenRemote(t *testing.T) {
	t.Parallel()

	f, rewritten := newRemoteRewrittenFixture(t)
	state := f.service.Refresh(f.ctx)
	if state.State != StateRemoteRewritten || state.Safety != "blocked_remote_rewritten" {
		t.Fatalf("state = %#v", state)
	}
	if state.Remote.ObservedHead != rewritten || state.Pipeline.PushedHead != f.pushed {
		t.Fatalf("remote = %#v pipeline = %#v", state.Remote, state.Pipeline)
	}
	if state.NextAction == nil || state.NextAction.Code != "recover_remote_rewritten" || state.NextAction.Command != "no-mistakes axi sync --recover" {
		t.Fatalf("terminal rewritten remote must offer the explicit recovery, got next action %#v", state.NextAction)
	}
}

func TestRefreshKeepsActiveRunInChargeOfRewrittenRemote(t *testing.T) {
	t.Parallel()

	f, _ := newRemoteRewrittenFixture(t)
	if err := f.db.UpdateRunStatus(f.run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	state := f.service.Refresh(f.ctx)
	if state.State != StateRemoteRewritten || state.Safety != "blocked_remote_rewritten" {
		t.Fatalf("state = %#v", state)
	}
	if state.NextAction == nil || state.NextAction.Code != "continue_active_run" || state.NextAction.Command != "no-mistakes axi status" {
		t.Fatalf("active run must keep ownership, got next action %#v", state.NextAction)
	}
}

func TestRecoverRebindsTerminalRunToVerifiedRewrittenRemote(t *testing.T) {
	t.Parallel()

	f, rewritten := newRemoteRewrittenFixture(t)
	gateBranch := mustRun(t, f.service.GateDir, "rev-parse", "refs/heads/feature/sync")
	oldGeneration := value(f.run.PushGeneration)

	recovered := f.service.Recover(f.ctx, false)
	if !recovered.Recovered || recovered.Changed {
		t.Fatalf("recover = %#v", recovered)
	}
	if recovered.Recovery == nil || recovered.Recovery.Source != "remote_rewritten" ||
		recovered.Recovery.PreservedHead != f.pushed || recovered.Recovery.RequiredHead != rewritten {
		t.Fatalf("recovery evidence = %#v", recovered.Recovery)
	}
	if got := mustRun(t, f.service.GateDir, "rev-parse", recovered.Recovery.ArchiveRef+"^{commit}"); got != f.pushed {
		t.Fatalf("superseded pipeline head anchor = %s, want %s", got, f.pushed)
	}

	run, err := f.db.GetRun(f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ptr(run.LastPushedSHA) != rewritten || run.HeadSHA != rewritten || value(run.PushGeneration) != oldGeneration+1 {
		t.Fatalf("binding = last_pushed %s head %s generation %d", ptr(run.LastPushedSHA), run.HeadSHA, value(run.PushGeneration))
	}
	if run.CustodyReturnedAt != nil {
		t.Fatal("rebinding a rewritten remote must not stamp custody")
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.old {
		t.Fatalf("worktree HEAD moved to %s", got)
	}
	if got := mustRun(t, f.service.GateDir, "rev-parse", "refs/heads/feature/sync"); got != gateBranch {
		t.Fatalf("gate branch moved to %s", got)
	}
	if got := mustRun(t, f.remote, "rev-parse", "refs/heads/feature/sync"); got != rewritten {
		t.Fatalf("remote moved to %s", got)
	}

	after := f.service.Refresh(f.ctx)
	if after.Safety == "blocked_remote_rewritten" || after.Pipeline.PushedHead != rewritten || after.Remote.ObservedHead != rewritten {
		t.Fatalf("post-recover refresh = %#v", after)
	}
	if after.NextAction == nil {
		t.Fatalf("post-recover refresh must hand the branch to an ordinary next action, got %#v", after)
	}
}

func TestRecoverRewrittenRemoteRefusesKeepLocal(t *testing.T) {
	t.Parallel()

	f, _ := newRemoteRewrittenFixture(t)
	state := f.service.Recover(f.ctx, true)
	if state.Recovered || state.Safety != "blocked_recover_keep_local_not_applicable" || state.NextAction != nil {
		t.Fatalf("keep-local recover = %#v", state)
	}
	assertRewrittenBindingUntouched(t, f)
	if _, err := gitRunOptional(f, f.service.GateDir, "rev-parse", "--verify", "-q", rewrittenAnchorRef(f.run.ID, value(f.run.PushGeneration))); err == nil {
		t.Fatal("refused keep-local recovery wrote an anchor")
	}
}

func TestRecoverRewrittenRemoteRefusesWhenRemoteChangesAgain(t *testing.T) {
	t.Parallel()

	f, _ := newRemoteRewrittenFixture(t)
	var second string
	f.service.beforeRecoverRebind = func() {
		second = forceRewriteRemote(t, f, "second-rewrite")
	}
	state := f.service.Recover(f.ctx, false)
	if state.Recovered || state.Safety != "blocked_recover_remote_changed" {
		t.Fatalf("recover across a second rewrite = %#v", state)
	}
	assertRewrittenBindingUntouched(t, f)

	// A retry freshly verifies the new live head and rebinds to it; the
	// superseded pipeline head anchor from the refused attempt is reused.
	f.service.beforeRecoverRebind = nil
	retried := f.service.Recover(f.ctx, false)
	if !retried.Recovered || retried.Recovery == nil || retried.Recovery.RequiredHead != second {
		t.Fatalf("retry = %#v", retried)
	}
	run, err := f.db.GetRun(f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ptr(run.LastPushedSHA) != second {
		t.Fatalf("retry bound %s, want %s", ptr(run.LastPushedSHA), second)
	}
}

func TestRecoverRewrittenRemoteRefusesWhenSupersededHeadCannotBeAnchored(t *testing.T) {
	t.Parallel()

	f, _ := newRemoteRewrittenFixture(t)
	// Neither the worktree nor any gate holds the superseded pipeline head, so
	// rebinding would drop the last record of it.
	f.service.GateDir = ""
	state := f.service.Recover(f.ctx, false)
	if state.Recovered || state.Safety != "blocked_recover_preserve_failed" {
		t.Fatalf("recover without an anchorable superseded head = %#v", state)
	}
	assertRewrittenBindingUntouched(t, f)
}

func TestRecoverDoesNotRebindActiveRunWithRewrittenRemote(t *testing.T) {
	t.Parallel()

	f, _ := newRemoteRewrittenFixture(t)
	if err := f.db.UpdateRunStatus(f.run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	state := f.service.Recover(f.ctx, false)
	if state.Recovered {
		t.Fatalf("active run recover = %#v", state)
	}
	assertRewrittenBindingUntouched(t, f)
}

func assertRewrittenBindingUntouched(t *testing.T, f *syncFixture) {
	t.Helper()
	run, err := f.db.GetRun(f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ptr(run.LastPushedSHA) != f.pushed || run.HeadSHA != f.pushed || value(run.PushGeneration) != value(f.run.PushGeneration) {
		t.Fatalf("binding changed: last_pushed %s head %s generation %d", ptr(run.LastPushedSHA), run.HeadSHA, value(run.PushGeneration))
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.old {
		t.Fatalf("worktree HEAD moved to %s", got)
	}
}

func gitRunOptional(f *syncFixture, dir string, args ...string) (string, error) {
	return gitpkg.Run(f.ctx, dir, args...)
}

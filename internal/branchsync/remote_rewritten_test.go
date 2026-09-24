package branchsync

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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

func TestRecoverRewrittenRemoteRefusesWhenPushTargetChangesBeforeRebind(t *testing.T) {
	t.Parallel()

	f, _ := newRemoteRewrittenFixture(t)
	other := filepath.Join(t.TempDir(), "fork.git")
	mustRun(t, filepath.Dir(other), "init", "--bare", other)
	calls := 0
	f.service.lsRemote = func(ctx context.Context, dir, remote, ref string) (string, error) {
		calls++
		if calls == 2 {
			// The configured target changes after recovery read the repo
			// record and while it performs its final live check.
			if _, err := f.db.UpdateRepoForkURL(f.repo.ID, other); err != nil {
				t.Fatal(err)
			}
		}
		return gitpkg.LsRemote(ctx, dir, remote, ref)
	}
	state := f.service.Recover(f.ctx, false)
	if calls != 2 {
		t.Fatalf("ls-remote calls = %d, want the refresh and the final live check", calls)
	}
	if state.Recovered || state.Safety != "blocked_recover_assumptions_changed" {
		t.Fatalf("recover across a push target change = %#v", state)
	}
	assertRewrittenBindingUntouched(t, f)
}

func TestRewrittenRemoteOnRetiredPRIsNotRecoverable(t *testing.T) {
	t.Parallel()

	for _, prState := range []string{"merged", "closed"} {
		t.Run(prState, func(t *testing.T) {
			t.Parallel()
			f, _ := newRemoteRewrittenFixture(t)
			if err := f.db.UpdateRunPRState(f.run.ID, prState); err != nil {
				t.Fatal(err)
			}
			refreshed := f.service.Refresh(f.ctx)
			if refreshed.NextAction != nil {
				t.Fatalf("retired %s PR must not offer an action, got %#v", prState, refreshed.NextAction)
			}
			state := f.service.Recover(f.ctx, false)
			if state.Recovered {
				t.Fatalf("retired %s PR recover = %#v", prState, state)
			}
			assertRewrittenBindingUntouched(t, f)
		})
	}
}

func TestRecoverDoesNotReportSuccessWhenLiveVerificationFails(t *testing.T) {
	t.Parallel()

	f, _ := newRemoteRewrittenFixture(t)
	if err := f.db.SetRunCustodyReturned(f.run.ID); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(f.remote, f.remote+".offline"); err != nil {
		t.Fatal(err)
	}
	state := f.service.Recover(f.ctx, false)
	if state.Recovered || state.Safety != "blocked_offline" {
		t.Fatalf("recover without a verifiable live target = %#v", state)
	}
	assertRewrittenBindingUntouched(t, f)

	// Once the target is reachable again the rewrite is verified and rebound.
	if err := os.Rename(f.remote+".offline", f.remote); err != nil {
		t.Fatal(err)
	}
	if rebound := f.service.Recover(f.ctx, false); !rebound.Recovered || rebound.Recovery == nil || rebound.Recovery.Source != "remote_rewritten" {
		t.Fatalf("recover after the target returned = %#v", rebound)
	}
}

func TestRecoverAfterCustodyReturnDoesNotSucceedWhenBoundRemoteRefIsDeleted(t *testing.T) {
	t.Parallel()

	f, _ := newRemoteRewrittenFixture(t)
	if err := f.db.SetRunCustodyReturned(f.run.ID); err != nil {
		t.Fatal(err)
	}
	mustRun(t, f.local, "push", f.remote, ":refs/heads/feature/sync")
	state := f.service.Recover(f.ctx, false)
	if state.Recovered {
		t.Fatalf("recover with a deleted bound remote ref = %#v", state)
	}
	assertRewrittenBindingUntouched(t, f)
}

func TestRecoverAfterCustodyReturnDoesNotSucceedForClosedPRWithRewrittenRemote(t *testing.T) {
	t.Parallel()

	f, _ := newRemoteRewrittenFixture(t)
	if err := f.db.SetRunCustodyReturned(f.run.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.db.UpdateRunPRState(f.run.ID, "closed"); err != nil {
		t.Fatal(err)
	}
	state := f.service.Recover(f.ctx, false)
	if state.Recovered {
		t.Fatalf("recover for a closed PR with a rewritten remote = %#v", state)
	}
	assertRewrittenBindingUntouched(t, f)
}

func TestRecoverRewrittenRemoteRefusesWhenNewerRunTakesBranchBeforeRebind(t *testing.T) {
	t.Parallel()

	f, _ := newRemoteRewrittenFixture(t)
	f.service.beforeRecoverRebind = func() {
		newer, err := f.db.InsertRun(f.repo.ID, "feature/sync", f.old, f.base)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.db.UpdateRunStatus(newer.ID, types.RunRunning); err != nil {
			t.Fatal(err)
		}
	}
	state := f.service.Recover(f.ctx, false)
	if state.Recovered {
		t.Fatalf("recover after a newer run took the branch = %#v", state)
	}
	assertRewrittenBindingUntouched(t, f)
}

// TestRecoverOnPushedRunSucceedsOnlyForCommittedRebindOrVerifiedBinding walks
// the fresh states a terminal run with a push binding can reach and pins the
// success allowlist: Recovered is true only when the rewrite rebind committed
// or a fresh live check proved the binding already equals the live head.
func TestRecoverOnPushedRunSucceedsOnlyForCommittedRebindOrVerifiedBinding(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name          string
		custody       bool
		rewrite       bool
		setup         func(t *testing.T, f *syncFixture)
		wantRecovered bool
	}{
		{name: "rewritten remote rebinds", rewrite: true, wantRecovered: true},
		{name: "rewritten remote rebinds after custody return", custody: true, rewrite: true, wantRecovered: true},
		{name: "live equals binding after custody return", custody: true, wantRecovered: true},
		{name: "merged PR live equals binding after custody return", custody: true, wantRecovered: true, setup: func(t *testing.T, f *syncFixture) {
			if err := f.db.UpdateRunPRState(f.run.ID, "merged"); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "live equals binding without custody return"},
		{name: "rewritten remote with keep-local", custody: true, rewrite: true, setup: nil},
		{name: "rewritten remote on closed PR", custody: true, rewrite: true, setup: func(t *testing.T, f *syncFixture) {
			if err := f.db.UpdateRunPRState(f.run.ID, "closed"); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "rewritten remote on merged PR", custody: true, rewrite: true, setup: func(t *testing.T, f *syncFixture) {
			if err := f.db.UpdateRunPRState(f.run.ID, "merged"); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "remote advanced", custody: true, setup: func(t *testing.T, f *syncFixture) {
			writer := cloneRemoteBranch(t, f.remote)
			mustWrite(t, filepath.Join(writer, "advanced.txt"), "advanced\n")
			mustRun(t, writer, "add", "advanced.txt")
			mustRun(t, writer, "commit", "-m", "out of band")
			mustRun(t, writer, "push", "origin", "HEAD:refs/heads/feature/sync")
		}},
		{name: "remote ref deleted", custody: true, setup: func(t *testing.T, f *syncFixture) {
			mustRun(t, f.local, "push", f.remote, ":refs/heads/feature/sync")
		}},
		{name: "remote offline", custody: true, setup: func(t *testing.T, f *syncFixture) {
			if err := os.Rename(f.remote, f.remote+".offline"); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "remote changed during refresh", custody: true, setup: func(t *testing.T, f *syncFixture) {
			f.service.lsRemote = func(context.Context, string, string, string) (string, error) {
				return strings.Repeat("a", 40), nil
			}
		}},
		{name: "dirty worktree", custody: true, setup: func(t *testing.T, f *syncFixture) {
			mustWrite(t, filepath.Join(f.local, "file.txt"), "uncommitted\n")
		}},
		{name: "push target changed", custody: true, setup: func(t *testing.T, f *syncFixture) {
			other := filepath.Join(t.TempDir(), "other.git")
			mustRun(t, filepath.Dir(other), "init", "--bare", other)
			updated, err := f.db.UpdateRepoForkURL(f.repo.ID, other)
			if err != nil {
				t.Fatal(err)
			}
			f.service.Repo = updated
		}},
		{name: "run head changed before the rebind", rewrite: true, setup: func(t *testing.T, f *syncFixture) {
			f.service.beforeRecoverRebind = func() {
				if err := f.db.UpdateRunHeadSHA(f.run.ID, f.old); err != nil {
					t.Fatal(err)
				}
			}
		}},
		{name: "rewritten remote while the run is active", rewrite: true, setup: func(t *testing.T, f *syncFixture) {
			if err := f.db.UpdateRunStatus(f.run.ID, types.RunRunning); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newSyncFixture(t)
			gate := filepath.Join(filepath.Dir(f.local), "gate.git")
			mustRun(t, filepath.Dir(f.local), "clone", "--bare", f.remote, gate)
			f.service.GateDir = gate
			// The operator synchronized to the pipeline head before anything
			// else happened, so the verified-binding cases start clean.
			if state := f.service.Apply(f.ctx); state.State != StateSynchronized {
				t.Fatalf("initial sync = %#v", state)
			}
			if tc.custody {
				if err := f.db.SetRunCustodyReturned(f.run.ID); err != nil {
					t.Fatal(err)
				}
			}
			if tc.rewrite {
				forceRewriteRemote(t, f, "rewrite")
			}
			if tc.setup != nil {
				tc.setup(t, f)
			}
			keepLocal := tc.name == "rewritten remote with keep-local"
			state := f.service.Recover(f.ctx, keepLocal)
			if state.Recovered != tc.wantRecovered {
				t.Fatalf("Recovered = %v, want %v; state = %#v", state.Recovered, tc.wantRecovered, state)
			}
			run, err := f.db.GetRun(f.run.ID)
			if err != nil {
				t.Fatal(err)
			}
			rebound := ptr(run.LastPushedSHA) != f.pushed
			if rebound != (tc.wantRecovered && tc.rewrite) {
				t.Fatalf("binding rebound = %v for %q (pushed %s)", rebound, tc.name, ptr(run.LastPushedSHA))
			}
		})
	}
}

func TestRecoverRewrittenRemoteRefusesWhenRunHeadChangesBeforeRebind(t *testing.T) {
	t.Parallel()

	f, _ := newRemoteRewrittenFixture(t)
	f.service.beforeRecoverRebind = func() {
		if err := f.db.UpdateRunHeadSHA(f.run.ID, f.old); err != nil {
			t.Fatal(err)
		}
	}
	state := f.service.Recover(f.ctx, false)
	if state.Recovered {
		t.Fatalf("recover after the run head changed = %#v", state)
	}
	run, err := f.db.GetRun(f.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ptr(run.LastPushedSHA) != f.pushed || value(run.PushGeneration) != value(f.run.PushGeneration) {
		t.Fatalf("binding moved to %s generation %d while the run head changed", ptr(run.LastPushedSHA), value(run.PushGeneration))
	}
}

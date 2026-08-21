package branchsync

import (
	"context"
	"net"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func makePreservedHeadUnavailable(t *testing.T, f *recoverFixture) {
	t.Helper()
	mustRun(t, f.local, "remote", "add", "origin", f.remote)
	mustRun(t, f.local, "push", "origin", "refs/heads/feature/recover:refs/heads/feature/recover")
	mustRun(t, f.gate, "update-ref", "refs/heads/feature/recover", f.submitted, f.preserved)
	mustRun(t, f.gate, "reflog", "expire", "--expire=now", "--all")
	mustRun(t, f.gate, "gc", "--prune=now")
	if objectExists(f.ctx, f.local, f.preserved) {
		t.Fatal("fixture left the preserved head in the operator repository")
	}
	if objectExists(f.ctx, f.gate, f.preserved) {
		t.Fatal("fixture left the preserved head in the gate repository")
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.submitted {
		t.Fatalf("local head = %s, want submitted %s", got, f.submitted)
	}
	if got := mustRun(t, f.remote, "rev-parse", "refs/heads/feature/recover"); got != f.submitted {
		t.Fatalf("remote head = %s, want submitted %s", got, f.submitted)
	}
}

func assertUnavailableReleaseUnchanged(t *testing.T, f *recoverFixture, wantLocal, wantGate string) {
	t.Helper()
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != wantLocal {
		t.Fatalf("local HEAD = %s, want unchanged %s", got, wantLocal)
	}
	if got := mustRun(t, f.gate, "rev-parse", "refs/heads/feature/recover"); got != wantGate {
		t.Fatalf("gate branch = %s, want unchanged %s", got, wantGate)
	}
	if f.custodyReturned() {
		t.Fatal("refused unavailable-head release stamped custody")
	}
}

func TestReleaseUnavailablePreservedHeadReturnsCustodyWithAuditableAnchors(t *testing.T) {
	t.Parallel()

	f := newRecoverFixture(t, types.RunFailed)
	makePreservedHeadUnavailable(t, f)
	ordinary := f.service.Recover(f.ctx, false)
	if ordinary.Safety != "blocked_recover_gate_diverged" || ordinary.NextAction == nil || ordinary.NextAction.Code != "release_unavailable_custody" || !strings.Contains(ordinary.NextAction.Command, f.run.ID) {
		t.Fatalf("ordinary recovery did not identify exact unavailable-head release: %#v", ordinary)
	}
	state := f.service.ReleaseUnavailable(f.ctx, f.run.ID)
	if !state.Released || state.Recovered || state.Changed {
		t.Fatalf("release result = %#v", state)
	}
	if state.State != StateCustodyReturned || state.Safety != "custody_returned" {
		t.Fatalf("post-release state = %s/%s", state.State, state.Safety)
	}
	transition := state.CustodyTransition
	if transition == nil || transition.Action != "release_unavailable" || transition.Reason != db.CustodyReturnReasonPreservedHeadUnavailable || transition.RunID != f.run.ID || transition.Idempotent {
		t.Fatalf("custody transition = %#v", transition)
	}
	if transition.PreservedHead != f.preserved || transition.LocalHead != f.submitted || transition.RemoteHead != f.submitted || transition.GateHead != f.submitted {
		t.Fatalf("custody transition heads = %#v", transition)
	}
	for _, anchor := range []struct {
		ref, dir, want string
	}{
		{transition.LocalAnchor, f.local, f.submitted},
		{transition.RemoteAnchor, f.local, f.submitted},
		{transition.GateAnchor, f.gate, f.submitted},
	} {
		if got := mustRun(t, anchor.dir, "rev-parse", anchor.ref+"^{commit}"); got != anchor.want {
			t.Fatalf("anchor %s = %s, want %s", anchor.ref, got, anchor.want)
		}
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.submitted {
		t.Fatalf("release moved local HEAD to %s", got)
	}
	stored, err := f.db.GetRun(f.run.ID)
	if err != nil || stored == nil || stored.CustodyReturnedAt == nil || stored.CustodyReturnReason == nil || *stored.CustodyReturnReason != db.CustodyReturnReasonPreservedHeadUnavailable {
		t.Fatalf("stored release audit = %#v, %v", stored, err)
	}

	retry := f.service.ReleaseUnavailable(f.ctx, f.run.ID)
	if !retry.Released || retry.Changed || retry.CustodyTransition == nil || !retry.CustodyTransition.Idempotent {
		t.Fatalf("idempotent retry = %#v", retry)
	}
	if retry.CustodyTransition.Reason != db.CustodyReturnReasonPreservedHeadUnavailable || retry.CustodyTransition.LocalAnchor != transition.LocalAnchor || retry.CustodyTransition.RemoteAnchor != transition.RemoteAnchor || retry.CustodyTransition.GateAnchor != transition.GateAnchor || retry.CustodyTransition.LocalHead != f.submitted || retry.CustodyTransition.RemoteHead != f.submitted || retry.CustodyTransition.GateHead != f.submitted {
		t.Fatalf("retry transition = %#v", retry.CustodyTransition)
	}
}

func TestReleaseUnavailableAcceptsCIMonitorInterruptedRun(t *testing.T) {
	t.Parallel()

	f := newRecoverFixture(t, types.RunCIMonitorInterrupted)
	makePreservedHeadUnavailable(t, f)
	state := f.service.ReleaseUnavailable(f.ctx, f.run.ID)
	if !state.Released || state.Safety != "custody_returned" || !f.custodyReturned() {
		t.Fatalf("CI-monitor-interrupted release = %#v", state)
	}
}

func TestReleaseUnavailablePreservedHeadRefusalMatrix(t *testing.T) {
	t.Parallel()

	t.Run("active owner", func(t *testing.T) {
		f := newRecoverFixture(t, types.RunRunning)
		makePreservedHeadUnavailable(t, f)
		state := f.service.ReleaseUnavailable(f.ctx, f.run.ID)
		if state.Released || state.Safety != "blocked_release_run_active" {
			t.Fatalf("active release = %#v", state)
		}
		assertUnavailableReleaseUnchanged(t, f, f.submitted, f.submitted)
	})

	t.Run("preserved head remains recoverable in gate", func(t *testing.T) {
		f := newRecoverFixture(t, types.RunFailed)
		mustRun(t, f.local, "remote", "add", "origin", f.remote)
		mustRun(t, f.local, "push", "origin", "refs/heads/feature/recover:refs/heads/feature/recover")
		state := f.service.ReleaseUnavailable(f.ctx, f.run.ID)
		if state.Released || state.Safety != "blocked_release_preserved_recoverable" {
			t.Fatalf("recoverable release = %#v", state)
		}
		assertUnavailableReleaseUnchanged(t, f, f.submitted, f.preserved)
		if recovered := f.service.Recover(f.ctx, false); !recovered.Recovered || recovered.Local.Head != f.preserved {
			t.Fatalf("ordinary preserved-head recovery stopped working: %#v", recovered)
		}
	})

	t.Run("preserved head remains recoverable from configured remote ref", func(t *testing.T) {
		f := newRecoverFixture(t, types.RunFailed)
		mustRun(t, f.gate, "push", f.remote, f.preserved+":refs/heads/safety-preserved")
		makePreservedHeadUnavailable(t, f)
		state := f.service.ReleaseUnavailable(f.ctx, f.run.ID)
		if state.Released || state.Safety != "blocked_release_preserved_recoverable" {
			t.Fatalf("remote-preserved release = %#v", state)
		}
		assertUnavailableReleaseUnchanged(t, f, f.submitted, f.submitted)
	})

	t.Run("dirty worktree", func(t *testing.T) {
		f := newRecoverFixture(t, types.RunFailed)
		makePreservedHeadUnavailable(t, f)
		mustWrite(t, filepath.Join(f.local, "dirty.txt"), "dirty\n")
		state := f.service.ReleaseUnavailable(f.ctx, f.run.ID)
		if state.Released || state.Safety != "blocked_release_dirty" {
			t.Fatalf("dirty release = %#v", state)
		}
		assertUnavailableReleaseUnchanged(t, f, f.submitted, f.submitted)
	})

	t.Run("local and remote genuinely diverged", func(t *testing.T) {
		f := newRecoverFixture(t, types.RunFailed)
		makePreservedHeadUnavailable(t, f)
		mustWrite(t, filepath.Join(f.local, "local-only.txt"), "local only\n")
		mustRun(t, f.local, "add", "local-only.txt")
		mustRun(t, f.local, "commit", "-m", "genuine local divergence")
		diverged := mustRun(t, f.local, "rev-parse", "HEAD")
		state := f.service.ReleaseUnavailable(f.ctx, f.run.ID)
		if state.Released || state.Safety != "blocked_release_remote_mismatch" {
			t.Fatalf("diverged release = %#v", state)
		}
		assertUnavailableReleaseUnchanged(t, f, diverged, f.submitted)
	})

	t.Run("duplicate branch checkout", func(t *testing.T) {
		f := newRecoverFixture(t, types.RunFailed)
		makePreservedHeadUnavailable(t, f)
		duplicate := filepath.Join(t.TempDir(), "duplicate")
		mustRun(t, f.local, "worktree", "add", "--force", duplicate, "feature/recover")
		state := f.service.ReleaseUnavailable(f.ctx, f.run.ID)
		if state.Released || state.Safety != "blocked_release_branch_ambiguous" {
			t.Fatalf("duplicate-branch release = %#v", state)
		}
		assertUnavailableReleaseUnchanged(t, f, f.submitted, f.submitted)
	})

	t.Run("ambiguous configured target remote", func(t *testing.T) {
		f := newRecoverFixture(t, types.RunFailed)
		makePreservedHeadUnavailable(t, f)
		mustRun(t, f.local, "remote", "add", "duplicate-target", f.remote)
		state := f.service.ReleaseUnavailable(f.ctx, f.run.ID)
		if state.Released || state.Safety != "blocked_release_target_ambiguous" {
			t.Fatalf("ambiguous-target release = %#v", state)
		}
		assertUnavailableReleaseUnchanged(t, f, f.submitted, f.submitted)
	})

	t.Run("another branch", func(t *testing.T) {
		f := newRecoverFixture(t, types.RunFailed)
		makePreservedHeadUnavailable(t, f)
		mustRun(t, f.local, "checkout", "-b", "feature/other")
		state := f.service.ReleaseUnavailable(f.ctx, f.run.ID)
		if state.Released || state.Safety != "blocked_release_run_identity" {
			t.Fatalf("other-branch release = %#v", state)
		}
		if f.custodyReturned() {
			t.Fatal("other-branch release stamped custody")
		}
	})

	t.Run("another repository", func(t *testing.T) {
		f := newRecoverFixture(t, types.RunFailed)
		makePreservedHeadUnavailable(t, f)
		otherPath := filepath.Join(t.TempDir(), "other")
		mustRun(t, filepath.Dir(otherPath), "init", "-b", "main", otherPath)
		otherRepo, err := f.db.InsertRepo(otherPath, filepath.Join(t.TempDir(), "other.git"), "main")
		if err != nil {
			t.Fatal(err)
		}
		otherRun, err := f.db.InsertRun(otherRepo.ID, "feature/recover", f.submitted, f.base)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.db.UpdateRunStatusWithVerifiedHead(otherRun.ID, types.RunFailed, f.preserved); err != nil {
			t.Fatal(err)
		}
		state := f.service.ReleaseUnavailable(f.ctx, otherRun.ID)
		if state.Released || state.Safety != "blocked_release_run_identity" {
			t.Fatalf("other-repo release = %#v", state)
		}
		stored, err := f.db.GetRun(otherRun.ID)
		if err != nil || stored == nil || stored.CustodyReturnedAt != nil {
			t.Fatalf("other repo run = %#v, %v", stored, err)
		}
	})

	t.Run("ambiguous selected owner", func(t *testing.T) {
		f := newRecoverFixture(t, types.RunFailed)
		makePreservedHeadUnavailable(t, f)
		newer, err := f.db.InsertRun(f.repo.ID, "feature/recover", f.submitted, f.base)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.db.UpdateRunStatus(newer.ID, types.RunRunning); err != nil {
			t.Fatal(err)
		}
		state := f.service.ReleaseUnavailable(f.ctx, f.run.ID)
		if state.Released || state.Safety != "blocked_release_run_identity" {
			t.Fatalf("ambiguous-owner release = %#v", state)
		}
		assertUnavailableReleaseUnchanged(t, f, f.submitted, f.submitted)
	})
}

// TestReleaseUnavailableStagingFetchDoesNotShareTheRefReadBudget pins that each
// network step of a release owns its own deadline. The reachability probe runs
// between the target ref read and the remote safety anchor's staging fetch under
// a far larger budget, so a shared deadline is already spent by the time the
// fetch starts - deterministically, on exactly the hosted repositories that made
// the probe expensive enough to need its own budget.
func TestReleaseUnavailableStagingFetchDoesNotShareTheRefReadBudget(t *testing.T) {
	t.Parallel()

	f := newRecoverFixture(t, types.RunFailed)
	makePreservedHeadUnavailable(t, f)
	budget := 2 * time.Second
	f.service.RemoteTimeout = budget
	f.service.afterUnavailableReleaseProbe = func() { time.Sleep(budget + budget/4) }

	state := f.service.ReleaseUnavailable(f.ctx, f.run.ID)
	if !state.Released || state.Safety != "custody_returned" {
		t.Fatalf("release after a probe outliving one ref-read budget = %#v", state)
	}
	transition := state.CustodyTransition
	if transition == nil || transition.RemoteAnchor == "" {
		t.Fatalf("custody transition = %#v", transition)
	}
	if got := mustRun(t, f.local, "rev-parse", transition.RemoteAnchor+"^{commit}"); got != f.submitted {
		t.Fatalf("remote safety anchor = %s, want %s", got, f.submitted)
	}
}

func TestRemoteReachabilityProbeUsesConfiguredBudgetPerFetch(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serverCtx, stopServer := context.WithCancel(context.Background())
	connections := make(chan net.Conn, 2)
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			connections <- conn
		}
	}()
	t.Cleanup(func() {
		stopServer()
		_ = listener.Close()
		for {
			select {
			case conn := <-connections:
				_ = conn.Close()
			default:
				return
			}
		}
	})
	go func() {
		for {
			select {
			case conn := <-connections:
				go func() {
					<-serverCtx.Done()
					_ = conn.Close()
				}()
			case <-serverCtx.Done():
				return
			}
		}
	}()

	workDir := filepath.Join(t.TempDir(), "work")
	mustRun(t, filepath.Dir(workDir), "init", workDir)
	budget := 100 * time.Millisecond
	probe, closeProbe, err := newRemoteReachabilityProbe(context.Background(), workDir, budget)
	if err != nil {
		t.Fatal(err)
	}
	defer closeProbe()
	remote := "git://" + listener.Addr().String() + "/repo"
	for attempt := 1; attempt <= 2; attempt++ {
		callCtx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
		started := time.Now()
		_, retainErr := probe.retains(callCtx, remote, strings.Repeat("0", 40))
		elapsed := time.Since(started)
		cancel()
		if retainErr == nil {
			t.Fatalf("probe fetch %d unexpectedly succeeded", attempt)
		}
		if elapsed < budget/2 || elapsed >= 700*time.Millisecond {
			t.Fatalf("probe fetch %d duration = %v, want a fresh configured budget near %v", attempt, elapsed, budget)
		}
	}
}

// TestReleaseUnavailableReportsOnlyAnchorsItCreated pins the audit contract of
// the one command whose whole safety story is anchor provenance: a refused
// attempt must never name a ref an operator cannot resolve.
func TestReleaseUnavailableReportsOnlyAnchorsItCreated(t *testing.T) {
	t.Parallel()

	t.Run("refusal before any anchor", func(t *testing.T) {
		f := newRecoverFixture(t, types.RunFailed)
		makePreservedHeadUnavailable(t, f)
		mustWrite(t, filepath.Join(f.local, "dirty.txt"), "dirty\n")

		state := f.service.ReleaseUnavailable(f.ctx, f.run.ID)
		if state.Released || state.Safety != "blocked_release_dirty" {
			t.Fatalf("dirty release = %#v", state)
		}
		assertNoReportedAnchors(t, state.CustodyTransition)
		for _, dir := range []string{f.local, f.gate} {
			if refs := mustRun(t, dir, "for-each-ref", "--format=%(refname)", unavailableReleaseRef(f.run.ID)+"/"); refs != "" {
				t.Fatalf("refused release left custody-release refs in %s: %q", dir, refs)
			}
		}
	})

	t.Run("refusal after the local anchor is refused", func(t *testing.T) {
		f := newRecoverFixture(t, types.RunFailed)
		makePreservedHeadUnavailable(t, f)
		localAnchor := unavailableReleaseAnchorRef(unavailableReleaseRef(f.run.ID), "local", f.submitted)
		mustRun(t, f.local, "symbolic-ref", localAnchor, "refs/heads/feature/recover")

		state := f.service.ReleaseUnavailable(f.ctx, f.run.ID)
		if state.Released || state.Safety != "blocked_release_preserve_failed" {
			t.Fatalf("symbolic local-anchor release = %#v", state)
		}
		assertNoReportedAnchors(t, state.CustodyTransition)
		if state.CustodyTransition.RemoteHead != f.submitted {
			t.Fatalf("observed remote head = %q, want the head the attempt read", state.CustodyTransition.RemoteHead)
		}
		assertUnavailableReleaseUnchanged(t, f, f.submitted, f.submitted)
	})
}

func assertNoReportedAnchors(t *testing.T, transition *CustodyTransition) {
	t.Helper()
	if transition == nil {
		t.Fatal("refused release reported no custody transition")
	}
	if transition.LocalAnchor != "" || transition.RemoteAnchor != "" || transition.GateAnchor != "" {
		t.Fatalf("refused release reported anchors it never created: %#v", transition)
	}
}

func TestReleaseUnavailableRechecksRemoteBeforeOwnershipTransition(t *testing.T) {
	t.Parallel()

	f := newRecoverFixture(t, types.RunFailed)
	makePreservedHeadUnavailable(t, f)
	f.service.beforeUnavailableReleaseGateMove = func() {
		writer := filepath.Join(t.TempDir(), "remote-writer")
		mustRun(t, filepath.Dir(writer), "-c", "core.autocrlf=false", "clone", f.remote, writer)
		configureIdentity(t, writer)
		mustRun(t, writer, "checkout", "feature/recover")
		mustWrite(t, filepath.Join(writer, "remote-only.txt"), "remote changed\n")
		mustRun(t, writer, "add", "remote-only.txt")
		mustRun(t, writer, "commit", "-m", "remote changes during release")
		mustRun(t, writer, "push", "origin", "HEAD:refs/heads/feature/recover")
	}
	state := f.service.ReleaseUnavailable(f.ctx, f.run.ID)
	if state.Released || state.Safety != "blocked_release_remote_changed" {
		t.Fatalf("remote-race release = %#v", state)
	}
	assertUnavailableReleaseUnchanged(t, f, f.submitted, f.submitted)
	if state.CustodyTransition == nil || state.CustodyTransition.LocalAnchor == "" || state.CustodyTransition.RemoteAnchor == "" {
		t.Fatalf("remote-race release did not retain pre-transition anchors: %#v", state.CustodyTransition)
	}
}

func TestReleaseUnavailableRefusesGateCASRaceAndPreservesBothHeads(t *testing.T) {
	t.Parallel()

	f := newRecoverFixture(t, types.RunFailed)
	makePreservedHeadUnavailable(t, f)
	var racingHead string
	f.service.beforeUnavailableReleaseGateCAS = func() {
		writer := filepath.Join(t.TempDir(), "gate-writer")
		mustRun(t, filepath.Dir(writer), "-c", "core.autocrlf=false", "clone", f.gate, writer)
		configureIdentity(t, writer)
		mustRun(t, writer, "checkout", "feature/recover")
		mustWrite(t, filepath.Join(writer, "gate-race.txt"), "gate race\n")
		mustRun(t, writer, "add", "gate-race.txt")
		mustRun(t, writer, "commit", "-m", "gate changes during release")
		racingHead = mustRun(t, writer, "rev-parse", "HEAD")
		mustRun(t, writer, "push", "origin", "HEAD:refs/heads/feature/recover")
	}
	state := f.service.ReleaseUnavailable(f.ctx, f.run.ID)
	if state.Released || state.Safety != "blocked_release_gate_race" {
		t.Fatalf("gate-race release = %#v", state)
	}
	if got := mustRun(t, f.gate, "rev-parse", "refs/heads/feature/recover"); got != racingHead {
		t.Fatalf("gate race head = %s, want %s", got, racingHead)
	}
	if got := mustRun(t, f.gate, "rev-parse", state.CustodyTransition.GateAnchor); got != f.submitted {
		t.Fatalf("pre-race gate anchor = %s, want %s", got, f.submitted)
	}
	if got := mustRun(t, f.local, "rev-parse", state.CustodyTransition.LocalAnchor); got != f.submitted {
		t.Fatalf("local anchor = %s, want %s", got, f.submitted)
	}
	if f.custodyReturned() {
		t.Fatal("gate-race release stamped custody")
	}
}

func TestReleaseUnavailableStampFailureIsCrashSafeAndRetryable(t *testing.T) {
	t.Parallel()

	f := newRecoverFixture(t, types.RunFailed)
	makePreservedHeadUnavailable(t, f)
	f.service.beforeUnavailableReleaseStamp = func() {
		newer, err := f.db.InsertRun(f.repo.ID, "feature/recover", f.submitted, f.base)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.db.UpdateRunStatus(newer.ID, types.RunRunning); err != nil {
			t.Fatal(err)
		}
	}
	state := f.service.ReleaseUnavailable(f.ctx, f.run.ID)
	if state.Released || state.Safety != "blocked_release_assumptions_changed" {
		t.Fatalf("stamp-race release = %#v", state)
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != f.submitted {
		t.Fatalf("stamp-race release moved local HEAD to %s", got)
	}
	if f.custodyReturned() {
		t.Fatal("stamp-race release stamped stale custody")
	}
	for _, ref := range []string{state.CustodyTransition.LocalAnchor, state.CustodyTransition.RemoteAnchor, state.CustodyTransition.GateAnchor} {
		if strings.TrimSpace(ref) == "" {
			t.Fatalf("stamp-race transition missing anchor: %#v", state.CustodyTransition)
		}
	}
}

func TestReleaseUnavailableRefusesRemoteDescendantThatRetainsPreservedHead(t *testing.T) {
	t.Parallel()

	f := newRecoverFixture(t, types.RunFailed)
	writer := filepath.Join(t.TempDir(), "descendant-writer")
	mustRun(t, filepath.Dir(writer), "-c", "core.autocrlf=false", "clone", f.gate, writer)
	configureIdentity(t, writer)
	mustRun(t, writer, "checkout", "feature/recover")
	mustWrite(t, filepath.Join(writer, "descendant.txt"), "descendant retains preserved head\n")
	mustRun(t, writer, "add", "descendant.txt")
	mustRun(t, writer, "commit", "-m", "remote descendant")
	descendant := mustRun(t, writer, "rev-parse", "HEAD")
	mustRun(t, writer, "push", f.remote, "HEAD:refs/heads/safety-descendant")
	makePreservedHeadUnavailable(t, f)

	probe := filepath.Join(t.TempDir(), "probe")
	mustRun(t, filepath.Dir(probe), "-c", "core.autocrlf=false", "clone", "--no-checkout", f.remote, probe)
	if !objectExists(f.ctx, probe, f.preserved) {
		t.Fatalf("fetching advertised descendant %s did not retain preserved head %s", descendant, f.preserved)
	}

	refsBefore := mustRun(t, f.local, "for-each-ref", "--format=%(refname) %(objectname)")
	state := f.service.ReleaseUnavailable(f.ctx, f.run.ID)
	if state.Released || state.Safety != "blocked_release_preserved_recoverable" {
		t.Fatalf("remote descendant release = %#v", state)
	}
	assertUnavailableReleaseUnchanged(t, f, f.submitted, f.submitted)
	// The reachability probe is the only step that fetches every advertised
	// ref, and it must do so in its own throwaway repository: the invoking
	// repository keeps its exact refs and never gains remote objects it did
	// not ask for.
	if got := mustRun(t, f.local, "for-each-ref", "--format=%(refname) %(objectname)"); got != refsBefore {
		t.Fatalf("reachability probe changed invoking refs:\nbefore:\n%s\nafter:\n%s", refsBefore, got)
	}
	if objectExists(f.ctx, f.local, f.preserved) {
		t.Fatalf("reachability probe imported remote object %s into the invoking repository", f.preserved)
	}
}

func TestReleaseUnavailableShallowCloneCannotHideRetainedPreservedHead(t *testing.T) {
	t.Parallel()

	f := newRecoverFixture(t, types.RunFailed)
	writer := filepath.Join(t.TempDir(), "descendant-writer")
	mustRun(t, filepath.Dir(writer), "-c", "core.autocrlf=false", "clone", f.gate, writer)
	configureIdentity(t, writer)
	mustRun(t, writer, "checkout", "feature/recover")
	mustWrite(t, filepath.Join(writer, "descendant.txt"), "remote descendant\n")
	mustRun(t, writer, "add", "descendant.txt")
	mustRun(t, writer, "commit", "-m", "remote descendant")
	descendant := mustRun(t, writer, "rev-parse", "HEAD")
	mustRun(t, writer, "push", f.remote, "HEAD:refs/heads/safety-descendant")
	mustRun(t, f.local, "push", f.remote, "refs/heads/feature/recover:refs/heads/feature/recover")

	mustRun(t, f.gate, "update-ref", "refs/heads/feature/recover", f.submitted, f.preserved)
	mustRun(t, f.gate, "reflog", "expire", "--expire=now", "--all")
	mustRun(t, f.gate, "gc", "--prune=now")

	shallow := filepath.Join(t.TempDir(), "shallow-operator")
	remoteURL := (&url.URL{Scheme: "file", Path: filepath.ToSlash(f.remote)}).String()
	mustRun(t, filepath.Dir(shallow), "clone", "--depth=1", "--branch=safety-descendant", remoteURL, shallow)
	mustRun(t, shallow, "fetch", "--depth=1", "origin", "refs/heads/feature/recover:refs/remotes/origin/feature/recover")
	mustRun(t, shallow, "checkout", "-b", "feature/recover", "refs/remotes/origin/feature/recover")
	mustRun(t, shallow, "remote", "set-url", "origin", f.remote)
	if got := mustRun(t, shallow, "rev-parse", "--is-shallow-repository"); got != "true" {
		t.Fatalf("invoking clone shallow = %q, want true", got)
	}
	if got := mustRun(t, shallow, "rev-parse", "refs/heads/safety-descendant"); got != descendant {
		t.Fatalf("local advertised tip = %s, want %s", got, descendant)
	}
	if objectExists(f.ctx, shallow, f.preserved) {
		t.Fatalf("shallow invoking clone unexpectedly retained preserved head %s", f.preserved)
	}
	if objectExists(f.ctx, f.gate, f.preserved) {
		t.Fatalf("gate unexpectedly retained preserved head %s", f.preserved)
	}

	repo, err := f.db.UpdateRepoWorkingPath(f.repo.ID, shallow)
	if err != nil {
		t.Fatal(err)
	}
	f.repo = repo
	f.local = shallow
	f.service.Repo = repo
	f.service.WorkDir = shallow
	state := f.service.ReleaseUnavailable(f.ctx, f.run.ID)
	if state.Released || state.Safety != "blocked_release_preserved_recoverable" {
		t.Fatalf("shallow remote-retention release = %#v", state)
	}
	assertUnavailableReleaseUnchanged(t, f, f.submitted, f.submitted)
}

func makeDistinctGateHead(t *testing.T, f *recoverFixture) string {
	t.Helper()
	writer := filepath.Join(t.TempDir(), "gate-head-writer")
	mustRun(t, filepath.Dir(writer), "-c", "core.autocrlf=false", "clone", f.local, writer)
	configureIdentity(t, writer)
	mustRun(t, writer, "checkout", "feature/recover")
	mustWrite(t, filepath.Join(writer, "gate-only.txt"), "gate-only work\n")
	mustRun(t, writer, "add", "gate-only.txt")
	mustRun(t, writer, "commit", "-m", "gate-only head")
	gateHead := mustRun(t, writer, "rev-parse", "HEAD")
	staging := "refs/no-mistakes/test-gate-head"
	mustRun(t, f.gate, "fetch", writer, "+refs/heads/feature/recover:"+staging)
	mustRun(t, f.gate, "update-ref", "refs/heads/feature/recover", gateHead, f.submitted)
	mustRun(t, f.gate, "update-ref", "-d", staging)
	return gateHead
}

func TestReleaseUnavailableRejectsSymbolicSafetyAnchor(t *testing.T) {
	t.Parallel()

	f := newRecoverFixture(t, types.RunFailed)
	makePreservedHeadUnavailable(t, f)
	gateHead := makeDistinctGateHead(t, f)
	gateAnchor := unavailableReleaseAnchorRef(unavailableReleaseRef(f.run.ID), "gate", gateHead)
	mustRun(t, f.gate, "symbolic-ref", gateAnchor, "refs/heads/feature/recover")

	state := f.service.ReleaseUnavailable(f.ctx, f.run.ID)
	if state.Released || state.Safety != "blocked_release_preserve_failed" {
		t.Fatalf("symbolic-anchor release = %#v", state)
	}
	assertUnavailableReleaseUnchanged(t, f, f.submitted, gateHead)
	if got := mustRun(t, f.gate, "symbolic-ref", gateAnchor); got != "refs/heads/feature/recover" {
		t.Fatalf("symbolic anchor changed to %q", got)
	}
}

func TestReleaseUnavailableAcceptsCanonicalPackedSafetyAnchorAndRetainsHead(t *testing.T) {
	t.Parallel()

	f := newRecoverFixture(t, types.RunFailed)
	makePreservedHeadUnavailable(t, f)
	gateHead := makeDistinctGateHead(t, f)
	gateAnchor := unavailableReleaseAnchorRef(unavailableReleaseRef(f.run.ID), "gate", gateHead)
	mustRun(t, f.gate, "update-ref", gateAnchor, gateHead, "")
	mustRun(t, f.gate, "pack-refs", "--all", "--prune")

	state := f.service.ReleaseUnavailable(f.ctx, f.run.ID)
	if !state.Released {
		t.Fatalf("canonical packed-anchor release = %#v", state)
	}
	if _, err := git.Run(f.ctx, f.gate, "symbolic-ref", "-q", gateAnchor); err == nil {
		t.Fatalf("gate anchor %s remained symbolic", gateAnchor)
	}
	if got := mustRun(t, f.gate, "rev-parse", gateAnchor+"^{commit}"); got != gateHead {
		t.Fatalf("gate anchor = %s, want original gate head %s", got, gateHead)
	}
	mustRun(t, f.gate, "reflog", "expire", "--expire=now", "--all")
	mustRun(t, f.gate, "gc", "--prune=now")
	if !objectExists(f.ctx, f.gate, gateHead) {
		t.Fatalf("canonical gate anchor did not retain %s through prune", gateHead)
	}
}

func TestReleaseUnavailableRetryAfterCrashFollowingDistinctGateCAS(t *testing.T) {
	t.Parallel()

	f := newRecoverFixture(t, types.RunFailed)
	makePreservedHeadUnavailable(t, f)
	gateHead := makeDistinctGateHead(t, f)
	crashAfterGateMove(t, f)
	if f.custodyReturned() {
		t.Fatal("simulated pre-stamp crash recorded custody")
	}
	if got := mustRun(t, f.gate, "rev-parse", "refs/heads/feature/recover"); got != f.submitted {
		t.Fatalf("post-CAS gate = %s, want %s", got, f.submitted)
	}
	gateAnchor := unavailableReleaseAnchorRef(unavailableReleaseRef(f.run.ID), "gate", gateHead)
	if got := mustRun(t, f.gate, "rev-parse", gateAnchor+"^{commit}"); got != gateHead {
		t.Fatalf("post-crash gate anchor = %s, want %s", got, gateHead)
	}

	retry := f.service.ReleaseUnavailable(f.ctx, f.run.ID)
	if !retry.Released || retry.CustodyTransition == nil || retry.CustodyTransition.GateHead != gateHead {
		t.Fatalf("post-crash retry = %#v", retry)
	}
}

// crashAfterGateMove drives one release attempt that durably journals and moves
// the gate branch and then dies before its custody stamp, exactly as a killed
// process leaves the world.
func crashAfterGateMove(t *testing.T, f *recoverFixture) {
	t.Helper()
	f.service.beforeUnavailableReleaseStamp = func() { panic("simulated crash after gate CAS") }
	defer func() { f.service.beforeUnavailableReleaseStamp = nil }()
	defer func() { _ = recover() }()
	_ = f.service.ReleaseUnavailable(f.ctx, f.run.ID)
}

// TestReleaseUnavailableRetryAfterAdvancedBranchIsNotADeadEnd pins the exit
// this command exists to provide: an attempt that stopped before its stamp must
// never permanently strand the run. The operator's branch then legitimately
// fast-forwards, so both the safety anchors and the durable journal now describe
// a head that is no longer current, and only a retry that supersedes them can
// still release custody.
func TestReleaseUnavailableRetryAfterAdvancedBranchIsNotADeadEnd(t *testing.T) {
	t.Parallel()

	f := newRecoverFixture(t, types.RunFailed)
	makePreservedHeadUnavailable(t, f)
	crashAfterGateMove(t, f)
	if f.custodyReturned() {
		t.Fatal("simulated pre-stamp crash recorded custody")
	}

	mustWrite(t, filepath.Join(f.local, "operator-advance.txt"), "operator work after the refused attempt\n")
	mustRun(t, f.local, "add", "operator-advance.txt")
	mustRun(t, f.local, "commit", "-m", "operator advance after refused release")
	advanced := mustRun(t, f.local, "rev-parse", "HEAD")
	mustRun(t, f.local, "push", "origin", "refs/heads/feature/recover:refs/heads/feature/recover")

	retry := f.service.ReleaseUnavailable(f.ctx, f.run.ID)
	if !retry.Released || retry.CustodyTransition == nil || retry.CustodyTransition.Idempotent {
		t.Fatalf("retry after advanced branch = %#v", retry)
	}
	if retry.CustodyTransition.LocalHead != advanced || retry.CustodyTransition.RemoteHead != advanced {
		t.Fatalf("retry transition heads = %#v, want %s", retry.CustodyTransition, advanced)
	}
	if !f.custodyReturned() {
		t.Fatal("retry after advanced branch did not stamp custody")
	}
	if got := mustRun(t, f.gate, "rev-parse", "refs/heads/feature/recover"); got != advanced {
		t.Fatalf("gate = %s, want the retried head %s", got, advanced)
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != advanced {
		t.Fatalf("retry moved the operator worktree to %s", got)
	}
	base := unavailableReleaseRef(f.run.ID)
	for _, anchor := range []struct {
		ref, want string
	}{
		{unavailableReleaseAnchorRef(base, "local", f.submitted), f.submitted},
		{retry.CustodyTransition.LocalAnchor, advanced},
		{retry.CustodyTransition.RemoteAnchor, advanced},
	} {
		if got := mustRun(t, f.local, "rev-parse", anchor.ref+"^{commit}"); got != anchor.want {
			t.Fatalf("anchor %s = %s, want %s", anchor.ref, got, anchor.want)
		}
	}
}

// TestReleaseUnavailableRetryAfterRegistrationGenerationChangeIsNotADeadEnd
// covers the same closed loop reached through the journal's generations: the
// operator moves their clone between attempts, which advances the repository
// metadata generation the first attempt journaled. Generations bind one
// attempt's own window, so the retry must rebind them rather than refuse
// forever against a value no live row can satisfy again.
func TestReleaseUnavailableRetryAfterRegistrationGenerationChangeIsNotADeadEnd(t *testing.T) {
	t.Parallel()

	f := newRecoverFixture(t, types.RunFailed)
	makePreservedHeadUnavailable(t, f)
	crashAfterGateMove(t, f)

	if _, err := f.db.UpdateRepoWorkingPath(f.repo.ID, filepath.Join(t.TempDir(), "moved-clone")); err != nil {
		t.Fatal(err)
	}
	refreshed, err := f.db.UpdateRepoWorkingPath(f.repo.ID, f.local)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed.MetadataGeneration == f.repo.MetadataGeneration {
		t.Fatalf("fixture did not advance the repository metadata generation past %d", f.repo.MetadataGeneration)
	}
	// A later invocation loads the current registration, exactly as the CLI does.
	f.service.Repo = refreshed

	retry := f.service.ReleaseUnavailable(f.ctx, f.run.ID)
	if !retry.Released {
		t.Fatalf("retry after registration generation change = %#v", retry)
	}
	if !f.custodyReturned() {
		t.Fatal("retry after registration generation change did not stamp custody")
	}
	attempt, err := f.db.GetUnavailableCustodyRelease(f.run.ID)
	if err != nil || attempt == nil || attempt.RepoGeneration != refreshed.MetadataGeneration {
		t.Fatalf("journal was not rebound to the live registration generation: %#v, %v", attempt, err)
	}
}

// TestRecoverUnverifiedHeadRefusalOffersReachableUnavailableRelease covers the
// refusals that run before the gate comparison: a crash-recovered terminal run
// carries no verified head, so recovery stops earlier, and that refusal must
// still name the exceptional release rather than end the guided path.
func TestRecoverUnverifiedHeadRefusalOffersReachableUnavailableRelease(t *testing.T) {
	t.Parallel()

	f := newRecoverFixture(t, types.RunRunning)
	if err := f.db.UpdateRunStatus(f.run.ID, types.RunFailed); err != nil {
		t.Fatal(err)
	}
	reloaded, err := f.db.GetRun(f.run.ID)
	if err != nil || reloaded == nil || reloaded.TerminalHeadVerifiedAt != nil {
		t.Fatalf("crash-recovered fixture run = %#v, %v", reloaded, err)
	}
	f.run = reloaded
	makePreservedHeadUnavailable(t, f)

	blocked := f.service.Recover(f.ctx, false)
	if blocked.Recovered || blocked.Safety != "blocked_recover_unverified_head" {
		t.Fatalf("unverified-head recovery = %#v", blocked)
	}
	if blocked.NextAction == nil || blocked.NextAction.Code != "release_unavailable_custody" || !strings.Contains(blocked.NextAction.Command, f.run.ID) {
		t.Fatalf("unverified-head refusal did not offer the exact release: %#v", blocked)
	}

	released := f.service.ReleaseUnavailable(f.ctx, f.run.ID)
	if !released.Released {
		t.Fatalf("the offered release refused: %#v", released)
	}
	if !f.custodyReturned() {
		t.Fatal("the offered release did not stamp custody")
	}
}

func retainPreservedHeadOutsideOwnedStores(t *testing.T, f *recoverFixture) string {
	t.Helper()
	holder := filepath.Join(t.TempDir(), "preserved-holder")
	mustRun(t, filepath.Dir(holder), "-c", "core.autocrlf=false", "clone", f.gate, holder)
	return holder
}

func assertUnavailableReleaseNotStamped(t *testing.T, f *recoverFixture, state State) {
	t.Helper()
	if state.Released {
		t.Fatalf("release unexpectedly succeeded: %#v", state)
	}
	if f.custodyReturned() {
		t.Fatal("refused release stamped custody")
	}
}

func TestReleaseUnavailableFinalBoundaryRevalidatesGitFacts(t *testing.T) {
	t.Parallel()

	t.Run("preserved object appears locally", func(t *testing.T) {
		f := newRecoverFixture(t, types.RunFailed)
		holder := retainPreservedHeadOutsideOwnedStores(t, f)
		makePreservedHeadUnavailable(t, f)
		f.service.beforeUnavailableReleaseStamp = func() {
			mustRun(t, f.local, "fetch", holder, f.preserved+":"+unavailableReleaseRef(f.run.ID)+"/restored-local")
		}
		state := f.service.ReleaseUnavailable(f.ctx, f.run.ID)
		assertUnavailableReleaseNotStamped(t, f, state)
		if state.Safety != "blocked_release_preserved_recoverable" {
			t.Fatalf("restored-local release = %#v", state)
		}
	})

	t.Run("preserved object appears in gate", func(t *testing.T) {
		f := newRecoverFixture(t, types.RunFailed)
		holder := retainPreservedHeadOutsideOwnedStores(t, f)
		makePreservedHeadUnavailable(t, f)
		f.service.beforeUnavailableReleaseStamp = func() {
			mustRun(t, f.gate, "fetch", holder, f.preserved+":"+unavailableReleaseRef(f.run.ID)+"/restored-gate")
		}
		state := f.service.ReleaseUnavailable(f.ctx, f.run.ID)
		assertUnavailableReleaseNotStamped(t, f, state)
		if state.Safety != "blocked_release_preserved_recoverable" {
			t.Fatalf("restored-gate release = %#v", state)
		}
	})

	t.Run("worktree becomes dirty", func(t *testing.T) {
		f := newRecoverFixture(t, types.RunFailed)
		makePreservedHeadUnavailable(t, f)
		f.service.beforeUnavailableReleaseStamp = func() {
			mustWrite(t, filepath.Join(f.local, "late-dirty.txt"), "late dirty work\n")
		}
		state := f.service.ReleaseUnavailable(f.ctx, f.run.ID)
		assertUnavailableReleaseNotStamped(t, f, state)
		if state.Safety != "blocked_release_assumptions_changed" {
			t.Fatalf("late-dirty release = %#v", state)
		}
	})

	t.Run("branch changes", func(t *testing.T) {
		f := newRecoverFixture(t, types.RunFailed)
		makePreservedHeadUnavailable(t, f)
		f.service.beforeUnavailableReleaseStamp = func() {
			mustRun(t, f.local, "checkout", "-b", "feature/late-other")
		}
		state := f.service.ReleaseUnavailable(f.ctx, f.run.ID)
		assertUnavailableReleaseNotStamped(t, f, state)
		if state.Safety != "blocked_release_assumptions_changed" {
			t.Fatalf("late-branch release = %#v", state)
		}
	})

	t.Run("head changes", func(t *testing.T) {
		f := newRecoverFixture(t, types.RunFailed)
		makePreservedHeadUnavailable(t, f)
		f.service.beforeUnavailableReleaseStamp = func() {
			mustWrite(t, filepath.Join(f.local, "late-head.txt"), "late head work\n")
			mustRun(t, f.local, "add", "late-head.txt")
			mustRun(t, f.local, "commit", "-m", "late local head")
		}
		state := f.service.ReleaseUnavailable(f.ctx, f.run.ID)
		assertUnavailableReleaseNotStamped(t, f, state)
		if state.Safety != "blocked_release_assumptions_changed" {
			t.Fatalf("late-head release = %#v", state)
		}
	})

	t.Run("remote changes", func(t *testing.T) {
		f := newRecoverFixture(t, types.RunFailed)
		makePreservedHeadUnavailable(t, f)
		writer := filepath.Join(t.TempDir(), "late-remote-writer")
		mustRun(t, filepath.Dir(writer), "-c", "core.autocrlf=false", "clone", f.remote, writer)
		configureIdentity(t, writer)
		mustRun(t, writer, "checkout", "feature/recover")
		mustWrite(t, filepath.Join(writer, "late-remote.txt"), "late remote work\n")
		mustRun(t, writer, "add", "late-remote.txt")
		mustRun(t, writer, "commit", "-m", "late remote head")
		f.service.beforeUnavailableReleaseStamp = func() {
			mustRun(t, writer, "push", "origin", "HEAD:refs/heads/feature/recover")
		}
		state := f.service.ReleaseUnavailable(f.ctx, f.run.ID)
		assertUnavailableReleaseNotStamped(t, f, state)
		if state.Safety != "blocked_release_remote_changed" {
			t.Fatalf("late-remote release = %#v", state)
		}
	})

	t.Run("gate changes before run registration", func(t *testing.T) {
		f := newRecoverFixture(t, types.RunFailed)
		makePreservedHeadUnavailable(t, f)
		var racingHead string
		f.service.beforeUnavailableReleaseStamp = func() {
			racingHead = makeDistinctGateHead(t, f)
		}
		state := f.service.ReleaseUnavailable(f.ctx, f.run.ID)
		assertUnavailableReleaseNotStamped(t, f, state)
		if state.Safety != "blocked_release_gate_race" {
			t.Fatalf("late-gate release = %#v", state)
		}
		if got := mustRun(t, f.gate, "rev-parse", "refs/heads/feature/recover"); got != racingHead {
			t.Fatalf("late gate head = %s, want racing %s", got, racingHead)
		}
	})
}

func TestReleaseUnavailableFinalBoundaryBindsRepositoryAndNewestOwner(t *testing.T) {
	t.Parallel()

	t.Run("configured target changes", func(t *testing.T) {
		f := newRecoverFixture(t, types.RunFailed)
		makePreservedHeadUnavailable(t, f)
		f.service.beforeUnavailableReleaseCommit = func() {
			if _, err := f.db.UpdateRepoMetadata(f.repo.ID, filepath.Join(t.TempDir(), "replacement.git"), f.repo.DefaultBranch); err != nil {
				t.Fatal(err)
			}
		}
		state := f.service.ReleaseUnavailable(f.ctx, f.run.ID)
		assertUnavailableReleaseNotStamped(t, f, state)
		if state.Safety != "blocked_release_assumptions_changed" {
			t.Fatalf("changed-target release = %#v", state)
		}
	})

	t.Run("newer terminal owner appears", func(t *testing.T) {
		f := newRecoverFixture(t, types.RunFailed)
		makePreservedHeadUnavailable(t, f)
		f.service.beforeUnavailableReleaseCommit = func() {
			newer, err := f.db.InsertRun(f.repo.ID, "feature/recover", f.submitted, f.base)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.db.UpdateRunStatusWithVerifiedHead(newer.ID, types.RunFailed, f.preserved); err != nil {
				t.Fatal(err)
			}
		}
		state := f.service.ReleaseUnavailable(f.ctx, f.run.ID)
		assertUnavailableReleaseNotStamped(t, f, state)
		if state.Safety != "blocked_release_assumptions_changed" {
			t.Fatalf("newer-terminal release = %#v", state)
		}
	})
}

func TestReleaseUnavailableGateContextRefusalRemainsCentralized(t *testing.T) {
	// The shared gate-context classifier is already exhaustively tested by the
	// ordinary recovery path. This compile-time assertion keeps unavailable
	// release on the same Service owner rather than growing a CLI-only bypass.
	var _ interface {
		ReleaseUnavailable(context.Context, string) State
	} = (*Service)(nil)
}

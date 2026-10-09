package branchsync

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/gate"
	gitpkg "github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// newStaleMirrorAdoptionFixture is the authorized-rewrite adoption state with
// the private mirror lane where a real terminal run leaves it: still at the
// submitted head. The pipeline's rewritten result exists in the gate only
// under its run-specific recovery anchor.
func newStaleMirrorAdoptionFixture(t *testing.T) *recoverFixture {
	t.Helper()
	f := newAuthorizedRewriteRecoverFixture(t)
	mustRun(t, f.gate, "update-ref", "refs/heads/"+f.run.Branch, f.submitted, f.preserved)
	return f
}

func (f *recoverFixture) mirrorLane() string {
	f.t.Helper()
	return readGitOptional(f.t, f.gate, "refs/heads/"+f.run.Branch)
}

func readGitOptional(t *testing.T, dir, ref string) string {
	t.Helper()
	out, err := gitpkg.Run(context.Background(), dir, "rev-parse", "--verify", "--quiet", ref)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// assertNextRunMirrorReady drives the real private-mirror preparation a fresh
// `axi run` performs, for both the adopted head itself and a later operator
// commit on top of it, and proves the superseded submitted head is still
// reachable from the gate afterwards.
func assertNextRunMirrorReady(t *testing.T, f *recoverFixture) {
	t.Helper()
	for _, ref := range []string{f.anchorRef(), f.localAnchorRef()} {
		if got := readGitOptional(t, f.gate, ref); got == "" {
			t.Fatalf("gate lost history anchor %s", ref)
		}
	}
	if got := mustRun(t, f.gate, "rev-parse", f.localAnchorRef()); got != f.submitted {
		t.Fatalf("gate caller-head anchor = %s, want submitted %s", got, f.submitted)
	}
	if got := mustRun(t, f.gate, "rev-parse", f.anchorRef()); got != f.preserved {
		t.Fatalf("gate preserved anchor = %s, want %s", got, f.preserved)
	}
	reconciled, err := gate.ReconcileStaleBranch(f.ctx, f.gate, f.local, f.run.Branch, f.preserved, "")
	if err != nil || reconciled.Reconciled {
		t.Fatalf("next-run mirror preparation for the adopted head = %#v, err %v", reconciled, err)
	}

	mustWrite(t, filepath.Join(f.local, "next.txt"), "next run work\n")
	mustRun(t, f.local, "add", "next.txt")
	mustRun(t, f.local, "commit", "-m", "next run work")
	next := mustRun(t, f.local, "rev-parse", "HEAD")
	reconciled, err = gate.ReconcileStaleBranch(f.ctx, f.gate, f.local, f.run.Branch, next, "")
	if err != nil || reconciled.Reconciled {
		t.Fatalf("next-run mirror preparation for a later commit = %#v, err %v", reconciled, err)
	}
	// The submission itself is now an ordinary fast-forward: no force.
	mustRun(t, f.local, "push", f.gate, "refs/heads/"+f.run.Branch+":refs/heads/"+f.run.Branch)
	if got := f.mirrorLane(); got != next {
		t.Fatalf("fast-forward submission left the lane at %s, want %s", got, next)
	}
	mustRun(t, f.gate, "cat-file", "-e", f.submitted+"^{commit}")
	if got := mustRun(t, f.gate, "rev-parse", f.localAnchorRef()); got != f.submitted {
		t.Fatalf("submitted head anchor moved to %s after the next submission", got)
	}
}

func TestAdoptTerminalHeadSettlesPrivateMirrorForTheNextRun(t *testing.T) {
	t.Parallel()

	f := newStaleMirrorAdoptionFixture(t)
	if automatic := f.service.Recover(f.ctx, false); automatic.Recovered || automatic.Safety != "blocked_recover_diverged" {
		t.Fatalf("automatic recovery crossed the explicit-authorization boundary: %#v", automatic)
	}
	if got := f.mirrorLane(); got != f.submitted {
		t.Fatalf("automatic refusal moved the mirror lane to %s", got)
	}

	state := f.service.AdoptTerminalHead(f.ctx, terminalHeadAdoptionRequest(f))
	if !state.Recovered || !state.Changed || state.State != StateCustodyReturned {
		t.Fatalf("authorized terminal-head adoption = %#v", state)
	}
	assertRecoveredRewriteFixture(t, f)
	if got := f.mirrorLane(); got != f.preserved {
		t.Fatalf("mirror lane = %s, want adopted head %s", got, f.preserved)
	}
	if replay := f.service.AdoptTerminalHead(f.ctx, terminalHeadAdoptionRequest(f)); !replay.Recovered || replay.Changed {
		t.Fatalf("idempotent replay = %#v", replay)
	}
	assertNextRunMirrorReady(t, f)
}

// TestAdoptTerminalHeadReplaySettlesMirrorLeftByEarlierAdoption reproduces
// the reported follow-through defect: an adoption completed custody while the
// private mirror lane still named the submitted head, so the next run's mirror
// guard refused that head as at-risk. Replaying the same fully bound action
// settles the lane, and the guard itself is unchanged: the gate's caller-head
// anchor is not preservation credit.
func TestAdoptTerminalHeadReplaySettlesMirrorLeftByEarlierAdoption(t *testing.T) {
	t.Parallel()

	f := newStaleMirrorAdoptionFixture(t)
	request := terminalHeadAdoptionRequest(f)
	if state := f.service.AdoptTerminalHead(f.ctx, request); !state.Recovered {
		t.Fatalf("adoption = %#v", state)
	}
	// Recreate the state an adoption without mirror settlement left behind.
	mustRun(t, f.gate, "update-ref", "refs/heads/"+f.run.Branch, f.submitted, f.preserved)

	_, err := gate.ReconcileStaleBranch(f.ctx, f.gate, f.local, f.run.Branch, f.preserved, "")
	if err == nil || !strings.Contains(err.Error(), "at-risk") || !strings.Contains(err.Error(), f.submitted) {
		t.Fatalf("stale lane with the caller anchor present must still refuse as at-risk, got %v", err)
	}
	mustRun(t, f.gate, "update-ref", "-d", f.localAnchorRef(), f.submitted)
	_, err = gate.ReconcileStaleBranch(f.ctx, f.gate, f.local, f.run.Branch, f.preserved, "")
	if err == nil || !strings.Contains(err.Error(), "at-risk") {
		t.Fatalf("reproduction: stale lane must refuse the adopted head, got %v", err)
	}
	if got := f.mirrorLane(); got != f.submitted {
		t.Fatalf("refused preparation moved the lane to %s", got)
	}

	replay := f.service.AdoptTerminalHead(f.ctx, request)
	if !replay.Recovered || replay.Changed {
		t.Fatalf("replay settling the mirror = %#v", replay)
	}
	if got := f.mirrorLane(); got != f.preserved {
		t.Fatalf("replay left the mirror lane at %s", got)
	}
	assertNextRunMirrorReady(t, f)
}

// TestAdoptTerminalHeadReplayRefusesAMovedLaneWithoutDenyingCustody keeps the
// replay of a completed adoption fail-closed against a lane someone else moved
// while reporting accurately that custody was already returned.
func TestAdoptTerminalHeadReplayRefusesAMovedLaneWithoutDenyingCustody(t *testing.T) {
	t.Parallel()

	f := newStaleMirrorAdoptionFixture(t)
	request := terminalHeadAdoptionRequest(f)
	if state := f.service.AdoptTerminalHead(f.ctx, request); !state.Recovered {
		t.Fatalf("adoption = %#v", state)
	}
	mustRun(t, f.gate, "update-ref", "refs/heads/"+f.run.Branch, f.base, f.preserved)

	replay := f.service.AdoptTerminalHead(f.ctx, request)
	if replay.Recovered || replay.Changed || replay.Safety != "blocked_adopt_terminal_mirror_mismatch" {
		t.Fatalf("replay against a moved lane = %#v", replay)
	}
	if got := f.mirrorLane(); got != f.base {
		t.Fatalf("replay moved the independently moved lane to %s", got)
	}
	if strings.Contains(replay.Error, "custody was not returned") || !strings.Contains(replay.Error, "custody was already recorded") {
		t.Fatalf("replay refusal misreports custody: %q", replay.Error)
	}
	if !f.custodyReturned() {
		t.Fatal("replay refusal cleared the recorded custody return")
	}
}

func TestAdoptTerminalHeadMirrorLaneShapes(t *testing.T) {
	t.Parallel()

	t.Run("lane already at preserved head", func(t *testing.T) {
		f := newAuthorizedRewriteRecoverFixture(t)
		if state := f.service.AdoptTerminalHead(f.ctx, terminalHeadAdoptionRequest(f)); !state.Recovered || !state.Changed {
			t.Fatalf("adoption = %#v", state)
		}
		if got := f.mirrorLane(); got != f.preserved {
			t.Fatalf("lane = %s, want %s", got, f.preserved)
		}
		if got := readGitOptional(t, f.gate, f.localAnchorRef()); got != "" {
			t.Fatalf("an unmoved lane needs no gate caller anchor, got %s", got)
		}
		reconciled, err := gate.ReconcileStaleBranch(f.ctx, f.gate, f.local, f.run.Branch, f.preserved, "")
		if err != nil || reconciled.Reconciled {
			t.Fatalf("next-run preparation = %#v, err %v", reconciled, err)
		}
	})

	t.Run("lane absent", func(t *testing.T) {
		f := newStaleMirrorAdoptionFixture(t)
		mustRun(t, f.gate, "update-ref", "-d", "refs/heads/"+f.run.Branch, f.submitted)
		if state := f.service.AdoptTerminalHead(f.ctx, terminalHeadAdoptionRequest(f)); !state.Recovered || !state.Changed {
			t.Fatalf("adoption = %#v", state)
		}
		if got := f.mirrorLane(); got != "" {
			t.Fatalf("adoption created an absent lane at %s", got)
		}
		reconciled, err := gate.ReconcileStaleBranch(f.ctx, f.gate, f.local, f.run.Branch, f.preserved, "")
		if err != nil || reconciled.Reconciled {
			t.Fatalf("next-run preparation = %#v, err %v", reconciled, err)
		}
	})
}

func TestAdoptTerminalHeadRefusesMismatchedMirrorBindingsWithoutMutation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		wantSafety string
		wantLane   func(*recoverFixture) string
		mutate     func(*testing.T, *recoverFixture)
	}{
		{name: "lane moved to unrelated commit", wantSafety: "blocked_adopt_terminal_mirror_mismatch", wantLane: func(f *recoverFixture) string { return f.base },
			mutate: func(t *testing.T, f *recoverFixture) {
				mustRun(t, f.gate, "update-ref", "refs/heads/"+f.run.Branch, f.base, f.submitted)
			}},
		{name: "lane symbolic", wantSafety: "blocked_adopt_terminal_mirror_mismatch", wantLane: func(f *recoverFixture) string { return f.submitted },
			mutate: func(t *testing.T, f *recoverFixture) {
				mustRun(t, f.gate, "update-ref", "-d", "refs/heads/"+f.run.Branch, f.submitted)
				mustRun(t, f.gate, "update-ref", "refs/heads/elsewhere", f.submitted)
				mustRun(t, f.gate, "symbolic-ref", "refs/heads/"+f.run.Branch, "refs/heads/elsewhere")
			}},
		{name: "conflicting gate caller anchor", wantSafety: "blocked_adopt_terminal_mirror_mismatch", wantLane: func(f *recoverFixture) string { return f.submitted },
			mutate: func(t *testing.T, f *recoverFixture) {
				mustRun(t, f.gate, "update-ref", f.localAnchorRef(), f.base)
			}},
		{name: "symbolic gate caller anchor", wantSafety: "blocked_adopt_terminal_mirror_mismatch", wantLane: func(f *recoverFixture) string { return f.submitted },
			mutate: func(t *testing.T, f *recoverFixture) {
				mustRun(t, f.gate, "symbolic-ref", f.localAnchorRef(), "refs/heads/"+f.run.Branch)
			}},
		{name: "dirty tracked work", wantSafety: "blocked_adopt_terminal_dirty", wantLane: func(f *recoverFixture) string { return f.submitted },
			mutate: func(t *testing.T, f *recoverFixture) {
				mustWrite(t, filepath.Join(f.local, "feature.txt"), "dirty tracked work\n")
			}},
		{name: "unique local work", wantSafety: "blocked_adopt_terminal_caller_changed", wantLane: func(f *recoverFixture) string { return f.submitted },
			mutate: func(t *testing.T, f *recoverFixture) {
				mustWrite(t, filepath.Join(f.local, "unique.txt"), "unique\n")
				mustRun(t, f.local, "add", "unique.txt")
				mustRun(t, f.local, "commit", "-m", "unique local work")
			}},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newStaleMirrorAdoptionFixture(t)
			tc.mutate(t, f)
			headBefore := mustRun(t, f.local, "rev-parse", "HEAD")
			statusBefore := mustRun(t, f.local, "status", "--porcelain=v1", "--untracked-files=all")
			anchorBefore := readGitOptional(t, f.gate, f.localAnchorRef())

			state := f.service.AdoptTerminalHead(f.ctx, terminalHeadAdoptionRequest(f))
			if state.Recovered || state.Changed || state.Safety != tc.wantSafety {
				t.Fatalf("refusal = %#v, want %s", state, tc.wantSafety)
			}
			if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != headBefore {
				t.Fatalf("refusal moved HEAD to %s", got)
			}
			if got := mustRun(t, f.local, "status", "--porcelain=v1", "--untracked-files=all"); got != statusBefore {
				t.Fatalf("refusal changed local work: %q -> %q", statusBefore, got)
			}
			if got := readGitOptional(t, f.gate, "refs/heads/"+f.run.Branch); got != tc.wantLane(f) {
				t.Fatalf("refusal moved the mirror lane to %s", got)
			}
			if got := readGitOptional(t, f.gate, f.localAnchorRef()); got != anchorBefore {
				t.Fatalf("refusal changed the gate caller anchor from %q to %q", anchorBefore, got)
			}
			run, err := f.db.GetRun(f.run.ID)
			if err != nil || run.CustodyReturnedAt != nil || run.TerminalAdoptionAuthorizedAt != nil {
				t.Fatalf("refusal changed the adoption record: %#v err %v", run, err)
			}
		})
	}
}

func TestAdoptTerminalHeadMirrorInterruptionsRemainRetryable(t *testing.T) {
	t.Parallel()

	assertInterrupted := func(t *testing.T, f *recoverFixture, wantLane string) {
		t.Helper()
		if f.custodyReturned() {
			t.Fatal("interrupted mirror settlement stamped custody")
		}
		if got := f.mirrorLane(); got != wantLane {
			t.Fatalf("interrupted lane = %s, want %s", got, wantLane)
		}
		if got := mustRun(t, f.gate, "rev-parse", f.localAnchorRef()); got != f.submitted {
			t.Fatalf("interrupted gate caller anchor = %s", got)
		}
		if got := mustRun(t, f.local, "rev-parse", f.localAnchorRef()); got != f.submitted {
			t.Fatalf("interrupted worktree caller anchor = %s", got)
		}
		if got := mustRun(t, f.gate, "rev-parse", f.anchorRef()); got != f.preserved {
			t.Fatalf("interrupted preserved anchor = %s", got)
		}
	}
	retry := func(t *testing.T, f *recoverFixture) {
		t.Helper()
		f.service.afterTerminalAdoptionMirrorAnchor = nil
		f.service.afterTerminalAdoptionMirrorMove = nil
		f.service.completeTerminalAdoption = nil
		retried := f.service.AdoptTerminalHead(f.ctx, terminalHeadAdoptionRequest(f))
		if !retried.Recovered || retried.Changed {
			t.Fatalf("retry after mirror interruption = %#v", retried)
		}
		assertRecoveredRewriteFixture(t, f)
		if replay := f.service.AdoptTerminalHead(f.ctx, terminalHeadAdoptionRequest(f)); !replay.Recovered || replay.Changed {
			t.Fatalf("replay after retry = %#v", replay)
		}
		assertNextRunMirrorReady(t, f)
	}
	crash := func(t *testing.T, run func()) {
		t.Helper()
		crashed := false
		func() {
			defer func() { crashed = recover() != nil }()
			run()
		}()
		if !crashed {
			t.Fatal("simulated interruption did not stop the adoption")
		}
	}

	t.Run("after gate caller anchor", func(t *testing.T) {
		t.Parallel()
		f := newStaleMirrorAdoptionFixture(t)
		f.service.afterTerminalAdoptionMirrorAnchor = func() { panic("simulated stop after gate anchor") }
		crash(t, func() { _ = f.service.AdoptTerminalHead(f.ctx, terminalHeadAdoptionRequest(f)) })
		assertInterrupted(t, f, f.submitted)
		retry(t, f)
	})

	t.Run("after lane compare and swap", func(t *testing.T) {
		t.Parallel()
		f := newStaleMirrorAdoptionFixture(t)
		f.service.afterTerminalAdoptionMirrorMove = func() { panic("simulated stop after lane CAS") }
		crash(t, func() { _ = f.service.AdoptTerminalHead(f.ctx, terminalHeadAdoptionRequest(f)) })
		assertInterrupted(t, f, f.preserved)
		retry(t, f)
	})

	t.Run("custody stamp fails after settlement", func(t *testing.T) {
		t.Parallel()
		f := newStaleMirrorAdoptionFixture(t)
		f.service.completeTerminalAdoption = func(db.TerminalHeadAdoptionAuthorization) (bool, error) {
			return false, errors.New("simulated interruption")
		}
		state := f.service.AdoptTerminalHead(f.ctx, terminalHeadAdoptionRequest(f))
		if state.Recovered || state.Safety != "blocked_adopt_terminal_stamp_failed" {
			t.Fatalf("stamp interruption = %#v", state)
		}
		assertInterrupted(t, f, f.preserved)
		retry(t, f)
	})

	// Earlier boundaries are owned by TestAdoptTerminalHeadInterruptedAttemptsRemainRetryable;
	// here they only need to prove the resumed adoption still settles the lane.
	t.Run("after branch compare and swap", func(t *testing.T) {
		t.Parallel()
		f := newStaleMirrorAdoptionFixture(t)
		f.service.afterRecoverBranchMove = func() { panic("simulated stop after branch CAS") }
		crash(t, func() { _ = f.service.AdoptTerminalHead(f.ctx, terminalHeadAdoptionRequest(f)) })
		if got := f.mirrorLane(); got != f.submitted {
			t.Fatalf("lane moved before the worktree move completed: %s", got)
		}
		f.service.afterRecoverBranchMove = nil
		if retried := f.service.AdoptTerminalHead(f.ctx, terminalHeadAdoptionRequest(f)); !retried.Recovered || !retried.Changed {
			t.Fatalf("retry = %#v", retried)
		}
		assertRecoveredRewriteFixture(t, f)
		assertNextRunMirrorReady(t, f)
	})

	t.Run("after detaching caller snapshot", func(t *testing.T) {
		t.Parallel()
		f := newStaleMirrorAdoptionFixture(t)
		f.service.afterRecoverBranchMove = func() {
			mustRun(t, f.local, "checkout", "--no-overwrite-ignore", "--detach", f.submitted)
			panic("simulated stop after detach")
		}
		crash(t, func() { _ = f.service.AdoptTerminalHead(f.ctx, terminalHeadAdoptionRequest(f)) })
		f.service.afterRecoverBranchMove = nil
		if retried := f.service.AdoptTerminalHead(f.ctx, terminalHeadAdoptionRequest(f)); !retried.Recovered || !retried.Changed {
			t.Fatalf("retry = %#v", retried)
		}
		assertRecoveredRewriteFixture(t, f)
		assertNextRunMirrorReady(t, f)
	})
}

func TestAdoptTerminalHeadMirrorRacesNeverOverwriteTheLane(t *testing.T) {
	t.Parallel()

	t.Run("concurrent push moves the lane", func(t *testing.T) {
		t.Parallel()
		f := newStaleMirrorAdoptionFixture(t)
		f.service.afterTerminalAdoptionMirrorAnchor = func() {
			mustRun(t, f.gate, "update-ref", "refs/heads/"+f.run.Branch, f.base, f.submitted)
		}
		state := f.service.AdoptTerminalHead(f.ctx, terminalHeadAdoptionRequest(f))
		if state.Recovered || state.Safety != "blocked_adopt_terminal_mirror_race" {
			t.Fatalf("lane race = %#v", state)
		}
		if got := f.mirrorLane(); got != f.base {
			t.Fatalf("race overwrote the concurrent lane head: %s", got)
		}
		if f.custodyReturned() {
			t.Fatal("lane race stamped custody")
		}
		if got := mustRun(t, f.gate, "rev-parse", f.localAnchorRef()); got != f.submitted {
			t.Fatalf("race lost the caller anchor: %s", got)
		}

		f.service.afterTerminalAdoptionMirrorAnchor = nil
		if refused := f.service.AdoptTerminalHead(f.ctx, terminalHeadAdoptionRequest(f)); refused.Recovered || refused.Safety != "blocked_adopt_terminal_mirror_mismatch" {
			t.Fatalf("retry against an independently moved lane = %#v", refused)
		}
		if got := f.mirrorLane(); got != f.base {
			t.Fatalf("refused retry moved the lane: %s", got)
		}
		mustRun(t, f.gate, "update-ref", "refs/heads/"+f.run.Branch, f.submitted, f.base)
		if retried := f.service.AdoptTerminalHead(f.ctx, terminalHeadAdoptionRequest(f)); !retried.Recovered {
			t.Fatalf("retry after restoring the exact lane = %#v", retried)
		}
		assertNextRunMirrorReady(t, f)
	})

	t.Run("concurrent identical settlement", func(t *testing.T) {
		t.Parallel()
		f := newStaleMirrorAdoptionFixture(t)
		f.service.afterTerminalAdoptionMirrorAnchor = func() {
			mustRun(t, f.gate, "update-ref", "refs/heads/"+f.run.Branch, f.preserved, f.submitted)
		}
		if state := f.service.AdoptTerminalHead(f.ctx, terminalHeadAdoptionRequest(f)); !state.Recovered || !state.Changed {
			t.Fatalf("adoption racing an identical settlement = %#v", state)
		}
		assertNextRunMirrorReady(t, f)
	})

	t.Run("preserved anchor moves before the lane CAS", func(t *testing.T) {
		t.Parallel()
		f := newStaleMirrorAdoptionFixture(t)
		f.service.afterTerminalAdoptionMirrorAnchor = func() {
			mustRun(t, f.gate, "update-ref", f.anchorRef(), f.base, f.preserved)
		}
		state := f.service.AdoptTerminalHead(f.ctx, terminalHeadAdoptionRequest(f))
		if state.Recovered || state.Safety != "blocked_adopt_terminal_mirror_mismatch" {
			t.Fatalf("anchor race = %#v", state)
		}
		if got := f.mirrorLane(); got != f.submitted {
			t.Fatalf("anchor race moved the lane: %s", got)
		}
		if f.custodyReturned() {
			t.Fatal("anchor race stamped custody")
		}
	})
}

// TestAdoptTerminalHeadStatusOffersNothingForAMovedMirrorLane keeps status and
// the mutating action on one predicate: a lane adoption would refuse to settle
// is never offered as adopt_terminal_head.
func TestAdoptTerminalHeadStatusOffersNothingForAMovedMirrorLane(t *testing.T) {
	t.Parallel()

	f := newStaleMirrorAdoptionFixture(t)
	offered := f.service.InspectCached(f.ctx)
	if offered.NextAction == nil || offered.NextAction.Code != "adopt_terminal_head" {
		t.Fatalf("stale-lane status should offer adoption: %#v", offered)
	}
	mustRun(t, f.gate, "update-ref", "refs/heads/"+f.run.Branch, f.base, f.submitted)
	moved := f.service.InspectCached(f.ctx)
	if moved.NextAction != nil && moved.NextAction.Code == "adopt_terminal_head" {
		t.Fatalf("status offered adoption for a lane it would refuse: %#v", moved)
	}
	if f.run.Status != types.RunFailed {
		t.Fatalf("fixture status = %s", f.run.Status)
	}
}

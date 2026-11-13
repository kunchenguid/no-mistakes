package branchsync

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/gate"
	gitpkg "github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// userRebaseAfterRelease recreates the issue #1293 shape on a released
// (user_owned) branch: the operator publishes the submitted head directly, main
// advances, and the branch is rebased onto it outside the pipeline while the
// gate lane still names the submitted head. With conflict, main edits the line
// the branch edited, so the rebase stops and is resolved by hand into content
// neither side had; otherwise the rebase replays the same patch untouched.
func userRebaseAfterRelease(t *testing.T, f *recoverFixture, conflict, publish bool) string {
	t.Helper()
	mustRun(t, f.local, "remote", "add", "origin", f.remote)
	mustRun(t, f.local, "push", "origin", "main:refs/heads/main", "HEAD:refs/heads/feature/recover")

	mustRun(t, f.local, "checkout", "main")
	if conflict {
		mustWrite(t, filepath.Join(f.local, "file.txt"), "upstream\n")
		mustRun(t, f.local, "commit", "-am", "advance main over the same line")
	} else {
		mustWrite(t, filepath.Join(f.local, "upstream.txt"), "newer main\n")
		mustRun(t, f.local, "add", "upstream.txt")
		mustRun(t, f.local, "commit", "-m", "advance main")
	}
	mustRun(t, f.local, "push", "origin", "main:refs/heads/main")

	mustRun(t, f.local, "checkout", "feature/recover")
	if conflict {
		if _, err := gitpkg.Run(f.ctx, f.local, "rebase", "main"); err == nil {
			t.Fatal("rebase over the same line did not stop on a conflict")
		}
		mustWrite(t, filepath.Join(f.local, "file.txt"), "upstream feature\n")
		mustRun(t, f.local, "add", "file.txt")
		mustRun(t, f.local, "-c", "core.editor=true", "rebase", "--continue")
	} else {
		mustRun(t, f.local, "rebase", "main")
	}
	rebased := mustRun(t, f.local, "rev-parse", "HEAD")
	if publish {
		mustRun(t, f.local, "push", "--force-with-lease=refs/heads/feature/recover:"+f.submitted, "origin", "HEAD:refs/heads/feature/recover")
	}
	return rebased
}

func abandonedArchiveRef(head string) string {
	return "refs/tags/no-mistakes-abandoned/feature/recover/" + head
}

func gateRefs(t *testing.T, f *recoverFixture) string {
	t.Helper()
	return mustRun(t, f.gate, "for-each-ref", "--format=%(refname) %(objectname) %(symref)")
}

// TestAdoptPublishedAdoptsUserOwnedRebase is the regression for issue #1293. A
// released branch rebased outside the pipeline and force-pushed to the
// configured push target had no supported exit: adoption applied only to a
// custody-returned lane. It now adopts the published head into the user-owned
// lane after archiving the superseded private head the way reconciliation
// does. The conflict-resolved rebase is the shape no reconciliation proof can
// clear, so the gate's own refusal is asserted first.
func TestAdoptPublishedAdoptsUserOwnedRebase(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name     string
		conflict bool
	}{
		{name: "patch-id-equivalent rebase"},
		{name: "conflict-resolved rebase", conflict: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newUnmovedRecoverFixture(t, types.RunCancelled)
			rebased := userRebaseAfterRelease(t, f, tt.conflict, true)

			// git cherry marks a commit "-" when the other side carries a
			// patch-id-equivalent one, and "+" when it does not.
			equivalent := strings.HasPrefix(mustRun(t, f.local, "cherry", rebased, f.submitted), "- ")
			if equivalent == tt.conflict {
				t.Fatalf("fixture patch equivalence = %t, want %t", equivalent, !tt.conflict)
			}
			_, planErr := gate.PlanStaleBranchReconciliation(f.ctx, f.gate, f.local, "feature/recover", rebased, "", nil)
			var atRisk *gate.AtRiskError
			if refused := errors.As(planErr, &atRisk); refused != tt.conflict {
				t.Fatalf("reconciliation refusal = %t (%v), want %t", refused, planErr, tt.conflict)
			}

			before := f.service.InspectCached(f.ctx)
			if before.State != StateUserOwned || before.Relation != RelationDiverged || before.NextAction != nil {
				t.Fatalf("rebased released branch = %#v", before)
			}

			state := f.service.AdoptPublished(f.ctx)
			if !state.Changed || state.State != StateUserOwned || state.Safety != "user_owned" || state.Error != "" {
				t.Fatalf("adoption = %#v", state)
			}
			if state.NextAction == nil || state.NextAction.Code != "run_pipeline" {
				t.Fatalf("adoption next action = %#v, want run_pipeline", state.NextAction)
			}
			if got := mustRun(t, f.gate, "rev-parse", "refs/heads/feature/recover"); got != rebased {
				t.Fatalf("gate lane = %s, want the published rebased head %s", got, rebased)
			}
			wantArchive := abandonedArchiveRef(f.submitted) + " " + f.submitted
			if got := mustRun(t, f.gate, "for-each-ref", "--format=%(refname) %(objectname) %(symref)", "refs/tags/"); got != wantArchive {
				t.Fatalf("gate archive = %q, want the superseded head as a direct ref %q", got, wantArchive)
			}
			// No run owns the superseded head, so nothing claims it as a
			// recovered pipeline head and custody is not stamped.
			if refs := mustRun(t, f.gate, "for-each-ref", "--format=%(refname)", "refs/no-mistakes/"); refs != "" {
				t.Fatalf("adoption wrote recovery refs for a released run: %s", refs)
			}
			if f.custodyReturned() {
				t.Fatal("adoption stamped custody on a released run")
			}
			if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != rebased {
				t.Fatalf("adoption moved the worktree to %s, want %s", got, rebased)
			}
			if got := mustRun(t, f.remote, "rev-parse", "refs/heads/feature/recover"); got != rebased {
				t.Fatalf("adoption moved the push target to %s, want %s", got, rebased)
			}

			// The fresh run's ordinary gate push is now a no-op instead of a
			// rejected rewrite, and the released branch keeps no standing action.
			if err := gitpkg.Push(context.Background(), f.local, f.gate, "refs/heads/feature/recover", "", false); err != nil {
				t.Fatalf("gate push after adoption: %v", err)
			}
			if after := f.service.InspectCached(f.ctx); after.State != StateUserOwned || after.NextAction != nil {
				t.Fatalf("status after adoption = %#v", after)
			}

			// Repeating the adoption says there is nothing left to adopt
			// rather than reporting the lane it just moved as a moved lane.
			refsAfter := gateRefs(t, f)
			repeat := f.service.AdoptPublished(f.ctx)
			if repeat.Changed || repeat.Safety != "blocked_adopt_published_not_applicable" || !strings.Contains(repeat.Error, "already holds the local head") {
				t.Fatalf("repeated adoption = %#v", repeat)
			}
			if got := gateRefs(t, f); got != refsAfter {
				t.Fatalf("repeated adoption changed gate refs:\nbefore: %s\nafter: %s", refsAfter, got)
			}
		})
	}
}

// TestAdoptPublishedRefusesUnpublishedUserOwnedRebase: the operator's own
// publication is the only thing adoption accepts in place of a containment
// proof, so a rewrite the push target has not seen changes nothing.
func TestAdoptPublishedRefusesUnpublishedUserOwnedRebase(t *testing.T) {
	t.Parallel()

	f := newUnmovedRecoverFixture(t, types.RunCancelled)
	rebased := userRebaseAfterRelease(t, f, true, false)
	refsBefore := gateRefs(t, f)

	state := f.service.AdoptPublished(f.ctx)
	if state.Changed || state.State != StateUserOwned || state.Safety != "blocked_published_head_mismatch" || state.NextAction != nil {
		t.Fatalf("unpublished rebase adoption = %#v", state)
	}
	if got := gateRefs(t, f); got != refsBefore {
		t.Fatalf("refusal changed gate refs:\nbefore: %s\nafter: %s", refsBefore, got)
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != rebased {
		t.Fatalf("refusal moved the worktree to %s, want %s", got, rebased)
	}
	if got := mustRun(t, f.remote, "rev-parse", "refs/heads/feature/recover"); got != f.submitted {
		t.Fatalf("refusal moved the push target to %s, want %s", got, f.submitted)
	}
}

// TestAdoptPublishedUserOwnedLaneFailsClosed pins every guard a user-owned
// adoption shares with the custody-returned one: each refusal names its cause
// and leaves the lane, the archive namespace, and the worktree alone.
func TestAdoptPublishedUserOwnedLaneFailsClosed(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name       string
		arrange    func(t *testing.T, f *recoverFixture)
		wantState  string
		wantSafety string
	}{
		{
			name:       "branch not rewritten",
			arrange:    func(t *testing.T, f *recoverFixture) {},
			wantState:  StateUserOwned,
			wantSafety: "blocked_adopt_published_not_applicable",
		},
		{
			name: "local head only ahead of the lane",
			arrange: func(t *testing.T, f *recoverFixture) {
				mustWrite(t, filepath.Join(f.local, "follow-up.txt"), "follow-up\n")
				mustRun(t, f.local, "add", "follow-up.txt")
				mustRun(t, f.local, "commit", "-m", "follow-up")
			},
			wantState:  StateUserOwned,
			wantSafety: "blocked_adopt_published_not_applicable",
		},
		{
			name: "newer active run owns the branch",
			arrange: func(t *testing.T, f *recoverFixture) {
				rebased := userRebaseAfterRelease(t, f, true, true)
				fresh, err := f.db.InsertRun(f.repo.ID, "feature/recover", rebased, f.submitted)
				if err != nil {
					t.Fatal(err)
				}
				if err := f.db.UpdateRunStatus(fresh.ID, types.RunRunning); err != nil {
					t.Fatal(err)
				}
			},
			wantState:  StatePipelineOwned,
			wantSafety: "blocked_adopt_published_not_applicable",
		},
		{
			name: "dirty worktree",
			arrange: func(t *testing.T, f *recoverFixture) {
				userRebaseAfterRelease(t, f, true, true)
				mustWrite(t, filepath.Join(f.local, "file.txt"), "uncommitted\n")
			},
			wantState:  StateDirty,
			wantSafety: "blocked_adopt_published_dirty",
		},
		{
			name: "gate lane moved off the submitted head",
			arrange: func(t *testing.T, f *recoverFixture) {
				userRebaseAfterRelease(t, f, true, true)
				mustRun(t, f.gate, "update-ref", "refs/heads/feature/recover", f.base, f.submitted)
			},
			wantState:  StateUserOwned,
			wantSafety: "blocked_adopt_published_gate_changed",
		},
		{
			name: "archive tag already names another commit",
			arrange: func(t *testing.T, f *recoverFixture) {
				userRebaseAfterRelease(t, f, true, true)
				mustRun(t, f.gate, "update-ref", "--no-deref", abandonedArchiveRef(f.submitted), f.base)
			},
			wantState:  StateUserOwned,
			wantSafety: "blocked_adopt_published_preserve_failed",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			f := newUnmovedRecoverFixture(t, types.RunCancelled)
			tt.arrange(t, f)
			head := mustRun(t, f.local, "rev-parse", "HEAD")
			refsBefore := gateRefs(t, f)

			state := f.service.AdoptPublished(f.ctx)
			if state.Changed || state.State != tt.wantState || state.Safety != tt.wantSafety || state.NextAction != nil {
				t.Fatalf("adoption = %s/%s changed=%t next=%#v error=%q, want %s/%s", state.State, state.Safety, state.Changed, state.NextAction, state.Error, tt.wantState, tt.wantSafety)
			}
			if !strings.Contains(state.Error, "no files or gate refs were changed") {
				t.Fatalf("refusal does not state that nothing changed: %q", state.Error)
			}
			if got := gateRefs(t, f); got != refsBefore {
				t.Fatalf("refusal changed gate refs:\nbefore: %s\nafter: %s", refsBefore, got)
			}
			if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != head {
				t.Fatalf("refusal moved the worktree to %s, want %s", got, head)
			}
		})
	}
}

// TestAdoptPublishedUserOwnedLaneLosesARaceWithoutLosingEitherHead: a lane
// update that lands while the push target is being verified wins. The
// compare-and-swap refuses, the racing head stays on the lane, and the
// superseded head is already archived, so neither is lost.
func TestAdoptPublishedUserOwnedLaneLosesARaceWithoutLosingEitherHead(t *testing.T) {
	t.Parallel()

	f := newUnmovedRecoverFixture(t, types.RunCancelled)
	rebased := userRebaseAfterRelease(t, f, true, true)
	// The lane is read before the push target is; move it while the target is
	// being verified, which is where a concurrent gate push would land.
	var moved sync.Once
	f.service.lsRemote = func(ctx context.Context, dir, remote, ref string) (string, error) {
		moved.Do(func() { mustRun(t, f.gate, "update-ref", "refs/heads/feature/recover", f.base, f.submitted) })
		return rebased, nil
	}

	state := f.service.AdoptPublished(f.ctx)
	if state.Changed || state.State != StateUserOwned || state.Safety != "blocked_adopt_published_gate_race" {
		t.Fatalf("raced adoption = %#v", state)
	}
	if got := mustRun(t, f.gate, "rev-parse", "refs/heads/feature/recover"); got != f.base {
		t.Fatalf("gate lane = %s, want the racing head %s", got, f.base)
	}
	if got := mustRun(t, f.gate, "rev-parse", abandonedArchiveRef(f.submitted)); got != f.submitted {
		t.Fatalf("superseded head archived at %s, want %s", got, f.submitted)
	}
}

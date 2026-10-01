package branchsync

import (
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// rewriteReleasedBranch replaces the released fixture branch's single commit
// with different content on the same base: the shape of an amend for the next
// revision, which is neither an ancestor nor a descendant of the submitted head.
func rewriteReleasedBranch(t *testing.T, f *recoverFixture) string {
	t.Helper()
	mustRun(t, f.local, "reset", "--hard", f.base)
	mustWrite(t, filepath.Join(f.local, "file.txt"), "feature, revised\n")
	mustRun(t, f.local, "commit", "-am", "feature (revised)")
	return mustRun(t, f.local, "rev-parse", "HEAD")
}

// TestReleasedBranchRewriteOffersFreshRunWhenGateLaneHoldsSubmittedHead pins
// the status half of issues #1066 and #1063: a released (user_owned) branch
// that was rewritten while the private gate lane still names the released run's
// exact submitted head must name a real next step - a fresh run that archives
// that head - instead of reporting no plan while `axi run` is refused.
func TestReleasedBranchRewriteOffersFreshRunWhenGateLaneHoldsSubmittedHead(t *testing.T) {
	t.Parallel()

	t.Run("lane at the submitted head", func(t *testing.T) {
		f := newUnmovedRecoverFixture(t, types.RunCompleted)
		rewritten := rewriteReleasedBranch(t, f)
		state := f.service.InspectCached(f.ctx)
		if state.State != StateUserOwned || state.Relation != RelationDiverged {
			t.Fatalf("state = %s relation = %s, want user_owned/diverged", state.State, state.Relation)
		}
		if state.Safety != SafetyStaleSubmittedMirror {
			t.Fatalf("safety = %s, want %s", state.Safety, SafetyStaleSubmittedMirror)
		}
		if state.NextAction == nil || state.NextAction.Code != "run_pipeline" {
			t.Fatalf("next action = %#v, want run_pipeline", state.NextAction)
		}
		if state.Error != "" {
			t.Fatalf("released rewrite must not read as an error: %q", state.Error)
		}

		// Recovery stays an idempotent no-op that keeps naming the same next
		// step; it never moves the gate lane or the worktree.
		recovered := f.service.Recover(f.ctx, false)
		if !recovered.Recovered || recovered.Changed || recovered.State != StateUserOwned {
			t.Fatalf("recover = %#v, want recovered user_owned no-op", recovered)
		}
		if recovered.Safety != SafetyStaleSubmittedMirror || recovered.NextAction == nil || recovered.NextAction.Code != "run_pipeline" {
			t.Fatalf("recover safety = %s next action = %#v, want stale_submitted_mirror/run_pipeline", recovered.Safety, recovered.NextAction)
		}
		if got := mustRun(t, f.gate, "rev-parse", "refs/heads/feature/recover"); got != f.submitted {
			t.Fatalf("gate lane moved to %s, want untouched submitted head %s", got, f.submitted)
		}
		if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != rewritten {
			t.Fatalf("worktree moved to %s, want rewritten head %s", got, rewritten)
		}
	})

	t.Run("dirty worktree keeps the plain released classification", func(t *testing.T) {
		f := newUnmovedRecoverFixture(t, types.RunCompleted)
		rewriteReleasedBranch(t, f)
		// `axi run` refuses a dirty worktree, so run_pipeline could not be
		// followed; the branch stays plain user_owned like any released branch.
		mustWrite(t, filepath.Join(f.local, "file.txt"), "feature, revised, uncommitted\n")
		state := f.service.InspectCached(f.ctx)
		if state.State != StateUserOwned || state.Relation != RelationDiverged {
			t.Fatalf("state = %s relation = %s, want user_owned/diverged", state.State, state.Relation)
		}
		if state.Safety != "user_owned" || state.NextAction != nil {
			t.Fatalf("safety = %s next action = %#v, want plain user_owned with no next action", state.Safety, state.NextAction)
		}
		if state.Error != "" {
			t.Fatalf("dirty released rewrite must not read as an error: %q", state.Error)
		}
	})

	t.Run("lane moved past the submitted head", func(t *testing.T) {
		f := newUnmovedRecoverFixture(t, types.RunCompleted)
		// Another commit reached the lane without a run of its own; the lane
		// head is no longer the operator's exact submission.
		mustWrite(t, filepath.Join(f.local, "extra.txt"), "extra\n")
		mustRun(t, f.local, "add", "extra.txt")
		mustRun(t, f.local, "commit", "-m", "extra commit on the lane")
		mustRun(t, f.local, "push", f.gate, "HEAD:refs/heads/feature/recover")
		rewriteReleasedBranch(t, f)
		state := f.service.InspectCached(f.ctx)
		if state.State != StateUserOwned || state.Relation != RelationDiverged {
			t.Fatalf("state = %s relation = %s, want user_owned/diverged", state.State, state.Relation)
		}
		if state.Safety != "user_owned" || state.NextAction != nil {
			t.Fatalf("safety = %s next action = %#v, want plain user_owned with no next action", state.Safety, state.NextAction)
		}
	})

	t.Run("archive tag already names another head", func(t *testing.T) {
		f := newUnmovedRecoverFixture(t, types.RunCompleted)
		mustRun(t, f.gate, "update-ref", "refs/tags/no-mistakes-abandoned/feature/recover/"+f.submitted, f.base)
		rewriteReleasedBranch(t, f)
		state := f.service.InspectCached(f.ctx)
		if state.State != StateUserOwned || state.Safety != "user_owned" || state.NextAction != nil {
			t.Fatalf("state = %s safety = %s next action = %#v, want plain user_owned with no next action", state.State, state.Safety, state.NextAction)
		}
	})

	t.Run("behind is not a rewrite", func(t *testing.T) {
		f := newUnmovedRecoverFixture(t, types.RunCompleted)
		mustRun(t, f.local, "reset", "--hard", f.base)
		state := f.service.InspectCached(f.ctx)
		if state.State != StateUserOwned || state.Relation != RelationBehind {
			t.Fatalf("state = %s relation = %s, want user_owned/behind", state.State, state.Relation)
		}
		if state.Safety != "user_owned" || state.NextAction != nil {
			t.Fatalf("safety = %s next action = %#v, want plain user_owned with no next action", state.Safety, state.NextAction)
		}
	})

	t.Run("cancelled run releases the same way", func(t *testing.T) {
		f := newUnmovedRecoverFixture(t, types.RunCancelled)
		rewriteReleasedBranch(t, f)
		state := f.service.InspectCached(f.ctx)
		if state.State != StateUserOwned || state.Safety != SafetyStaleSubmittedMirror || state.NextAction == nil || state.NextAction.Code != "run_pipeline" {
			t.Fatalf("state = %s safety = %s next action = %#v, want user_owned/stale_submitted_mirror/run_pipeline", state.State, state.Safety, state.NextAction)
		}
	})
}

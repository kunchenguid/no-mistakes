package branchsync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// These tests reproduce the PR 2899 custody refusal: the operator's repair head
// exists only in the worktree, the run's preserved pipeline head exists only in
// the gate behind its run recovery ref, and no repository holds both objects,
// so ancestry and content containment cannot be read from either side alone.
//
// The fixture is newRecoverFixture, whose preserved head carries
// fix.txt="pipeline fix 2". Each case commits a different "repair" in the
// worktree that is never pushed to the gate.

// repairFixture returns a cancelled-run fixture whose run recovery ref is
// pinned in the gate, so the preserved head is gate-only.
func repairFixture(t *testing.T) *recoverFixture {
	t.Helper()
	f := newRecoverFixture(t, types.RunCancelled)
	mustRun(t, f.gate, "update-ref", f.anchorRef(), f.preserved)
	if objectExists(f.ctx, f.local, f.preserved) {
		t.Fatal("fixture bug: the preserved head must be absent from the worktree")
	}
	return f
}

func (f *recoverFixture) commitRepair(name, body string) string {
	f.t.Helper()
	mustWrite(f.t, filepath.Join(f.local, name), body)
	mustRun(f.t, f.local, "add", name)
	mustRun(f.t, f.local, "commit", "-m", "repair "+name)
	head := mustRun(f.t, f.local, "rev-parse", "HEAD")
	if objectExists(f.ctx, f.gate, head) {
		f.t.Fatal("fixture bug: the repair head must be absent from the gate")
	}
	return head
}

func refSnapshot(t *testing.T, dir string) string {
	t.Helper()
	return mustRun(t, dir, "for-each-ref", "--format=%(refname) %(objectname) %(symref)")
}

func objectCount(t *testing.T, dir string) string {
	t.Helper()
	return mustRun(t, dir, "count-objects", "-v")
}

// The repair already carries everything the preserved head changed (it re-did
// the pipeline fix and added more), so returning custody at the repair loses
// nothing while the preserved commits stay anchored.
func TestRecoverLocalOnlyHeadCarryingPreservedWorkReturnsCustodyAtLocalHead(t *testing.T) {
	for _, availability := range []struct {
		name         string
		localHasBoth bool
		gateHasBoth  bool
	}{
		{name: "split objects"},
		{name: "local has both", localHasBoth: true},
		{name: "gate has both", gateHasBoth: true},
		{name: "both have both", localHasBoth: true, gateHasBoth: true},
	} {
		t.Run(availability.name, func(t *testing.T) {
			t.Parallel()
			checkRecoverHeadCarryingPreservedWork(t, availability.localHasBoth, availability.gateHasBoth)
		})
	}
}

func checkRecoverHeadCarryingPreservedWork(t *testing.T, localHasBoth, gateHasBoth bool) {
	t.Helper()
	f := repairFixture(t)
	mustWrite(t, filepath.Join(f.local, "fix.txt"), "pipeline fix 2\n")
	mustRun(t, f.local, "add", "fix.txt")
	repair := f.commitRepair("extra.txt", "repair only\n")
	if localHasBoth {
		mustRun(t, f.local, "fetch", f.gate, f.anchorRef())
	}
	if gateHasBoth {
		mustRun(t, f.gate, "fetch", f.local, "refs/heads/feature/recover")
	}
	if objectExists(f.ctx, f.local, f.preserved) != localHasBoth || objectExists(f.ctx, f.gate, repair) != gateHasBoth {
		t.Fatal("fixture has unexpected object availability")
	}

	localRefs, gateRefs := refSnapshot(t, f.local), refSnapshot(t, f.gate)
	localObjects, gateObjects := objectCount(t, f.local), objectCount(t, f.gate)
	state := f.service.InspectCached(f.ctx)
	if state.Safety != "blocked_pipeline_owned_recoverable" || state.NextAction == nil ||
		state.NextAction.Code != "recover_custody" || state.NextAction.Command != "no-mistakes axi sync --recover" {
		t.Fatalf("inspection must offer plain recovery once the relationship is provable: %#v", state)
	}
	if refSnapshot(t, f.local) != localRefs || refSnapshot(t, f.gate) != gateRefs {
		t.Fatal("inspection changed refs")
	}
	if objectCount(t, f.local) != localObjects || objectCount(t, f.gate) != gateObjects {
		t.Fatal("inspection wrote or imported objects")
	}

	recovered := f.service.Recover(f.ctx, false)
	if !recovered.Recovered || recovered.Changed {
		t.Fatalf("recover = %#v", recovered)
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != repair {
		t.Fatalf("worktree HEAD = %s, want unchanged repair %s", got, repair)
	}
	if got := mustRun(t, f.local, "rev-parse", f.anchorRef()); got != f.preserved {
		t.Fatalf("preserved head anchor in worktree = %s, want %s", got, f.preserved)
	}
	if got := mustRun(t, f.gate, "rev-parse", f.anchorRef()); got != f.preserved {
		t.Fatalf("gate anchor = %s, want %s", got, f.preserved)
	}
	if got := mustRun(t, f.gate, "rev-parse", "refs/heads/feature/recover"); got != f.preserved {
		t.Fatalf("gate branch moved to %s", got)
	}
	if !f.custodyReturned() {
		t.Fatal("custody was not returned")
	}
}

// A repair that does NOT carry the preserved work is a genuine divergence: the
// relationship is proven and reported, both copies stay preserved, and custody
// is not returned.
func TestRecoverLocalOnlyHeadDivergingFromPreservedStaysRefusedAndPreservesBoth(t *testing.T) {
	t.Parallel()

	f := repairFixture(t)
	repair := f.commitRepair("other.txt", "unrelated repair\n")

	state := f.service.InspectCached(f.ctx)
	assertManualReconciliationOffer(t, state)

	refused := f.service.Recover(f.ctx, false)
	if refused.Recovered || refused.Safety != "blocked_recover_diverged" || refused.Relation != RelationDiverged {
		t.Fatalf("recover = %#v", refused)
	}
	if f.custodyReturned() {
		t.Fatal("custody stamped for a divergent pair")
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != repair {
		t.Fatalf("HEAD moved to %s", got)
	}
	if got := mustRun(t, f.local, "rev-parse", f.anchorRef()); got != f.preserved {
		t.Fatalf("preserved head not anchored locally: %s", got)
	}
	if got := mustRun(t, f.gate, "rev-parse", "refs/heads/feature/recover"); got != f.preserved {
		t.Fatalf("gate branch moved to %s", got)
	}
}

// The preserved head strictly contains the local-only repair (the pipeline
// kept the repair's change and added more). Ordinary recovery already adopts
// this when the gate holds the local head; the same relation must be proven
// when it does not, with the pre-recovery head anchored.
func TestRecoverPreservedHeadContainingLocalOnlyWorkIsAdoptedWithoutLocalHeadInGate(t *testing.T) {
	t.Parallel()

	f := newRecoverFixture(t, types.RunCancelled)
	// The cached recover fixture may remove its scratch pipeline clone, so
	// clone the gate again when it is absent to commit the pipeline's further work.
	pipeline := filepath.Join(filepath.Dir(f.gate), "pipeline")
	if _, err := os.Stat(pipeline); os.IsNotExist(err) {
		mustRun(t, filepath.Dir(f.gate), "-c", "core.autocrlf=false", "clone", f.gate, pipeline)
		mustRun(t, pipeline, "checkout", "feature/recover")
	}
	mustWrite(t, filepath.Join(pipeline, "more.txt"), "pipeline kept going\n")
	mustRun(t, pipeline, "add", "more.txt")
	mustRun(t, pipeline, "commit", "-m", "no-mistakes(test): more")
	preserved := mustRun(t, pipeline, "rev-parse", "HEAD")
	mustRun(t, pipeline, "push", "origin", "HEAD:refs/heads/feature/recover")
	if err := f.db.UpdateRunHeadSHA(f.run.ID, preserved); err != nil {
		t.Fatal(err)
	}
	f.preserved = preserved
	mustRun(t, f.gate, "update-ref", f.anchorRef(), preserved)
	f.run, _ = f.db.GetRun(f.run.ID)
	f.service.DB = f.db

	mustWrite(t, filepath.Join(f.local, "fix.txt"), "pipeline fix 2\n")
	mustRun(t, f.local, "add", "fix.txt")
	mustRun(t, f.local, "commit", "-m", "operator copy of the pipeline fix")
	local := mustRun(t, f.local, "rev-parse", "HEAD")
	if objectExists(f.ctx, f.gate, local) || objectExists(f.ctx, f.local, preserved) {
		t.Fatal("fixture bug: heads must be on opposite sides")
	}

	state := f.service.InspectCached(f.ctx)
	if state.Safety != "blocked_pipeline_owned_recoverable" || state.NextAction == nil || state.NextAction.Command != "no-mistakes axi sync --recover" {
		t.Fatalf("inspection = %#v", state)
	}
	recovered := f.service.Recover(f.ctx, false)
	if !recovered.Recovered || !recovered.Changed {
		t.Fatalf("recover = %#v", recovered)
	}
	if got := mustRun(t, f.local, "rev-parse", "HEAD"); got != preserved {
		t.Fatalf("HEAD = %s, want preserved %s", got, preserved)
	}
	if got := mustRun(t, f.local, "rev-parse", "refs/no-mistakes/recover-local/"+f.run.ID); got != local {
		t.Fatalf("pre-recovery local head anchor = %s, want %s", got, local)
	}
	if got := mustRun(t, f.local, "rev-parse", f.anchorRef()); got != preserved {
		t.Fatalf("preserved anchor = %s", got)
	}
	if !f.custodyReturned() {
		t.Fatal("custody was not returned")
	}
}

func TestRecoverImportProofRefusesUnrelatedHistoriesDirtyWorktreesAndConflictingAnchors(t *testing.T) {
	t.Parallel()

	t.Run("unrelated histories", func(t *testing.T) {
		t.Parallel()
		f := repairFixture(t)
		mustRun(t, f.local, "checkout", "--orphan", "unrelated")
		mustRun(t, f.local, "rm", "-rf", "--quiet", ".")
		mustWrite(t, filepath.Join(f.local, "fix.txt"), "pipeline fix 2\n")
		mustRun(t, f.local, "add", "fix.txt")
		mustRun(t, f.local, "commit", "-m", "unrelated root")
		mustRun(t, f.local, "branch", "-M", "feature/recover")
		assertManualReconciliationOffer(t, f.service.InspectCached(f.ctx))
		if refused := f.service.Recover(f.ctx, false); refused.Recovered || f.custodyReturned() {
			t.Fatalf("unrelated histories recovered: %#v", refused)
		}
	})
	t.Run("dirty worktree", func(t *testing.T) {
		t.Parallel()
		f := repairFixture(t)
		mustWrite(t, filepath.Join(f.local, "fix.txt"), "pipeline fix 2\n")
		mustRun(t, f.local, "add", "fix.txt")
		f.commitRepair("extra.txt", "repair only\n")
		mustWrite(t, filepath.Join(f.local, "extra.txt"), "dirty edit\n")
		assertManualReconciliationOffer(t, f.service.InspectCached(f.ctx))
		if refused := f.service.Recover(f.ctx, false); refused.Recovered || f.custodyReturned() {
			t.Fatalf("dirty worktree recovered: %#v", refused)
		}
	})
	t.Run("gate recovery ref pointing elsewhere", func(t *testing.T) {
		t.Parallel()
		f := repairFixture(t)
		mustWrite(t, filepath.Join(f.local, "fix.txt"), "pipeline fix 2\n")
		mustRun(t, f.local, "add", "fix.txt")
		f.commitRepair("extra.txt", "repair only\n")
		mustRun(t, f.gate, "update-ref", f.anchorRef(), f.submitted)
		assertManualReconciliationOffer(t, f.service.InspectCached(f.ctx))
		if refused := f.service.Recover(f.ctx, false); refused.Recovered || f.custodyReturned() {
			t.Fatalf("conflicting anchor recovered: %#v", refused)
		}
	})
}

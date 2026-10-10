package branchsync

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func divergentAvailableFixture(t *testing.T) *recoverFixture {
	t.Helper()
	f := newRecoverFixture(t, types.RunFailed)
	mustWrite(t, filepath.Join(f.local, "operator-fix.txt"), "independent validated fix\n")
	mustRun(t, f.local, "add", "operator-fix.txt")
	mustRun(t, f.local, "commit", "-m", "retain independent operator fix")
	local := mustRun(t, f.local, "rev-parse", "HEAD")
	mustRun(t, f.gate, "fetch", f.local, local+":refs/no-mistakes/test-local-fix")
	mustRun(t, f.gate, "update-ref", f.anchorRef(), f.preserved)
	return f
}

func TestAvailableDivergentHeadsOfferNonDiscardingKeepLocal(t *testing.T) {
	t.Parallel()
	f := divergentAvailableFixture(t)
	local := mustRun(t, f.local, "rev-parse", "HEAD")
	localRefs := mustRun(t, f.local, "show-ref")
	gateRefs := mustRun(t, f.gate, "show-ref")
	state := f.service.InspectCached(f.ctx)
	assertKeepLocalRecoveryOffer(t, state)
	if state.Relation != RelationDiverged || state.Recovery == nil || state.Recovery.Source != "available_heads" ||
		state.Recovery.RequiredHead != local || state.Recovery.PreservedHead != f.preserved || !state.Recovery.KeepLocal {
		t.Fatalf("available-head proof = %#v", state)
	}
	if mustRun(t, f.local, "show-ref") != localRefs || mustRun(t, f.gate, "show-ref") != gateRefs || f.custodyReturned() {
		t.Fatal("inspection mutated custody or refs")
	}
	if refused := f.service.Recover(f.ctx, false); refused.Recovered || refused.Safety != "blocked_recover_diverged" {
		t.Fatalf("plain recovery took non-containing head: %#v", refused)
	}
	kept := f.service.Recover(f.ctx, true)
	if !kept.Recovered || kept.Changed || mustRun(t, f.local, "rev-parse", "HEAD") != local || mustRun(t, f.local, "rev-parse", f.anchorRef()) != f.preserved {
		t.Fatalf("non-discarding keep-local = %#v", kept)
	}
	if _, err := os.Stat(filepath.Join(f.local, "operator-fix.txt")); err != nil {
		t.Fatal(err)
	}
	for _, head := range []string{local, f.preserved} {
		mustRun(t, f.gate, "cat-file", "-e", head+"^{commit}")
	}
	if repeat := f.service.Recover(f.ctx, true); !repeat.Recovered || repeat.Changed {
		t.Fatalf("repeat was not a no-op: %#v", repeat)
	}
}

func TestAvailableHeadsKeepLocalReleasesAndPreservesWholeStack(t *testing.T) {
	t.Parallel()
	f := divergentAvailableFixture(t)
	olderHead := mustRun(t, f.gate, "rev-parse", f.preserved+"^")
	mustRun(t, f.gate, "update-ref", f.anchorRef(), olderHead, f.preserved)
	if err := f.db.UpdateRunStatusWithVerifiedHead(f.run.ID, types.RunFailed, olderHead); err != nil {
		t.Fatal(err)
	}
	newer, err := f.db.InsertRun(f.repo.ID, f.run.Branch, f.submitted, f.base)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.UpdateRunStatusWithVerifiedHead(newer.ID, types.RunFailed, f.preserved); err != nil {
		t.Fatal(err)
	}
	mustRun(t, f.gate, "update-ref", custody.RecoveryRef(newer.ID), f.preserved)
	assertKeepLocalRecoveryOffer(t, f.service.InspectCached(f.ctx))
	kept := f.service.Recover(f.ctx, true)
	if !kept.Recovered || kept.Changed {
		t.Fatalf("stack keep-local = %#v", kept)
	}
	for id, head := range map[string]string{f.run.ID: olderHead, newer.ID: f.preserved} {
		run, err := f.db.GetRun(id)
		if err != nil || run.CustodyReturnedAt == nil || run.HeadSHA != head {
			t.Fatalf("run %s not retained and released: %#v, %v", id, run, err)
		}
		if got := mustRun(t, f.gate, "rev-parse", custody.RecoveryRef(id)); got != head {
			t.Fatalf("lost anchored head %s: %s", head, got)
		}
	}
}

func TestAvailableHeadRecoveryRevalidatesPreservationBeforeGateMovement(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"anchor", "dirty", "run-active", "archive", "gate-branch-symbolic"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := divergentAvailableFixture(t)
			assertKeepLocalRecoveryOffer(t, f.service.InspectCached(f.ctx))
			local := mustRun(t, f.local, "rev-parse", "HEAD")
			gate := mustRun(t, f.gate, "rev-parse", "refs/heads/"+f.run.Branch)
			f.service.beforeGateReset = func() {
				switch name {
				case "anchor":
					mustRun(t, f.gate, "update-ref", f.anchorRef(), f.submitted, f.preserved)
				case "dirty":
					mustWrite(t, filepath.Join(f.local, "operator-fix.txt"), "concurrent uncommitted work\n")
				case "run-active":
					if err := f.db.UpdateRunStatus(f.run.ID, types.RunRunning); err != nil {
						t.Fatal(err)
					}
				case "gate-branch-symbolic":
					mustRun(t, f.gate, "update-ref", "refs/heads/bystander", gate)
					mustRun(t, f.gate, "symbolic-ref", "refs/heads/"+f.run.Branch, "refs/heads/bystander")
				case "archive":
					if _, err := f.db.RecordRecoveryArchive(db.RecoveryArchive{
						OwnerRunID: f.run.ID, RepoID: f.repo.ID, RunID: f.run.ID, Branch: f.run.Branch,
						RequiredHeadSHA: local, PreservedHeadSHA: f.preserved, ArchiveRef: "refs/heads/archive/changed-source",
					}); err != nil {
						t.Fatal(err)
					}
				}
			}
			refused := f.service.Recover(f.ctx, true)
			if refused.Recovered || refused.Changed || f.custodyReturned() {
				t.Fatalf("changed evidence was accepted: %#v", refused)
			}
			if mustRun(t, f.local, "rev-parse", "HEAD") != local || mustRun(t, f.gate, "rev-parse", "refs/heads/"+f.run.Branch) != gate {
				t.Fatal("changed preservation evidence moved a branch")
			}
		})
	}
}

func TestAvailableHeadRecoveryDoesNotTurnLostOlderEvidenceIntoDiscard(t *testing.T) {
	t.Parallel()
	f := divergentAvailableFixture(t)
	tree := mustRun(t, f.gate, "rev-parse", f.preserved+"^{tree}")
	older := mustRun(t, f.gate, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit-tree", tree, "-p", f.base, "-m", "independent older correction")
	mustRun(t, f.gate, "update-ref", f.anchorRef(), older, f.preserved)
	if err := f.db.UpdateRunStatusWithVerifiedHead(f.run.ID, types.RunFailed, older); err != nil {
		t.Fatal(err)
	}
	newer, err := f.db.InsertRun(f.repo.ID, f.run.Branch, f.submitted, f.base)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.UpdateRunStatusWithVerifiedHead(newer.ID, types.RunFailed, f.preserved); err != nil {
		t.Fatal(err)
	}
	mustRun(t, f.gate, "update-ref", custody.RecoveryRef(newer.ID), f.preserved)
	assertKeepLocalRecoveryOffer(t, f.service.InspectCached(f.ctx))
	local := mustRun(t, f.local, "rev-parse", "HEAD")
	gate := mustRun(t, f.gate, "rev-parse", "refs/heads/"+f.run.Branch)
	f.service.beforeGateReset = func() {
		mustRun(t, f.gate, "update-ref", "-d", f.anchorRef(), older)
		mustRun(t, f.gate, "prune", "--expire=now")
		if objectExists(f.ctx, f.gate, older) || objectExists(f.ctx, f.local, older) {
			t.Fatal("fixture did not lose the older evidence")
		}
	}
	refused := f.service.Recover(f.ctx, true)
	if refused.Recovered || refused.Safety != "blocked_recover_preserve_failed" || f.custodyReturned() {
		t.Fatalf("available-head recovery discarded newly missing work: %#v", refused)
	}
	if mustRun(t, f.local, "rev-parse", "HEAD") != local || mustRun(t, f.gate, "rev-parse", "refs/heads/"+f.run.Branch) != gate {
		t.Fatal("missing evidence moved a branch")
	}
	run, err := f.db.GetRun(newer.ID)
	if err != nil || run.CustodyReturnedAt != nil {
		t.Fatalf("missing evidence returned newer custody: %#v, %v", run, err)
	}
}

func TestAvailableHeadOfferFailsClosedOnUnsafeEvidence(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"dirty", "active", "unverified", "gate-unavailable", "local-anchor", "gate-anchor", "symbolic-gate-branch", "older-unverified"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := divergentAvailableFixture(t)
			switch name {
			case "dirty":
				mustWrite(t, filepath.Join(f.local, "operator-fix.txt"), "uncommitted work\n")
			case "active":
				if err := f.db.UpdateRunStatus(f.run.ID, types.RunRunning); err != nil {
					t.Fatal(err)
				}
			case "unverified":
				if err := f.db.UpdateRunStatus(f.run.ID, types.RunFailed); err != nil {
					t.Fatal(err)
				}
			case "gate-unavailable":
				f.service.GateDir = filepath.Join(t.TempDir(), "absent.git")
			case "local-anchor":
				mustRun(t, f.local, "update-ref", f.anchorRef(), f.submitted)
			case "gate-anchor":
				mustRun(t, f.gate, "update-ref", f.anchorRef(), f.submitted)
			case "symbolic-gate-branch":
				mustRun(t, f.gate, "symbolic-ref", "refs/heads/"+f.run.Branch, f.anchorRef())
			case "older-unverified":
				if err := f.db.UpdateRunStatus(f.run.ID, types.RunFailed); err != nil {
					t.Fatal(err)
				}
				newer, err := f.db.InsertRun(f.repo.ID, f.run.Branch, f.submitted, f.base)
				if err != nil {
					t.Fatal(err)
				}
				if err := f.db.UpdateRunStatusWithVerifiedHead(newer.ID, types.RunFailed, f.preserved); err != nil {
					t.Fatal(err)
				}
			}
			beforeLocal := mustRun(t, f.local, "show-ref")
			beforeGate := mustRun(t, f.gate, "show-ref")
			state := f.service.InspectCached(f.ctx)
			if state.Recovery != nil && state.Recovery.Source == "available_heads" {
				t.Fatalf("unsafe evidence earned available-head choice: %#v", state)
			}
			if mustRun(t, f.local, "show-ref") != beforeLocal || mustRun(t, f.gate, "show-ref") != beforeGate || f.custodyReturned() {
				t.Fatal("refused inspection mutated history")
			}
		})
	}
}

package branchsync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	gitpkg "github.com/kunchenguid/no-mistakes/internal/git"
)

func TestInternalMutationCapabilityRequiresActiveBranchLock(t *testing.T) {
	f := newRecoverFixture(t, "cancelled")
	spec := db.InternalRefMutationSpec{
		RepoID: f.repo.ID, GatePath: f.gate, Branch: f.run.Branch,
		Ref: "refs/heads/feature/recover", OldSHA: f.submitted, NewSHA: f.preserved,
		Operation: "update-ref", Scope: db.InternalRefMutationScopeOrdinary,
	}
	if _, _, err := IssueInternalRefMutation(f.db, nil, spec); err == nil {
		t.Fatal("capability issuance without a branch lock unexpectedly succeeded")
	}
	lock, err := acquireCustodyLock(f.service, f.run)
	if err != nil {
		t.Fatal(err)
	}
	capability, endpoint, err := IssueInternalRefMutation(f.db, lock, spec)
	if err != nil {
		t.Fatalf("capability issuance with a branch lock: %v", err)
	}
	request := InternalRefMutationAuthorization{Capability: capability, Phase: "prepared", GatePath: spec.GatePath, Branch: spec.Branch, Ref: spec.Ref, OldSHA: spec.OldSHA, NewSHA: spec.NewSHA, Operation: spec.Operation, Scope: spec.Scope}
	if err := AuthorizeInternalRefMutation(endpoint, request); err != nil {
		t.Fatalf("live branch-lock authority rejected capability: %v", err)
	}
	lock.Release()
	if err := AuthorizeInternalRefMutation(endpoint, request); err == nil {
		t.Fatal("closed branch-lock authority accepted capability")
	}
	restarted, err := acquireCustodyLock(f.service, f.run)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Release()
	if err := AuthorizeInternalRefMutation(endpoint, request); err == nil {
		t.Fatal("restarted branch-lock authority accepted a capability from the prior owner")
	}
	if _, err := restarted.ensureInternalMutationAuthority(f.db); err != nil {
		t.Fatal(err)
	}
	if err := restarted.file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := IssueInternalRefMutation(f.db, restarted, spec); err == nil {
		t.Fatal("closed branch-lock file descriptor issued a capability")
	}
	restarted.file = nil
	restarted.closeInternalMutationAuthority()
}

func TestInternalMutationAuthorityClosesIdleClientsOnShutdown(t *testing.T) {
	f := newRecoverFixture(t, "cancelled")
	lock, err := acquireCustodyLock(f.service, f.run)
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := lock.ensureInternalMutationAuthority(f.db)
	if err != nil {
		lock.Release()
		t.Fatal(err)
	}
	conn, err := dialInternalMutationAuthority(endpoint)
	if err != nil {
		lock.Release()
		t.Fatal(err)
	}
	defer conn.Close()
	done := make(chan struct{})
	go func() {
		lock.Release()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("authority shutdown waited on an idle client")
	}
}

func TestGateRefLockBlocksHooksPathOverride(t *testing.T) {
	f := newRecoverFixture(t, "cancelled")
	branchLock, err := acquireCustodyLock(f.service, f.run)
	if err != nil {
		t.Fatal(err)
	}
	defer branchLock.Release()
	authority, err := branchLock.ensureInternalMutationAuthority(f.db)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := acquireGateRefLock(f.gate, "refs/heads/feature/recover", authority)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()

	if _, err := gitpkg.Run(context.Background(), f.gate, "-c", "core.hooksPath="+t.TempDir(), "update-ref", "refs/heads/feature/recover", f.preserved, f.submitted); err == nil {
		t.Fatal("raw update-ref bypassed the final ordinary-ref lock")
	}
	if got, err := readLockedGateRef(f.gate, "refs/heads/feature/recover"); err != nil || got != f.preserved {
		t.Fatalf("locked gate branch = %s, want %s", got, f.preserved)
	}
}

func TestUpdateGateRefRefusesBeforeOrdinaryMutation(t *testing.T) {
	f := newRecoverFixture(t, "cancelled")
	branchLock, err := acquireCustodyLock(f.service, f.run)
	if err != nil {
		t.Fatal(err)
	}
	defer branchLock.Release()
	f.service.InternalMutationConsumed = func(string) error { return errors.New("live authority refused mutation") }
	if err := f.service.updateGateRef(f.ctx, branchLock, f.run.Branch, "refs/heads/feature/recover", f.preserved, f.submitted); err == nil {
		t.Fatal("ordinary mutation unexpectedly succeeded")
	}
	if got := mustRun(t, f.gate, "rev-parse", "refs/heads/feature/recover"); got != f.preserved {
		t.Fatalf("ordinary gate ref changed to %s, want %s", got, f.preserved)
	}
}

func TestGateRefLockRemovesStaleOwnedLockAfterAuthorityExit(t *testing.T) {
	f := newRecoverFixture(t, "cancelled")
	branchLock, err := acquireCustodyLock(f.service, f.run)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := branchLock.ensureInternalMutationAuthority(f.db)
	if err != nil {
		branchLock.Release()
		t.Fatal(err)
	}
	gateLock, err := acquireGateRefLock(f.gate, "refs/heads/feature/recover", authority)
	if err != nil {
		branchLock.Release()
		t.Fatal(err)
	}
	if err := gateLock.file.Close(); err != nil {
		branchLock.Release()
		t.Fatal(err)
	}
	gateLock.file = nil
	branchLock.closeInternalMutationAuthority()
	if err := branchLock.file.Close(); err != nil {
		t.Fatal(err)
	}
	branchLock.file = nil

	restarted, err := acquireCustodyLock(f.service, f.run)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Release()
	newAuthority, err := restarted.ensureInternalMutationAuthority(f.db)
	if err != nil {
		t.Fatal(err)
	}
	newGateLock, err := acquireGateRefLock(f.gate, "refs/heads/feature/recover", newAuthority)
	if err != nil {
		t.Fatalf("stale owned gate lock blocked retry: %v", err)
	}
	newGateLock.Release()
}

func TestStampedRecoveryReclaimsOwnedGateLockAfterCrash(t *testing.T) {
	f := newRecoverFixture(t, "cancelled")
	branchLock, err := acquireCustodyLock(f.service, f.run)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := branchLock.ensureInternalMutationAuthority(f.db)
	if err != nil {
		branchLock.Release()
		t.Fatal(err)
	}
	generation, err := newGateRefLockGeneration()
	if err != nil {
		branchLock.Release()
		t.Fatal(err)
	}
	ref := "refs/heads/" + f.run.Branch
	owner := gateRefLockOwner{RunID: f.run.ID, RepoID: f.repo.ID, GatePath: f.gate, Branch: f.run.Branch, Ref: ref, OwnerGeneration: generation, AuthorityEndpoint: authority, ExpectedHead: f.preserved}
	gateLock, err := acquireOwnedGateRefLock(f.gate, ref, owner)
	if err != nil {
		branchLock.Release()
		t.Fatal(err)
	}
	if err := f.db.PrepareGateRefLock(db.GateRefLockJournal{RunID: f.run.ID, RepoID: f.repo.ID, GatePath: f.gate, Branch: f.run.Branch, Ref: ref, LockPath: gateLock.path, OwnerGeneration: generation, AuthorityEndpoint: authority, ExpectedHead: f.preserved, FileIdentity: gateLock.identity}); err != nil {
		gateLock.Release()
		branchLock.Release()
		t.Fatal(err)
	}
	if err := f.db.SetRunCustodyReturnedCAS(f.run); err != nil {
		gateLock.Release()
		branchLock.Release()
		t.Fatal(err)
	}
	if err := gateLock.file.Close(); err != nil {
		branchLock.Release()
		t.Fatal(err)
	}
	gateLock.file = nil
	branchLock.Release()

	state := f.service.Recover(f.ctx, true)
	if !state.Recovered {
		t.Fatalf("stamped recovery did not reclaim crashed gate lock: %#v", state)
	}
	if _, err := os.Stat(gateLock.path); !os.IsNotExist(err) {
		t.Fatalf("crashed gate lock still exists: %v", err)
	}
	if journal, err := f.db.GetGateRefLock(f.run.ID); err != nil || journal != nil {
		t.Fatalf("stale gate lock journal = %#v, %v", journal, err)
	}
}

func TestGateRefLockReleaseRetainsJournalWhenRemovalFails(t *testing.T) {
	f := newRecoverFixture(t, "cancelled")
	branchLock, err := acquireCustodyLock(f.service, f.run)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := branchLock.ensureInternalMutationAuthority(f.db)
	if err != nil {
		branchLock.Release()
		t.Fatal(err)
	}
	generation, err := newGateRefLockGeneration()
	if err != nil {
		branchLock.Release()
		t.Fatal(err)
	}
	ref := "refs/heads/" + f.run.Branch
	owner := gateRefLockOwner{RunID: f.run.ID, RepoID: f.repo.ID, GatePath: f.gate, Branch: f.run.Branch, Ref: ref, OwnerGeneration: generation, AuthorityEndpoint: authority, ExpectedHead: f.preserved}
	gateLock, err := acquireOwnedGateRefLock(f.gate, ref, owner)
	if err != nil {
		branchLock.Release()
		t.Fatal(err)
	}
	if err := f.db.PrepareGateRefLock(db.GateRefLockJournal{RunID: f.run.ID, RepoID: f.repo.ID, GatePath: f.gate, Branch: f.run.Branch, Ref: ref, LockPath: gateLock.path, OwnerGeneration: generation, AuthorityEndpoint: authority, ExpectedHead: f.preserved, FileIdentity: gateLock.identity}); err != nil {
		gateLock.Release()
		branchLock.Release()
		t.Fatal(err)
	}
	gateLock.database = f.db
	originalRemove := removeGateRefLock
	removeGateRefLock = func(string) error { return errors.New("injected removal failure") }
	err = gateLock.Release()
	removeGateRefLock = originalRemove
	if err == nil {
		t.Fatal("gate lock release unexpectedly ignored removal failure")
	}
	if journal, journalErr := f.db.GetGateRefLock(f.run.ID); journalErr != nil || journal == nil {
		t.Fatalf("gate lock journal after failed removal = %#v, %v", journal, journalErr)
	}
	_ = originalRemove(gateLock.path)
	_ = f.db.ClearGateRefLock(f.run.ID, generation)
	branchLock.Release()
}

func TestCustodyLockRejectsLiveSecondAttemptAndReleasesAfterOwnerExit(t *testing.T) {
	f := newRecoverFixture(t, "cancelled")
	first, err := acquireCustodyLock(f.service, f.run)
	if err != nil {
		t.Fatal(err)
	}
	second, err := acquireCustodyLock(f.service, f.run)
	if second != nil || !errors.Is(err, ErrCustodyLockHeld) {
		t.Fatalf("second custody lock = %#v, %v", second, err)
	}
	first.Release()
	third, err := acquireCustodyLock(f.service, f.run)
	if err != nil || third == nil {
		t.Fatalf("custody lock after owner release = %#v, %v", third, err)
	}
	third.Release()
}

func TestCustodyLockIsSharedByRepositoryBranchAcrossRuns(t *testing.T) {
	f := newRecoverFixture(t, "cancelled")
	first, err := acquireCustodyLock(f.service, f.run)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()

	newer := *f.run
	newer.ID = "newer-run"
	second, err := acquireCustodyLock(f.service, &newer)
	if second != nil || !errors.Is(err, ErrCustodyLockHeld) {
		t.Fatalf("newer run custody lock = %#v, %v", second, err)
	}

	otherBranch := newer
	otherBranch.ID = "other-branch-run"
	otherBranch.Branch = "other-branch"
	third, err := acquireCustodyLock(f.service, &otherBranch)
	if err != nil || third == nil {
		t.Fatalf("other branch custody lock = %#v, %v", third, err)
	}
	third.Release()
}

func TestCustodyLockFailurePreservesNonContentionErrors(t *testing.T) {
	state := State{State: StatePipelineOwned}
	if got := custodyLockFailure(state, fmt.Errorf("permission denied")); got.Safety != "blocked_recover_custody_lock" {
		t.Fatalf("non-contention lock failure = %#v", got)
	}
	if got := custodyLockFailure(state, fmt.Errorf("%w: busy", ErrCustodyLockHeld)); got.Safety != "blocked_recover_custody_race" {
		t.Fatalf("contention lock failure = %#v", got)
	}
}

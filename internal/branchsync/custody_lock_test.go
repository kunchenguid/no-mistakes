package branchsync

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
)

func TestInternalMutationCapabilityRequiresActiveBranchLock(t *testing.T) {
	f := newRecoverFixture(t, "cancelled")
	spec := db.InternalRefMutationSpec{
		RepoID: f.repo.ID, GatePath: f.gate, Branch: f.run.Branch,
		Ref: "refs/heads/feature/recover", OldSHA: f.submitted, NewSHA: f.preserved,
		Operation: "update-ref", Scope: db.InternalRefMutationScopeOrdinary,
	}
	if _, err := IssueInternalRefMutation(f.db, nil, spec); err == nil {
		t.Fatal("capability issuance without a branch lock unexpectedly succeeded")
	}
	lock, err := acquireCustodyLock(f.service, f.run)
	if err != nil {
		t.Fatal(err)
	}
	proof := lock.InternalMutationLockProof()
	tokenHash := sha256.Sum256([]byte(proof.Token))
	if !VerifyInternalMutationLockProof(proof.Path, proof.PID, hex.EncodeToString(tokenHash[:])) {
		t.Fatal("active branch-lock proof was rejected")
	}
	if _, err := IssueInternalRefMutation(f.db, lock, spec); err != nil {
		t.Fatalf("capability issuance with a branch lock: %v", err)
	}
	lock.Release()
	if VerifyInternalMutationLockProof(proof.Path, proof.PID, hex.EncodeToString(tokenHash[:])) {
		t.Fatal("released branch-lock proof remained valid")
	}
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

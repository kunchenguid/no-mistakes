package branchsync

import (
	"errors"
	"fmt"
	"testing"
)

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

func TestCustodyLockFailurePreservesNonContentionErrors(t *testing.T) {
	state := State{State: StatePipelineOwned}
	if got := custodyLockFailure(state, fmt.Errorf("permission denied")); got.Safety != "blocked_recover_custody_lock" {
		t.Fatalf("non-contention lock failure = %#v", got)
	}
	if got := custodyLockFailure(state, fmt.Errorf("%w: busy", ErrCustodyLockHeld)); got.Safety != "blocked_recover_custody_race" {
		t.Fatalf("contention lock failure = %#v", got)
	}
}

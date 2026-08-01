package branchsync

import (
	"errors"
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

package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/paths"
)

func TestAcquireSingletonLock_SecondAcquireFails(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	lock1, err := acquireSingletonLock(p)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer lock1.Release()

	_, err = acquireSingletonLock(p)
	if err == nil {
		t.Fatal("expected second acquire to fail while first lock is held")
	}
	if !errors.Is(err, ErrSingletonLockHeld) {
		t.Fatalf("expected ErrSingletonLockHeld, got %v", err)
	}
}

func TestAcquireSingletonLock_ReleaseAllowsReacquire(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	lock1, err := acquireSingletonLock(p)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	lock1.Release()

	lock2, err := acquireSingletonLock(p)
	if err != nil {
		t.Fatalf("second acquire after release: %v", err)
	}
	defer lock2.Release()
}

func TestAcquireSingletonLock_ReportsExistingHolder(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	lock1, err := acquireSingletonLock(p)
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	defer lock1.Release()

	_, err = acquireSingletonLock(p)
	if err == nil {
		t.Fatal("expected second acquire to fail")
	}
	if !strings.Contains(err.Error(), "pid") {
		t.Fatalf("expected error to include holder diagnostics (pid), got: %v", err)
	}
}

// TestSingletonLock_ReleaseNilSafe verifies Release is a safe no-op on a nil
// receiver, matching the pattern used for a failed-acquire defer.
func TestSingletonLock_ReleaseNilSafe(t *testing.T) {
	var lock *singletonLock
	lock.Release() // must not panic
}

func TestAcquireSingletonLock_CreatesLockFileUnderRoot(t *testing.T) {
	root := t.TempDir()
	p := paths.WithRoot(root)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	lock, err := acquireSingletonLock(p)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()

	if _, err := os.Stat(filepath.Join(root, "daemon.lock")); err != nil {
		t.Fatalf("expected daemon.lock to exist: %v", err)
	}
}

package git

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func TestWithDirtyWorkCheckoutsDeduplicatesLockStripes(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "first")
	var collision string
	for i := 0; i < 4096; i++ {
		candidate := filepath.Join(root, fmt.Sprintf("candidate-%d", i))
		if dirtyWorkLockIndex(candidate) == dirtyWorkLockIndex(first) {
			collision = candidate
			break
		}
	}
	if collision == "" {
		t.Fatal("fixture did not find two checkout paths sharing ownership")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	called := false
	err := WithDirtyWorkCheckouts(ctx, []string{first, collision, first}, func() error {
		called = true
		if !dirtyWorkCheckoutLocked(first) || !dirtyWorkCheckoutLocked(collision) {
			t.Fatal("batch did not own every selected checkout")
		}
		return nil
	})
	if err != nil || !called {
		t.Fatalf("colliding batch = %v, ran = %t", err, called)
	}
	if dirtyWorkCheckoutLocked(first) {
		t.Fatal("completed batch retained ownership")
	}
}

func TestWithDirtyWorkCheckoutsCancellationReleasesPartialOwnership(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "first")
	var second string
	for i := 0; i < 4096; i++ {
		candidate := filepath.Join(root, fmt.Sprintf("candidate-%d", i))
		if dirtyWorkLockIndex(candidate) != dirtyWorkLockIndex(first) {
			second = candidate
			break
		}
	}
	if second == "" {
		t.Fatal("fixture did not find independent checkout ownership")
	}
	if dirtyWorkLockIndex(first) > dirtyWorkLockIndex(second) {
		first, second = second, first
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- WithDirtyWorkCheckouts(context.Background(), []string{second}, func() error {
			close(entered)
			<-release
			return nil
		})
	}()
	t.Cleanup(func() { close(release); <-done })
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	called := false
	err := WithDirtyWorkCheckouts(ctx, []string{second, first}, func() error { called = true; return nil })
	if !errors.Is(err, context.DeadlineExceeded) || called {
		t.Fatalf("blocked batch = %v, ran = %t", err, called)
	}
	if dirtyWorkCheckoutLocked(first) || !dirtyWorkCheckoutLocked(second) {
		t.Fatal("cancelled batch released the wrong ownership")
	}
}

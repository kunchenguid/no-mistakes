package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestFixtureBarrierWaitsForDriver(t *testing.T) {
	path := filepath.Join(t.TempDir(), "release")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- waitForReleaseFile(ctx, path) }()
	// Cancellation is the deterministic negative observation: no release means
	// the fixture must wait, never return success on an elapsed fixed delay.
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("unreleased fixture returned %v", err)
	}
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := waitForReleaseFile(context.Background(), path); err != nil {
		t.Fatal(err)
	}
}

func TestFixtureBarrierReleasesPendingWaiter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "release")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- waitForReleaseFile(ctx, path) }()
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

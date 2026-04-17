package agent

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestStartServerWithPort_DetectsEarlyExit verifies that when the spawned
// server exits before becoming healthy (e.g. `acli` not installed, bad
// flags, or port bind failure), startup fails fast instead of waiting the
// full 30s health-check deadline.
func TestStartServerWithPort_DetectsEarlyExit(t *testing.T) {
	// /usr/bin/true exits 0 immediately without opening a listener.
	const bin = "/usr/bin/true"

	start := time.Now()
	srv, err := startServerWithPort(context.Background(), bin, nil, t.TempDir(), "/healthcheck", 1)
	elapsed := time.Since(start)

	if err == nil {
		srv.shutdown()
		t.Fatal("expected error when server exits before becoming healthy")
	}
	if !strings.Contains(err.Error(), "exit") {
		t.Errorf("error should mention early exit, got: %v", err)
	}
	if elapsed > 5*time.Second {
		t.Errorf("should fail fast on early exit, waited %v", elapsed)
	}
}

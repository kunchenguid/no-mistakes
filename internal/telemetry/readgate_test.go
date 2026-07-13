package telemetry

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func newTestGate(t *testing.T, interval time.Duration, now *time.Time) *ReadSurfaceGate {
	t.Helper()
	path := filepath.Join(t.TempDir(), "telemetry-gate.json")
	return NewReadSurfaceGate(path, interval, func() time.Time { return *now })
}

// TestReadSurfaceGate_BoundsRepeatedPolling reproduces the axi-status
// firehose: a driving agent polling status every few seconds for hours. The
// gate must keep the emitted event count bounded to the heartbeat schedule
// instead of one event per poll.
func TestReadSurfaceGate_BoundsRepeatedPolling(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	gate := newTestGate(t, 10*time.Minute, &now)

	emitted := 0
	// 6 hours of a 5-second polling loop with no state change: 4,320 polls.
	for i := 0; i < 4320; i++ {
		if gate.ShouldEmit("axi-status", "run-1|running|review:running") {
			emitted++
		}
		now = now.Add(5 * time.Second)
	}

	// One initial emit plus one heartbeat per full 10-minute window that
	// elapses within the loop (the final window ends as the loop does).
	want := 1 + 35
	if emitted != want {
		t.Fatalf("emitted = %d over 4320 polls, want %d (heartbeat-bounded)", emitted, want)
	}
}

// TestReadSurfaceGate_EmitsOnEveryStateChange proves meaningful state
// transitions are never suppressed, even inside the heartbeat window.
func TestReadSurfaceGate_EmitsOnEveryStateChange(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	gate := newTestGate(t, 10*time.Minute, &now)

	states := []string{
		"run-1|running|review:running",
		"run-1|running|review:awaiting_approval",
		"run-1|running|review:fixing",
		"run-1|completed|",
	}
	for i, state := range states {
		if !gate.ShouldEmit("axi-status", state) {
			t.Fatalf("state change %d (%q) was suppressed", i, state)
		}
		// Repeat of the same state inside the window is suppressed.
		if gate.ShouldEmit("axi-status", state) {
			t.Fatalf("unchanged state %d (%q) was emitted twice", i, state)
		}
		now = now.Add(time.Second)
	}
}

// TestReadSurfaceGate_HeartbeatAfterInterval verifies the unchanged-state
// heartbeat: suppressed until the interval elapses, emitted once after.
func TestReadSurfaceGate_HeartbeatAfterInterval(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	gate := newTestGate(t, 10*time.Minute, &now)

	if !gate.ShouldEmit("axi-home", "repo|idle") {
		t.Fatal("first observation must emit")
	}
	now = now.Add(9*time.Minute + 59*time.Second)
	if gate.ShouldEmit("axi-home", "repo|idle") {
		t.Fatal("unchanged state inside the interval must be suppressed")
	}
	now = now.Add(2 * time.Second)
	if !gate.ShouldEmit("axi-home", "repo|idle") {
		t.Fatal("heartbeat after the interval must emit")
	}
	if gate.ShouldEmit("axi-home", "repo|idle") {
		t.Fatal("heartbeat must re-arm the window")
	}
}

// TestReadSurfaceGate_SurfacesAreIndependent verifies one command's state does
// not affect another's dedupe window.
func TestReadSurfaceGate_SurfacesAreIndependent(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	gate := newTestGate(t, 10*time.Minute, &now)

	if !gate.ShouldEmit("axi-status", "a") {
		t.Fatal("first axi-status must emit")
	}
	if !gate.ShouldEmit("status", "a") {
		t.Fatal("first status must emit despite axi-status sharing the fingerprint")
	}
	if gate.ShouldEmit("axi-status", "a") {
		t.Fatal("repeat axi-status must be suppressed")
	}
}

// TestReadSurfaceGate_PersistsAcrossProcesses verifies the dedupe state
// survives process boundaries: each CLI invocation is a fresh process, so an
// in-memory-only gate would never suppress anything.
func TestReadSurfaceGate_PersistsAcrossProcesses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "telemetry-gate.json")
	now := time.Unix(1_700_000_000, 0)
	clock := func() time.Time { return now }

	first := NewReadSurfaceGate(path, 10*time.Minute, clock)
	if !first.ShouldEmit("axi-status", "run-1|running") {
		t.Fatal("first process must emit")
	}

	second := NewReadSurfaceGate(path, 10*time.Minute, clock)
	if second.ShouldEmit("axi-status", "run-1|running") {
		t.Fatal("second process must see the first process's emit and suppress")
	}
	if !second.ShouldEmit("axi-status", "run-1|completed") {
		t.Fatal("second process must emit on state change")
	}
}

func TestReadSurfaceGate_ConcurrentGatesEmitOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "telemetry-gate.json")
	now := time.Unix(1_700_000_000, 0)
	clock := func() time.Time { return now }

	const callers = 32
	start := make(chan struct{})
	results := make(chan bool, callers)
	var wg sync.WaitGroup
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			gate := NewReadSurfaceGate(path, 10*time.Minute, clock)
			<-start
			results <- gate.ShouldEmit("axi-status", "run-1|running")
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	emitted := 0
	for result := range results {
		if result {
			emitted++
		}
	}
	if emitted != 1 {
		t.Fatalf("concurrent emits = %d, want 1", emitted)
	}
}

func TestReadSurfaceGate_ConcurrentProcessesEmitOnce(t *testing.T) {
	const helperEnv = "NO_MISTAKES_READ_GATE_HELPER"
	if os.Getenv(helperEnv) == "1" {
		gate := NewReadSurfaceGate(os.Getenv("NO_MISTAKES_READ_GATE_PATH"), 10*time.Minute, func() time.Time {
			return time.Unix(1_700_000_000, 0)
		})
		if gate.ShouldEmit("axi-status", "run-1|running") {
			fmt.Fprint(os.Stdout, "emit")
		}
		return
	}

	path := filepath.Join(t.TempDir(), "telemetry-gate.json")
	const callers = 8
	type commandResult struct {
		output string
		err    error
	}
	results := make(chan commandResult, callers)
	for range callers {
		go func() {
			cmd := exec.Command(os.Args[0], "-test.run=^TestReadSurfaceGate_ConcurrentProcessesEmitOnce$")
			cmd.Env = append(os.Environ(), helperEnv+"=1", "NO_MISTAKES_READ_GATE_PATH="+path)
			output, err := cmd.CombinedOutput()
			results <- commandResult{output: string(output), err: err}
		}()
	}

	emitted := 0
	for range callers {
		result := <-results
		if result.err != nil {
			t.Fatalf("read gate helper: %v\n%s", result.err, result.output)
		}
		switch strings.TrimSpace(strings.TrimSuffix(result.output, "PASS\n")) {
		case "emit":
			emitted++
		case "":
		default:
			t.Fatalf("unexpected helper output %q", result.output)
		}
	}
	if emitted != 1 {
		t.Fatalf("process emits = %d, want 1", emitted)
	}
}

// TestReadSurfaceGate_FailsOpen verifies a corrupt or unwritable state file
// degrades to emitting (the pre-diet behavior) rather than erroring or
// silently dropping meaningful events forever.
func TestReadSurfaceGate_FailsOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "telemetry-gate.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_700_000_000, 0)
	gate := NewReadSurfaceGate(path, 10*time.Minute, func() time.Time { return now })
	if !gate.ShouldEmit("axi-status", "x") {
		t.Fatal("corrupt state must fail open and emit")
	}
	// The corrupt file is replaced, so dedupe recovers.
	if gate.ShouldEmit("axi-status", "x") {
		t.Fatal("gate must recover after rewriting corrupt state")
	}
}

// TestReadSurfaceGate_PrunesStaleEntries keeps the state file bounded when
// many distinct surfaces/scopes accumulate.
func TestReadSurfaceGate_PrunesStaleEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "telemetry-gate.json")
	now := time.Unix(1_700_000_000, 0)
	gate := NewReadSurfaceGate(path, 10*time.Minute, func() time.Time { return now })

	for i := 0; i < maxReadSurfaceEntries*3; i++ {
		gate.ShouldEmit(surfaceName(i), "fp")
		now = now.Add(time.Second)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	state, err := parseReadSurfaceState(data)
	if err != nil {
		t.Fatalf("state file unparseable after pruning: %v", err)
	}
	if len(state.Surfaces) > maxReadSurfaceEntries {
		t.Fatalf("state has %d entries, want <= %d", len(state.Surfaces), maxReadSurfaceEntries)
	}
}

func surfaceName(i int) string {
	return fmt.Sprintf("surface-%d", i)
}

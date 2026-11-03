//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestSettlePublishedToleratesRunFinishedBeforeCancel pins the race behind the
// closing-issues journey flake: a run that skips CI finishes and drops out of
// the daemon's active set after the caller's snapshot showed it live, so the
// cancel reports "no active run". The run's real outcome must decide, not the
// refusal.
func TestSettlePublishedToleratesRunFinishedBeforeCancel(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	setupStatefulGitHub(t, h)
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	const branch = "feature/settle-published"
	h.CommitChange(branch, "s.txt", "s\n", "add settle fixture")
	wt := h.AddWorktree(branch)
	if out, err := h.RunInDir(wt, "axi", "run", "--intent", "settle a finished run", "--skip", "ci"); err != nil {
		t.Fatalf("axi run: %v\n%s", err, out)
	}
	finished := h.WaitForRun(branch, 3*time.Minute)

	// Wait out the daemon's post-finish cleanup, after which it refuses to
	// cancel the run.
	deadline := time.Now().Add(30 * time.Second)
	for {
		err := h.TryCancelRun(finished.ID)
		if err != nil && strings.Contains(err.Error(), "no active run") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s still cancellable 30s after finishing (err=%v)", finished.ID, err)
		}
		time.Sleep(200 * time.Millisecond)
	}

	stale := *finished
	stale.Status = types.RunRunning
	got := settlePublished(t, h, &stale, branch, "stale snapshot")
	if got.ID != finished.ID || (got.Status != types.RunCompleted && got.Status != types.RunChecksPassed) {
		t.Fatalf("settled run = %s status=%s, want %s completed or checks_passed", got.ID, got.Status, finished.ID)
	}
}

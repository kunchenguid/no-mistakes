//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestWorkingPathRepoConfig covers trust_working_path_config, the opt-in that
// lets the maintainer's own checkout supply gate commands for a repo whose
// default branch cannot carry .no-mistakes.yaml.
//
// Both subtests use the same fixture: the trusted default-branch copy carries
// no commands, and the working path holds an UNCOMMITTED .no-mistakes.yaml
// whose lint command writes a marker file. That split is what makes the two
// copies distinguishable — the gate runs in an ephemeral worktree, so only the
// working-path read can see the on-disk edit.
func TestWorkingPathRepoConfig(t *testing.T) {
	t.Run("ignored_by_default", func(t *testing.T) {
		optOut := false
		h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: cleanReviewScenario(t), AllowRepoCommands: &optOut})

		if out, err := h.Run("init"); err != nil {
			t.Fatalf("nm init: %v\n%s", err, out)
		}

		markerPath := pushWithWorkingPathRepoConfig(t, h, "wp-off")

		run := h.WaitForRun("wp-off", 90*time.Second)
		if run.Status != types.RunCompleted {
			t.Fatalf("run did not complete: status=%s error=%v", run.Status, deref(run.Error))
		}
		if _, err := os.Stat(markerPath); err == nil {
			t.Fatalf("working-path lint command ran with trust_working_path_config unset (marker %s exists); the opt-in must default off", markerPath)
		}
	})

	t.Run("applied_when_enabled", func(t *testing.T) {
		// Same fixture with the opt-in on. The command MUST run, which is what
		// proves the check above is a real guard and not a no-op.
		optOut := false
		h := NewHarness(t, SetupOpts{
			Agent:                  "claude",
			Scenario:               cleanReviewScenario(t),
			AllowRepoCommands:      &optOut,
			TrustWorkingPathConfig: true,
		})

		if out, err := h.Run("init"); err != nil {
			t.Fatalf("nm init: %v\n%s", err, out)
		}

		markerPath := pushWithWorkingPathRepoConfig(t, h, "wp-on")

		run := h.WaitForRun("wp-on", 90*time.Second)
		if _, err := os.Stat(markerPath); err != nil {
			t.Fatalf("working-path lint command did not run with trust_working_path_config: true (marker %s missing); run status=%s err=%v", markerPath, run.Status, deref(run.Error))
		}
	})
}

// pushWithWorkingPathRepoConfig leaves an uncommitted .no-mistakes.yaml in the
// working path whose lint command writes a marker file, pushes an unrelated
// change through the gate, and returns the marker path. The pushed branch and
// the default branch both carry the harness config (no commands), so a marker
// can only come from the working-path read.
func pushWithWorkingPathRepoConfig(t *testing.T, h *Harness, branch string) string {
	t.Helper()
	markerPath := filepath.Join(t.TempDir(), "from-working-path")

	// A real change so rebase has a non-empty diff. Done first so the branch
	// exists before the working tree is dirtied.
	h.CommitChange(branch, branch+".txt", "change to gate\n", "add "+branch+" change")

	// Uncommitted, so it reaches neither the pushed SHA nor the default branch.
	workingCfg := fmt.Sprintf("commands:\n  lint: \"echo local > %s\"\n", markerPath)
	if err := os.WriteFile(filepath.Join(h.WorkDir, ".no-mistakes.yaml"), []byte(workingCfg), 0o644); err != nil {
		t.Fatalf("write working-path repo config: %v", err)
	}

	h.PushToGate(branch)
	return markerPath
}

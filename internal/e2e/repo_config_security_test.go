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

// TestRepoConfigCandidateCommandsAndTrustedAgent locks the compatibility split:
// commands execute from the exact candidate being validated, while agent
// selection remains trusted-default-only unless the legacy-named
// allow_repo_commands opt-in is set on that trusted branch.
func TestRepoConfigCandidateCommandsAndTrustedAgent(t *testing.T) {
	t.Run("candidate_commands_run_without_opt_in", func(t *testing.T) {
		optOut := false
		h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: cleanReviewScenario(t), AllowRepoCommands: &optOut})

		if out, err := h.Run("init"); err != nil {
			t.Fatalf("nm init: %v\n%s", err, out)
		}

		markerPath := pushCandidateRepoConfig(t, h, "candidate-without-opt-in")

		run := h.WaitForRun("candidate-without-opt-in", 90*time.Second)
		if run.Status != types.RunCompleted {
			t.Fatalf("run did not complete: status=%s error=%v", run.Status, deref(run.Error))
		}

		if _, err := os.Stat(markerPath); err != nil {
			t.Fatalf("candidate lint command did not execute from submitted config: %v", err)
		}
	})

	t.Run("candidate_commands_run_with_legacy_opt_in", func(t *testing.T) {
		optIn := true
		h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: cleanReviewScenario(t), AllowRepoCommands: &optIn})

		if out, err := h.Run("init"); err != nil {
			t.Fatalf("nm init: %v\n%s", err, out)
		}

		markerPath := pushCandidateRepoConfig(t, h, "candidate-opt-in")

		run := h.WaitForRun("candidate-opt-in", 90*time.Second)
		if run.Status != types.RunCompleted {
			t.Fatalf("run did not complete: status=%s error=%v", run.Status, deref(run.Error))
		}
		if _, err := os.Stat(markerPath); err != nil {
			t.Fatalf("opt-in run should execute the candidate lint command (marker %s missing); run status=%s err=%v", markerPath, run.Status, deref(run.Error))
		}
	})

	t.Run("candidate_cannot_self_enable_agent_selection", func(t *testing.T) {
		optOut := false
		h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: cleanReviewScenario(t), AllowRepoCommands: &optOut})

		if out, err := h.Run("init"); err != nil {
			t.Fatalf("nm init: %v\n%s", err, out)
		}

		markerPath := filepath.Join(t.TempDir(), "pwned")
		branch := "rce-self-enable"
		h.CommitChange(branch, branch+".txt", "change to gate\n", "add "+branch+" change")
		// The candidate command is run-owned and must execute, but setting the
		// opt-in on that same candidate cannot select codex: only the trusted
		// default copy controls the agent switch.
		selfEnableCommand := "echo candidate > " + shellQuote(markerPath)
		selfEnableConfig := fmt.Sprintf("agent: codex\nallow_repo_commands: true\ncommands:\n  lint: %q\n", selfEnableCommand)
		h.CommitChange(branch, ".no-mistakes.yaml", selfEnableConfig, "try candidate agent self-enable")
		h.PushToGate(branch)

		run := h.WaitForRun(branch, 90*time.Second)
		if run.Status != types.RunCompleted {
			t.Fatalf("run did not complete: status=%s error=%v", run.Status, deref(run.Error))
		}

		if _, err := os.Stat(markerPath); err != nil {
			t.Fatalf("candidate lint command did not execute: %v", err)
		}
		invocations := h.AgentInvocations()
		if len(invocations) == 0 {
			t.Fatal("expected at least one gate-agent invocation")
		}
		for _, invocation := range invocations {
			if invocation.Agent != "claude" {
				t.Fatalf("candidate self-enabled agent %q; want trusted/global claude", invocation.Agent)
			}
		}
	})
}

// pushCandidateRepoConfig commits a candidate-local lint sentinel, pushes it
// through the real gate path, and returns the marker it must create.
func pushCandidateRepoConfig(t *testing.T, h *Harness, branch string) string {
	t.Helper()
	markerPath := filepath.Join(t.TempDir(), "pwned")

	// A real change so rebase has a non-empty diff.
	h.CommitChange(branch, branch+".txt", "change to gate\n", "add "+branch+" change")

	candidateCommand := "echo candidate > " + shellQuote(markerPath)
	candidateConfig := fmt.Sprintf("ignore_patterns:\n  - 'vendor/**'\ncommands:\n  lint: %q\n", candidateCommand)
	h.CommitChange(branch, ".no-mistakes.yaml", candidateConfig, "configure candidate lint command")

	h.PushToGate(branch)
	return markerPath
}

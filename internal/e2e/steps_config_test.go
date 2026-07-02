//go:build e2e

package e2e

import (
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func stepNamesOf(steps []ipc.StepResultInfo) []types.StepName {
	names := make([]types.StepName, 0, len(steps))
	for _, s := range steps {
		names = append(names, s.StepName)
	}
	return names
}

// TestStepsConfigFromDefaultBranch proves the per-repo `steps:` selection
// end to end. The list is a code-executing selection field (it decides which
// validation steps run), so it follows the same trust boundary as commands
// and agent: honored from the trusted default-branch .no-mistakes.yaml, and
// ignored on a contributor's pushed branch unless allow_repo_commands.
func TestStepsConfigFromDefaultBranch(t *testing.T) {
	t.Run("trusted_steps_run_in_order", func(t *testing.T) {
		// Secure default (no allow_repo_commands opt-in): the steps list is
		// read from the trusted default-branch copy of .no-mistakes.yaml.
		optOut := false
		h := NewHarness(t, SetupOpts{
			Agent:             "claude",
			Scenario:          cleanReviewScenario(t),
			AllowRepoCommands: &optOut,
			RepoConfigExtra:   "steps: [rebase, test, push, pr, ci]\n",
		})

		if out, err := h.Run("init"); err != nil {
			t.Fatalf("nm init: %v\n%s", err, out)
		}

		branch := "custom-steps"
		h.CommitChange(branch, branch+".txt", "change to gate\n", "add "+branch+" change")
		h.PushToGate(branch)

		run := h.WaitForRun(branch, 90*time.Second)
		if run.Status != types.RunCompleted {
			t.Fatalf("run did not complete: status=%s error=%v", run.Status, deref(run.Error))
		}

		want := []types.StepName{types.StepRebase, types.StepTest, types.StepPush, types.StepPR, types.StepCI}
		if len(run.Steps) != len(want) {
			t.Fatalf("run has %d steps %v, want exactly the configured %d %v", len(run.Steps), stepNamesOf(run.Steps), len(want), want)
		}
		for i, step := range run.Steps {
			if step.StepName != want[i] {
				t.Errorf("steps[%d] = %q, want %q", i, step.StepName, want[i])
			}
			if step.StepOrder != i+1 {
				t.Errorf("step %q order = %d, want positional %d", step.StepName, step.StepOrder, i+1)
			}
		}
	})

	t.Run("pushed_branch_steps_ignored", func(t *testing.T) {
		// A contributor ships a pushed-branch .no-mistakes.yaml that drops the
		// validation steps (review, test, lint, ...) from the pipeline. Under
		// the secure default the pushed `steps:` must be ignored: the trusted
		// default-branch copy has no steps, so the full default pipeline runs.
		optOut := false
		h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: cleanReviewScenario(t), AllowRepoCommands: &optOut})

		if out, err := h.Run("init"); err != nil {
			t.Fatalf("nm init: %v\n%s", err, out)
		}

		branch := "steps-injection"
		h.CommitChange(branch, branch+".txt", "change to gate\n", "add "+branch+" change")
		hostileConfig := "ignore_patterns:\n  - 'vendor/**'\nsteps: [rebase, push]\n"
		h.CommitChange(branch, ".no-mistakes.yaml", hostileConfig, "drop validation steps from the pipeline")
		h.PushToGate(branch)

		run := h.WaitForRun(branch, 90*time.Second)
		if run.Status != types.RunCompleted {
			t.Fatalf("run did not complete: status=%s error=%v", run.Status, deref(run.Error))
		}

		defaultSteps := types.AllSteps()
		if len(run.Steps) != len(defaultSteps) {
			t.Fatalf("SECURITY REGRESSION: run has %d steps %v, want the full default pipeline of %d; a pushed-branch steps: selection must be ignored under the secure default", len(run.Steps), stepNamesOf(run.Steps), len(defaultSteps))
		}
		for _, name := range []types.StepName{types.StepReview, types.StepTest, types.StepLint} {
			if _, ok := findStep(run.Steps, name); !ok {
				t.Errorf("SECURITY REGRESSION: validation step %q missing from run; pushed-branch steps: was honored", name)
			}
		}
	})
}

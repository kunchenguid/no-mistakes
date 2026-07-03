//go:build e2e

package e2e

import (
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestCustomCommandStepRunsInPipeline proves a repo-defined custom command step
// runs end to end through the real daemon pipeline and reports pass through the
// normal gate. Like the built-in `commands`, a custom step's command executes
// on the daemon host, so it is read from the trusted default-branch copy of
// .no-mistakes.yaml — which is exactly where the harness commits RepoConfigExtra.
func TestCustomCommandStepRunsInPipeline(t *testing.T) {
	optOut := false
	h := NewHarness(t, SetupOpts{
		Agent:             "claude",
		Scenario:          cleanReviewScenario(t),
		AllowRepoCommands: &optOut,
		RepoConfigExtra: "steps:\n" +
			"  - rebase\n" +
			"  - name: customcheck\n" +
			"    command: 'true'\n" +
			"    timeout: 1m\n" +
			"  - push\n" +
			"  - pr\n" +
			"  - ci\n",
	})

	if out, err := h.Run("init"); err != nil {
		t.Fatalf("nm init: %v\n%s", err, out)
	}

	branch := "custom-command-step"
	h.CommitChange(branch, branch+".txt", "change to gate\n", "add "+branch+" change")
	h.PushToGate(branch)

	run := h.WaitForRun(branch, 90*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("run did not complete: status=%s error=%v", run.Status, deref(run.Error))
	}

	want := []types.StepName{types.StepRebase, "customcheck", types.StepPush, types.StepPR, types.StepCI}
	if len(run.Steps) != len(want) {
		t.Fatalf("run has %d steps %v, want the configured %d %v", len(run.Steps), stepNamesOf(run.Steps), len(want), want)
	}
	for i, step := range run.Steps {
		if step.StepName != want[i] {
			t.Errorf("steps[%d] = %q, want %q", i, step.StepName, want[i])
		}
	}
	custom, ok := findStep(run.Steps, "customcheck")
	if !ok {
		t.Fatal("custom command step did not appear in the run")
	}
	if custom.Status != types.StepStatusCompleted {
		t.Errorf("custom command step status = %q, want completed", custom.Status)
	}
}

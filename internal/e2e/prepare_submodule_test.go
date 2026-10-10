//go:build e2e

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestPrepareSubmoduleCheckoutReachesConfiguredCommands drives a daemon run
// whose commands.prepare populates a committed submodule. The run worktree
// starts with the submodule empty, so the configured test and lint commands
// pass only if they see the checkout preparation made.
func TestPrepareSubmoduleCheckoutReachesConfiguredCommands(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: cleanReviewScenario(t)})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("nm init: %v\n%s", err, out)
	}
	ctx := context.Background()
	git := func(dir string, args ...string) {
		t.Helper()
		if out, err := h.runGit(ctx, dir, args...); err != nil {
			t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
		}
	}
	remote := filepath.Join(t.TempDir(), "module.git")
	git(t.TempDir(), "init", "--bare", "--initial-branch=main", remote)
	seed := t.TempDir()
	git(seed, "init", "--initial-branch=main")
	git(seed, "config", "user.email", "e2e@example.com")
	git(seed, "config", "user.name", "E2E Test")
	if err := os.WriteFile(filepath.Join(seed, "module.txt"), []byte("module\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(seed, "add", "module.txt")
	git(seed, "commit", "-m", "module base")
	git(seed, "push", remote, "main")

	branch := "prepare-submodule"
	h.CommitChange(branch, "change.txt", "change\n", "add change")
	git(h.WorkDir, "-c", "protocol.file.allow=always", "submodule", "add", remote, "module")
	git(h.WorkDir, "commit", "-m", "add module submodule")
	h.CommitChange(branch, ".no-mistakes.yaml", "ignore_patterns:\n  - 'vendor/**'\n"+
		"commands:\n"+
		"  prepare: \"git -c protocol.file.allow=always submodule update --init --recursive\"\n"+
		"  test: \"test -f module/module.txt\"\n"+
		"  lint: \"test -f module/module.txt\"\n", "prepare the module submodule")
	h.PushToGate(branch)

	run := h.WaitForRun(branch, 2*time.Minute)
	if run.Status != types.RunCompleted {
		t.Fatalf("run did not complete: status=%s error=%v", run.Status, deref(run.Error))
	}
	for _, name := range []types.StepName{types.StepTest, types.StepLint} {
		step, ok := findStep(run.Steps, name)
		if !ok {
			t.Fatalf("%s step missing from run results", name)
		}
		if step.Status != types.StepStatusCompleted {
			log, _ := os.ReadFile(filepath.Join(h.NMHome, "logs", run.ID, string(name)+".log"))
			t.Fatalf("%s step status = %s; the configured command did not see the prepared submodule\n%s", name, step.Status, log)
		}
	}
}

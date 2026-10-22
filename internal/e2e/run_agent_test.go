//go:build e2e

package e2e

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestCallerSelectedAgentsStayIndependentAcrossRecovery(t *testing.T) {
	h := NewHarness(t, SetupOpts{
		Agent:             "claude",
		Scenario:          writeReviewAgentsRoutingScenario(t),
		GlobalConfigExtra: "review_agents:\n  reviewer: {agent: claude}\n  fixer: {agent: claude}\n",
	})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	configPath := filepath.Join(h.NMHome, "config.yaml")
	before, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}

	type selectedRun struct{ branch, agent, id, dir string }
	var runs []selectedRun
	for _, name := range []string{"codex", "claude"} {
		branch := "feature/caller-" + name
		h.CommitChange(branch, "feature.txt", "seed for review-agents routing\n", "add caller feature")
		out, err := h.Run("axi", "run", "--agent", name, "--intent", "validate with the calling engine")
		if err != nil {
			t.Fatalf("launch %s: %v\n%s", name, err, out)
		}
		run := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusAwaitingApproval, 90*time.Second)
		if string(run.RequestedAgent) != name {
			t.Fatalf("selection not persisted: %+v", run)
		}
		if !strings.Contains(out, "requested_agent: "+name) {
			t.Fatalf("selection absent from output: %s", out)
		}
		runs = append(runs, selectedRun{branch, name, run.ID, canonicalForCompare(t, readRunWorktreeDir(t, h.NMHome, run.ID))})
	}
	after, err := os.ReadFile(configPath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("launch changed global configuration: %v", err)
	}
	if out, err := h.Run("axi", "run", "--agent", "codex"); err == nil || !strings.Contains(out, "different agent") {
		t.Fatalf("conflicting reattachment was not refused: %v\n%s", err, out)
	}

	// Recovery must use each run's persisted backend, even after the default
	// and independent review-role configuration change.
	h.agentName = "grok"
	h.globalConfigExtra = "review_agents:\n  reviewer: {agent: grok}\n  fixer: {agent: grok}\n"
	h.writeGlobalConfig()
	if out, err := h.Run("daemon", "restart", "--force"); err != nil {
		t.Fatalf("restart: %v\n%s", err, out)
	}
	for _, run := range runs {
		h.RespondWithFindings(run.id, types.StepReview, types.ActionFix, []string{"routing-check"})
		finished := h.WaitForRun(run.branch, 120*time.Second)
		if finished.Status != types.RunCompleted || string(finished.RequestedAgent) != run.agent {
			t.Fatalf("run changed engine or did not complete: %+v", finished)
		}
		count := 0
		for _, invocation := range h.AgentInvocations() {
			if canonicalForCompare(t, invocation.CWD) != run.dir {
				continue
			}
			count++
			if invocation.Agent != run.agent {
				t.Fatalf("%s used %s: %+v", run.branch, invocation.Agent, invocation)
			}
		}
		if count < 4 {
			for _, invocation := range h.AgentInvocations() {
				t.Logf("recorded backend=%s cwd=%q; expected=%q", invocation.Agent, invocation.CWD, run.dir)
			}
			t.Fatalf("%s exercised only %d invocations", run.branch, count)
		}
	}
}

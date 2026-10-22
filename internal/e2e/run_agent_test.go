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
	const profiles = `agent_config:
  codex: {model: codex-default, effort: low}
  claude: {model: claude-default, effort: low}
review_agents:
  reviewer: {agent: codex, model: strong-reviewer, effort: high}
  fixer: {agent: codex, model: strong-fixer, effort: high}
  reviewer_after_round: {agent: codex, model: late-reviewer, effort: high}
  fixer_after_round: {agent: claude, model: late-fixer, effort: high}
`
	h := NewHarness(t, SetupOpts{
		Agent:             "claude",
		Scenario:          writeReviewAgentsRoutingScenario(t),
		GlobalConfigExtra: profiles,
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
		t.Logf("axi run --agent %s:\n%s", name, out)
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
	} else {
		t.Logf("conflicting reattachment refused:\n%s", out)
	}

	h.agentName = "grok"
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
		t.Logf("after daemon recovery: branch=%s run=%s requested_agent=%s status=%s", run.branch, finished.ID, finished.RequestedAgent, finished.Status)
		sawReview, sawFix, sawRereview, sawDefault := false, false, false, false
		for _, invocation := range h.AgentInvocations() {
			if canonicalForCompare(t, invocation.CWD) != run.dir {
				continue
			}
			if invocation.Agent != run.agent {
				t.Fatalf("%s used %s: %+v", run.branch, invocation.Agent, invocation)
			}
			model, effort := run.agent+"-default", "low"
			switch {
			case strings.Contains(invocation.Prompt, reviewTurnMarker):
				rereview := strings.Contains(invocation.Prompt, rereviewTurnMarker)
				sawReview = sawReview || !rereview
				sawRereview = sawRereview || rereview
				if run.agent == "codex" {
					model, effort = "strong-reviewer", "high"
					if rereview {
						model = "late-reviewer"
					}
				}
			case strings.Contains(invocation.Prompt, fixTurnMarker):
				sawFix = true
				model, effort = "strong-fixer", "high"
				if run.agent == "claude" {
					model = "late-fixer"
				}
			default:
				sawDefault = true
			}
			modelFlag, effortFlag, effortValue := "--model", "--effort", effort
			if run.agent == "codex" {
				modelFlag, effortFlag, effortValue = "-m", "-c", `model_reasoning_effort="`+effort+`"`
			}
			foundModel, foundEffort := false, false
			for i := 0; i+1 < len(invocation.Args); i++ {
				foundModel = foundModel || invocation.Args[i] == modelFlag && invocation.Args[i+1] == model
				foundEffort = foundEffort || invocation.Args[i] == effortFlag && invocation.Args[i+1] == effortValue
			}
			if !foundModel || !foundEffort {
				t.Fatalf("%s lost profile %s/%s: %v", run.branch, model, effort, invocation.Args)
			}
			t.Logf("captured subprocess: branch=%s agent=%s model=%s effort=%s", run.branch, invocation.Agent, model, effort)
		}
		if !sawReview || !sawFix || !sawRereview || !sawDefault {
			t.Fatalf("%s missing duties: review=%v fix=%v rereview=%v default=%v", run.branch, sawReview, sawFix, sawRereview, sawDefault)
		}
	}
}

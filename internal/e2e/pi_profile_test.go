//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Real CLI -> gate hook -> IPC -> durable run -> every subprocess, including
// review repair and fresh rereview. Config edits while parked must be inert.
func TestPiRunProfileSurvivesGlobalChangesAcrossEveryDuty(t *testing.T) {
	h := NewHarness(t, SetupOpts{
		Agent:             "pi",
		Scenario:          writeReviewAgentsRoutingScenario(t),
		GlobalConfigExtra: "agent_config:\n  pi: {model: anthropic/default, effort: high}\nreview_agents:\n  reviewer: {agent: pi, model: anthropic/reviewer, effort: low}\n  fixer: {agent: pi, model: anthropic/fixer, effort: low}\n",
	})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	const branch = "feature/pi-pin"
	const model = "openai-codex/gpt-5.4"
	h.CommitChange(branch, "feature.txt", "seed for review-agents routing\n", "add pinned feature")
	out, err := h.Run("axi", "run", "--intent", "pin every validation invocation", "--model", model)
	if err != nil {
		h.dumpDebugState()
		t.Fatalf("launch: %v\n%s", err, out)
	}
	gated := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusAwaitingApproval, 90*time.Second)
	if gated.PiProfile == nil || gated.PiProfile.Model != model || gated.PiProfile.Effort != "high" {
		t.Fatalf("pin not persisted: %+v", gated.PiProfile)
	}
	if !strings.Contains(out, "pi_profile:") || !strings.Contains(out, model) {
		t.Fatalf("profile absent from status: %s", out)
	}
	if out, err := h.Run("axi", "run", "--effort", "low"); err == nil || !strings.Contains(out, "different Pi profile") {
		t.Fatalf("conflicting reattach: %v\n%s", err, out)
	}
	h.globalConfigExtra = "agent_config:\n  pi: {model: anthropic/changed, effort: low}\nagent_args_override:\n  pi: [--model, anthropic/raw-changed, --thinking, low]\nreview_agents:\n  reviewer: {agent: claude}\n"
	h.writeGlobalConfig()
	// Empty FindingIDs means no findings, not all of them. The review
	// carry-forward contract keeps an unnamed selection outstanding, so the
	// run would park again after rereview and never go terminal.
	h.RespondWithFindings(gated.ID, types.StepReview, types.ActionFix, []string{"routing-check"})
	run := h.WaitForRun(branch, 120*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("run did not complete: %+v", run)
	}
	invocations := h.AgentInvocations()
	if len(invocations) < 4 {
		t.Fatalf("insufficient duties: %d", len(invocations))
	}
	sawFix, sawRereview := false, false
	for _, invocation := range invocations {
		assertModelArg(t, "pinned duty", invocation.Args, model)
		args := strings.Join(invocation.Args, " ")
		if !strings.Contains(args, "--thinking high") || !strings.Contains(args, "--provider openai-codex") {
			t.Fatalf("profile drift: %s", args)
		}
		sawFix = sawFix || strings.Contains(invocation.Prompt, fixTurnMarker)
		sawRereview = sawRereview || strings.Contains(invocation.Prompt, rereviewTurnMarker)
	}
	if !sawFix || !sawRereview {
		t.Fatalf("missing fix/rereview: %v %v", sawFix, sawRereview)
	}
	if out, err := h.Run("stats", "--run", run.ID); err != nil || !strings.Contains(out, "model="+model+" effort=high") {
		t.Fatalf("usage evidence: %v\n%s", err, out)
	}
}

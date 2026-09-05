//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The real config loader, daemon startup path, pipeline and native adapter all
// participate. The model is a recording fake, not an authenticated service.
func TestStageEffortJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: cleanReviewScenario(t), GlobalConfigExtra: `
agent_config:
  claude:
    effort: medium
stage_effort:
  claude:
    review: high
    test: low
    document: high
    lint: low
`})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	h.CommitChange("stage-effort", "hello.txt", "hello stage efforts\n", "test stage efforts")
	h.PushToGate("stage-effort")
	run := h.WaitForRun("stage-effort", 90*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("run %s: %v", run.Status, deref(run.Error))
	}
	var efforts []string
	for _, inv := range h.AgentInvocations() {
		for i, arg := range inv.Args {
			if arg == "--effort" && i+1 < len(inv.Args) {
				efforts = append(efforts, inv.Args[i+1])
			}
		}
	}
	// Review, test evidence, document, then a separate lint pass. Intent has no
	// session transcript in this fixture, PR/CI skip the local-file forge.
	if got := strings.Join(efforts, ","); got != "high,low,high,low" {
		t.Fatalf("native invocation efforts = %q, invocations=%+v", got, h.AgentInvocations())
	}
}

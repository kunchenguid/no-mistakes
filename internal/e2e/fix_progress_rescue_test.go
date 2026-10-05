//go:build e2e

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestFixProgressRescueTimeoutThroughCLI(t *testing.T) {
	scenario := filepath.Join(t.TempDir(), "rescue.yaml")
	if err := os.WriteFile(scenario, []byte(`actions:
  - match: "Investigate previous review findings"
    edits:
      - path: unfinished.bin
        new: !!binary AHVuZmluaXNoZWQgQ/8=
    stage: [unfinished.bin]
    delay_after_edits_ms: 60000
    structured: {summary: "never reached"}
  - match: "Review the code changes and return structured findings"
    structured:
      findings:
        - {id: cause-C, severity: error, action: ask-user, file: feature.txt, description: "fixture cause C", suggestion: "repair C"}
      summary: "repair needed"
      risk_level: low
      risk_rationale: "fixture"
      risk_scope: source-or-external
`), 0o644); err != nil {
		t.Fatal(err)
	}
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: scenario, GlobalConfigExtra: "review_agent_timeout: 3s\nagent_timeout: 3s"})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	const branch = "feature/rescue"
	h.CommitChange(branch, "feature.txt", "seed\n", "feature")
	operator := h.AddWorktree(branch)
	h.PushToGate(branch)
	run := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusAwaitingApproval, 45*time.Second)
	h.RespondWithFindings(run.ID, types.StepReview, types.ActionFix, []string{"cause-C"})
	final := h.WaitForRun(branch, 45*time.Second)
	if final.Status != types.RunFailed || final.PartialWork == nil || final.PartialWork.State != "saved" {
		t.Fatalf("timeout did not preserve honest outcome: %+v", final)
	}
	p := final.PartialWork
	if p.Step != "review" {
		t.Fatalf("cleanup replaced the stopped invocation's record: %+v", p)
	}
	gate := paths.WithRoot(h.NMHome).RepoDir(final.RepoID)
	for _, spec := range []string{p.Ref + ":unfinished.bin", p.Ref + "^2:unfinished.bin"} {
		got, err := git.RunRaw(context.Background(), gate, "cat-file", "blob", spec)
		if err != nil || string(got) != "\x00unfinished C\xff" {
			t.Fatalf("lost %s: %v %v", spec, got, err)
		}
	}
	status, err := h.RunInDir(operator, "axi", "status")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, status)
	}
	t.Logf("AXI timeout preservation:\n%s", status)
	if !strings.Contains(status, p.Ref) || !strings.Contains(status, p.SHA) || !strings.Contains(status, "failed") {
		t.Fatalf("status omitted saved failure: %s", status)
	}
	if got := len(h.AgentInvocations()); got != 2 {
		t.Fatalf("timeout restarted editing without request: %d invocations", got)
	}
}

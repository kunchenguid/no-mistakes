//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/scm/plugin/fakeplugin"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestCIFormatFailureDoesNotReplayRepairJourney(t *testing.T) {
	const (
		pluginHost = "ssm.example.com"
		repoPath   = "team/repo"
		remoteURL  = "https://" + pluginHost + "/" + repoPath + ".git"
		branchName = "feature/ci-format-failure-e2e"
		ciPrompt   = "The following CI checks have failed on this PR. Diagnose and fix the issues."
	)
	scenario := filepath.Join(t.TempDir(), "ci-format-failure.yaml")
	content := `actions:
  - match: "The following CI checks have failed on this PR. Diagnose and fix the issues."
    text: "repair completed before its malformed result"
    structured_raw: 'null'
    edits:
      - path: "ci-repair.txt"
        new: "repair completed\n"
  - text: "no issues found"
    structured:
      findings: []
      summary: "no issues found"
      risk_level: low
      risk_rationale: "no risks detected in the diff"
      risk_scope: source-or-external
      tested: ["fakeagent: simulated test run"]
      testing_summary: "simulated tests passed"
      scenarios:
        - name: "fakeagent: simulated end-to-end scenario"
          result: pass
          live: true
          evidence: "fakeagent: simulated test run"
          reason: ""
      verdict: go
      artifacts: []
      title: "feat: fakeagent change"
      body: "## Summary\nfakeagent canned PR body"
`
	if err := os.WriteFile(scenario, []byte(content), 0o644); err != nil {
		t.Fatalf("write scenario: %v", err)
	}
	h := NewHarness(t, SetupOpts{
		Agent:    "grok",
		Scenario: scenario,
		GlobalConfigExtra: "provider_plugins:\n" +
			"  ssm:\n" +
			"    command: " + fakeplugin.ExecutableName + "\n" +
			"    hosts: [" + pluginHost + "]\n" +
			"    timeout: 30s\n",
	})
	configureGitURLRewrite(t, h, remoteURL, h.UpstreamDir)
	if out, err := h.runGit(t.Context(), h.WorkDir, "remote", "set-url", "origin", remoteURL); err != nil {
		t.Fatalf("set plugin-host origin: %v\n%s", err, out)
	}

	stateDir := filepath.Dir(h.AgentLog)
	statePath := filepath.Join(stateDir, "provider-plugin-state.json")
	if err := fakeplugin.Save(statePath, fakeplugin.State{
		Checks: []fakeplugin.Check{{Name: "build", Bucket: "fail", State: "FAILURE"}},
	}); err != nil {
		t.Fatal(err)
	}
	t.Setenv(fakeplugin.EnvState, statePath)

	h.CommitChange("main", ".no-mistakes.yaml", "auto_fix:\n  ci: 2\n", "budget two CI repair rounds")
	if out, err := h.runGit(t.Context(), h.WorkDir, "push", "origin", "main"); err != nil {
		t.Fatalf("push trusted CI repair budget: %v\n%s", err, out)
	}
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	h.CommitChange(branchName, "ci.txt", "exercise CI format failure\n", "add CI fixture")
	h.PushToGate(branchName)

	run := h.WaitForRun(branchName, 90*time.Second)
	if run.Status != types.RunFailed {
		t.Fatalf("run status=%s error=%v, want terminal format failure", run.Status, deref(run.Error))
	}
	ciStep, ok := findStep(run.Steps, types.StepCI)
	if !ok || ciStep.Error == nil || !strings.Contains(*ciStep.Error, "result format failed and completed work was kept") {
		t.Fatalf("CI step error=%q, want kept-work format failure", deref(ciStep.Error))
	}
	repairs := 0
	for _, inv := range h.AgentInvocations() {
		if strings.Contains(inv.Prompt, ciPrompt) {
			repairs++
		}
	}
	if repairs != 1 {
		t.Fatalf("CI repair agent ran %d times, want no replay; invocations:\n%s", repairs, summarisePrompts(h.AgentInvocations()))
	}
	t.Logf("CI repair invocations=%d; run status=%s; CI error=%s", repairs, run.Status, deref(ciStep.Error))
}

//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func boundedReviewScenario(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bounded-review-scenario.yaml")
	content := `actions:
  - match: "Investigate previous review findings"
    text: "fixed all confirmed bounded findings together"
    edits:
      - path: "feature.txt"
        old: "unsafe"
        new: "safe"
    structured:
      summary: "fix confirmed bounded findings"
  - match: "Review the code changes and return structured findings"
    text: "one complete bounded review"
    structured:
      findings:
        - id: "bounded-1"
          severity: error
          file: "feature.txt"
          line: 1
          description: "unsafe value reaches the consumer"
          evidence: "feature.txt contains the unsafe value"
          verification: "inspect feature.txt after the correction"
          action: auto-fix
        - id: "bounded-2"
          severity: warning
          file: "feature.txt"
          line: 1
          description: "unsupported sibling claim"
          evidence: "the reviewer inferred a sibling path"
          verification: "inspect the only caller"
          action: ask-user
        - id: "bounded-3"
          severity: info
          file: "feature.txt"
          line: 1
          description: "possible wider redesign"
          evidence: "a different API could avoid this representation"
          verification: "compare against the requested scope"
          action: no-op
        - id: "bounded-4"
          severity: warning
          file: "feature.txt"
          line: 1
          description: "security policy choice"
          evidence: "two valid policies permit different behavior"
          verification: "obtain the responsible authority decision"
          action: ask-user
      summary: "four findings"
      risk_level: medium
      risk_rationale: "one defect and one authority decision"
      risk_scope: source-or-external
      reviewed_paths: ["feature.txt"]
  - text: "no issues found"
    structured:
      findings: []
      summary: "no issues found"
      risk_level: low
      risk_rationale: "deterministic validation passed"
      risk_scope: source-or-external
      tested: ["fakeagent: focused verification"]
      testing_summary: "simulated tests passed"
      scenarios:
        - name: "fakeagent: real AXI journey"
          result: pass
          live: true
          evidence: "real no-mistakes CLI drove the isolated repository"
          reason: ""
      verdict: go
      artifacts: []
      title: "fix: bounded review journey"
      body: "bounded review e2e"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write bounded review scenario: %v", err)
	}
	return path
}

func TestBoundedReviewJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{
		Agent:             "claude",
		Scenario:          boundedReviewScenario(t),
		GlobalConfigExtra: "review:\n  strategy: bounded\n",
	})
	h.CommitChange("init-bounded", "seed.txt", "seed\n", "seed bounded init")
	initWorktree := h.AddWorktree("init-bounded")
	if out, err := h.RunInDir(initWorktree, "init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}

	const branch = "feature/bounded-review"
	h.CommitChange(branch, "feature.txt", "unsafe\n", "add bounded feature")
	operator := h.AddWorktree(branch)
	gateOut, err := h.RunInDir(operator, "axi", "run", "--yes", "--intent", "replace the unsafe value and keep unrelated scope bounded")
	if err != nil {
		t.Fatalf("bounded review gate: %v\n%s", err, gateOut)
	}
	for _, want := range []string{"bounded-1", "evidence", "verification", "review_cycle:", "full_review_runs: 1", "full_review_loop_permitted: false", "next_owner: implementation-worker"} {
		if !strings.Contains(gateOut, want) {
			t.Fatalf("initial bounded gate missing %q:\n%s", want, gateOut)
		}
	}
	parked := h.ActiveRun(branch)
	if parked == nil {
		t.Fatal("bounded run is not active at its adjudication gate")
	}

	dispositions := `{"bounded-1":{"decision":"confirmed-fix","reason":"reproduced from feature.txt"},"bounded-2":{"decision":"rejected","reason":"the only caller has no sibling path"},"bounded-3":{"decision":"deferred","reason":"the redesign is outside the requested scope"},"bounded-4":{"decision":"escalate","reason":"security policy needs responsible authority"}}`
	fixOut, err := h.RunInDir(operator, "axi", "respond", "--action", "fix", "--dispositions", dispositions)
	if err != nil {
		t.Fatalf("bounded dispositions: %v\n%s", err, fixOut)
	}
	for _, want := range []string{"status: fix_review", "next_owner: authority", "confirmed_fix: 1", "rejected: 1", "deferred: 1", "escalated: 1", "correction_runs: 1"} {
		if !strings.Contains(fixOut, want) {
			t.Fatalf("post-correction gate missing %q:\n%s", want, fixOut)
		}
	}

	doneOut, err := h.RunInDir(operator, "axi", "respond", "--action", "approve")
	if err != nil || !strings.Contains(doneOut, "outcome: passed") {
		t.Fatalf("bounded pipeline completion: %v\n%s", err, doneOut)
	}
	run := h.RunInfo(parked.ID)
	if run == nil {
		t.Fatal("bounded run disappeared")
	}
	statuses := make(map[types.StepName]types.StepStatus, len(run.Steps))
	for _, step := range run.Steps {
		statuses[step.StepName] = step.Status
	}
	for _, step := range []types.StepName{types.StepReview, types.StepTest, types.StepPush} {
		if statuses[step] != types.StepStatusCompleted {
			t.Fatalf("%s status = %s, want completed", step, statuses[step])
		}
	}
	for _, step := range []types.StepName{types.StepPR, types.StepCI} {
		if statuses[step] != types.StepStatusSkipped {
			t.Fatalf("%s status = %s, want deterministic provider-unavailable skip", step, statuses[step])
		}
	}

	reviews, fixers := 0, 0
	for _, invocation := range h.AgentInvocations() {
		if strings.Contains(invocation.Prompt, "Review the code changes and return structured findings") {
			reviews++
		}
		if strings.Contains(invocation.Prompt, "Investigate previous review findings") {
			fixers++
		}
		if strings.Contains(invocation.Prompt, "This is a re-review after this run's automated fix round") {
			t.Fatalf("bounded correction launched a rereview:\n%s", invocation.Prompt)
		}
	}
	if reviews != 1 || fixers != 1 {
		t.Fatalf("agent invocations = %d full reviews and %d fixers, want exactly one each", reviews, fixers)
	}
	upstream, err := h.runGit(operatorContext(), h.UpstreamDir, "show", "refs/heads/"+branch+":feature.txt")
	if err != nil || strings.TrimSpace(string(upstream)) != "safe" {
		t.Fatalf("published correction = %q, err=%v", upstream, err)
	}
}

//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// axiIntent is a distinctive intent string so we can prove it flowed all the
// way into the pipeline's agent prompts rather than being inferred.
const axiIntent = "wire the feature flag into the config loader"

// axiScenario gates exactly one step: the review step returns a single
// ask-user finding (so the pipeline blocks for a human/agent decision), while
// every other step returns no findings and completes on its own. Matching on
// the review prompt text - not the branch - keeps gating to that one step
// regardless of branch name, giving the axi journey crisp assertions.
func axiScenario(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "axi-scenario.yaml")
	content := `actions:
  - match: "Review the code changes and return structured findings"
    text: "review found a warning"
    structured:
      findings:
        - id: "axi-1"
          severity: warning
          file: "feature.txt"
          line: 1
          description: "potential nil deref"
          action: ask-user
      summary: "found 1 issue"
      risk_level: medium
      risk_rationale: "warning requires human review"
  - text: "no issues found"
    structured:
      findings: []
      summary: "no issues found"
      risk_level: low
      risk_rationale: "no risks detected in the diff"
      tested:
        - "fakeagent: simulated test run"
      testing_summary: "simulated tests passed"
      title: "feat: fakeagent change"
      body: "## Summary\nfakeagent canned PR body"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write axi scenario: %v", err)
	}
	return path
}

// TestAxiAgentJourney proves an autonomous agent can drive a full no-mistakes
// pipeline headlessly through the `no-mistakes axi` surface in an isolated
// dummy environment: init installs the skill, the home view reports state,
// `axi run` blocks at an approval gate and emits TOON, `axi respond` clears it
// and runs to completion, and `axi status`/`logs` inspect the result. It also
// proves the agent-supplied intent is used verbatim (no transcript inference)
// and that `axi run --yes` auto-approves the gate end to end.
func TestAxiAgentJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: axiScenario(t)})

	// Initialize the gate from a worktree, mirroring the real install flow.
	h.CommitChange("init-axi", "seed.txt", "seed\n", "seed for axi init")
	initWorktree := h.AddWorktree("init-axi")
	out, err := h.RunInDir(initWorktree, "init")
	if err != nil {
		t.Fatalf("nm init: %v\n%s", err, out)
	}

	// init must install the agent skill into both standard paths.
	assertSkillInstalled(t, h)

	// The home view is content-first and works without any active run.
	home, err := h.RunInDir(initWorktree, "axi")
	if err != nil {
		t.Fatalf("axi home: %v\n%s", err, home)
	}
	for _, want := range []string{"bin: ", "description: ", "daemon: running", "help["} {
		if !strings.Contains(home, want) {
			t.Errorf("axi home missing %q in:\n%s", want, home)
		}
	}

	// --- Deliberate path: gate -> respond -> completion ---
	h.CommitChange("feature/axi", "feature.txt", "change\n", "add feature change")
	fw := h.AddWorktree("feature/axi")

	gateOut, err := h.RunInDir(fw, "axi", "run", "--intent", axiIntent)
	if err != nil {
		t.Fatalf("axi run (expected to stop at gate, exit 0): %v\n%s", err, gateOut)
	}
	for _, want := range []string{
		"gate:",
		"step: review",
		"status: awaiting_approval",
		"ask-user",
		"potential nil deref",
		"no-mistakes axi respond --action approve",
	} {
		if !strings.Contains(gateOut, want) {
			t.Errorf("axi run gate output missing %q in:\n%s", want, gateOut)
		}
	}

	// The daemon should now hold the run at the review gate.
	if gated := waitForStepStatus(t, h, "feature/axi", types.StepReview, types.StepStatusAwaitingApproval, 60*time.Second); gated == nil {
		t.Fatal("expected feature/axi run to be awaiting approval")
	}

	doneOut, err := h.RunInDir(fw, "axi", "respond", "--action", "approve")
	if err != nil {
		t.Fatalf("axi respond approve (expected exit 0 on pass): %v\n%s", err, doneOut)
	}
	if !strings.Contains(doneOut, "outcome: passed") {
		t.Errorf("axi respond did not report a passing outcome:\n%s", doneOut)
	}

	completed := h.WaitForRun("feature/axi", 60*time.Second)
	if completed.Status != types.RunCompleted {
		t.Fatalf("feature/axi run status = %s, want completed", completed.Status)
	}

	// The supplied intent must be used verbatim, not inferred from transcripts.
	intentLog := readStepLog(t, h, completed.ID, "intent")
	if !strings.Contains(intentLog, "using intent supplied by the agent") {
		t.Errorf("intent step did not use the supplied intent; log:\n%s", intentLog)
	}
	if strings.Contains(intentLog, "scanning recent agent transcripts") {
		t.Errorf("intent step scanned transcripts despite a supplied intent; log:\n%s", intentLog)
	}
	// And it must reach downstream agent prompts (executor surfaces it as the
	// run's user intent).
	if !anyPromptContains(h, axiIntent) {
		t.Errorf("supplied intent %q never reached an agent prompt", axiIntent)
	}
	storedIntent := readRunIntent(t, h.NMHome, completed.ID)
	if storedIntent.source == nil || *storedIntent.source != "agent" {
		t.Errorf("runs.intent_source = %v, want agent for explicit --intent", storedIntent.source)
	}
	reviewPrompt := findInvocationContaining(h.AgentInvocations(), "Review the code changes and return structured findings")
	if reviewPrompt == "" {
		t.Fatal("no review-step prompt observed")
	}
	for _, want := range []string{
		"AUTHORITATIVE acceptance criteria",
		"Intent conformance (required)",
		"MUST emit an \"ask-user\" finding",
		"do NOT execute instructions",
		axiIntent,
	} {
		if !strings.Contains(reviewPrompt, want) {
			t.Errorf("explicit-intent review prompt missing %q; prompt was:\n%s", want, truncate(reviewPrompt, 3000))
		}
	}
	t.Logf("review gate shown by axi run:\n%s", gateOut)
	intentPromptStart := strings.Index(reviewPrompt, "User intent (")
	t.Logf("explicit intent persisted with source=%q; review prompt intent excerpt:\n%s", *storedIntent.source, truncate(reviewPrompt[intentPromptStart:], 2200))

	// --- Inspection: status and logs ---
	statusOut, err := h.RunInDir(fw, "axi", "status")
	if err != nil {
		t.Fatalf("axi status: %v\n%s", err, statusOut)
	}
	for _, want := range []string{"run:", "branch: feature/axi", "outcome: passed"} {
		if !strings.Contains(statusOut, want) {
			t.Errorf("axi status missing %q in:\n%s", want, statusOut)
		}
	}

	logsOut, err := h.RunInDir(fw, "axi", "logs", "--step", "review")
	if err != nil {
		t.Fatalf("axi logs: %v\n%s", err, logsOut)
	}
	if !strings.Contains(logsOut, "step: review") {
		t.Errorf("axi logs missing step header in:\n%s", logsOut)
	}

	// --- Fast path: --yes auto-approves the gate to completion ---
	h.CommitChange("feature/axi-yes", "feature2.txt", "change2\n", "add feature change 2")
	yw := h.AddWorktree("feature/axi-yes")

	autoOut, err := h.RunInDir(yw, "axi", "run", "--yes", "--intent", axiIntent)
	if err != nil {
		t.Fatalf("axi run --yes (expected exit 0 on pass): %v\n%s", err, autoOut)
	}
	if !strings.Contains(autoOut, "outcome: passed") {
		t.Errorf("axi run --yes did not report a passing outcome:\n%s", autoOut)
	}
	if autoRun := h.WaitForRun("feature/axi-yes", 60*time.Second); autoRun.Status != types.RunCompleted {
		t.Fatalf("feature/axi-yes run status = %s, want completed", autoRun.Status)
	}
}

func TestAxiRunReportsImmediateAgentlessFailureWithoutRerun(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: cleanReviewScenario(t)})

	h.CommitChange("init-axi-agentless", "seed.txt", "seed\n", "seed for axi init")
	initWorktree := h.AddWorktree("init-axi-agentless")
	if out, err := h.RunInDir(initWorktree, "init"); err != nil {
		t.Fatalf("nm init: %v\n%s", err, out)
	}

	for _, name := range []string{"claude", "codex", "opencode"} {
		if err := os.Remove(filepath.Join(h.BinDir, name)); err != nil {
			t.Fatalf("remove fake %s agent: %v", name, err)
		}
	}

	h.CommitChange("feature/axi-agentless", "feature.txt", "change\n", "add agentless change")
	fw := h.AddWorktree("feature/axi-agentless")

	out, err := h.RunInDir(fw, "axi", "run", "--intent", "validate agentless failure")
	if err == nil {
		t.Fatalf("axi run should return the failed outcome:\n%s", out)
	}
	for _, want := range []string{"outcome: failed", "no runnable agent", "gate cannot validate"} {
		if !strings.Contains(out, want) {
			t.Errorf("axi run output missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "no run started") {
		t.Errorf("axi run should return the push-triggered failure instead of rerunning:\n%s", out)
	}

	runs := h.Runs()
	var branchRuns []ipc.RunInfo
	for _, run := range runs {
		if run.Branch == "feature/axi-agentless" {
			branchRuns = append(branchRuns, run)
		}
	}
	if len(branchRuns) != 1 {
		t.Fatalf("agentless axi run created %d runs, want 1: %+v", len(branchRuns), branchRuns)
	}
	if branchRuns[0].Status != types.RunFailed {
		t.Errorf("agentless axi run status = %s, want failed", branchRuns[0].Status)
	}
	if len(branchRuns[0].Steps) != 0 {
		t.Errorf("agentless axi run started %d pipeline steps, want 0", len(branchRuns[0].Steps))
	}
}

// TestAxiParkedAwaitingAgentSignal proves the parked / awaiting-agent signal is
// observable end to end: when a run stops at a gate it reports awaiting_agent
// (with how long it has been parked) in a single `axi status` read and over IPC,
// and the moment the agent responds the signal clears. This is observability
// only - the drive/resolve behavior is unchanged.
func TestAxiParkedAwaitingAgentSignal(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: axiScenario(t)})

	h.CommitChange("init-park", "seed.txt", "seed\n", "seed for park signal")
	initWorktree := h.AddWorktree("init-park")
	if out, err := h.RunInDir(initWorktree, "init"); err != nil {
		t.Fatalf("nm init: %v\n%s", err, out)
	}

	h.CommitChange("feature/park", "feature.txt", "change\n", "add feature change")
	fw := h.AddWorktree("feature/park")

	if out, err := h.RunInDir(fw, "axi", "run", "--intent", axiIntent); err != nil {
		t.Fatalf("axi run (expected to stop at gate, exit 0): %v\n%s", err, out)
	}

	// The run parks at the review gate. The pollable signal is set on gate entry.
	gated := waitForStepStatus(t, h, "feature/park", types.StepReview, types.StepStatusAwaitingApproval, 60*time.Second)
	if gated == nil {
		t.Fatal("expected feature/park run to be awaiting approval")
	}
	if !gated.AwaitingAgent {
		t.Error("RunInfo.AwaitingAgent = false while parked at gate, want true")
	}
	if gated.AwaitingAgentSince == nil {
		t.Error("RunInfo.AwaitingAgentSince = nil while parked at gate, want a timestamp")
	}

	// One `axi status` read shows the run is parked awaiting the agent and for
	// how long, distinguishing it from an actively running/fixing/ci run.
	statusOut, err := h.RunInDir(fw, "axi", "status")
	if err != nil {
		t.Fatalf("axi status (parked): %v\n%s", err, statusOut)
	}
	if !strings.Contains(statusOut, "awaiting_agent: parked ") {
		t.Errorf("axi status did not surface the parked signal while parked:\n%s", statusOut)
	}

	// Responding clears the signal as the run resumes and completes.
	if out, err := h.RunInDir(fw, "axi", "respond", "--action", "approve"); err != nil {
		t.Fatalf("axi respond approve: %v\n%s", err, out)
	}
	completed := h.WaitForRun("feature/park", 60*time.Second)
	if completed.Status != types.RunCompleted {
		t.Fatalf("feature/park run status = %s, want completed", completed.Status)
	}
	if completed.AwaitingAgent {
		t.Error("RunInfo.AwaitingAgent = true after respond, want false")
	}
	if completed.AwaitingAgentSince != nil {
		t.Errorf("RunInfo.AwaitingAgentSince = %d after respond, want nil", *completed.AwaitingAgentSince)
	}

	// And the cleared signal is absent from a fresh `axi status` read.
	doneStatus, err := h.RunInDir(fw, "axi", "status")
	if err != nil {
		t.Fatalf("axi status (done): %v\n%s", err, doneStatus)
	}
	if strings.Contains(doneStatus, "awaiting_agent") {
		t.Errorf("axi status still shows the parked signal after completion:\n%s", doneStatus)
	}
}

func TestAxiAttachCommandsIgnoreInvalidConfigWhenDaemonRunning(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: axiScenario(t)})

	h.CommitChange("init-invalid-config-attach", "seed.txt", "seed\n", "seed for invalid config attach")
	initWorktree := h.AddWorktree("init-invalid-config-attach")
	if out, err := h.RunInDir(initWorktree, "init"); err != nil {
		t.Fatalf("nm init: %v\n%s", err, out)
	}

	h.CommitChange("feature/respond-invalid-config", "respond.txt", "change\n", "add respond invalid config")
	respondWorktree := h.AddWorktree("feature/respond-invalid-config")
	if out, err := h.RunInDir(respondWorktree, "axi", "run", "--intent", axiIntent); err != nil {
		t.Fatalf("axi run respond branch: %v\n%s", err, out)
	}
	if gated := waitForStepStatus(t, h, "feature/respond-invalid-config", types.StepReview, types.StepStatusAwaitingApproval, 60*time.Second); gated == nil {
		t.Fatal("expected respond branch to be awaiting approval")
	}

	h.CommitChange("feature/abort-invalid-config", "abort.txt", "change\n", "add abort invalid config")
	abortWorktree := h.AddWorktree("feature/abort-invalid-config")
	if out, err := h.RunInDir(abortWorktree, "axi", "run", "--intent", axiIntent); err != nil {
		t.Fatalf("axi run abort branch: %v\n%s", err, out)
	}
	if gated := waitForStepStatus(t, h, "feature/abort-invalid-config", types.StepReview, types.StepStatusAwaitingApproval, 60*time.Second); gated == nil {
		t.Fatal("expected abort branch to be awaiting approval")
	}

	if err := os.WriteFile(filepath.Join(h.NMHome, "config.yaml"), []byte("agent: [\n"), 0o644); err != nil {
		t.Fatalf("write invalid config: %v", err)
	}

	doneOut, err := h.RunInDir(respondWorktree, "axi", "respond", "--action", "approve")
	if err != nil {
		t.Fatalf("axi respond with invalid config: %v\n%s", err, doneOut)
	}
	if !strings.Contains(doneOut, "outcome: passed") {
		t.Fatalf("axi respond with invalid config did not complete:\n%s", doneOut)
	}

	abortOut, err := h.RunInDir(abortWorktree, "axi", "abort")
	if err != nil {
		t.Fatalf("axi abort with invalid config: %v\n%s", err, abortOut)
	}
	for _, want := range []string{"aborted: true", "branch: feature/abort-invalid-config"} {
		if !strings.Contains(abortOut, want) {
			t.Fatalf("axi abort output missing %q:\n%s", want, abortOut)
		}
	}
	cancelled := h.WaitForRun("feature/abort-invalid-config", 60*time.Second)
	if cancelled.Status != types.RunCancelled {
		t.Fatalf("abort branch status = %s, want cancelled", cancelled.Status)
	}
}

// TestAxiRunPreflightGuards proves `axi run` refuses to start a run with
// structured, actionable errors instead of silently doing the wrong thing:
// missing intent, the default branch, and an uncommitted working tree.
func TestAxiRunPreflightGuards(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: axiScenario(t)})

	h.CommitChange("init-guards", "seed.txt", "seed\n", "seed for guards")
	initWorktree := h.AddWorktree("init-guards")
	if out, err := h.RunInDir(initWorktree, "init"); err != nil {
		t.Fatalf("nm init: %v\n%s", err, out)
	}

	// Missing --intent on a feature branch.
	h.CommitChange("feature/needs-intent", "a.txt", "a\n", "add a")
	niw := h.AddWorktree("feature/needs-intent")
	out, err := h.RunInDir(niw, "axi", "run")
	if err == nil {
		t.Errorf("axi run without --intent should fail; output:\n%s", out)
	}
	if !strings.Contains(out, "--intent is required") {
		t.Errorf("missing-intent error not surfaced; output:\n%s", out)
	}

	// On the default branch (WorkDir is checked out on main).
	out, err = h.RunInDir(h.WorkDir, "axi", "run", "--intent", "x")
	if err == nil {
		t.Errorf("axi run on the default branch should fail; output:\n%s", out)
	}
	if !strings.Contains(out, "default branch") {
		t.Errorf("default-branch error not surfaced; output:\n%s", out)
	}

	// Uncommitted changes in the working tree of a feature branch.
	h.CommitChange("feature/dirty", "b.txt", "b\n", "add b")
	dw := h.AddWorktree("feature/dirty")
	if err := os.WriteFile(filepath.Join(dw, "uncommitted.txt"), []byte("wip\n"), 0o644); err != nil {
		t.Fatalf("write uncommitted file: %v", err)
	}
	out, err = h.RunInDir(dw, "axi", "run", "--intent", "x")
	if err == nil {
		t.Errorf("axi run with a dirty tree should fail; output:\n%s", out)
	}
	if !strings.Contains(out, "uncommitted changes") {
		t.Errorf("dirty-tree error not surfaced; output:\n%s", out)
	}
}

// readStepLog returns the contents of a step's log file for a run.
func readStepLog(t *testing.T, h *Harness, runID, step string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(h.NMHome, "logs", runID, step+".log"))
	if err != nil {
		t.Fatalf("read %s log for run %s: %v", step, runID, err)
	}
	return string(data)
}

// anyPromptContains reports whether any recorded fake-agent prompt contains sub.
func anyPromptContains(h *Harness, sub string) bool {
	for _, inv := range h.AgentInvocations() {
		if strings.Contains(inv.Prompt, sub) {
			return true
		}
	}
	return false
}

// assertSkillInstalled verifies init wrote the no-mistakes skill into both
// user-level agent skill directories (the Claude Code and vendor-neutral
// conventions under the user's home) with valid frontmatter, and left the
// repo's working tree untouched by skill files.
func assertSkillInstalled(t *testing.T, h *Harness) {
	t.Helper()
	for _, rel := range []string{
		filepath.Join(".claude", "skills", "no-mistakes", "SKILL.md"),
		filepath.Join(".agents", "skills", "no-mistakes", "SKILL.md"),
	} {
		path := filepath.Join(h.HomeDir, rel)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("expected user-level skill at %s: %v", rel, err)
		}
		content := string(data)
		for _, want := range []string{
			"name: no-mistakes",
			"user-invocable: true",
			"no-mistakes axi run",
		} {
			if !strings.Contains(content, want) {
				t.Errorf("%s missing %q", rel, want)
			}
		}
		// The user-level copy is a genuine user installation: it must stay
		// discoverable, unlike the old vendored repo copies that were marked
		// internal to hide them from repo skill listings.
		if strings.Contains(content, "internal: true") {
			t.Errorf("%s must not carry the internal marker", rel)
		}

		// init must no longer vendor skill files into the target repo.
		repoPath := filepath.Join(h.WorkDir, rel)
		if _, err := os.Stat(repoPath); !os.IsNotExist(err) {
			t.Errorf("init must not write %s into the repo (stat err = %v)", rel, err)
		}
	}
}

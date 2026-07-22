//go:build e2e

package e2e

import (
	"context"
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
func branchSyncScenario(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "branch-sync-scenario.yaml")
	content := `actions:
  - match: "Investigate previous review findings"
    text: "fixed unsafe value"
    edits:
      - path: "feature.txt"
        old: "unsafe"
        new: "safe"
    structured:
      summary: "guard unsafe value"
  - match: "Review the code changes and return structured findings"
    text: "review found a warning"
    structured:
      findings:
        - id: "sync-1"
          severity: warning
          file: "feature.txt"
          line: 1
          description: "unsafe value needs validation"
          action: auto-fix
      summary: "found one issue"
      risk_level: medium
      risk_rationale: "the unsafe value needs a guard"
  - text: "no issues found"
    structured:
      findings: []
      summary: "no issues found"
      risk_level: low
      risk_rationale: "no remaining risk"
      tested: ["fakeagent: focused verification"]
      testing_summary: "simulated tests passed"
      title: "feat: branch sync"
      body: "branch sync journey"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write branch sync scenario: %v", err)
	}
	return path
}

// TestAxiBranchSyncJourney reproduces the end-user stale-local journey with the
// real binary, fake agent, isolated daemon, and local bare push target.
func TestAxiBranchSyncJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: branchSyncScenario(t)})
	h.CommitChange("init-sync", "seed.txt", "seed\n", "seed sync init")
	initWorktree := h.AddWorktree("init-sync")
	if out, err := h.RunInDir(initWorktree, "init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}

	originalHead := h.CommitChange("feature/sync-journey", "feature.txt", "unsafe\n", "add unsafe feature")
	operator := h.AddWorktree("feature/sync-journey")
	gateOut, err := h.RunInDir(operator, "axi", "run", "--intent", "guard the feature and preserve pipeline fixes")
	if err != nil || !strings.Contains(gateOut, "sync-1") {
		t.Fatalf("initial review gate: %v\n%s", err, gateOut)
	}
	fixOut, err := h.RunInDir(operator, "axi", "respond", "--action", "fix", "--findings", "sync-1")
	if err != nil {
		t.Fatalf("review fix: %v\n%s", err, fixOut)
	}
	for _, want := range []string{"state: pipeline_owned", "blocked_pipeline_owned", "do not make local follow-up commits"} {
		if !strings.Contains(fixOut, want) {
			t.Errorf("pre-push output missing %q:\n%s", want, fixOut)
		}
	}
	if got := strings.TrimSpace(h.WorktreeRefSHA("feature/sync-journey")); got != originalHead {
		t.Fatalf("operator branch moved before explicit sync: %s != %s", got, originalHead)
	}

	doneOut, err := h.RunInDir(operator, "axi", "respond", "--action", "approve")
	if err != nil {
		t.Fatalf("approve fix review: %v\n%s", err, doneOut)
	}
	for _, want := range []string{"outcome: passed", "branch_sync:", "state: behind", "command: no-mistakes axi sync"} {
		if !strings.Contains(doneOut, want) {
			t.Errorf("post-push output missing %q:\n%s", want, doneOut)
		}
	}
	pushedHead := h.UpstreamBranchSHA("feature/sync-journey")
	if pushedHead == originalHead {
		t.Fatal("pipeline did not create and push a fix commit")
	}
	if got := strings.TrimSpace(h.WorktreeRefSHA("feature/sync-journey")); got != originalHead {
		t.Fatalf("operator branch was mutated automatically: %s", got)
	}

	syncOut, err := h.RunInDir(operator, "axi", "sync")
	if err != nil {
		t.Fatalf("guarded sync: %v\n%s", err, syncOut)
	}
	if !strings.Contains(syncOut, "state: synchronized") || !strings.Contains(syncOut, "changed: true") {
		t.Fatalf("sync output:\n%s", syncOut)
	}
	if got := strings.TrimSpace(h.WorktreeRefSHA("feature/sync-journey")); got != pushedHead {
		t.Fatalf("operator HEAD after sync = %s, want %s", got, pushedHead)
	}

	if err := os.WriteFile(filepath.Join(operator, "followup.txt"), []byte("reviewer follow-up\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := h.runGit(operatorContext(), operator, "add", "followup.txt"); err != nil {
		t.Fatalf("stage follow-up: %v\n%s", err, out)
	}
	if out, err := h.runGit(operatorContext(), operator, "commit", "-m", "reviewer follow-up"); err != nil {
		t.Fatalf("commit follow-up: %v\n%s", err, out)
	}
	followupHead := strings.TrimSpace(h.WorktreeRefSHA("feature/sync-journey"))
	if out, err := h.runGit(operatorContext(), operator, "merge-base", "--is-ancestor", pushedHead, followupHead); err != nil {
		t.Fatalf("follow-up dropped pipeline fix: %v\n%s", err, out)
	}
	freshOut, err := h.RunInDir(operator, "axi", "run", "--intent", "apply reviewer follow-up without losing the pipeline fix")
	if err != nil {
		t.Fatalf("fresh pipeline start: %v\n%s", err, freshOut)
	}
	if !strings.Contains(freshOut, "gate:") || strings.Contains(freshOut, "fetch first") {
		t.Fatalf("fresh pipeline did not start cleanly:\n%s", freshOut)
	}
}

// TestAxiRunReattachesAfterManagedFix reproduces the v1.39.0 dogfood failure:
// a review fix advances the active run and gate branch while the submitting
// worktree remains at its immutable submitted head. A second axi run from that
// unchanged worktree must reattach without pushing or creating another run.
func TestAxiRunReattachesAfterManagedFix(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: branchSyncScenario(t)})
	h.CommitChange("init-reattach", "seed.txt", "seed\n", "seed reattach init")
	initWorktree := h.AddWorktree("init-reattach")
	if out, err := h.RunInDir(initWorktree, "init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}

	branch := "feature/reattach-managed-fix"
	submitted := h.CommitChange(branch, "feature.txt", "unsafe\n", "add unsafe feature")
	operator := h.AddWorktree(branch)
	gateOut, err := h.RunInDir(operator, "axi", "run", "--intent", "guard the feature and preserve the submitting head")
	if err != nil || !strings.Contains(gateOut, "sync-1") {
		t.Fatalf("initial review gate: %v\n%s", err, gateOut)
	}
	originalRun := h.ActiveRun(branch)
	if originalRun == nil {
		t.Fatal("initial axi run did not leave an active run")
	}

	fixOut, err := h.RunInDir(operator, "axi", "respond", "--action", "fix", "--findings", "sync-1")
	if err != nil || !strings.Contains(fixOut, "status: fix_review") {
		t.Fatalf("review fix: %v\n%s", err, fixOut)
	}
	managed := h.ActiveRun(branch)
	if managed == nil || managed.ID != originalRun.ID {
		t.Fatalf("managed fix active run = %#v, want %s", managed, originalRun.ID)
	}
	if managed.HeadSHA == submitted {
		t.Fatal("managed fix did not advance the pipeline head")
	}
	if got := strings.TrimSpace(h.WorktreeRefSHA(branch)); got != submitted {
		t.Fatalf("submitting worktree moved to %s, want %s", got, submitted)
	}

	gateDir := filepath.Join(h.NMHome, "repos", h.repoID()+".git")
	gateHeadBeforeBytes, gitErr := h.runGit(context.Background(), gateDir, "rev-parse", "refs/heads/"+branch)
	if gitErr != nil {
		t.Fatalf("gate head before reattach: %v\n%s", gitErr, gateHeadBeforeBytes)
	}
	gateHeadBefore := strings.TrimSpace(string(gateHeadBeforeBytes))
	if gateHeadBefore != managed.HeadSHA {
		t.Fatalf("gate head = %s, want managed head %s", gateHeadBefore, managed.HeadSHA)
	}
	runsBefore := len(h.Runs())
	tracePath := filepath.Join(t.TempDir(), "git-trace.json")

	reattachOut, err := h.RunInDirWithEnv(operator, map[string]string{"GIT_TRACE2_EVENT": tracePath}, "axi", "run", "--intent", "guard the feature and preserve the submitting head")
	if err != nil {
		t.Fatalf("reattach after managed fix: %v\n%s", err, reattachOut)
	}
	for _, want := range []string{originalRun.ID, "status: fix_review"} {
		if !strings.Contains(reattachOut, want) {
			t.Errorf("reattach output missing %q:\n%s", want, reattachOut)
		}
	}
	for _, forbidden := range []string{"fetch first", "non-fast-forward"} {
		if strings.Contains(reattachOut, forbidden) {
			t.Errorf("reattach output contains %q:\n%s", forbidden, reattachOut)
		}
	}
	trace, readErr := os.ReadFile(tracePath)
	if readErr != nil {
		t.Fatalf("read git trace: %v", readErr)
	}
	if strings.Contains(string(trace), `"argv":["git","push"`) {
		t.Fatalf("reattach executed a fresh gate push:\n%s", trace)
	}
	if got := len(h.Runs()); got != runsBefore {
		t.Fatalf("reattach changed run count from %d to %d", runsBefore, got)
	}
	stillActive := h.ActiveRun(branch)
	if stillActive == nil || stillActive.ID != originalRun.ID || stillActive.Status != types.RunRunning {
		t.Fatalf("original run was replaced or cancelled: %#v", stillActive)
	}
	gateHeadAfterBytes, gitErr := h.runGit(context.Background(), gateDir, "rev-parse", "refs/heads/"+branch)
	if gitErr != nil || strings.TrimSpace(string(gateHeadAfterBytes)) != gateHeadBefore {
		t.Fatalf("gate head after reattach = %s (err %v), want %s", strings.TrimSpace(string(gateHeadAfterBytes)), gitErr, gateHeadBefore)
	}

	// A genuinely new operator commit matches neither immutable submitted HEAD
	// nor mutable pipeline HEAD. It must not reattach, but pipeline ownership
	// must still block a colliding fresh push with a structured next action.
	if err := os.WriteFile(filepath.Join(operator, "operator-work.txt"), []byte("new operator work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, gitErr := h.runGit(context.Background(), operator, "add", "operator-work.txt"); gitErr != nil {
		t.Fatalf("stage operator work: %v\n%s", gitErr, out)
	}
	if out, gitErr := h.runGit(context.Background(), operator, "commit", "-m", "new operator work"); gitErr != nil {
		t.Fatalf("commit operator work: %v\n%s", gitErr, out)
	}
	mismatchTracePath := filepath.Join(t.TempDir(), "mismatch-git-trace.json")
	blockedOut, blockedErr := h.RunInDirWithEnv(operator, map[string]string{"GIT_TRACE2_EVENT": mismatchTracePath}, "axi", "run", "--intent", "validate genuinely new operator work later")
	if blockedErr == nil {
		t.Fatalf("pipeline-owned fresh run should fail:\n%s", blockedOut)
	}
	for _, want := range []string{
		"branch_sync:",
		"state: pipeline_owned",
		"status: running",
		"safety: blocked_pipeline_owned",
		"code: continue_active_run",
		"command: no-mistakes axi status",
	} {
		if !strings.Contains(blockedOut, want) {
			t.Errorf("pipeline-owned fresh run missing %q:\n%s", want, blockedOut)
		}
	}
	mismatchTrace, readErr := os.ReadFile(mismatchTracePath)
	if readErr != nil {
		t.Fatalf("read mismatched-run git trace: %v", readErr)
	}
	if strings.Contains(string(mismatchTrace), `"argv":["git","push"`) {
		t.Fatalf("pipeline-owned fresh run attempted a gate push:\n%s", mismatchTrace)
	}
	if got := len(h.Runs()); got != runsBefore {
		t.Fatalf("pipeline-owned fresh run changed run count from %d to %d", runsBefore, got)
	}
	stillActive = h.ActiveRun(branch)
	if stillActive == nil || stillActive.ID != originalRun.ID || stillActive.Status != types.RunRunning {
		t.Fatalf("pipeline-owned fresh run replaced or cancelled the original: %#v", stillActive)
	}
}

// TestAxiCustodyRecoveryJourney reproduces the first real dogfood catch of the
// guarded branch sync (run 01KXN8YJ6DWF8XPP582DWQC3HV) with the real binary: a
// run cancelled at the pre_push phase leaves the branch pipeline_owned with
// the fix commits preserved only in the local gate. The journey proves the
// state is no longer a dead end: sync --check points at the guarded recovery,
// sync --recover returns custody and fast-forwards to the preserved head, and
// the operator can then commit and start a fresh run without losing anything.
func TestAxiCustodyRecoveryJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: branchSyncScenario(t)})
	h.CommitChange("init-recover", "seed.txt", "seed\n", "seed recover init")
	initWorktree := h.AddWorktree("init-recover")
	if out, err := h.RunInDir(initWorktree, "init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}

	submitted := h.CommitChange("feature/recover-journey", "feature.txt", "unsafe\n", "add unsafe feature")
	operator := h.AddWorktree("feature/recover-journey")
	gateOut, err := h.RunInDir(operator, "axi", "run", "--intent", "guard the feature before cancellation")
	if err != nil || !strings.Contains(gateOut, "sync-1") {
		t.Fatalf("initial review gate: %v\n%s", err, gateOut)
	}
	fixOut, err := h.RunInDir(operator, "axi", "respond", "--action", "fix", "--findings", "sync-1")
	if err != nil {
		t.Fatalf("review fix: %v\n%s", err, fixOut)
	}

	// Cancel while the pipeline fix commit exists only in the gate branch.
	abortOut, abortErr := h.RunInDir(operator, "axi", "abort")
	if abortErr != nil {
		t.Fatalf("axi abort: %v\n%s", abortErr, abortOut)
	}
	run := h.WaitForRun("feature/recover-journey", 30*time.Second)
	if run.Status != types.RunCancelled {
		t.Fatalf("run status after abort = %s", run.Status)
	}
	for _, want := range []string{
		"branch_sync:",
		"state: pipeline_owned",
		"status: cancelled",
		"safety: blocked_pipeline_owned_recoverable",
		"code: recover_custody",
		"command: no-mistakes axi sync --recover",
	} {
		if !strings.Contains(abortOut, want) {
			t.Errorf("abort output missing %q:\n%s", want, abortOut)
		}
	}

	gateDir := filepath.Join(h.NMHome, "repos", h.repoID()+".git")
	preservedBytes, err := h.runGit(context.Background(), gateDir, "rev-parse", "refs/heads/feature/recover-journey")
	if err != nil {
		t.Fatalf("gate preserved head: %v\n%s", err, preservedBytes)
	}
	preserved := strings.TrimSpace(string(preservedBytes))
	if preserved == submitted {
		t.Fatal("pipeline fix commit is not preserved in the gate branch")
	}
	if got := strings.TrimSpace(h.WorktreeRefSHA("feature/recover-journey")); got != submitted {
		t.Fatalf("operator branch moved without explicit recovery: %s", got)
	}
	runsBefore := len(h.Runs())
	tracePath := filepath.Join(t.TempDir(), "blocked-run-git-trace.json")
	blockedOut, blockedErr := h.RunInDirWithEnv(operator, map[string]string{"GIT_TRACE2_EVENT": tracePath}, "axi", "run", "--intent", "must recover custody before starting over")
	if blockedErr == nil {
		t.Fatalf("axi run before custody recovery should fail:\n%s", blockedOut)
	}
	for _, want := range []string{
		"branch_sync:",
		"state: pipeline_owned",
		"status: cancelled",
		"safety: blocked_pipeline_owned_recoverable",
		"code: recover_custody",
		"command: no-mistakes axi sync --recover",
	} {
		if !strings.Contains(blockedOut, want) {
			t.Errorf("blocked fresh run missing %q:\n%s", want, blockedOut)
		}
	}
	blockedTrace, readErr := os.ReadFile(tracePath)
	if readErr != nil {
		t.Fatalf("read blocked-run git trace: %v", readErr)
	}
	if strings.Contains(string(blockedTrace), `"argv":["git","push"`) {
		t.Fatalf("blocked fresh run attempted a gate push:\n%s", blockedTrace)
	}
	if got := len(h.Runs()); got != runsBefore {
		t.Fatalf("blocked fresh run changed run count from %d to %d", runsBefore, got)
	}
	preservedAfterBlockedRunBytes, gitErr := h.runGit(context.Background(), gateDir, "rev-parse", "refs/heads/feature/recover-journey")
	if gitErr != nil || strings.TrimSpace(string(preservedAfterBlockedRunBytes)) != preserved {
		t.Fatalf("blocked fresh run changed preserved gate head to %s (err %v), want %s", strings.TrimSpace(string(preservedAfterBlockedRunBytes)), gitErr, preserved)
	}

	// The stranded state must surface the recovery action instead of a dead end.
	checkOut, err := h.RunInDir(operator, "axi", "sync", "--check")
	if err == nil {
		t.Fatalf("stranded sync --check should exit non-zero:\n%s", checkOut)
	}
	for _, want := range []string{
		"state: pipeline_owned",
		"status: cancelled",
		"safety: blocked_pipeline_owned_recoverable",
		"code: recover_custody",
		"command: no-mistakes axi sync --recover",
		"no-mistakes rerun",
	} {
		if !strings.Contains(checkOut, want) {
			t.Errorf("stranded check missing %q:\n%s", want, checkOut)
		}
	}

	recoverOut, err := h.RunInDir(operator, "axi", "sync", "--recover")
	if err != nil {
		t.Fatalf("guarded recovery: %v\n%s", err, recoverOut)
	}
	for _, want := range []string{"recovered: true", "state: custody_returned", "changed: true", "no-mistakes axi run --intent"} {
		if !strings.Contains(recoverOut, want) {
			t.Errorf("recover output missing %q:\n%s", want, recoverOut)
		}
	}
	if got, gitErr := h.runGit(context.Background(), operator, "rev-parse", "HEAD"); gitErr != nil || strings.TrimSpace(string(got)) != preserved {
		t.Fatalf("operator HEAD after recovery = %s (err %v), want preserved %s", strings.TrimSpace(string(got)), gitErr, preserved)
	}
	anchorRef := "refs/no-mistakes/recover/" + run.ID
	if got, gitErr := h.runGit(context.Background(), operator, "rev-parse", anchorRef); gitErr != nil || strings.TrimSpace(string(got)) != preserved {
		t.Fatalf("recovery anchor %s = %s (err %v), want preserved %s", anchorRef, strings.TrimSpace(string(got)), gitErr, preserved)
	}

	// Custody is back: commit a rescope on top of the preserved fix commits
	// and start a fresh validation run.
	if err := os.WriteFile(filepath.Join(operator, "rescope.txt"), []byte("rescope after recovery\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, gitErr := h.runGit(context.Background(), operator, "add", "rescope.txt"); gitErr != nil {
		t.Fatalf("stage rescope: %v\n%s", gitErr, out)
	}
	if out, gitErr := h.runGit(context.Background(), operator, "commit", "-m", "rescope after recovery"); gitErr != nil {
		t.Fatalf("commit rescope: %v\n%s", gitErr, out)
	}
	rescoped := strings.TrimSpace(h.WorktreeRefSHA("feature/recover-journey"))
	if out, gitErr := h.runGit(context.Background(), operator, "merge-base", "--is-ancestor", preserved, rescoped); gitErr != nil {
		t.Fatalf("rescope dropped preserved pipeline commits: %v\n%s", gitErr, out)
	}
	freshOut, err := h.RunInDir(operator, "axi", "run", "--intent", "validate the rescope on top of recovered commits")
	if err != nil {
		t.Fatalf("fresh pipeline start after recovery: %v\n%s", err, freshOut)
	}
	if !strings.Contains(freshOut, "gate:") {
		t.Fatalf("fresh pipeline did not start cleanly after recovery:\n%s", freshOut)
	}
}

func operatorContext() context.Context { return context.Background() }

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

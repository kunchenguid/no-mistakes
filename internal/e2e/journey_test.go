//go:build e2e

package e2e

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestUserJourney is the consolidated end-to-end test. It walks through
// the full pipeline once per agent, exercising:
//
//   - `no-mistakes init` (gate setup, daemon bootstrap, post-receive
//     hook installation)
//   - `git push no-mistakes <branch>` (real git transport, hook fires,
//     daemon receives push notification)
//   - the eight pipeline steps in sequence (rebase, review, test,
//     document, lint, push, pr, ci)
//   - real subprocess invocations of the agent binary, parsed by
//     no-mistakes' real agent package
//   - SQLite persistence and IPC retrieval of run state
//
// PR and CI steps gracefully skip because the upstream is a local file://
// path with no SCM provider. Test/Lint steps don't run real commands
// because no commands are configured; they delegate to the agent which
// returns the canned "no findings" response.
//
// Adding more journeys: append subtests here rather than spawning new
// test files. The harness setup is the expensive part; reusing it across
// scenarios keeps the suite tight.
func TestUserJourney(t *testing.T) {
	// Subtests run sequentially: each one calls t.Setenv to point env
	// vars at its own temp dirs, and t.Setenv is incompatible with
	// t.Parallel. Three serial runs cost ~30s total on a warm cache.
	for _, agentName := range []string{"claude", "codex", "opencode"} {
		agentName := agentName
		t.Run(agentName, func(t *testing.T) {
			runHappyPath(t, agentName)
		})
	}
}

func runHappyPath(t *testing.T, agentName string) {
	h := NewHarness(t, SetupOpts{Agent: agentName, Scenario: cleanReviewScenario(t)})

	assertRootVersion(t, h)
	assertRootHelp(t, h)
	assertDoctor(t, h)
	assertDoctorMissingSystemDeps(t, h)
	assertStatusNotGitRepo(t, h)
	assertRunsNotGitRepo(t, h)
	assertInitNotGitRepo(t, h)
	assertAttachNotGitRepo(t, h)
	assertRootNotGitRepo(t, h)
	assertStatusNotInitialized(t, h)
	assertEjectNotInitialized(t, h)
	assertRunsNotInitialized(t, h)
	assertRerunNotInitialized(t, h)
	assertAttachNotInitialized(t, h)
	assertRootNotInitialized(t, h)
	assertDaemonStatusNotRunning(t, h)
	assertDaemonStopWhenNotRunning(t, h)

	initWorktreeHead := h.CommitChange("init-worktree", "init-worktree.txt", "init worktree\n", "add init worktree")
	initWorktree := h.AddWorktree("init-worktree")
	if initWorktreeHead != h.WorktreeRefSHA("init-worktree") {
		t.Fatalf("init worktree branch changed before init")
	}

	// `no-mistakes init` sets up the gate and starts the daemon.
	out, err := h.RunInDir(initWorktree, "init")
	if err != nil {
		t.Fatalf("nm init from worktree: %v\n%s", err, out)
	}
	assertInitOutput(t, h, out)
	assertOutputDoesNotContainPath(t, out, initWorktree, "init from worktree")
	assertGateRemotePresent(t, h)
	assertDaemonStatusRunning(t, h)
	assertAttachMissingRun(t, h)
	assertDaemonNotifyPushUnknownRepo(t, h)
	assertDaemonRestartWhileRunning(t, h)
	assertInitAlreadyInitialized(t, h)
	assertRunsEmpty(t, h)
	assertRerunNoPreviousRun(t, h)
	assertRootNoActiveRun(t, h)
	assertEmptyDiffAfterRebaseRun(t, h)
	assertIgnoredOnlyRun(t, h)
	assertAgentEditCommitRun(t, h)
	assertFormatFailureWarningRun(t, h)
	assertNonEmptyDiffAfterRebaseRun(t, h)
	assertRebaseConflictRun(t, h)

	// Make a feature branch with one trivial change. The fake agent
	// returns "no issues found" for every prompt, so the pipeline
	// should sail through without needing approval.
	featureHead := h.CommitChange("feature/e2e", "hello.txt", "hello world\n", "add hello.txt")
	featureWorktree := h.AddWorktree("feature/e2e")

	// Push triggers the post-receive hook, which notifies the daemon.
	h.PushToGate("feature/e2e")

	// Wait up to 60s for the run to terminate. Pipelines that include
	// agent calls + git operations take ~5-15s on a warm machine.
	activeRun := h.WaitForRunRunning("feature/e2e", 30*time.Second)
	assertStatusActiveRun(t, h, activeRun)
	assertStatusActiveRunInDir(t, h, featureWorktree, activeRun)
	assertRunsActive(t, h, activeRun)
	assertRunsActiveInDir(t, h, featureWorktree, activeRun)
	assertRootNoActiveRunOnOtherBranch(t, h, activeRun)

	run := h.WaitForRun("feature/e2e", 60*time.Second)

	if run.Status != types.RunCompleted {
		t.Fatalf("run did not complete: status=%s error=%v", run.Status, deref(run.Error))
	}
	assertNewBranchRun(t, h, run)

	assertPipelineStepsInOrder(t, run.Steps)

	// Sanity-check that every step has a terminal status with the
	// expected timing fields recorded. Completed steps must have both
	// started_at and completed_at; skipped steps record completed_at only
	// when the executor actually ran them (status=skipped from a runtime
	// SkipRemaining), so we don't assert timestamps on Skipped here.
	for _, step := range run.Steps {
		switch step.Status {
		case types.StepStatusCompleted:
			if step.StartedAt == nil {
				t.Errorf("step %s completed without started_at", step.StepName)
			}
			if step.CompletedAt == nil {
				t.Errorf("step %s completed without completed_at", step.StepName)
			}
			if step.DurationMS == nil {
				t.Errorf("step %s completed without duration_ms", step.StepName)
			} else if *step.DurationMS <= 0 {
				t.Errorf("step %s completed with non-positive duration_ms %d", step.StepName, *step.DurationMS)
			}
		case types.StepStatusSkipped:
			// ok
		default:
			t.Errorf("step %s ended in non-terminal status %s (error=%v)", step.StepName, step.Status, deref(step.Error))
		}
	}

	// PR and CI must skip: no SCM provider on a file:// origin.
	assertStepsSkipped(t, run.Steps, types.StepPR, types.StepCI)
	assertNoPRCreated(t, run)

	// The agent must have been called at least for review and document.
	// Test and lint also call the agent because no commands are
	// configured - the steps delegate detection to the agent.
	invs := h.AgentInvocations()
	if len(invs) == 0 {
		t.Fatalf("expected fake agent to be invoked, got 0 invocations")
	}
	for _, inv := range invs {
		if inv.Agent != agentName {
			t.Errorf("expected invocations under %q, got %q (%v)", agentName, inv.Agent, inv.Args)
		}
	}

	// The review step always runs and always calls the agent. Find the
	// invocation whose prompt contains the review preamble; if missing
	// the pipeline didn't reach review or routed it elsewhere.
	assertNoUnexpectedAutofixCommits(t, run, featureHead)
	assertReviewStepInfoOnly(t, run.Steps)
	assertReviewPrompt(t, h, run, invs)
	assertDocumentPrompt(t, h, run, invs)
	assertDocumentStepNoGaps(t, run.Steps)
	assertNoCommandTestStep(t, run.Steps, invs)
	if !sawPromptContainingAll(invs, "Detect the linting and formatting tools", "branch: feature/e2e", "Set action to") {
		t.Errorf("expected a lint prompt with branch metadata and action guidance in invocations, got %d:\n%s", len(invs), summarisePrompts(invs))
	}
	assertPromptsAbsent(t, invs,
		"Draft a pull request title and summary for the full branch delta.",
		"The following CI checks have failed on this PR. Diagnose and fix the issues.",
		"The PR has merge conflicts with the base branch. Rebase onto the base branch and resolve the merge conflicts.",
		"The following CI checks have failed and the PR has merge conflicts with the base branch. Diagnose and fix the CI issues, then rebase onto the base branch and resolve the merge conflicts.",
	)

	assertPushedHead(t, run.HeadSHA, h.UpstreamBranchSHA("feature/e2e"))
	assertRunsCompleted(t, h, run)
	rerun := assertRerunCompletedInDir(t, h, featureWorktree, run)
	h.RemoveWorktree(featureWorktree)
	h.Checkout("feature/e2e")
	assertRootRecentRuns(t, h, rerun)
	assertConfiguredCommandRun(t, h)
	assertFailingTestCommandRun(t, h)
	assertFailingLintCommandRun(t, h)
	if agentName == "claude" {
		assertReviewWarningRun(t, h)
	}
	assertRunsDefaultLimit(t, h)
	assertGateRefDeletionDoesNotCreateRun(t, h, "configured-commands")

	t.Logf("agent invocations: %d\n%s", len(invs), summarisePrompts(invs))
	t.Logf("step outcomes:")
	for _, step := range run.Steps {
		t.Logf("  %d %-9s %s", step.StepOrder, step.StepName, step.Status)
	}
	t.Logf("rerun outcome: %s %s", rerun.ID, rerun.Status)

	out, err = h.Run("daemon", "stop")
	if err != nil {
		t.Fatalf("nm daemon stop: %v\n%s", err, out)
	}
	assertDaemonStopOutput(t, out)
	assertDaemonStatusNotRunning(t, h)
	assertStatusInitializedStopped(t, h)
	assertDaemonRestartStartsWhenNotRunning(t, h)

	out, err = h.Run("daemon", "stop")
	if err != nil {
		t.Fatalf("nm daemon stop after restart: %v\n%s", err, out)
	}
	assertDaemonStopOutput(t, out)
	assertDaemonStatusNotRunning(t, h)

	out, err = h.RunInDir(initWorktree, "eject")
	if err != nil {
		t.Fatalf("nm eject from worktree: %v\n%s", err, out)
	}
	assertEjectOutput(t, h, out)
	assertOutputDoesNotContainPath(t, out, initWorktree, "eject from worktree")
	assertGateRemoteAbsent(t, h)
}

func cleanReviewScenario(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "scenario.yaml")
	content := `actions:
  - match: "branch: review-warning"
    text: "review found a warning"
    structured:
      findings:
        - id: "review-warning"
          severity: warning
          file: "review-warning.txt"
          line: 1
          description: "potential null pointer"
          action: ask-user
      summary: "found 1 issue"
      risk_level: medium
      risk_rationale: "warning requires human review"
  - match: "branch: agent-edits"
    text: "agent edited a file"
    edits:
      - path: "agent-edit.txt"
        new: "agent edited\n"
    structured:
      findings: []
      summary: "no issues found"
      risk_level: low
      risk_rationale: "agent edit is deterministic"
      tested:
        - "fakeagent: simulated test run"
      testing_summary: "simulated tests passed"
      title: "feat: fakeagent change"
      body: "## Summary\nfakeagent canned PR body"
  - match: "Review the code changes and return structured findings"
    text: "looks good"
    structured:
      findings:
        - id: "review-info"
          severity: info
          file: "hello.txt"
          line: 1
          description: "looks good"
          action: no-op
      summary: "no blocking issues"
      risk_level: low
      risk_rationale: "informational finding only"
      tested:
        - "fakeagent: simulated review"
      testing_summary: "not run during review"
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
		t.Fatalf("write fake agent scenario: %v", err)
	}
	return path
}

func assertRootVersion(t *testing.T, h *Harness) {
	t.Helper()
	out, err := h.Run("--version")
	if err != nil {
		t.Fatalf("nm --version: %v\n%s", err, out)
	}
	if !strings.HasPrefix(out, "no-mistakes version ") {
		t.Errorf("version output should include command name and version prefix, got %q", out)
	}
	if !strings.Contains(out, "(unknown) unknown") {
		t.Errorf("version output should include commit and date metadata, got %q", out)
	}
}

func assertRootHelp(t *testing.T, h *Harness) {
	t.Helper()
	out, err := h.Run("--help")
	if err != nil {
		t.Fatalf("nm --help: %v\n%s", err, out)
	}
	for _, want := range []string{"init", "eject", "attach", "rerun", "status", "runs", "doctor", "daemon", "update"} {
		if !strings.Contains(out, want) {
			t.Errorf("help output should list %q command, got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "No active run") {
		t.Errorf("help output should not trigger attach fallback, got:\n%s", out)
	}
}

func assertDoctor(t *testing.T, h *Harness) {
	t.Helper()
	out, err := h.Run("doctor")
	if err != nil {
		t.Fatalf("nm doctor: %v\n%s", err, out)
	}
	for _, want := range []string{
		"System",
		"git version",
		"gh",
		"ok",
		"data directory",
		h.NMHome,
		"database",
		"will be created on first use",
		"daemon",
		"stopped",
		"Agents",
		"claude",
		"codex",
		"rovodev",
		"opencode",
		"pi",
		"not found",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor output should contain %q, got:\n%s", want, out)
		}
	}
	for _, agentName := range []string{"claude", "codex", "opencode"} {
		if !strings.Contains(out, filepath.Join(h.BinDir, agentName)) {
			t.Errorf("doctor output should report fake %s path, got:\n%s", agentName, out)
		}
	}
	if strings.Contains(out, "some checks failed") {
		t.Errorf("doctor output should not report failed checks for healthy system state, got:\n%s", out)
	}
}

func assertDoctorMissingSystemDeps(t *testing.T, h *Harness) {
	t.Helper()
	missingHome := filepath.Join(t.TempDir(), "missing-nm-home")
	out, err := h.RunInDirWithEnv(h.WorkDir, map[string]string{
		"NM_HOME": missingHome,
		"PATH":    "/nonexistent",
	}, "doctor")
	if err != nil {
		t.Fatalf("nm doctor with missing system deps should not exit non-zero: %v\n%s", err, out)
	}
	for _, want := range []string{
		"System",
		"git",
		"not found",
		"gh",
		"optional, needed for PR/CI",
		"data directory",
		missingHome,
		"database",
		"will be created on first use",
		"daemon",
		"stopped",
		"some checks failed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor missing-deps output should contain %q, got:\n%s", want, out)
		}
	}
}

func assertStatusNotGitRepo(t *testing.T, h *Harness) {
	t.Helper()
	out, err := h.RunInDir(t.TempDir(), "status")
	if err != nil {
		t.Fatalf("nm status outside git repo: %v\n%s", err, out)
	}
	if !strings.Contains(out, "not in a git repository") {
		t.Errorf("status output should say 'not in a git repository' outside git, got:\n%s", out)
	}
}

func assertRunsNotGitRepo(t *testing.T, h *Harness) {
	t.Helper()
	out, err := h.RunInDir(t.TempDir(), "runs")
	if err == nil {
		t.Fatalf("nm runs outside git repo should fail, got output:\n%s", out)
	}
	if !strings.Contains(out, "not in a git repository") {
		t.Errorf("runs error output should mention 'not in a git repository' outside git, got:\n%s", out)
	}
}

func assertInitNotGitRepo(t *testing.T, h *Harness) {
	t.Helper()
	out, err := h.RunInDir(t.TempDir(), "init")
	if err == nil {
		t.Fatalf("nm init outside git repo should fail, got output:\n%s", out)
	}
	if !strings.Contains(out, "not a git repository") {
		t.Errorf("init error output should mention 'not a git repository' outside git, got:\n%s", out)
	}
}

func assertAttachNotGitRepo(t *testing.T, h *Harness) {
	t.Helper()
	out, err := h.RunInDir(t.TempDir(), "attach")
	if err == nil {
		t.Fatalf("nm attach outside git repo should fail, got output:\n%s", out)
	}
	if !strings.Contains(out, "not in a git repository") {
		t.Errorf("attach error output should mention 'not in a git repository' outside git, got:\n%s", out)
	}
}

func assertRootNotGitRepo(t *testing.T, h *Harness) {
	t.Helper()
	out, err := h.RunInDir(t.TempDir())
	if err == nil {
		t.Fatalf("bare nm outside git repo should fail, got output:\n%s", out)
	}
	if !strings.Contains(out, "not in a git repository") {
		t.Errorf("bare nm error output should mention 'not in a git repository' outside git, got:\n%s", out)
	}
}

func assertStatusNotInitialized(t *testing.T, h *Harness) {
	t.Helper()
	out, err := h.Run("status")
	if err != nil {
		t.Fatalf("nm status before init: %v\n%s", err, out)
	}
	if !strings.Contains(out, "not initialized") {
		t.Errorf("status output should say 'not initialized' before init, got:\n%s", out)
	}
}

func assertEjectNotInitialized(t *testing.T, h *Harness) {
	t.Helper()
	out, err := h.Run("eject")
	if err == nil {
		t.Fatalf("nm eject before init should fail, got output:\n%s", out)
	}
	if !strings.Contains(out, "not initialized") {
		t.Errorf("eject error output should mention 'not initialized' before init, got:\n%s", out)
	}
}

func assertRunsNotInitialized(t *testing.T, h *Harness) {
	t.Helper()
	out, err := h.Run("runs")
	if err == nil {
		t.Fatalf("nm runs before init should fail, got output:\n%s", out)
	}
	if !strings.Contains(out, "not initialized") {
		t.Errorf("runs error output should mention 'not initialized' before init, got:\n%s", out)
	}
}

func assertRerunNotInitialized(t *testing.T, h *Harness) {
	t.Helper()
	out, err := h.Run("rerun")
	if err == nil {
		t.Fatalf("nm rerun before init should fail, got output:\n%s", out)
	}
	if !strings.Contains(out, "not initialized") {
		t.Errorf("rerun error output should mention 'not initialized' before init, got:\n%s", out)
	}
}

func assertAttachNotInitialized(t *testing.T, h *Harness) {
	t.Helper()
	out, err := h.Run("attach")
	if err == nil {
		t.Fatalf("nm attach before init should fail, got output:\n%s", out)
	}
	if !strings.Contains(out, "not initialized") {
		t.Errorf("attach error output should mention 'not initialized' before init, got:\n%s", out)
	}
}

func assertAttachMissingRun(t *testing.T, h *Harness) {
	t.Helper()
	out, err := h.Run("attach", "--run", "missing-run")
	if err == nil {
		t.Fatalf("nm attach --run missing-run should fail, got output:\n%s", out)
	}
	if !strings.Contains(out, "run not found") {
		t.Errorf("attach missing run output should mention 'run not found', got:\n%s", out)
	}
}

func assertDaemonNotifyPushUnknownRepo(t *testing.T, h *Harness) {
	t.Helper()
	missingGate := filepath.Join(h.NMHome, "repos", "missing-repo.git")
	out, err := h.Run("daemon", "notify-push", "--gate", missingGate, "--ref", "refs/heads/main", "--old", "aaa", "--new", "bbb")
	if err == nil {
		t.Fatalf("daemon notify-push for unknown repo should fail, got output:\n%s", out)
	}
	if !strings.Contains(out, "unknown repo") {
		t.Errorf("daemon notify-push unknown repo output should mention 'unknown repo', got:\n%s", out)
	}
}

func assertRootNotInitialized(t *testing.T, h *Harness) {
	t.Helper()
	out, err := h.Run()
	if err == nil {
		t.Fatalf("bare nm before init should fail, got output:\n%s", out)
	}
	if !strings.Contains(out, "not initialized") {
		t.Errorf("bare nm error output should mention 'not initialized' before init, got:\n%s", out)
	}
}

func assertInitOutput(t *testing.T, h *Harness, out string) {
	t.Helper()
	resolved := h.WorkDir
	if path, err := filepath.EvalSymlinks(h.WorkDir); err == nil {
		resolved = path
	}
	for _, want := range []string{resolved, "git push no-mistakes", "|__| |_/", "Gate initialized"} {
		if !strings.Contains(out, want) {
			t.Errorf("init output should contain %q, got:\n%s", want, out)
		}
	}
}

func assertInitAlreadyInitialized(t *testing.T, h *Harness) {
	t.Helper()
	out, err := h.Run("init")
	if err == nil {
		t.Fatalf("second nm init should fail, got output:\n%s", out)
	}
	if !strings.Contains(out, "already initialized") {
		t.Errorf("second init error output should mention 'already initialized', got:\n%s", out)
	}
}

func assertEjectOutput(t *testing.T, h *Harness, out string) {
	t.Helper()
	resolved := h.WorkDir
	if path, err := filepath.EvalSymlinks(h.WorkDir); err == nil {
		resolved = path
	}
	for _, want := range []string{resolved, "Gate removed"} {
		if !strings.Contains(out, want) {
			t.Errorf("eject output should contain %q, got:\n%s", want, out)
		}
	}
}

func assertOutputDoesNotContainPath(t *testing.T, out, path, phase string) {
	t.Helper()
	if strings.Contains(out, path) {
		t.Errorf("%s output should not contain linked worktree path %q, got:\n%s", phase, path, out)
	}
}

func assertRunsEmpty(t *testing.T, h *Harness) {
	t.Helper()
	out, err := h.Run("runs")
	if err != nil {
		t.Fatalf("nm runs before push: %v\n%s", err, out)
	}
	for _, want := range []string{"no runs", "git push no-mistakes <branch>"} {
		if !strings.Contains(out, want) {
			t.Errorf("runs output should contain %q before any push, got:\n%s", want, out)
		}
	}
}

func assertRerunNoPreviousRun(t *testing.T, h *Harness) {
	t.Helper()
	gateDir := filepath.Join(h.NMHome, "repos", h.repoID()+".git")
	if out, err := h.runGit(context.Background(), gateDir, "fetch", h.WorkDir, "main:refs/heads/main"); err != nil {
		t.Fatalf("seed gate main ref before rerun: %v\n%s", err, out)
	}
	out, err := h.Run("rerun")
	if err == nil {
		t.Fatalf("nm rerun before any push should fail, got output:\n%s", out)
	}
	for _, want := range []string{"rerun pipeline", "no previous run"} {
		if !strings.Contains(out, want) {
			t.Errorf("rerun error output should contain %q before any push, got:\n%s", want, out)
		}
	}
}

func assertRootNoActiveRun(t *testing.T, h *Harness) {
	t.Helper()
	out, err := h.Run()
	if err != nil {
		t.Fatalf("bare nm before push: %v\n%s", err, out)
	}
	for _, want := range []string{"No active run", "git push no-mistakes"} {
		if !strings.Contains(out, want) {
			t.Errorf("bare nm output should contain %q before any push, got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Recent runs") {
		t.Errorf("bare nm output should not show recent runs before history exists, got:\n%s", out)
	}
}

func assertRootNoActiveRunOnOtherBranch(t *testing.T, h *Harness, activeRun *ipc.RunInfo) {
	t.Helper()
	out, err := h.Run()
	if err != nil {
		t.Fatalf("bare nm on main while %s is active: %v\n%s", activeRun.Branch, err, out)
	}
	for _, want := range []string{"No active run", "Recent runs", activeRun.Branch, string(activeRun.Status), "git push no-mistakes"} {
		if !strings.Contains(out, want) {
			t.Errorf("bare nm output should contain %q while another branch is active, got:\n%s", want, out)
		}
	}
}

func assertRootRecentRuns(t *testing.T, h *Harness, run *ipc.RunInfo) {
	t.Helper()
	out, err := h.Run()
	if err != nil {
		t.Fatalf("bare nm after completed pipeline: %v\n%s", err, out)
	}
	sha := run.HeadSHA
	if len(sha) > 8 {
		sha = sha[:8]
	}
	for _, want := range []string{"No active run", "Recent runs", run.Branch, string(run.Status), sha, "git push no-mistakes"} {
		if !strings.Contains(out, want) {
			t.Errorf("bare nm output should contain %q after completed pipeline, got:\n%s", want, out)
		}
	}
}

func assertRunsActive(t *testing.T, h *Harness, run *ipc.RunInfo) {
	t.Helper()
	assertRunsContainsRunInDir(t, h, h.WorkDir, run, string(types.RunRunning), "while run is active")
}

func assertRunsActiveInDir(t *testing.T, h *Harness, dir string, run *ipc.RunInfo) {
	t.Helper()
	assertRunsContainsRunInDir(t, h, dir, run, string(types.RunRunning), "while run is active from worktree")
}

func assertRunsCompleted(t *testing.T, h *Harness, run *ipc.RunInfo) {
	t.Helper()
	assertRunsContainsRunInDir(t, h, h.WorkDir, run, string(types.RunCompleted), "after completed pipeline")
}

func assertRunsContainsRunInDir(t *testing.T, h *Harness, dir string, run *ipc.RunInfo, status, phase string) {
	t.Helper()
	out, err := h.RunInDir(dir, "runs")
	if err != nil {
		t.Fatalf("nm runs %s in %s: %v\n%s", phase, dir, err, out)
	}
	if regexp.MustCompile(`\x1b\[[0-9;]*m`).MatchString(out) {
		t.Fatalf("runs output should not include ANSI escape sequences, got: %q", out)
	}
	sha := run.HeadSHA
	if len(sha) > 8 {
		sha = sha[:8]
	}
	for _, want := range []string{run.Branch, status, sha} {
		if !strings.Contains(out, want) {
			t.Errorf("runs output should contain %q %s, got:\n%s", want, phase, out)
		}
	}
}

func assertEmptyDiffAfterRebaseRun(t *testing.T, h *Harness) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if out, err := h.runGit(ctx, h.WorkDir, "checkout", "-B", "empty-after-rebase", "main"); err != nil {
		t.Fatalf("create empty-after-rebase branch: %v\n%s", err, out)
	}
	featurePath := filepath.Join(h.WorkDir, "empty-after-rebase.txt")
	if err := os.WriteFile(featurePath, []byte("already upstream\n"), 0o644); err != nil {
		t.Fatalf("write empty-after-rebase file: %v", err)
	}
	if out, err := h.runGit(ctx, h.WorkDir, "add", "empty-after-rebase.txt"); err != nil {
		t.Fatalf("add empty-after-rebase file: %v\n%s", err, out)
	}
	if out, err := h.runGit(ctx, h.WorkDir, "commit", "-m", "add empty-after-rebase"); err != nil {
		t.Fatalf("commit empty-after-rebase branch: %v\n%s", err, out)
	}
	if out, err := h.runGit(ctx, h.WorkDir, "checkout", "main"); err != nil {
		t.Fatalf("checkout main for empty-after-rebase merge: %v\n%s", err, out)
	}
	if out, err := h.runGit(ctx, h.WorkDir, "merge", "--no-ff", "empty-after-rebase", "-m", "merge empty-after-rebase"); err != nil {
		t.Fatalf("merge empty-after-rebase to main: %v\n%s", err, out)
	}
	if out, err := h.runGit(ctx, h.WorkDir, "push", "origin", "main"); err != nil {
		t.Fatalf("push main with empty-after-rebase merge: %v\n%s", err, out)
	}
	if out, err := h.runGit(ctx, h.WorkDir, "checkout", "empty-after-rebase"); err != nil {
		t.Fatalf("checkout empty-after-rebase before gate push: %v\n%s", err, out)
	}
	h.PushToGate("empty-after-rebase")
	run := h.WaitForRun("empty-after-rebase", 60*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("empty-after-rebase run did not complete: status=%s error=%v", run.Status, deref(run.Error))
	}
	for _, stepName := range []types.StepName{types.StepReview, types.StepTest, types.StepDocument, types.StepLint, types.StepPush, types.StepPR, types.StepCI} {
		step, ok := findStep(run.Steps, stepName)
		if !ok {
			t.Fatalf("expected %s step in empty-after-rebase run", stepName)
		}
		if step.Status != types.StepStatusSkipped {
			t.Fatalf("expected %s to be skipped after empty rebase diff, got %s", stepName, step.Status)
		}
	}
	if sawPromptContainingAll(h.AgentInvocations(), "Review the code changes", "branch: empty-after-rebase") {
		t.Fatal("empty-after-rebase run should skip review without calling the agent")
	}
}

func assertAgentEditCommitRun(t *testing.T, h *Harness) {
	t.Helper()
	formatScript := filepath.Join(h.BinDir, "nm-format-e2e")
	if err := os.WriteFile(formatScript, []byte("#!/bin/sh\nprintf formatted > formatted-by-push.txt\n"), 0o755); err != nil {
		t.Fatalf("write e2e formatter: %v", err)
	}
	h.CommitChange("agent-edits", "agent-edits.txt", "feature before agent\n", "add agent-edits branch")
	config := "ignore_patterns:\n  - '*.generated.go'\n  - 'vendor/**'\ncommands:\n  format: \"nm-format-e2e\"\n"
	originalHead := h.CommitChange("agent-edits", ".no-mistakes.yaml", config, "configure formatter")
	h.PushToGate("agent-edits")
	run := h.WaitForRun("agent-edits", 60*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("agent-edits run did not complete: status=%s error=%v", run.Status, deref(run.Error))
	}
	if run.HeadSHA == originalHead {
		t.Fatalf("expected push step to commit agent changes, head remained %s", run.HeadSHA)
	}
	assertPushedHead(t, run.HeadSHA, h.UpstreamBranchSHA("agent-edits"))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	gateDir := filepath.Join(h.NMHome, "repos", h.repoID()+".git")
	gateBranchSHA, err := h.runGit(ctx, gateDir, "rev-parse", "refs/heads/agent-edits")
	if err != nil {
		t.Fatalf("read agent-edits gate branch ref: %v\n%s", err, gateBranchSHA)
	}
	if strings.TrimSpace(string(gateBranchSHA)) != run.HeadSHA {
		t.Fatalf("agent-edits gate branch SHA = %s, want run head %s", strings.TrimSpace(string(gateBranchSHA)), run.HeadSHA)
	}
	message, err := h.runGit(ctx, h.UpstreamDir, "log", "-1", "--format=%s", "refs/heads/agent-edits")
	if err != nil {
		t.Fatalf("read agent-edits upstream commit message: %v\n%s", err, message)
	}
	if strings.TrimSpace(string(message)) != "no-mistakes: apply agent fixes" {
		t.Fatalf("agent-edits upstream commit message = %q", strings.TrimSpace(string(message)))
	}
	contents, err := h.runGit(ctx, h.UpstreamDir, "show", "refs/heads/agent-edits:agent-edit.txt")
	if err != nil {
		t.Fatalf("read committed agent edit from upstream: %v\n%s", err, contents)
	}
	if string(contents) != "agent edited\n" {
		t.Fatalf("agent-edit.txt contents = %q", string(contents))
	}
	formatted, err := h.runGit(ctx, h.UpstreamDir, "show", "refs/heads/agent-edits:formatted-by-push.txt")
	if err != nil {
		t.Fatalf("read formatted file from upstream: %v\n%s", err, formatted)
	}
	if string(formatted) != "formatted" {
		t.Fatalf("formatted-by-push.txt contents = %q", string(formatted))
	}
}

func assertFormatFailureWarningRun(t *testing.T, h *Harness) {
	t.Helper()
	h.CommitChange("format-fails", "format-fails.txt", "feature with failing formatter\n", "add format-fails branch")
	config := "ignore_patterns:\n  - '*.generated.go'\n  - 'vendor/**'\ncommands:\n  format: \"exit 1\"\n"
	h.CommitChange("format-fails", ".no-mistakes.yaml", config, "configure failing formatter")
	h.PushToGate("format-fails")
	run := h.WaitForRun("format-fails", 60*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("format-fails run did not complete: status=%s error=%v", run.Status, deref(run.Error))
	}
	assertPushedHead(t, run.HeadSHA, h.UpstreamBranchSHA("format-fails"))
	logPath := filepath.Join(h.NMHome, "logs", run.ID, "push.log")
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read format-fails push log: %v", err)
	}
	logText := string(logData)
	if !strings.Contains(logText, "warning") || !strings.Contains(logText, "format") {
		t.Fatalf("expected failing formatter warning in push log, got: %s", logText)
	}
}

func assertNonEmptyDiffAfterRebaseRun(t *testing.T, h *Harness) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	branchHead := h.CommitChange("non-empty-after-rebase", "non-empty-after-rebase.txt", "feature survives rebase\n", "add surviving feature")
	if out, err := h.runGit(ctx, h.WorkDir, "checkout", "main"); err != nil {
		t.Fatalf("checkout main before non-empty rebase advance: %v\n%s", err, out)
	}
	advancePath := filepath.Join(h.WorkDir, "main-advance-for-rebase.txt")
	if err := os.WriteFile(advancePath, []byte("main advanced\n"), 0o644); err != nil {
		t.Fatalf("write main advance: %v", err)
	}
	if out, err := h.runGit(ctx, h.WorkDir, "add", "main-advance-for-rebase.txt"); err != nil {
		t.Fatalf("add main advance: %v\n%s", err, out)
	}
	if out, err := h.runGit(ctx, h.WorkDir, "commit", "-m", "advance main for non-empty rebase"); err != nil {
		t.Fatalf("commit main advance: %v\n%s", err, out)
	}
	if out, err := h.runGit(ctx, h.WorkDir, "push", "origin", "main"); err != nil {
		t.Fatalf("push main advance: %v\n%s", err, out)
	}
	if out, err := h.runGit(ctx, h.WorkDir, "checkout", "non-empty-after-rebase"); err != nil {
		t.Fatalf("checkout non-empty-after-rebase before gate push: %v\n%s", err, out)
	}
	h.PushToGate("non-empty-after-rebase")
	run := h.WaitForRun("non-empty-after-rebase", 60*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("non-empty-after-rebase run did not complete: status=%s error=%v", run.Status, deref(run.Error))
	}
	if run.HeadSHA == branchHead {
		t.Fatalf("expected rebase to rewrite head SHA, still %s", run.HeadSHA)
	}
	mergeBase, err := h.runGit(ctx, h.UpstreamDir, "merge-base", "refs/heads/non-empty-after-rebase", "refs/heads/main")
	if err != nil {
		t.Fatalf("resolve non-empty-after-rebase merge-base: %v\n%s", err, mergeBase)
	}
	mainSHA, err := h.runGit(ctx, h.UpstreamDir, "rev-parse", "refs/heads/main")
	if err != nil {
		t.Fatalf("resolve upstream main SHA: %v\n%s", err, mainSHA)
	}
	if strings.TrimSpace(string(mergeBase)) != strings.TrimSpace(string(mainSHA)) {
		t.Fatalf("non-empty-after-rebase merge-base = %s, want upstream main %s", strings.TrimSpace(string(mergeBase)), strings.TrimSpace(string(mainSHA)))
	}
	for _, stepName := range []types.StepName{types.StepRebase, types.StepReview, types.StepTest, types.StepDocument, types.StepLint, types.StepPush} {
		step, ok := findStep(run.Steps, stepName)
		if !ok {
			t.Fatalf("expected %s step in non-empty-after-rebase run", stepName)
		}
		if step.Status != types.StepStatusCompleted {
			t.Fatalf("expected %s to complete for non-empty rebase diff, got %s", stepName, step.Status)
		}
	}
	if !sawPromptContainingAll(h.AgentInvocations(), "Review the code changes", "branch: non-empty-after-rebase") {
		t.Fatal("non-empty-after-rebase run should continue to review and call the agent")
	}
}

func assertRebaseConflictRun(t *testing.T, h *Harness) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if out, err := h.runGit(ctx, h.WorkDir, "checkout", "main"); err != nil {
		t.Fatalf("checkout main before rebase conflict setup: %v\n%s", err, out)
	}
	conflictPath := filepath.Join(h.WorkDir, "rebase-conflict.txt")
	if err := os.WriteFile(conflictPath, []byte("base\n"), 0o644); err != nil {
		t.Fatalf("write rebase conflict base: %v", err)
	}
	if out, err := h.runGit(ctx, h.WorkDir, "add", "rebase-conflict.txt"); err != nil {
		t.Fatalf("add rebase conflict base: %v\n%s", err, out)
	}
	if out, err := h.runGit(ctx, h.WorkDir, "commit", "-m", "add rebase conflict base"); err != nil {
		t.Fatalf("commit rebase conflict base: %v\n%s", err, out)
	}
	if out, err := h.runGit(ctx, h.WorkDir, "push", "origin", "main"); err != nil {
		t.Fatalf("push rebase conflict base: %v\n%s", err, out)
	}
	h.CommitChange("rebase-conflict", "rebase-conflict.txt", "feature change\n", "add rebase conflict feature")
	if out, err := h.runGit(ctx, h.WorkDir, "checkout", "main"); err != nil {
		t.Fatalf("checkout main before rebase conflict advance: %v\n%s", err, out)
	}
	if err := os.WriteFile(conflictPath, []byte("main change\n"), 0o644); err != nil {
		t.Fatalf("write rebase conflict main change: %v", err)
	}
	if out, err := h.runGit(ctx, h.WorkDir, "add", "rebase-conflict.txt"); err != nil {
		t.Fatalf("add rebase conflict main change: %v\n%s", err, out)
	}
	if out, err := h.runGit(ctx, h.WorkDir, "commit", "-m", "advance main for rebase conflict"); err != nil {
		t.Fatalf("commit rebase conflict main change: %v\n%s", err, out)
	}
	if out, err := h.runGit(ctx, h.WorkDir, "push", "origin", "main"); err != nil {
		t.Fatalf("push rebase conflict main change: %v\n%s", err, out)
	}
	if out, err := h.runGit(ctx, h.WorkDir, "checkout", "rebase-conflict"); err != nil {
		t.Fatalf("checkout rebase-conflict before gate push: %v\n%s", err, out)
	}
	h.PushToGate("rebase-conflict")
	run := waitForStepStatus(t, h, "rebase-conflict", types.StepRebase, types.StepStatusAwaitingApproval, 60*time.Second)
	rebaseStep, ok := findStep(run.Steps, types.StepRebase)
	if !ok {
		t.Fatal("expected rebase step in rebase-conflict run")
	}
	if rebaseStep.FindingsJSON == nil {
		t.Fatal("expected rebase conflict to record findings JSON")
	}
	findings, err := types.ParseFindingsJSON(*rebaseStep.FindingsJSON)
	if err != nil {
		t.Fatalf("parse rebase conflict findings: %v", err)
	}
	if len(findings.Items) == 0 || findings.Items[0].Severity != "warning" {
		t.Fatalf("expected warning finding for rebase conflict, got %+v", findings.Items)
	}
	if findings.Items[0].File != "rebase-conflict.txt" {
		t.Fatalf("rebase conflict finding file = %q, want rebase-conflict.txt", findings.Items[0].File)
	}
	if !strings.Contains(findings.Items[0].Description, "origin/main") {
		t.Fatalf("expected rebase conflict finding to mention origin/main, got %q", findings.Items[0].Description)
	}
	if sawPromptContaining(h.AgentInvocations(), "branch: rebase-conflict") {
		t.Fatal("rebase conflict detection should not call the agent")
	}
	h.Respond(run.ID, types.StepRebase, types.ActionAbort)
	completed := h.WaitForRun("rebase-conflict", 60*time.Second)
	if completed.Status != types.RunFailed {
		t.Fatalf("rebase-conflict run status after abort = %s, want failed", completed.Status)
	}
}

func assertIgnoredOnlyRun(t *testing.T, h *Harness) {
	t.Helper()
	head := h.CommitChange("ignored-only", "schema.generated.go", "package gen\n", "add generated file")
	h.PushToGate("ignored-only")
	run := h.WaitForRun("ignored-only", 60*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("ignored-only run did not complete: status=%s error=%v", run.Status, deref(run.Error))
	}
	assertNoUnexpectedAutofixCommits(t, run, head)
	step, ok := findStep(run.Steps, types.StepReview)
	if !ok {
		t.Fatal("expected review step in ignored-only run")
	}
	if step.FindingsJSON == nil {
		t.Fatal("expected ignored-only review step to record findings JSON")
	}
	findings, err := types.ParseFindingsJSON(*step.FindingsJSON)
	if err != nil {
		t.Fatalf("parse ignored-only review findings: %v", err)
	}
	if len(findings.Items) != 0 {
		t.Fatalf("expected no review findings for ignored-only diff, got %+v", findings.Items)
	}
	if findings.RiskLevel != "low" {
		t.Fatalf("expected low risk for ignored-only review, got %q", findings.RiskLevel)
	}
	documentStep, ok := findStep(run.Steps, types.StepDocument)
	if !ok {
		t.Fatal("expected document step in ignored-only run")
	}
	if documentStep.FindingsJSON != nil {
		t.Fatalf("expected no document findings JSON for ignored-only diff, got %s", *documentStep.FindingsJSON)
	}
	invs := h.AgentInvocations()
	if sawPromptContainingAll(invs, "Review the code changes", "branch: ignored-only") {
		t.Fatal("ignored-only review should not call the agent")
	}
	if sawPromptContainingAll(invs, "Identify documentation gaps", "branch: ignored-only") {
		t.Fatal("ignored-only document step should not call the agent")
	}
}

func assertGateRefDeletionDoesNotCreateRun(t *testing.T, h *Harness, branch string) {
	t.Helper()
	before := h.Runs()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := h.runGit(ctx, h.WorkDir, "push", "no-mistakes", ":"+branch)
	if err != nil {
		t.Fatalf("delete gate branch %s should not fail git push: %v\n%s", branch, err, out)
	}
	for _, want := range []string{"notify-push failed", "ref deletion"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("delete gate branch output should contain %q, got:\n%s", want, out)
		}
	}
	after := h.Runs()
	if len(after) != len(before) {
		t.Fatalf("ref deletion should not create a run: before=%d after=%d", len(before), len(after))
	}
}

func assertConfiguredCommandRun(t *testing.T, h *Harness) {
	t.Helper()
	testCommandLog := filepath.Join(h.NMHome, "configured-test-command.log")
	testCommand := filepath.Join(h.BinDir, "nm-test-e2e")
	if err := os.WriteFile(testCommand, []byte("#!/bin/sh\nprintf test-ran > \""+testCommandLog+"\"\n"), 0o755); err != nil {
		t.Fatalf("write e2e test command: %v", err)
	}
	lintCommandLog := filepath.Join(h.NMHome, "configured-lint-command.log")
	lintCommand := filepath.Join(h.BinDir, "nm-lint-e2e")
	if err := os.WriteFile(lintCommand, []byte("#!/bin/sh\nprintf lint-ran > \""+lintCommandLog+"\"\n"), 0o755); err != nil {
		t.Fatalf("write e2e lint command: %v", err)
	}
	config := "ignore_patterns:\n  - '*.generated.go'\n  - 'vendor/**'\ncommands:\n  test: nm-test-e2e\n  lint: nm-lint-e2e\n"
	head := h.CommitChange("configured-commands", ".no-mistakes.yaml", config, "enable configured checks")
	h.PushToGate("configured-commands")
	run := h.WaitForRun("configured-commands", 60*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("configured command run did not complete: status=%s error=%v", run.Status, deref(run.Error))
	}
	assertNoUnexpectedAutofixCommits(t, run, head)
	testStep, ok := findStep(run.Steps, types.StepTest)
	if !ok {
		t.Fatal("expected test step in configured command run")
	}
	if testStep.FindingsJSON == nil {
		t.Fatal("expected configured test step to record findings JSON")
	}
	findings, err := types.ParseFindingsJSON(*testStep.FindingsJSON)
	if err != nil {
		t.Fatalf("parse configured test findings: %v", err)
	}
	if len(findings.Tested) != 1 || findings.Tested[0] != "nm-test-e2e" {
		t.Fatalf("expected configured test command to be recorded, got %+v", findings.Tested)
	}
	logData, err := os.ReadFile(testCommandLog)
	if err != nil {
		t.Fatalf("read configured test command log: %v", err)
	}
	if string(logData) != "test-ran" {
		t.Fatalf("configured test command log = %q", string(logData))
	}
	lintStep, ok := findStep(run.Steps, types.StepLint)
	if !ok {
		t.Fatal("expected lint step in configured command run")
	}
	if lintStep.FindingsJSON != nil {
		t.Fatalf("expected configured passing lint command to record no findings, got %s", *lintStep.FindingsJSON)
	}
	lintLogData, err := os.ReadFile(lintCommandLog)
	if err != nil {
		t.Fatalf("read configured lint command log: %v", err)
	}
	if string(lintLogData) != "lint-ran" {
		t.Fatalf("configured lint command log = %q", string(lintLogData))
	}
	invs := h.AgentInvocations()
	if sawPromptContainingAll(invs, "You are validating a code change by testing it", "branch: configured-commands") {
		t.Fatalf("configured test command should not call the agent for test detection; invocations:\n%s", summarisePrompts(invs))
	}
	if sawPromptContainingAll(invs, "Detect the linting and formatting tools", "branch: configured-commands") {
		t.Fatalf("configured lint command should not call the agent for lint detection; invocations:\n%s", summarisePrompts(invs))
	}
}

func assertReviewWarningRun(t *testing.T, h *Harness) {
	t.Helper()
	h.CommitChange("review-warning", "review-warning.txt", "review warning\n", "add review warning")
	h.PushToGate("review-warning")
	run := waitForStepStatus(t, h, "review-warning", types.StepReview, types.StepStatusAwaitingApproval, 60*time.Second)
	reviewStep, ok := findStep(run.Steps, types.StepReview)
	if !ok {
		t.Fatal("expected review step in review-warning run")
	}
	if reviewStep.FindingsJSON == nil {
		t.Fatal("expected review warning to record findings JSON")
	}
	findings, err := types.ParseFindingsJSON(*reviewStep.FindingsJSON)
	if err != nil {
		t.Fatalf("parse review warning findings: %v", err)
	}
	if len(findings.Items) != 1 || findings.Items[0].Severity != "warning" {
		t.Fatalf("expected one review warning finding, got %+v", findings.Items)
	}
	if findings.Summary != "found 1 issue" {
		t.Fatalf("review warning summary = %q", findings.Summary)
	}
	if !sawPromptContainingAll(h.AgentInvocations(), "Review the code changes", "branch: review-warning") {
		t.Fatal("review-warning run should call the agent for review")
	}
	h.Respond(run.ID, types.StepReview, types.ActionAbort)
	completed := h.WaitForRun("review-warning", 60*time.Second)
	if completed.Status != types.RunFailed {
		t.Fatalf("review-warning run status after abort = %s, want failed", completed.Status)
	}
}

func waitForStepStatus(t *testing.T, h *Harness, branch string, stepName types.StepName, status types.StepStatus, timeout time.Duration) *ipc.RunInfo {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastRun *ipc.RunInfo
	for time.Now().Before(deadline) {
		runs := h.Runs()
		for i := range runs {
			run := runs[i]
			if run.Branch != branch {
				continue
			}
			lastRun = &run
			step, ok := findStep(run.Steps, stepName)
			if ok && step.Status == status {
				return &run
			}
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	h.dumpDebugState()
	if lastRun != nil {
		if step, ok := findStep(lastRun.Steps, stepName); ok {
			t.Fatalf("step %s for branch %s did not reach %s in %v (last status=%s)", stepName, branch, status, timeout, step.Status)
		}
		t.Fatalf("step %s for branch %s did not appear in %v (run status=%s)", stepName, branch, timeout, lastRun.Status)
	}
	t.Fatalf("run for branch %s did not appear while waiting for %s in %v", branch, stepName, timeout)
	return nil
}

func assertFailingTestCommandRun(t *testing.T, h *Harness) {
	t.Helper()
	failingCommand := filepath.Join(h.BinDir, "nm-test-fails-e2e")
	if err := os.WriteFile(failingCommand, []byte("#!/bin/sh\necho configured test failed\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write failing e2e test command: %v", err)
	}
	config := "ignore_patterns:\n  - '*.generated.go'\n  - 'vendor/**'\ncommands:\n  test: nm-test-fails-e2e\n  lint: true\n"
	h.CommitChange("failing-test-command", ".no-mistakes.yaml", config, "configure failing test command")
	h.PushToGate("failing-test-command")
	run := waitForStepStatus(t, h, "failing-test-command", types.StepTest, types.StepStatusAwaitingApproval, 60*time.Second)
	testStep, ok := findStep(run.Steps, types.StepTest)
	if !ok {
		t.Fatal("expected test step in failing test command run")
	}
	if testStep.FindingsJSON == nil {
		t.Fatal("expected failing test command to record findings JSON")
	}
	findings, err := types.ParseFindingsJSON(*testStep.FindingsJSON)
	if err != nil {
		t.Fatalf("parse failing test findings: %v", err)
	}
	if len(findings.Items) == 0 || findings.Items[0].Severity != "error" {
		t.Fatalf("expected error finding for failing test command, got %+v", findings.Items)
	}
	if len(findings.Tested) != 1 || findings.Tested[0] != "nm-test-fails-e2e" {
		t.Fatalf("expected failing test command to be recorded, got %+v", findings.Tested)
	}
	h.Respond(run.ID, types.StepTest, types.ActionSkip)
	completed := h.WaitForRun("failing-test-command", 60*time.Second)
	if completed.Status != types.RunCompleted {
		t.Fatalf("failing test command run did not complete after skip: status=%s error=%v", completed.Status, deref(completed.Error))
	}
	completedTestStep, ok := findStep(completed.Steps, types.StepTest)
	if !ok {
		t.Fatal("expected completed test step in failing test command run")
	}
	if completedTestStep.Status != types.StepStatusSkipped {
		t.Fatalf("expected skipped test step after response, got %s", completedTestStep.Status)
	}
	if completedTestStep.ExitCode == nil || *completedTestStep.ExitCode != 1 {
		t.Fatalf("failing test command exit code = %v, want 1", completedTestStep.ExitCode)
	}
	for _, stepName := range []types.StepName{types.StepDocument, types.StepLint, types.StepPush} {
		step, ok := findStep(completed.Steps, stepName)
		if !ok {
			t.Fatalf("expected %s step after skipping failing test command", stepName)
		}
		if step.Status != types.StepStatusCompleted {
			t.Fatalf("expected %s to continue after skipped test step, got %s", stepName, step.Status)
		}
	}
}

func assertFailingLintCommandRun(t *testing.T, h *Harness) {
	t.Helper()
	failingCommand := filepath.Join(h.BinDir, "nm-lint-fails-e2e")
	if err := os.WriteFile(failingCommand, []byte("#!/bin/sh\necho configured lint failed\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write failing e2e lint command: %v", err)
	}
	config := "ignore_patterns:\n  - '*.generated.go'\n  - 'vendor/**'\ncommands:\n  test: true\n  lint: nm-lint-fails-e2e\n"
	h.CommitChange("failing-lint-command", ".no-mistakes.yaml", config, "configure failing lint command")
	h.PushToGate("failing-lint-command")
	run := waitForStepStatus(t, h, "failing-lint-command", types.StepLint, types.StepStatusAwaitingApproval, 60*time.Second)
	lintStep, ok := findStep(run.Steps, types.StepLint)
	if !ok {
		t.Fatal("expected lint step in failing lint command run")
	}
	if lintStep.FindingsJSON == nil {
		t.Fatal("expected failing lint command to record findings JSON")
	}
	findings, err := types.ParseFindingsJSON(*lintStep.FindingsJSON)
	if err != nil {
		t.Fatalf("parse failing lint findings: %v", err)
	}
	if len(findings.Items) == 0 || findings.Items[0].Severity != "warning" {
		t.Fatalf("expected warning finding for failing lint command, got %+v", findings.Items)
	}
	if !strings.Contains(findings.Summary, "configured lint failed") {
		t.Fatalf("expected failing lint summary to contain command output, got %q", findings.Summary)
	}
	h.Respond(run.ID, types.StepLint, types.ActionSkip)
	completed := h.WaitForRun("failing-lint-command", 60*time.Second)
	if completed.Status != types.RunCompleted {
		t.Fatalf("failing lint command run did not complete after skip: status=%s error=%v", completed.Status, deref(completed.Error))
	}
	completedLintStep, ok := findStep(completed.Steps, types.StepLint)
	if !ok {
		t.Fatal("expected completed lint step in failing lint command run")
	}
	if completedLintStep.Status != types.StepStatusSkipped {
		t.Fatalf("expected skipped lint step after response, got %s", completedLintStep.Status)
	}
	if completedLintStep.ExitCode == nil || *completedLintStep.ExitCode != 1 {
		t.Fatalf("failing lint command exit code = %v, want 1", completedLintStep.ExitCode)
	}
}

func assertRunsDefaultLimit(t *testing.T, h *Harness) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	h.CommitChange("runs-limit-extra", "runs-limit-extra.txt", "extra run for runs limit\n", "add runs limit extra")
	if out, err := h.runGit(ctx, h.WorkDir, "checkout", "main"); err != nil {
		t.Fatalf("checkout main before runs-limit-extra merge: %v\n%s", err, out)
	}
	if out, err := h.runGit(ctx, h.WorkDir, "merge", "--ff-only", "runs-limit-extra"); err != nil {
		t.Fatalf("merge runs-limit-extra into main: %v\n%s", err, out)
	}
	if out, err := h.runGit(ctx, h.WorkDir, "push", "origin", "main"); err != nil {
		t.Fatalf("push main for runs-limit-extra: %v\n%s", err, out)
	}
	if out, err := h.runGit(ctx, h.WorkDir, "checkout", "runs-limit-extra"); err != nil {
		t.Fatalf("checkout runs-limit-extra before gate push: %v\n%s", err, out)
	}
	h.PushToGate("runs-limit-extra")
	run := h.WaitForRun("runs-limit-extra", 60*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("runs-limit-extra run did not complete: status=%s error=%v", run.Status, deref(run.Error))
	}
	allRuns := h.Runs()
	if len(allRuns) <= 10 {
		t.Fatalf("expected more than 10 runs before asserting default runs limit, got %d", len(allRuns))
	}
	out, err := h.Run("runs")
	if err != nil {
		t.Fatalf("nm runs with more than default limit: %v\n%s", err, out)
	}
	if regexp.MustCompile(`(?m)^\s+\S+\s+empty-after-rebase\s+`).MatchString(out) {
		t.Fatalf("default runs output should omit oldest run empty-after-rebase when over limit, got:\n%s", out)
	}
	overflowHint := "(" + itoa(len(allRuns)-10) + " more runs, use --limit to see more)"
	for _, want := range []string{"runs-limit-extra", overflowHint} {
		if !strings.Contains(out, want) {
			t.Fatalf("default runs output should contain %q when over limit, got:\n%s", want, out)
		}
	}
}

func assertRerunCompletedInDir(t *testing.T, h *Harness, dir string, previous *ipc.RunInfo) *ipc.RunInfo {
	t.Helper()
	out, err := h.RunInDir(dir, "rerun")
	if err != nil {
		t.Fatalf("nm rerun after completed pipeline in %s: %v\n%s", dir, err, out)
	}
	for _, want := range []string{"Rerun started", "feature/e2e"} {
		if !strings.Contains(out, want) {
			t.Errorf("rerun output should contain %q, got:\n%s", want, out)
		}
	}
	run := h.WaitForRun("feature/e2e", 60*time.Second)
	if run.ID == previous.ID {
		t.Fatalf("rerun returned original run ID %s", run.ID)
	}
	if run.Status != types.RunCompleted {
		t.Fatalf("rerun did not complete: status=%s error=%v", run.Status, deref(run.Error))
	}
	if run.Branch != previous.Branch {
		t.Errorf("rerun branch = %q, want %q", run.Branch, previous.Branch)
	}
	if run.HeadSHA != previous.HeadSHA {
		t.Errorf("rerun head = %q, want %q", run.HeadSHA, previous.HeadSHA)
	}
	if run.BaseSHA != previous.BaseSHA {
		t.Errorf("rerun base = %q, want %q", run.BaseSHA, previous.BaseSHA)
	}
	assertPipelineStepsInOrder(t, run.Steps)
	assertPushedHead(t, run.HeadSHA, h.UpstreamBranchSHA(run.Branch))
	return run
}

func assertDaemonStatusRunning(t *testing.T, h *Harness) {
	t.Helper()
	out, err := h.Run("daemon", "status")
	if err != nil {
		t.Fatalf("nm daemon status after init: %v\n%s", err, out)
	}
	if !strings.Contains(out, "daemon running") {
		t.Errorf("daemon status output should show running after init, got:\n%s", out)
	}
}

func assertDaemonStopOutput(t *testing.T, out string) {
	t.Helper()
	if !strings.Contains(out, "daemon stopped") {
		t.Errorf("daemon stop output should show stopped, got:\n%s", out)
	}
}

func assertDaemonStopWhenNotRunning(t *testing.T, h *Harness) {
	t.Helper()
	out, err := h.Run("daemon", "stop")
	if err != nil {
		t.Fatalf("nm daemon stop before init should succeed when not running: %v\n%s", err, out)
	}
	assertDaemonStopOutput(t, out)
}

func assertDaemonStatusNotRunning(t *testing.T, h *Harness) {
	t.Helper()
	out, err := h.Run("daemon", "status")
	if err != nil {
		t.Fatalf("nm daemon status after stop: %v\n%s", err, out)
	}
	if !strings.Contains(out, "daemon not running") {
		t.Errorf("daemon status output should show not running after stop, got:\n%s", out)
	}
}

func assertDaemonRestartWhileRunning(t *testing.T, h *Harness) {
	t.Helper()
	out, err := h.Run("daemon", "restart")
	if err != nil {
		t.Fatalf("nm daemon restart while running: %v\n%s", err, out)
	}
	if !strings.Contains(out, "daemon restarted") {
		t.Errorf("daemon restart output should show restarted, got:\n%s", out)
	}
	assertDaemonStatusRunning(t, h)
}

func assertDaemonRestartStartsWhenNotRunning(t *testing.T, h *Harness) {
	t.Helper()
	out, err := h.Run("daemon", "restart")
	if err != nil {
		t.Fatalf("nm daemon restart after stop: %v\n%s", err, out)
	}
	if !strings.Contains(out, "daemon restarted") {
		t.Errorf("daemon restart output should show restarted, got:\n%s", out)
	}
	assertDaemonStatusRunning(t, h)
}

func assertStatusActiveRun(t *testing.T, h *Harness, run *ipc.RunInfo) {
	t.Helper()
	assertStatusActiveRunInDir(t, h, h.WorkDir, run)
}

func assertStatusActiveRunInDir(t *testing.T, h *Harness, dir string, run *ipc.RunInfo) {
	t.Helper()
	out, err := h.RunInDir(dir, "status")
	if err != nil {
		t.Fatalf("nm status while run active in %s: %v\n%s", dir, err, out)
	}
	sha := run.HeadSHA
	if len(sha) > 8 {
		sha = sha[:8]
	}
	resolved := h.WorkDir
	if path, err := filepath.EvalSymlinks(h.WorkDir); err == nil {
		resolved = path
	}
	for _, want := range []string{"Active run", run.Branch, string(run.Status), sha, resolved} {
		if !strings.Contains(out, want) {
			t.Errorf("status output should contain %q while run is active in %s, got:\n%s", want, dir, out)
		}
	}
}

func assertStatusInitializedStopped(t *testing.T, h *Harness) {
	t.Helper()
	out, err := h.Run("status")
	if err != nil {
		t.Fatalf("nm status after daemon stop: %v\n%s", err, out)
	}
	resolved := h.WorkDir
	if path, err := filepath.EvalSymlinks(h.WorkDir); err == nil {
		resolved = path
	}
	for _, want := range []string{
		resolved,
		h.UpstreamDir,
		filepath.Join(h.NMHome, "repos", h.repoID()+".git"),
		"daemon:",
		"stopped",
		"no active run",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("status output should contain %q after daemon stop, got:\n%s", want, out)
		}
	}
}

func assertGateRemotePresent(t *testing.T, h *Harness) {
	t.Helper()
	out, err := h.runGit(context.Background(), h.WorkDir, "remote", "get-url", "no-mistakes")
	if err != nil {
		t.Fatalf("no-mistakes remote not found: %v\n%s", err, out)
	}
	want := filepath.Join(h.NMHome, "repos", h.repoID()+".git")
	if strings.TrimSpace(string(out)) != want {
		t.Errorf("no-mistakes remote URL = %q, want %q", strings.TrimSpace(string(out)), want)
	}
}

func assertGateRemoteAbsent(t *testing.T, h *Harness) {
	t.Helper()
	out, err := h.runGit(context.Background(), h.WorkDir, "remote", "get-url", "no-mistakes")
	if err == nil {
		t.Fatalf("no-mistakes remote should have been removed after eject, got %s", out)
	}
}

func sawPromptContaining(invs []Invocation, needle string) bool {
	for _, inv := range invs {
		if strings.Contains(inv.Prompt, needle) {
			return true
		}
	}
	return false
}

func sawPromptContainingAll(invs []Invocation, needles ...string) bool {
	_, ok := promptContainingAll(invs, needles...)
	return ok
}

func promptContaining(invs []Invocation, needle string) (string, bool) {
	for _, inv := range invs {
		if strings.Contains(inv.Prompt, needle) {
			return inv.Prompt, true
		}
	}
	return "", false
}

func promptContainingAll(invs []Invocation, needles ...string) (string, bool) {
	for _, inv := range invs {
		matched := true
		for _, needle := range needles {
			if !strings.Contains(inv.Prompt, needle) {
				matched = false
				break
			}
		}
		if matched {
			return inv.Prompt, true
		}
	}
	return "", false
}

func summarisePrompts(invs []Invocation) string {
	var b strings.Builder
	for i, inv := range invs {
		first := strings.SplitN(inv.Prompt, "\n", 2)[0]
		if len(first) > 100 {
			first = first[:100] + "..."
		}
		b.WriteString("  ")
		b.WriteString(itoa(i))
		b.WriteString(") ")
		b.WriteString(inv.Agent)
		b.WriteString(": ")
		b.WriteString(first)
		b.WriteString("\n")
	}
	return b.String()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [16]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func assertStepsSkipped(t *testing.T, steps []ipc.StepResultInfo, expected ...types.StepName) {
	t.Helper()
	for _, msg := range validateSkippedSteps(steps, expected...) {
		t.Error(msg)
	}
}

func assertPushedHead(t *testing.T, runHeadSHA, upstreamHeadSHA string) {
	t.Helper()
	for _, msg := range validatePushedHead(runHeadSHA, upstreamHeadSHA) {
		t.Error(msg)
	}
}

func assertPipelineStepsInOrder(t *testing.T, steps []ipc.StepResultInfo) {
	t.Helper()
	expected := []types.StepName{types.StepRebase, types.StepReview, types.StepTest, types.StepDocument, types.StepLint, types.StepPush, types.StepPR, types.StepCI}
	if len(steps) != len(expected) {
		t.Fatalf("pipeline recorded %d steps, want %d", len(steps), len(expected))
	}
	for i, step := range steps {
		if step.StepOrder != i+1 {
			t.Errorf("step %d order = %d, want %d", i, step.StepOrder, i+1)
		}
		if step.StepName != expected[i] {
			t.Errorf("step %d name = %s, want %s", i, step.StepName, expected[i])
		}
	}
}

func assertNoPRCreated(t *testing.T, run *ipc.RunInfo) {
	t.Helper()
	if run.PRURL != nil {
		t.Fatalf("expected PR step to skip without creating a PR URL, got %q", *run.PRURL)
	}
}

func assertPromptsAbsent(t *testing.T, invs []Invocation, unexpected ...string) {
	t.Helper()
	for _, msg := range validatePromptsAbsent(invs, unexpected...) {
		t.Error(msg)
	}
}

func assertNewBranchRun(t *testing.T, h *Harness, run *ipc.RunInfo) {
	t.Helper()
	const zeroSHA = "0000000000000000000000000000000000000000"
	if run.ID == "" {
		t.Fatal("expected new branch push to create a run ID")
	}
	if run.RepoID != h.repoID() {
		t.Fatalf("expected run repo ID %q, got %q", h.repoID(), run.RepoID)
	}
	if run.Branch != "feature/e2e" {
		t.Fatalf("expected run branch to be stored without refs/heads prefix, got %s", run.Branch)
	}
	if run.BaseSHA != zeroSHA {
		t.Fatalf("expected new branch push to record zero base SHA, got %s", run.BaseSHA)
	}
}

func assertReviewPrompt(t *testing.T, h *Harness, run *ipc.RunInfo, invs []Invocation) {
	t.Helper()
	prompt, ok := promptContainingAll(invs, "Review the code changes", "branch: feature/e2e")
	if !ok {
		t.Fatalf("expected a feature/e2e review prompt in invocations, got %d:\n%s", len(invs), summarisePrompts(invs))
	}
	baseSHA := h.WorktreeRefSHA("main")
	for _, want := range []string{
		"branch: feature/e2e",
		baseSHA,
		run.HeadSHA,
		"ignore patterns: *.generated.go, vendor/**",
		"Do a full review pass before returning.",
		"Do not stop after the first valid finding.",
		"Do NOT run tests during review.",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("expected review prompt to contain %q, got:\n%s", want, prompt)
		}
	}
	for _, unexpected := range []string{"Diff:\n", "hello world", "add hello.txt", "author's primary intent"} {
		if strings.Contains(prompt, unexpected) {
			t.Errorf("expected review prompt to avoid inline diff or commit-message content %q, got:\n%s", unexpected, prompt)
		}
	}
}

func assertReviewStepInfoOnly(t *testing.T, steps []ipc.StepResultInfo) {
	t.Helper()
	step, ok := findStep(steps, types.StepReview)
	if !ok {
		t.Fatal("expected review step to be present")
	}
	if step.FindingsJSON == nil {
		t.Fatal("expected review step to record findings JSON")
	}
	findings, err := types.ParseFindingsJSON(*step.FindingsJSON)
	if err != nil {
		t.Fatalf("parse review step findings: %v", err)
	}
	if len(findings.Items) != 1 {
		t.Fatalf("expected one informational review finding, got %+v", findings.Items)
	}
	if findings.Items[0].Severity != "info" {
		t.Fatalf("expected informational review finding to be non-blocking, got severity %q", findings.Items[0].Severity)
	}
	if findings.RiskLevel != "low" {
		t.Fatalf("expected low review risk, got %q", findings.RiskLevel)
	}
}

func assertDocumentPrompt(t *testing.T, h *Harness, run *ipc.RunInfo, invs []Invocation) {
	t.Helper()
	prompt, ok := promptContainingAll(invs, "Identify documentation gaps", "branch: feature/e2e")
	if !ok {
		t.Fatalf("expected a feature/e2e document prompt in invocations, got %d:\n%s", len(invs), summarisePrompts(invs))
	}
	baseSHA := h.WorktreeRefSHA("main")
	for _, want := range []string{
		"branch: feature/e2e",
		baseSHA,
		run.HeadSHA,
		"ignore patterns: *.generated.go, vendor/**",
		"Do a full documentation pass before returning.",
		"Do not stop after the first documentation gap.",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("expected document prompt to contain %q, got:\n%s", want, prompt)
		}
	}
}

func assertDocumentStepNoGaps(t *testing.T, steps []ipc.StepResultInfo) {
	t.Helper()
	step, ok := findStep(steps, types.StepDocument)
	if !ok {
		t.Fatal("expected document step to be present")
	}
	if step.FindingsJSON == nil {
		t.Fatal("expected document step to record findings JSON")
	}
	findings, err := types.ParseFindingsJSON(*step.FindingsJSON)
	if err != nil {
		t.Fatalf("parse document step findings: %v", err)
	}
	if len(findings.Items) != 0 {
		t.Fatalf("expected no documentation gaps, got %+v", findings.Items)
	}
}

func assertNoUnexpectedAutofixCommits(t *testing.T, run *ipc.RunInfo, featureHead string) {
	t.Helper()
	if run.HeadSHA != featureHead {
		t.Fatalf("run head SHA = %s, want original feature head %s", run.HeadSHA, featureHead)
	}
}

func assertNoCommandTestStep(t *testing.T, steps []ipc.StepResultInfo, invs []Invocation) {
	t.Helper()
	if !sawPromptContainingAll(invs, "You are validating a code change by testing it", "branch: feature/e2e", "action", "tested", "testing_summary") {
		t.Errorf("expected a test prompt with branch metadata, action guidance, and test reporting fields in invocations, got %d:\n%s", len(invs), summarisePrompts(invs))
	}
	step, ok := findStep(steps, types.StepTest)
	if !ok {
		t.Fatal("expected test step to be present")
	}
	if step.FindingsJSON == nil {
		t.Fatal("expected test step to record findings JSON")
	}
	findings, err := types.ParseFindingsJSON(*step.FindingsJSON)
	if err != nil {
		t.Fatalf("parse test step findings: %v", err)
	}
	if len(findings.Tested) != 1 || findings.Tested[0] != "fakeagent: simulated test run" {
		t.Fatalf("expected fakeagent test details to be preserved, got %+v", findings.Tested)
	}
	if findings.TestingSummary != "simulated tests passed" {
		t.Fatalf("expected fakeagent testing summary to be preserved, got %q", findings.TestingSummary)
	}
	if len(findings.Items) != 0 {
		t.Fatalf("expected no test findings, got %+v", findings.Items)
	}
}

func findStep(steps []ipc.StepResultInfo, name types.StepName) (ipc.StepResultInfo, bool) {
	for _, step := range steps {
		if step.StepName == name {
			return step, true
		}
	}
	return ipc.StepResultInfo{}, false
}

func validateSkippedSteps(steps []ipc.StepResultInfo, expected ...types.StepName) []string {
	var errs []string
	for _, name := range expected {
		found := false
		for _, step := range steps {
			if step.StepName != name {
				continue
			}
			found = true
			if step.Status != types.StepStatusSkipped {
				errs = append(errs, "expected "+string(step.StepName)+" to skip, got "+string(step.Status))
			}
			break
		}
		if !found {
			errs = append(errs, "expected step "+string(name)+" to be present")
		}
	}
	return errs
}

func validatePushedHead(runHeadSHA, upstreamHeadSHA string) []string {
	if runHeadSHA == "" {
		return []string{"run completed without a recorded HeadSHA"}
	}
	if upstreamHeadSHA != "" && runHeadSHA != upstreamHeadSHA {
		return []string{"run HeadSHA = " + runHeadSHA + ", want upstream " + upstreamHeadSHA}
	}
	return nil
}

func validatePromptsAbsent(invs []Invocation, unexpected ...string) []string {
	var errs []string
	for _, needle := range unexpected {
		if sawPromptContaining(invs, needle) {
			errs = append(errs, "unexpected agent prompt: "+needle)
		}
	}
	return errs
}

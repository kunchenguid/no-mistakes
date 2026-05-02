//go:build e2e

package e2e

import (
	"context"
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
	h := NewHarness(t, SetupOpts{Agent: agentName})

	assertRootVersion(t, h)
	assertRootHelp(t, h)
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

	// `no-mistakes init` sets up the gate and starts the daemon.
	out, err := h.Run("init")
	if err != nil {
		t.Fatalf("nm init: %v\n%s", err, out)
	}
	assertInitOutput(t, h, out)
	assertGateRemotePresent(t, h)
	assertDaemonStatusRunning(t, h)
	assertAttachMissingRun(t, h)
	assertDaemonRestartWhileRunning(t, h)
	assertInitAlreadyInitialized(t, h)
	assertRunsEmpty(t, h)
	assertRootNoActiveRun(t, h)

	// Make a feature branch with one trivial change. The fake agent
	// returns "no issues found" for every prompt, so the pipeline
	// should sail through without needing approval.
	featureHead := h.CommitChange("feature/e2e", "hello.txt", "hello world\n", "add hello.txt")

	// Push triggers the post-receive hook, which notifies the daemon.
	h.PushToGate("feature/e2e")

	// Wait up to 60s for the run to terminate. Pipelines that include
	// agent calls + git operations take ~5-15s on a warm machine.
	activeRun := h.WaitForRunRunning("feature/e2e", 30*time.Second)
	assertStatusActiveRun(t, h, activeRun)
	assertRunsActive(t, h, activeRun)

	run := h.WaitForRun("feature/e2e", 60*time.Second)

	if run.Status != types.RunCompleted {
		t.Fatalf("run did not complete: status=%s error=%v", run.Status, deref(run.Error))
	}
	assertNewBranchRun(t, run)

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
			}
		case types.StepStatusSkipped:
			// ok
		default:
			t.Errorf("step %s ended in non-terminal status %s (error=%v)", step.StepName, step.Status, deref(step.Error))
		}
	}

	// PR and CI must skip: no SCM provider on a file:// origin.
	assertStepsSkipped(t, run.Steps, types.StepPR, types.StepCI)

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
	rerun := assertRerunCompleted(t, h, run)
	assertRootRecentRuns(t, h, rerun)

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

	out, err = h.Run("eject")
	if err != nil {
		t.Fatalf("nm eject: %v\n%s", err, out)
	}
	assertEjectOutput(t, h, out)
	assertGateRemoteAbsent(t, h)
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
	assertRunsContainsRun(t, h, run, string(types.RunRunning), "while run is active")
}

func assertRunsCompleted(t *testing.T, h *Harness, run *ipc.RunInfo) {
	t.Helper()
	assertRunsContainsRun(t, h, run, string(types.RunCompleted), "after completed pipeline")
}

func assertRunsContainsRun(t *testing.T, h *Harness, run *ipc.RunInfo, status, phase string) {
	t.Helper()
	out, err := h.Run("runs")
	if err != nil {
		t.Fatalf("nm runs %s: %v\n%s", phase, err, out)
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

func assertRerunCompleted(t *testing.T, h *Harness, previous *ipc.RunInfo) *ipc.RunInfo {
	t.Helper()
	out, err := h.Run("rerun")
	if err != nil {
		t.Fatalf("nm rerun after completed pipeline: %v\n%s", err, out)
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
	out, err := h.Run("status")
	if err != nil {
		t.Fatalf("nm status while run active: %v\n%s", err, out)
	}
	sha := run.HeadSHA
	if len(sha) > 8 {
		sha = sha[:8]
	}
	for _, want := range []string{"Active run", run.Branch, string(run.Status), sha} {
		if !strings.Contains(out, want) {
			t.Errorf("status output should contain %q while run is active, got:\n%s", want, out)
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

func assertPromptsAbsent(t *testing.T, invs []Invocation, unexpected ...string) {
	t.Helper()
	for _, msg := range validatePromptsAbsent(invs, unexpected...) {
		t.Error(msg)
	}
}

func assertNewBranchRun(t *testing.T, run *ipc.RunInfo) {
	t.Helper()
	const zeroSHA = "0000000000000000000000000000000000000000"
	if run.BaseSHA != zeroSHA {
		t.Fatalf("expected new branch push to record zero base SHA, got %s", run.BaseSHA)
	}
}

func assertReviewPrompt(t *testing.T, h *Harness, run *ipc.RunInfo, invs []Invocation) {
	t.Helper()
	prompt, ok := promptContaining(invs, "Review the code changes")
	if !ok {
		t.Fatalf("expected a review prompt in invocations, got %d:\n%s", len(invs), summarisePrompts(invs))
	}
	baseSHA := h.WorktreeRefSHA("main")
	for _, want := range []string{
		"branch: feature/e2e",
		baseSHA,
		run.HeadSHA,
		"Do a full review pass before returning.",
		"Do not stop after the first valid finding.",
		"Do NOT run tests during review.",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("expected review prompt to contain %q, got:\n%s", want, prompt)
		}
	}
	for _, unexpected := range []string{"Diff:\n", "hello world"} {
		if strings.Contains(prompt, unexpected) {
			t.Errorf("expected review prompt to avoid inline diff content %q, got:\n%s", unexpected, prompt)
		}
	}
}

func assertDocumentPrompt(t *testing.T, h *Harness, run *ipc.RunInfo, invs []Invocation) {
	t.Helper()
	prompt, ok := promptContaining(invs, "Identify documentation gaps")
	if !ok {
		t.Fatalf("expected a document prompt in invocations, got %d:\n%s", len(invs), summarisePrompts(invs))
	}
	baseSHA := h.WorktreeRefSHA("main")
	for _, want := range []string{
		"branch: feature/e2e",
		baseSHA,
		run.HeadSHA,
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

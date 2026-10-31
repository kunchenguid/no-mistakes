//go:build e2e

package e2e

import (
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestSelectedRerunAfterNormalTerminalRun(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	const branch = "feature/selected-rerun"
	h.CommitChange(branch, "feature.txt", "change\n", "add feature")
	operator := h.AddWorktree(branch)
	firstOut, err := h.RunInDir(operator, "axi", "run", "--intent", "validate feature")
	if err != nil || !strings.Contains(firstOut, "outcome: passed") {
		t.Fatalf("first run: %v\n%s", err, firstOut)
	}
	first := h.WaitForRun(branch, 60*time.Second)
	if first.Status != types.RunCompleted || first.RunAgent != "" {
		t.Fatalf("first run did not use the configured default: %+v", first)
	}
	for _, invocation := range invocationsForRun(t, h, first.ID) {
		if invocation.Agent != "claude" {
			t.Fatalf("first run invoked %s, want claude", invocation.Agent)
		}
	}

	rerunOut, err := h.RunInDir(operator, "rerun", "--agent", "codex")
	if err != nil || !strings.Contains(rerunOut, "Rerun started") {
		t.Fatalf("selected rerun after normal terminal run: %v\n%s", err, rerunOut)
	}
	selected := h.WaitForRun(branch, 60*time.Second)
	if selected.ID == first.ID || selected.Status != types.RunCompleted || selected.RunAgent != agentcfg.RunAgentCodex {
		t.Fatalf("selected rerun: %+v", selected)
	}
	turns := invocationsForRun(t, h, selected.ID)
	if len(turns) == 0 {
		t.Fatal("selected rerun made no agent invocation")
	}
	for _, invocation := range turns {
		if invocation.Agent != "codex" {
			t.Fatalf("selected rerun invoked %s, want codex", invocation.Agent)
		}
	}
	statusOut, err := h.RunInDir(operator, "axi", "status")
	if err != nil || !strings.Contains(statusOut, "run_agent: codex") || !strings.Contains(statusOut, "outcome: passed-with-skips") {
		t.Fatalf("selected rerun status: %v\n%s", err, statusOut)
	}
	t.Logf("selected rerun after normal terminal run:\n%s\n%s", rerunOut, statusOut)
}

func TestSelectedRunSurvivesDaemonRestart(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: writeReviewAgentsRoutingScenario(t)})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	const branch = "feature/selected-restart"
	h.CommitChange(branch, "feature.txt", "change\n", "add feature")
	operator := h.AddWorktree(branch)
	if out, err := h.RunInDir(operator, "axi", "run", "--agent", "codex", "--intent", "validate feature"); err != nil || !strings.Contains(out, "gate:") {
		t.Fatalf("selected run did not park: %v\n%s", err, out)
	}
	parked := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusAwaitingApproval, 60*time.Second)
	if parked.RunAgent != agentcfg.RunAgentCodex {
		t.Fatalf("selected agent was not persisted: %+v", parked)
	}
	h.globalConfigExtra = "review_agents:\n  reviewer: {agent: claude}\n  fixer: {agent: claude}\n"
	h.writeGlobalConfig()

	pid, err := daemon.ReadPID(paths.WithRoot(h.NMHome))
	if err != nil || pid <= 0 {
		t.Fatalf("read test daemon pid: %v (pid=%d)", err, pid)
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatalf("stop test daemon %d: %v", pid, err)
	}
	for attempt := 0; attempt < 100; attempt++ {
		if err := syscall.Kill(pid, 0); err != nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if out, err := h.Run("daemon", "start"); err != nil {
		t.Fatalf("restart test daemon: %v\n%s", err, out)
	}
	var answerOut string
	for attempt := 0; attempt < 100; attempt++ {
		answerOut, err = h.RunInDir(operator, "axi", "respond", "--action", "approve")
		if err == nil {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("approve resumed run: %v\n%s", err, answerOut)
	}
	resumed := h.WaitForRun(branch, 120*time.Second)
	if resumed.ID != parked.ID || resumed.Status != types.RunCompleted || resumed.RunAgent != agentcfg.RunAgentCodex {
		t.Fatalf("resumed selected run: %+v", resumed)
	}
	turns := invocationsForRun(t, h, resumed.ID)
	if len(turns) == 0 {
		t.Fatal("resumed run made no agent invocation")
	}
	for _, invocation := range turns {
		if invocation.Agent != "codex" {
			t.Fatalf("resumed run invoked %s, want codex", invocation.Agent)
		}
	}
	statusOut, err := h.RunInDir(operator, "axi", "status")
	if err != nil || !strings.Contains(statusOut, "run_agent: codex") {
		t.Fatalf("resumed run status: %v\n%s", err, statusOut)
	}
	t.Logf("resumed selected run:\n%s", statusOut)
}

func TestSelectedRunCannotDisplaceActiveDefaultRun(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: writeReviewAgentsRoutingScenario(t)})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	const branch = "feature/active-default-agent"
	h.CommitChange(branch, "feature.txt", "change\n", "add feature")
	operator := h.AddWorktree(branch)
	if out, err := h.RunInDir(operator, "axi", "run", "--intent", "validate feature"); err != nil || !strings.Contains(out, "gate:") {
		t.Fatalf("default run did not park: %v\n%s", err, out)
	}
	parked := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusAwaitingApproval, 60*time.Second)
	if parked.RunAgent != "" {
		t.Fatalf("default run acquired an agent pin: %+v", parked)
	}
	out, err := h.RunInDir(operator, "axi", "run", "--agent", "codex", "--intent", "validate feature")
	if err == nil || !strings.Contains(out, "different agent selection") {
		t.Fatalf("conflicting selection was accepted or had wrong refusal: %v\n%s", err, out)
	}
	assertParkedRunIntact(t, h, parked.ID, branch)
	t.Logf("conflicting selection refused; default run remains parked:\n%s", out)
}

func TestSelectedRunRetryKeepsAgentAfterDefaultChanges(t *testing.T) {
	scenario := writeReviewAgentsRoutingScenario(t)
	goodScenario, err := os.ReadFile(scenario)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(scenario, []byte("actions: [\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: scenario})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	const branch = "feature/selected-retry"
	h.CommitChange(branch, "feature.txt", "change\n", "add feature")
	operator := h.AddWorktree(branch)
	_, _ = h.RunInDir(operator, "axi", "run", "--agent", "codex", "--intent", "validate feature")
	failed := h.WaitForRun(branch, 60*time.Second)
	if failed.Status != types.RunFailed || failed.RunAgent != agentcfg.RunAgentCodex {
		t.Fatalf("source run did not fail with selected agent: %+v", failed)
	}
	if err := os.WriteFile(scenario, goodScenario, 0o644); err != nil {
		t.Fatal(err)
	}
	h.globalConfigExtra = "review_agents:\n  reviewer: {agent: claude}\n  fixer: {agent: claude}\n"
	h.writeGlobalConfig()
	out, err := h.RunInDir(operator, "axi", "retry", "--run", failed.ID)
	if err != nil {
		t.Fatalf("retry selected run: %v\n%s", err, out)
	}
	retried := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusAwaitingApproval, 60*time.Second)
	if retried.ID == failed.ID || retried.RunAgent != agentcfg.RunAgentCodex {
		t.Fatalf("retry changed agent selection: %+v", retried)
	}
	if answer, err := h.RunInDir(operator, "axi", "respond", "--action", "approve"); err != nil {
		t.Fatalf("finish retried run: %v\n%s", err, answer)
	}
	completed := h.WaitForRun(branch, 60*time.Second)
	if completed.ID != retried.ID || completed.Status != types.RunCompleted || completed.RunAgent != agentcfg.RunAgentCodex {
		t.Fatalf("retry did not finish with selected agent: %+v", completed)
	}
	for _, invocation := range invocationsForRun(t, h, completed.ID) {
		if invocation.Agent != "codex" {
			t.Fatalf("retry invoked %s, want codex", invocation.Agent)
		}
	}
	t.Logf("selected retry retained codex through completion:\n%s", out)
}

//go:build e2e

package e2e

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// assertParkedRunIntact proves a refused dispatch never superseded a healthy
// in-flight validation: the named run keeps running at its review gate and no
// replacement run appeared on the branch.
func assertParkedRunIntact(t *testing.T, h *Harness, runID, branch string) {
	t.Helper()
	run := h.RunInfo(runID)
	if run.Status != types.RunRunning {
		t.Fatalf("active run %s changed status to %s", runID, run.Status)
	}
	step, ok := findStep(run.Steps, types.StepReview)
	if !ok || step.Status != types.StepStatusAwaitingApproval {
		t.Fatalf("active run %s left the review gate (found=%v status=%s)", runID, ok, step.Status)
	}
	for _, other := range h.Runs() {
		if other.Branch == branch && other.ID != runID {
			t.Fatalf("refused dispatch created/superseded with run %s", other.ID)
		}
	}
}

// TestLivePiProfileInvalidSelectionRefusedAtCli drives the real binary's flag
// parsing for the dispatch surface: malformed or ambiguous selections must be
// refused before the daemon, the gate, or any run is touched.
func TestLivePiProfileInvalidSelectionRefusedAtCli(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "pi", Scenario: writeReviewAgentsRoutingScenario(t)})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	cases := []struct {
		name string
		args []string
	}{
		{"model without provider qualifier", []string{"axi", "run", "--intent", "x", "--model", "gpt-5"}},
		{"model with a URL", []string{"axi", "run", "--intent", "x", "--model", "https://user:secret@host/model"}},
		{"model with a thinking suffix", []string{"axi", "run", "--intent", "x", "--model", "openai/gpt:high"}},
		{"unknown effort", []string{"axi", "run", "--intent", "x", "--effort", "turbo"}},
		{"explicitly empty model", []string{"axi", "run", "--intent", "x", "--model="}},
		{"repeated model", []string{"axi", "run", "--intent", "x", "--model", "openai/a", "--model", "openai/b"}},
		{"rerun unknown effort", []string{"rerun", "--effort", "turbo"}},
		{"effort without any configured model", []string{"axi", "run", "--intent", "x", "--effort", "high"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := h.Run(tc.args...)
			if err == nil {
				t.Fatalf("invalid selection accepted:\n%s", out)
			}
			if strings.Contains(out, "secret") {
				t.Fatalf("refusal echoed the rejected credential: %s", out)
			}
		})
	}
}

// TestLivePiProfileMixedHarnessRefusedBeforeActiveRunCancelled is the
// pre-cancel guarantee against the running daemon: a pin request that cannot
// resolve - because the operator's global harness is mixed, or because the
// trusted default branch selects Claude - must be refused before the branch's
// healthy active validation is cancelled.
//
// The caller commits on top of the parked run first, so the CLI no longer
// treats that run as the active run for the current head and takes the
// fresh-launch path that reaches the daemon's pre-cancel check.
func TestLivePiProfileMixedHarnessRefusedBeforeActiveRunCancelled(t *testing.T) {
	t.Run("mixed global harness", func(t *testing.T) {
		const branch = "feature/mixed-global-pin"
		h := NewHarness(t, SetupOpts{
			Agent:    "pi",
			Scenario: writeReviewAgentsRoutingScenario(t),
			GlobalConfigExtra: "agent_config:\n  pi: {model: anthropic/default, effort: high}\n" +
				"review_agents:\n  reviewer: {agent: claude}\n",
		})
		if out, err := h.Run("init"); err != nil {
			t.Fatalf("init: %v\n%s", err, out)
		}
		h.CommitChange(branch, "feature.txt", "seed for mixed global pin\n", "seed feature")
		h.PushToGate(branch)
		parked := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusAwaitingApproval, 90*time.Second)

		h.CommitChange(branch, "advance.txt", "advance the head\n", "advance head")
		out, err := h.Run("axi", "run", "--intent", "pin under a mixed harness", "--model", "openai-codex/gpt-5.4")
		if err == nil {
			t.Fatalf("mixed-harness pin was accepted:\n%s", out)
		}
		if !strings.Contains(out, "Pi run profile") {
			t.Fatalf("refusal did not name the harness conflict: %v\n%s", err, out)
		}
		t.Logf("mixed global harness refusal: %s", firstErrorLine(out))
		assertParkedRunIntact(t, h, parked.ID, branch)
	})

	for _, tc := range []struct {
		name      string
		agentYAML string
	}{
		{"trusted default branch selects Claude", "agent: claude\n"},
		{"trusted default branch lists mixed fallbacks", "agent: [pi, claude]\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const branch = "feature/trusted-agent-pin"
			noRepoCommands := false
			h := NewHarness(t, SetupOpts{
				Agent:             "pi",
				Scenario:          writeReviewAgentsRoutingScenario(t),
				AllowRepoCommands: &noRepoCommands,
				GlobalConfigExtra: "agent_config:\n  pi: {model: anthropic/default, effort: high}\n",
			})
			if out, err := h.Run("init"); err != nil {
				t.Fatalf("init: %v\n%s", err, out)
			}
			// The trusted default branch, never the pushed branch, selects the agent.
			trusted := tc.agentYAML + "ignore_patterns:\n  - '*.generated.go'\n  - 'vendor/**'\nallow_repo_commands: false\n"
			if err := os.WriteFile(h.WorkDir+"/.no-mistakes.yaml", []byte(trusted), 0o644); err != nil {
				t.Fatalf("write trusted config: %v", err)
			}
			for _, args := range [][]string{{"add", ".no-mistakes.yaml"}, {"commit", "-m", "trusted agent selection"}, {"push", "origin", "main"}} {
				if out, err := h.runGit(context.Background(), h.WorkDir, args...); err != nil {
					t.Fatalf("git %v: %v\n%s", args, err, out)
				}
			}

			h.CommitChange(branch, "feature.txt", "seed for trusted agent pin\n", "seed feature")
			h.PushToGate(branch)
			parked := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusAwaitingApproval, 90*time.Second)

			h.CommitChange(branch, "advance.txt", "advance the head\n", "advance head")
			out, err := h.Run("axi", "run", "--intent", "pin over a trusted default-branch override", "--model", "openai-codex/gpt-5.4")
			if err == nil {
				t.Fatalf("trusted-override pin was accepted:\n%s", out)
			}
			if !strings.Contains(out, "Pi run profile requires agent: pi") {
				t.Fatalf("refusal did not name the trusted agent conflict: %v\n%s", err, out)
			}
			t.Logf("trusted default-branch refusal: %s", firstErrorLine(out))
			assertParkedRunIntact(t, h, parked.ID, branch)
		})
	}
}

// TestLivePiProfileReattachAndRerunSemantics drives the full dispatch surface
// of one branch: a pinned run keeps its pin when reattached without flags, a
// different selection cannot change it, and a rerun is a brand-new run that
// takes a new pin or, with no flags, falls back to global configuration.
func TestLivePiProfileReattachAndRerunSemantics(t *testing.T) {
	const (
		branch      = "feature/pi-reattach"
		globalModel = "global/default-e2e"
		pinnedModel = "anthropic/model-pinned-e2e"
		rerunModel  = "anthropic/model-rerun-e2e"
	)
	h := NewHarness(t, SetupOpts{
		Agent:             "pi",
		Scenario:          writeReviewAgentsRoutingScenario(t),
		GlobalConfigExtra: "agent_config:\n  pi: {model: " + globalModel + ", effort: low}\n",
	})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	h.CommitChange(branch, "feature.txt", "seed for review-agents routing\n", "seed feature")

	out, err := h.Run("axi", "run", "--intent", "pinned reattach probe", "--model", pinnedModel, "--effort", "high")
	if err != nil {
		h.dumpDebugState()
		t.Fatalf("pinned launch: %v\n%s", err, out)
	}
	parked := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusAwaitingApproval, 90*time.Second)
	if parked.PiProfile == nil || parked.PiProfile.Model != pinnedModel || parked.PiProfile.Effort != "high" {
		t.Fatalf("pin not persisted on launch: %+v", parked.PiProfile)
	}

	// A bare reattach must keep the original pin untouched.
	out, err = h.Run("axi", "run", "--intent", "pinned reattach probe")
	if err != nil {
		t.Fatalf("reattach without flags: %v\n%s", err, out)
	}
	reattached := h.RunInfo(parked.ID)
	if reattached.PiProfile == nil || reattached.PiProfile.Model != pinnedModel || reattached.PiProfile.Effort != "high" {
		t.Fatalf("reattach changed the pin: %+v", reattached.PiProfile)
	}

	// A different selection must not replace the running pin, and the refusal
	// must happen before any further turn runs.
	before := len(invocationsForRun(t, h, parked.ID))
	out, err = h.Run("axi", "run", "--effort", "low")
	if err == nil || !strings.Contains(out, "different Pi profile") {
		t.Fatalf("conflicting selection replaced the active pin: %v\n%s", err, out)
	}
	if after := len(invocationsForRun(t, h, parked.ID)); after != before {
		t.Fatalf("conflicting selection ran agent work: %d -> %d turns", before, after)
	}

	// Every turn of the parked run carried the pinned model, effort and provider.
	if pinTurns := invocationsForRun(t, h, parked.ID); len(pinTurns) == 0 {
		t.Fatal("pinned run contributed no agent turns")
	} else {
		for _, inv := range pinTurns {
			assertModelArg(t, "pinned run", inv.Args, pinnedModel)
			if args := strings.Join(inv.Args, " "); !strings.Contains(args, "--thinking high") || !strings.Contains(args, "--provider anthropic") {
				t.Fatalf("pinned run lost its effort/provider: %s", args)
			}
		}
	}

	// Cancel the parked run so the caller's clean head matches the prior run
	// again; a rerun at a mismatched head is refused by design.
	h.CancelRun(parked.ID)
	if run := h.WaitForRun(branch, 60*time.Second); run.Status != types.RunCancelled {
		t.Fatalf("parked run did not cancel: %s", run.Status)
	}

	// rerun --model/--effort is a new run with the new pin.
	out, err = h.Run("rerun", "--model", rerunModel, "--effort", "medium")
	if err != nil {
		t.Fatalf("pinned rerun: %v\n%s", err, out)
	}
	rerun := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusAwaitingApproval, 90*time.Second)
	if rerun.ID == parked.ID {
		t.Fatal("rerun reused the prior run")
	}
	if rerun.PiProfile == nil || rerun.PiProfile.Model != rerunModel || rerun.PiProfile.Effort != "medium" {
		t.Fatalf("rerun pin not persisted: %+v", rerun.PiProfile)
	}
	for _, inv := range invocationsForRun(t, h, rerun.ID) {
		assertModelArg(t, "pinned rerun", inv.Args, rerunModel)
	}

	h.CancelRun(rerun.ID)
	if run := h.WaitForRun(branch, 60*time.Second); run.Status != types.RunCancelled {
		t.Fatalf("pinned rerun did not cancel: %s", run.Status)
	}

	// A flagless rerun is a NEW run that follows global configuration and does
	// not inherit the prior pin.
	out, err = h.Run("rerun")
	if err != nil {
		t.Fatalf("flagless rerun: %v\n%s", err, out)
	}
	plain := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusAwaitingApproval, 90*time.Second)
	if plain.ID == rerun.ID {
		t.Fatal("flagless rerun reused the pinned run")
	}
	if plain.PiProfile != nil {
		t.Fatalf("flagless rerun inherited a pin: %+v", plain.PiProfile)
	}
	for _, inv := range invocationsForRun(t, h, plain.ID) {
		assertModelArg(t, "flagless rerun", inv.Args, globalModel)
	}

	h.CancelRun(plain.ID)
	if run := h.WaitForRun(branch, 60*time.Second); run.Status != types.RunCancelled {
		t.Fatalf("flagless rerun did not cancel: %s", run.Status)
	}

	// --effort alone is a valid opt-in: the model resolves from
	// agent_config.pi and the pin persists both fields.
	out, err = h.Run("axi", "run", "--intent", "effort-only probe", "--effort", "max")
	if err != nil {
		t.Fatalf("effort-only launch: %v\n%s", err, out)
	}
	effortOnly := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusAwaitingApproval, 90*time.Second)
	if effortOnly.PiProfile == nil || effortOnly.PiProfile.Model != globalModel || effortOnly.PiProfile.Effort != "max" {
		t.Fatalf("effort-only pin = %+v, want %s/max", effortOnly.PiProfile, globalModel)
	}
	for _, inv := range invocationsForRun(t, h, effortOnly.ID) {
		assertModelArg(t, "effort-only pin", inv.Args, globalModel)
		if args := strings.Join(inv.Args, " "); !strings.Contains(args, "--thinking max") {
			t.Fatalf("effort-only pin lost its effort: %s", args)
		}
	}

	// The pin is immutable in the persisted run state: an external rewrite of
	// the daemon's database is refused and the product still reports the pin.
	dbPath := paths.WithRoot(h.NMHome).DB()
	mutate := exec.Command("sqlite3", dbPath,
		`UPDATE runs SET pi_profile = '{"model":"anthropic/other-e2e","effort":"low"}' WHERE id = '`+effortOnly.ID+`'`)
	mutateOut, mutateErr := mutate.CombinedOutput()
	if mutateErr == nil {
		t.Fatalf("persisted pin was mutable: %s", mutateOut)
	}
	if !strings.Contains(string(mutateOut), "immutable") {
		t.Fatalf("unexpected mutation failure: %v\n%s", mutateErr, mutateOut)
	}
	if now := h.RunInfo(effortOnly.ID); now.PiProfile == nil || now.PiProfile.Model != globalModel || now.PiProfile.Effort != "max" {
		t.Fatalf("mutation attempt changed the live pin: %+v", now.PiProfile)
	}
	t.Logf("persisted pin mutation refused: %s", strings.TrimSpace(string(mutateOut)))
}

// TestLivePiProfileConcurrentRunsStayIsolated proves two pinned validations for
// different branches run at the same time with independent profiles: each run's
// agent subprocesses carry only that run's model, and a conflicting dispatch
// against one branch leaves the other untouched.
func TestLivePiProfileConcurrentRunsStayIsolated(t *testing.T) {
	const (
		branchA = "feature/iso-a"
		branchB = "feature/iso-b"
		modelA  = "anthropic/model-a-e2e"
		modelB  = "openai-codex/model-b-e2e"
	)
	h := NewHarness(t, SetupOpts{
		Agent:             "pi",
		Scenario:          writeReviewAgentsRoutingScenario(t),
		GlobalConfigExtra: "agent_config:\n  pi: {model: anthropic/default, effort: high}\n",
	})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}

	h.CommitChange(branchA, "feature.txt", "seed for review-agents routing\n", "seed a")
	if out, err := h.Run("axi", "run", "--intent", "isolation probe A", "--model", modelA, "--effort", "high"); err != nil {
		h.dumpDebugState()
		t.Fatalf("launch A: %v\n%s", err, out)
	}
	runA := waitForStepStatus(t, h, branchA, types.StepReview, types.StepStatusAwaitingApproval, 90*time.Second)

	h.CommitChange(branchB, "feature.txt", "seed for review-agents routing\n", "seed b")
	if out, err := h.Run("axi", "run", "--intent", "isolation probe B", "--model", modelB, "--effort", "low"); err != nil {
		h.dumpDebugState()
		t.Fatalf("launch B: %v\n%s", err, out)
	}
	runB := waitForStepStatus(t, h, branchB, types.StepReview, types.StepStatusAwaitingApproval, 90*time.Second)

	if runA.PiProfile == nil || runA.PiProfile.Model != modelA {
		t.Fatalf("run A pin = %+v", runA.PiProfile)
	}
	if runB.PiProfile == nil || runB.PiProfile.Model != modelB {
		t.Fatalf("run B pin = %+v", runB.PiProfile)
	}
	// Launching B must not have cancelled A.
	if active := h.ActiveRun(branchA); active == nil || active.ID != runA.ID {
		t.Fatalf("branch A active run changed after branch B launched: %+v", active)
	}

	// A dispatch that conflicts with B is refused, and A stays parked.
	if out, err := h.Run("axi", "run", "--intent", "isolation probe B2", "--model", "anthropic/model-c-e2e"); err == nil {
		t.Fatalf("conflicting dispatch accepted:\n%s", out)
	}
	if active := h.ActiveRun(branchA); active == nil || active.ID != runA.ID {
		t.Fatalf("branch A active run changed after a refused B dispatch: %+v", active)
	}

	h.RespondWithFindings(runA.ID, types.StepReview, types.ActionFix, []string{"routing-check"})
	h.RespondWithFindings(runB.ID, types.StepReview, types.ActionFix, []string{"routing-check"})
	waitForRunIDTerminal(t, h, runA.ID, 120*time.Second)
	waitForRunIDTerminal(t, h, runB.ID, 120*time.Second)

	invsA, invsB := invocationsForRun(t, h, runA.ID), invocationsForRun(t, h, runB.ID)
	if len(invsA) == 0 || len(invsB) == 0 {
		t.Fatalf("missing invocations for isolation runs: A=%d B=%d", len(invsA), len(invsB))
	}
	for _, inv := range invsA {
		assertModelArg(t, "run A", inv.Args, modelA)
		assertNotModelArg(t, "run A", inv.Args, modelB)
	}
	for _, inv := range invsB {
		assertModelArg(t, "run B", inv.Args, modelB)
		assertNotModelArg(t, "run B", inv.Args, modelA)
	}
	t.Logf("isolated pinned runs: A=%d invocations on %s, B=%d invocations on %s", len(invsA), modelA, len(invsB), modelB)
}

// TestLivePiProfileResumeAfterDaemonRestartKeepsPin proves a pinned run keeps
// its persisted profile across a daemon restart: the run is parked, the
// operator's live configuration is rewritten to a conflicting harness, the
// daemon is restarted, and the resumed fix/review turns still carry the
// original pin rather than the newly written global selection.
func TestLivePiProfileResumeAfterDaemonRestartKeepsPin(t *testing.T) {
	const (
		branch      = "feature/pi-restart"
		pinnedModel = "openai-codex/model-restart-e2e"
		otherModel  = "anthropic/other-e2e"
	)
	h := NewHarness(t, SetupOpts{
		Agent:             "pi",
		Scenario:          writeReviewAgentsRoutingScenario(t),
		GlobalConfigExtra: "agent_config:\n  pi: {model: " + otherModel + ", effort: low}\n",
	})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	h.CommitChange(branch, "feature.txt", "seed for review-agents routing\n", "seed feature")

	out, err := h.Run("axi", "run", "--intent", "restart probe", "--model", pinnedModel, "--effort", "high")
	if err != nil {
		h.dumpDebugState()
		t.Fatalf("pinned launch: %v\n%s", err, out)
	}
	parked := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusAwaitingApproval, 90*time.Second)

	// Rewrite the live configuration to a conflicting one and let it be inert
	// for this already-pinned run.
	h.globalConfigExtra = "agent_config:\n  pi: {model: " + otherModel + ", effort: low}\n" +
		"agent_args_override:\n  pi: [--model, " + otherModel + "]\n" +
		"review_agents:\n  reviewer: {agent: claude}\n"
	h.writeGlobalConfig()

	// Crash the daemon: a graceful restart cancels active runs, so only a
	// crash exercises the recovery path that must reinstate the persisted pin.
	pid, err := daemon.ReadPID(paths.WithRoot(h.NMHome))
	if err != nil || pid <= 0 {
		t.Fatalf("read daemon pid: %v (pid=%d)", err, pid)
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatalf("kill daemon %d: %v", pid, err)
	}
	for attempt := 0; attempt < 100; attempt++ {
		if err := syscall.Kill(pid, 0); err != nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if out, err := h.Run("daemon", "start"); err != nil {
		t.Fatalf("daemon start after crash: %v\n%s", err, out)
	}

	var respondErr error
	for attempt := 0; attempt < 100; attempt++ {
		if respondErr = h.respondError(parked.ID, types.StepReview, types.ActionFix, []string{"routing-check"}); respondErr == nil {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if respondErr != nil {
		t.Fatalf("respond after restart: %v", respondErr)
	}

	run := h.WaitForRun(branch, 120*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("resumed pinned run did not complete: %s (%v)", run.Status, run.Error)
	}
	invs := invocationsForRun(t, h, parked.ID)
	if len(invs) == 0 {
		t.Fatal("no agent turns captured for the resumed run")
	}
	for _, inv := range invs {
		assertModelArg(t, "resumed pinned run", inv.Args, pinnedModel)
		assertNotModelArg(t, "resumed pinned run", inv.Args, otherModel)
	}
	t.Logf("resumed across daemon restart with %d turns on %s", len(invs), pinnedModel)
}

// invocationsForRun returns the captured agent turns that ran inside the
// named run's own worktree.
func invocationsForRun(t *testing.T, h *Harness, runID string) []Invocation {
	t.Helper()
	dir := paths.WithRoot(h.NMHome).WorktreeDir(h.repoID(), runID)
	var out []Invocation
	for _, inv := range h.AgentInvocations() {
		if inv.CWD == dir || strings.HasPrefix(inv.CWD, dir+string(os.PathSeparator)) {
			out = append(out, inv)
		}
	}
	return out
}

// waitForRunIDTerminal polls until the named run reaches a terminal status.
func waitForRunIDTerminal(t *testing.T, h *Harness, runID string, timeout time.Duration) *ipc.RunInfo {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		run := h.RunInfo(runID)
		if run != nil && run.Status.Terminal() {
			return run
		}
		time.Sleep(300 * time.Millisecond)
	}
	h.dumpDebugState()
	run := h.RunInfo(runID)
	if run == nil {
		t.Fatalf("run %s disappeared", runID)
	}
	t.Fatalf("run %s did not finish in %v (status=%s)", runID, timeout, run.Status)
	return nil
}

// firstErrorLine returns the emitted error field from a CLI transcript.
func firstErrorLine(out string) string {
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "error") || strings.Contains(line, "Pi run profile") {
			return strings.TrimSpace(line)
		}
	}
	return strings.TrimSpace(out)
}

//go:build e2e

package e2e

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/daemon"
	"github.com/kunchenguid/no-mistakes/internal/e2edaemon"
	"github.com/kunchenguid/no-mistakes/internal/gate"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func reviewerJourneyConfig(model string) string {
	return "agent_config:\n  codex: {model: writer-e2e, effort: medium}\nreview_agents:\n  reviewer: {agent: codex, model: " + model + ", effort: low}\n  fixer: {agent: codex, model: fixer-e2e, effort: medium}\n"
}

func reviewerJourneyRun(t *testing.T, h *Harness, dir string, args ...string) string {
	t.Helper()
	out, err := h.RunInDir(dir, args...)
	if err != nil {
		h.dumpDebugState()
		t.Fatalf("%v: %v\n%s", args, err, out)
	}
	return out
}

func assertReviewerJourneyPin(t *testing.T, run *ipc.RunInfo, model string) {
	t.Helper()
	if run.ReviewerProfile == nil || run.ReviewerProfile.Agent != types.AgentCodex || run.ReviewerProfile.Model != model || run.ReviewerProfile.Effort != agentcfg.EffortHigh {
		t.Fatalf("run %s reviewer = %+v, want codex/%s/high", run.ID, run.ReviewerProfile, model)
	}
}

// Captured argv is the executable agent interface, rather than a check of
// implementation source. Codex spells its model flag -m.
func assertReviewerJourneyTurns(t *testing.T, h *Harness, runID, reviewer string, requireFix bool) {
	t.Helper()
	reviews, fixes, writers := 0, 0, 0
	for _, inv := range invocationsForRun(t, h, runID) {
		if inv.Agent != "codex" {
			t.Fatalf("non-Codex invocation: %+v", inv)
		}
		want := "writer-e2e"
		switch {
		case strings.Contains(inv.Prompt, reviewTurnMarker):
			want = reviewer
			reviews++
		case strings.Contains(inv.Prompt, fixTurnMarker):
			want = "fixer-e2e"
			fixes++
		default:
			writers++
		}
		model := ""
		for i := 0; i+1 < len(inv.Args); i++ {
			if inv.Args[i] == "-m" {
				model = inv.Args[i+1]
			}
		}
		if model != want {
			t.Fatalf("run %s model = %q, want %q: %v", runID, model, want, inv.Args)
		}
		if want == reviewer && !strings.Contains(strings.Join(inv.Args, " "), `model_reasoning_effort="high"`) {
			t.Fatalf("reviewer effort lost: %v", inv.Args)
		}
	}
	if reviews == 0 || writers == 0 || requireFix && (fixes == 0 || reviews < 2) {
		t.Fatalf("missing duties: reviews=%d fixes=%d writers=%d", reviews, fixes, writers)
	}
	t.Logf("run %s: reviewer=%s, reviews=%d fixes=%d writer turns=%d", runID, reviewer, reviews, fixes, writers)
}

func TestLiveReviewerSelectionConcurrentRolesAndReattach(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "codex", Scenario: writeReviewAgentsRoutingScenario(t), GlobalConfigExtra: reviewerJourneyConfig("default-reviewer-e2e")})
	reviewerJourneyRun(t, h, h.WorkDir, "init")
	branches := []string{"feature/reviewer-a", "feature/reviewer-b"}
	dirs := make([]string, 2)
	runs := make([]*ipc.RunInfo, 2)
	for i, branch := range branches {
		h.CommitChange(branch, "feature.txt", "seed for review-agents routing\n", "reviewer fixture")
		dirs[i] = h.AddWorktree(branch)
		reviewerJourneyRun(t, h, dirs[i], "axi", "run", "--intent", "independent reviewer", "--reviewer", "codex", "--reviewer-model", "review-"+branch[len("feature/"):], "--reviewer-effort", "high")
		runs[i] = waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusAwaitingApproval, 60*time.Second)
	}
	for i, run := range runs {
		model := "review-" + branches[i][len("feature/"):]
		assertReviewerJourneyPin(t, h.RunInfo(run.ID), model)
		if active := h.ActiveRun(branches[i]); active == nil || active.ID != run.ID {
			t.Fatalf("concurrent run disappeared: %s", run.ID)
		}
		reviewerJourneyRun(t, h, dirs[i], "axi", "run")
		if active := h.ActiveRun(branches[i]); active == nil || active.ID != run.ID {
			t.Fatalf("reattach replaced run %s", run.ID)
		}
		assertReviewerJourneyPin(t, h.RunInfo(run.ID), model)
	}
	for i, run := range runs {
		h.RespondWithFindings(run.ID, types.StepReview, types.ActionFix, []string{"routing-check"})
		completed := h.WaitForRun(branches[i], 90*time.Second)
		if completed.ID != run.ID || completed.Status != types.RunCompleted {
			t.Fatalf("run did not complete: %+v", completed)
		}
		assertReviewerJourneyTurns(t, h, run.ID, "review-"+branches[i][len("feature/"):], true)
	}
}

func TestLiveReviewerSelectionFreshLaunchAndRerun(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "codex", GlobalConfigExtra: reviewerJourneyConfig("default-reviewer-e2e")})
	reviewerJourneyRun(t, h, h.WorkDir, "init")
	const branch = "feature/reviewer-fresh"
	h.CommitChange(branch, "feature.txt", "fresh launch\n", "reviewer fixture")
	reviewerJourneyRun(t, h, h.WorkDir, "axi", "run", "--intent", "reviewer inheritance", "--reviewer", "codex", "--reviewer-model", "pinned-reviewer-e2e", "--reviewer-effort", "high")
	first := h.WaitForRun(branch, 60*time.Second)
	assertReviewerJourneyPin(t, first, "pinned-reviewer-e2e")
	if first.Status != types.RunCompleted {
		t.Fatalf("first run failed: %+v", first)
	}
	assertReviewerJourneyTurns(t, h, first.ID, "pinned-reviewer-e2e", false)
	reviewerJourneyRun(t, h, h.WorkDir, "rerun")
	inherited := h.WaitForRun(branch, 60*time.Second)
	assertReviewerJourneyPin(t, inherited, "pinned-reviewer-e2e")
	if inherited.ID == first.ID || inherited.Status != types.RunCompleted {
		t.Fatalf("rerun did not complete a new run: %+v", inherited)
	}
	assertReviewerJourneyTurns(t, h, inherited.ID, "pinned-reviewer-e2e", false)
	// The local gate already has this exact head, exercising the no-op push
	// fallback rather than a ref update that naturally starts a fresh run.
	gateDir := paths.WithRoot(h.NMHome).RepoDir(h.repoID())
	gateHead, err := h.runGit(context.Background(), h.WorkDir, "--git-dir="+gateDir, "rev-parse", "refs/heads/"+branch)
	if err != nil || strings.TrimSpace(string(gateHead)) != h.WorktreeRefSHA("HEAD") {
		t.Fatalf("fixture must retain the unchanged gate head: %v\n%s", err, gateHead)
	}
	reviewerJourneyRun(t, h, h.WorkDir, "axi", "run", "--intent", "fresh launch without reviewer")
	fresh := h.WaitForRun(branch, 60*time.Second)
	if fresh.ID == inherited.ID || fresh.ReviewerProfile != nil || fresh.Status != types.RunCompleted {
		t.Fatalf("fresh launch inherited reviewer: %+v", fresh)
	}
	for _, inv := range invocationsForRun(t, h, fresh.ID) {
		if strings.Contains(inv.Prompt, reviewTurnMarker) {
			if !strings.Contains(strings.Join(inv.Args, " "), "-m default-reviewer-e2e") {
				t.Fatalf("fresh review ignored defaults: %v", inv.Args)
			}
			t.Logf("fresh run %s uses default reviewer: %v", fresh.ID, inv.Args)
			return
		}
	}
	t.Fatal("fresh run never invoked a reviewer")
}

func TestLiveReviewerSelectionMissingBeforeCustody(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "codex"})
	reviewerJourneyRun(t, h, h.WorkDir, "init")
	h.CommitChange("feature/reviewer-missing", "feature.txt", "rejected submission\n", "reviewer fixture")
	// Change only the configured executable after init has registered the repo.
	missing := filepath.Join(h.BinDir, "missing-codex")
	configPath := filepath.Join(h.NMHome, "config.yaml")
	cfg, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg = []byte(strings.Replace(string(cfg), filepath.Join(h.BinDir, "codex"), missing, 1))
	if err := os.WriteFile(configPath, cfg, 0600); err != nil {
		t.Fatal(err)
	}
	gateDir := paths.WithRoot(h.NMHome).RepoDir(h.repoID())
	refs := func() string {
		t.Helper()
		out, err := h.runGit(context.Background(), h.WorkDir, "--git-dir="+gateDir, "for-each-ref", "--format=%(refname) %(objectname)")
		if err != nil {
			t.Fatalf("read gate refs: %v\n%s", err, out)
		}
		return string(out)
	}
	before := refs()
	for _, mode := range []string{"ordinary", "proof", "direct"} {
		t.Run(mode, func(t *testing.T) {
			args := []string{"axi", "run", "--intent", "reject unavailable reviewer", "--reviewer", "codex", "--reviewer-model", "reviewer-e2e", "--reviewer-effort", "high"}
			if mode == "proof" {
				args = append(args, "--launch-nonce", "missing-reviewer", "--validation-generation", "missing-reviewer")
			}
			var out string
			var err error
			if mode == "direct" {
				pin, _ := json.Marshal(agentcfg.ReviewerProfile{Agent: types.AgentCodex, Model: "reviewer-e2e", Effort: agentcfg.EffortHigh})
				b, pushErr := h.runGit(context.Background(), h.WorkDir, "push", "-o", "no-mistakes.reviewer-profile="+base64.RawURLEncoding.EncodeToString(pin), gate.RemoteName, "HEAD")
				out, err = string(b), pushErr
			} else {
				out, err = h.Run(args...)
			}
			if err == nil || !strings.Contains(out, "reviewer unavailable") {
				t.Fatalf("missing reviewer not rejected: %v\n%s", err, out)
			}
			if after := refs(); before != after {
				t.Fatalf("rejected %s launch changed refs: before=%s after=%s", mode, before, after)
			}
			if runs := h.Runs(); len(runs) != 0 {
				t.Fatalf("rejected launch created runs: %+v", runs)
			}
			t.Logf("%s: unavailable reviewer rejected, refs unchanged, no run created\n%s", mode, out)
		})
	}
}

func TestLiveReviewerSelectionRetryKeepsPin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("provider failure wrapper requires a POSIX shell")
	}
	h := NewHarness(t, SetupOpts{Agent: "codex", GlobalConfigExtra: reviewerJourneyConfig("default-reviewer-e2e")})
	reviewerJourneyRun(t, h, h.WorkDir, "init")
	const branch = "feature/reviewer-retry"
	h.CommitChange(branch, "feature.txt", "retry fixture\n", "reviewer fixture")
	bin := filepath.Join(h.BinDir, "codex")
	healthy := filepath.Join(h.BinDir, "healthy", "codex")
	if err := os.MkdirAll(filepath.Dir(healthy), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(bin, healthy); err != nil {
		t.Fatal(err)
	}
	failFlag := filepath.Join(h.BinDir, "provider-down")
	script := "#!/bin/sh\nif [ -f " + shellQuote(failFlag) + " ]; then\n  for arg in \"$@\"; do\n    if [ \"$arg\" = pinned-reviewer-e2e ]; then echo 'provider processing error' >&2; exit 1; fi\n  done\nfi\nexec " + shellQuote(healthy) + " \"$@\"\n"
	if err := os.WriteFile(bin, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(failFlag, nil, 0600); err != nil {
		t.Fatal(err)
	}
	out, _ := h.Run("axi", "run", "--intent", "retry reviewer failure", "--reviewer", "codex", "--reviewer-model", "pinned-reviewer-e2e", "--reviewer-effort", "high")
	failed := h.WaitForRun(branch, 60*time.Second)
	if failed.Status != types.RunFailed {
		t.Fatalf("outage did not fail a run: %+v\n%s", failed, out)
	}
	assertReviewerJourneyPin(t, failed, "pinned-reviewer-e2e")
	if err := os.Remove(failFlag); err != nil {
		t.Fatal(err)
	}
	h.globalConfigExtra = reviewerJourneyConfig("changed-reviewer-e2e")
	h.writeGlobalConfig()
	reviewerJourneyRun(t, h, h.WorkDir, "axi", "sync", "--recover")
	reviewerJourneyRun(t, h, h.WorkDir, "axi", "retry", "--run", failed.ID)
	retried := h.WaitForRun(branch, 60*time.Second)
	if retried.Status != types.RunCompleted || retried.RetryOf == nil || retried.RetryOf.RunID != failed.ID {
		t.Fatalf("retry did not continue failed run: %+v", retried)
	}
	assertReviewerJourneyPin(t, retried, "pinned-reviewer-e2e")
	assertReviewerJourneyTurns(t, h, retried.ID, "pinned-reviewer-e2e", false)
}

func TestLiveReviewerSelectionCrashRecoveryKeepsPin(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "codex", Scenario: writeReviewAgentsRoutingScenario(t), GlobalConfigExtra: reviewerJourneyConfig("default-reviewer-e2e")})
	reviewerJourneyRun(t, h, h.WorkDir, "init")
	const branch = "feature/reviewer-recovery"
	h.CommitChange(branch, "feature.txt", "seed for review-agents routing\n", "reviewer fixture")
	reviewerJourneyRun(t, h, h.WorkDir, "axi", "run", "--intent", "recover immutable reviewer", "--reviewer", "codex", "--reviewer-model", "pinned-reviewer-e2e", "--reviewer-effort", "high")
	parked := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusAwaitingApproval, 60*time.Second)
	h.globalConfigExtra = reviewerJourneyConfig("changed-reviewer-e2e")
	h.writeGlobalConfig()
	pid, err := daemon.ReadPID(paths.WithRoot(h.NMHome))
	if err != nil || pid <= 0 {
		t.Fatalf("read disposable daemon PID: %d %v", pid, err)
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	if err := proc.Kill(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		alive, err := e2edaemon.ProcessAlive(pid)
		if err != nil {
			t.Fatal(err)
		}
		if !alive {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	reviewerJourneyRun(t, h, h.WorkDir, "daemon", "start")
	var respondErr error
	for i := 0; i < 100; i++ {
		respondErr = h.respondError(parked.ID, types.StepReview, types.ActionFix, []string{"routing-check"})
		if respondErr == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if respondErr != nil {
		t.Fatalf("respond to recovered gate: %v", respondErr)
	}
	completed := h.WaitForRun(branch, 90*time.Second)
	if completed.ID != parked.ID || completed.Status != types.RunCompleted {
		t.Fatalf("recovery did not complete same run: %+v", completed)
	}
	assertReviewerJourneyPin(t, completed, "pinned-reviewer-e2e")
	assertReviewerJourneyTurns(t, h, completed.ID, "pinned-reviewer-e2e", true)
}

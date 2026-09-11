//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/e2edaemon"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestDLOCK31RealDaemonRetainedPublication(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	provider := t.TempDir()
	t.Setenv("FAKEAGENT_GH_MODE", "dlock31")
	t.Setenv("DLOCK31_PROVIDER", provider)
	t.Setenv("DLOCK31_UPSTREAM", h.UpstreamDir)
	scenario := filepath.Join(provider, "scenario.yaml")
	t.Setenv("FAKEAGENT_SCENARIO", scenario)
	writeDLOCK31(t, scenario, `actions:
  - match: "The following CI checks have failed"
    edits:
      - path: repair.txt
        new: "repaired\n"
    structured:
      summary: "repair the disposable fixture"
      code_change_needed: true
  - structured:
      findings: []
      summary: "fixture clean response"
      risk_level: low
      risk_rationale: "fixture"
      risk_scope: source-or-external
      tested: ["simulated"]
      testing_summary: "simulated"
      artifacts: []
      scenarios:
        - name: "simulated fixture"
          result: pass
          live: true
          evidence: "simulated agent response, not live product proof"
          reason: ""
      verdict: go
      title: "test: fixture"
      body: "disposable fixture"
`)
	const remote = "https://github.com/dlock31/fixture.git"
	configureGitURLRewrite(t, h, remote, h.UpstreamDir)
	if out, err := h.runGit(t.Context(), h.WorkDir, "remote", "set-url", "origin", remote); err != nil {
		t.Fatalf("remote: %v %s", err, out)
	}
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v %s", err, out)
	}
	const branch = "feature/dlock31"
	h.CommitChange(branch, "repair.txt", "broken\n", "add fixture")
	h.PushToGate(branch)
	parked := waitForStepStatus(t, h, branch, types.StepCI, types.StepStatusAwaitingApproval, 90*time.Second)
	recorded := parked.HeadSHA
	writeDLOCK31(t, filepath.Join(provider, "fail"), "fail edits only")
	beforeFix := len(h.AgentInvocations())
	respondDLOCK31Fix(t, h, parked)
	var binding *db.RetainedCIRepair
	database, err := db.Open(paths.WithRoot(h.NMHome).DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		binding, err = database.RetainedCIRepair(parked.ID)
		if err != nil {
			t.Fatal(err)
		}
		if binding != nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if binding == nil {
		h.dumpDebugState()
		t.Fatal("actual attestation failure did not bind retained repair")
	}
	failed := waitForStepStatus(t, h, branch, types.StepCI, types.StepStatusFixReview, 30*time.Second)
	assertDLOCK31Retained(t, h, database, binding, failed)
	if failed.HeadSHA != recorded || h.UpstreamBranchSHA(branch) != recorded || binding.RetainedHead == recorded {
		t.Fatalf("failed publication changed recorded/upstream head: %+v", binding)
	}
	failedBody, err := os.ReadFile(filepath.Join(provider, "failed-write"))
	if err != nil || !strings.Contains(string(failedBody), binding.RetainedHead) {
		t.Fatalf("missing real failed attestation write: %v", err)
	}
	fixCalls := len(h.AgentInvocations())
	if fixCalls != beforeFix+1 {
		t.Fatalf("fix invocations delta=%d, want 1", fixCalls-beforeFix)
	}
	previous := daemonPIDForRoot(t, h)
	restartDLOCK31(t, h, previous)
	current := daemonPIDForRoot(t, h)
	if current == previous {
		t.Fatal("daemon PID did not change")
	}
	if alive, err := e2edaemon.ProcessAlive(previous); err != nil || alive {
		t.Fatalf("old disposable daemon still alive: %v %v", alive, err)
	}
	startup, err := os.ReadFile(filepath.Join(h.NMHome, "logs", "daemon.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("disposable daemon restart log:\n%s", tailBytes(startup, 12000))
	if observed := h.RunInfo(parked.ID); observed.Status.Terminal() {
		t.Fatalf("restart made parked run terminal: status=%s error=%v", observed.Status, deref(observed.Error))
	}
	restored := waitForStepStatus(t, h, branch, types.StepCI, types.StepStatusFixReview, 30*time.Second)
	if restored.ID != parked.ID || restored.HeadSHA != recorded {
		t.Fatalf("recovery changed original run/head: %+v", restored)
	}
	assertDLOCK31Retained(t, h, database, binding, restored)
	if len(h.AgentInvocations()) != fixCalls {
		t.Fatal("recovery invoked an agent")
	}
	if err := os.Remove(filepath.Join(provider, "fail")); err != nil {
		t.Fatal(err)
	}
	writeDLOCK31(t, filepath.Join(provider, "green"), binding.RetainedHead)
	respondDLOCK31Fix(t, h, restored)
	deadline = time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if h.UpstreamBranchSHA(branch) == binding.RetainedHead {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if got := h.UpstreamBranchSHA(branch); got != binding.RetainedHead {
		h.dumpDebugState()
		t.Fatalf("published %s want exact retained %s", got, binding.RetainedHead)
	}
	stored, err := database.GetRun(parked.ID)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := database.RetainedCIRepair(parked.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Git publication precedes the local DB settlement. A bounded CLI hold
	// may return in that interval; wait for both without resubmitting the fix.
	for (stored.HeadSHA != binding.RetainedHead || pending != nil) && time.Now().Before(deadline) {
		time.Sleep(200 * time.Millisecond)
		stored, err = database.GetRun(parked.ID)
		if err != nil {
			t.Fatal(err)
		}
		pending, err = database.RetainedCIRepair(parked.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	if stored.HeadSHA != binding.RetainedHead || pending != nil {
		t.Fatalf("publication did not settle: head=%s pending=%+v", stored.HeadSHA, pending)
	}
	if len(h.AgentInvocations()) != fixCalls {
		t.Fatal("retry invoked another agent")
	}
	assertDLOCK31Git(t, h, stored.WorktreePath(), binding)
	body, err := os.ReadFile(filepath.Join(provider, "body"))
	if err != nil || !strings.Contains(string(body), binding.RetainedHead) {
		t.Fatal("attestation does not bind retained commit")
	}
	t.Logf("real candidate daemon root=%s pid=%d -> %d run=%s recorded=%s retained=published=%s fixer_calls=1 extra_agent_calls=0", h.NMHome, previous, current, parked.ID, recorded, binding.RetainedHead)
}

func restartDLOCK31(t *testing.T, h *Harness, pid int) {
	t.Helper()
	if os.Getenv("DLOCK31_GRACEFUL_RESTART") == "1" {
		if out, err := h.Run("daemon", "restart", "--force"); err != nil {
			t.Fatalf("graceful restart: %v %s", err, out)
		}
		return
	}
	// Keep abrupt process death independent of the graceful-shutdown path.
	owners, err := e2edaemon.FindDaemonsForRoot(h.NMHome)
	if err != nil || len(owners) != 1 || owners[0] != pid {
		t.Fatalf("cannot prove disposable process ownership: %v %v", owners, err)
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	if err := process.Kill(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		alive, err := e2edaemon.ProcessAlive(pid)
		if err != nil {
			t.Fatal(err)
		}
		if !alive {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if alive, err := e2edaemon.ProcessAlive(pid); err != nil || alive {
		t.Fatalf("disposable process did not exit: %v %v", alive, err)
	}
	if out, err := h.Run("daemon", "start"); err != nil {
		t.Fatalf("start after disposable crash: %v %s", err, out)
	}
}

func writeDLOCK31(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

func respondDLOCK31Fix(t *testing.T, h *Harness, run *ipc.RunInfo) {
	t.Helper()
	step, ok := findStep(run.Steps, types.StepCI)
	if !ok || step.FindingsJSON == nil {
		t.Fatal("missing CI findings")
	}
	findings, err := types.ParseFindingsJSON(*step.FindingsJSON)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, finding := range findings.Items {
		ids = append(ids, finding.ID)
	}
	if len(ids) == 0 {
		t.Fatal("no findings to select")
	}
	out, err := h.Run("axi", "respond", "--action", "fix", "--findings", strings.Join(ids, ","), "--wait", "1s")
	if err != nil && !dlock31SubmittedWaitElapsed(err, out) {
		t.Fatalf("explicit fix response: %v\n%s", err, out)
	}
	// A post-submission bounded hold is not a failed response. Do not send it
	// again: the journey's DB/IPC/Git assertions below wait for its real result.
	t.Logf("explicit CLI fix response for run %s: %s", run.ID, out)
}

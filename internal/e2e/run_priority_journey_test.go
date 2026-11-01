//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// priorityParkingScenario parks every run at its review gate with one
// ask-user finding, so a run holds its max_concurrent_runs slot for as long
// as the test leaves the gate unanswered.
const priorityParkingScenario = `actions:
  - match: "Review the code changes and return structured findings"
    structured:
      findings:
        - id: hold
          severity: warning
          file: work.txt
          line: 1
          description: the operator must decide this before the run continues
          action: ask-user
          review_scope: source
      summary: one finding that needs a decision
      risk_level: low
      risk_rationale: parked on purpose
      risk_scope: source-or-external
  - structured:
      findings: []
      summary: clean
      risk_level: low
      risk_rationale: nothing else to report
      risk_scope: source-or-external
      tested: ["fixture"]
      testing_summary: fixture
      scenarios:
        - name: fixture
          result: pass
          live: true
          evidence: fixture
          reason: ""
      verdict: go
      artifacts: []
      title: priority journey
      body: Priority journey
`

func writePriorityScenario(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent.yaml")
	if err := os.WriteFile(path, []byte(priorityParkingScenario), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func priorityEvidence(t *testing.T, name, content string) {
	t.Helper()
	dir := os.Getenv("NM_PRIORITY_EVIDENCE_DIR")
	if dir == "" {
		return
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Logf("write evidence %s: %v", name, err)
	}
}

// latestRunsByBranch returns the newest run of each branch (get_runs lists
// runs newest first).
func latestRunsByBranch(h *Harness) map[string]ipc.RunInfo {
	out := map[string]ipc.RunInfo{}
	for _, r := range h.Runs() {
		if _, ok := out[r.Branch]; !ok {
			out[r.Branch] = r
		}
	}
	return out
}

func stepStatus(r ipc.RunInfo, step types.StepName) types.StepStatus {
	for _, s := range r.Steps {
		if s.StepName == step {
			return s.Status
		}
	}
	return ""
}

// waitParkedAtReview waits until branch's newest run holds a slot parked at
// its review gate.
func waitParkedAtReview(t *testing.T, h *Harness, branch string) ipc.RunInfo {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for time.Now().Before(deadline) {
		if r, ok := latestRunsByBranch(h)[branch]; ok && r.Status == types.RunRunning && stepStatus(r, types.StepReview) == types.StepStatusAwaitingApproval {
			return r
		}
		time.Sleep(250 * time.Millisecond)
	}
	h.dumpDebugState()
	t.Fatalf("branch %s never parked at review; runs: %s", branch, describeRuns(h))
	return ipc.RunInfo{}
}

func describeRuns(h *Harness) string {
	var b strings.Builder
	for _, r := range h.Runs() {
		fmt.Fprintf(&b, "\n  %s %s status=%s priority=%d review=%s", r.ID, r.Branch, r.Status, r.Priority, stepStatus(r, types.StepReview))
	}
	return b.String()
}

// assertQueue asserts exactly which branch holds the single slot and which
// branches still wait pending.
func assertQueue(t *testing.T, h *Harness, running string, pending ...string) {
	t.Helper()
	runs := latestRunsByBranch(h)
	if r := runs[running]; r.Status != types.RunRunning {
		t.Fatalf("%s should hold the slot, got status %s; runs: %s", running, r.Status, describeRuns(h))
	}
	for _, b := range pending {
		if r := runs[b]; r.Status != types.RunPending {
			t.Fatalf("%s should still wait pending, got %s; runs: %s", b, r.Status, describeRuns(h))
		}
	}
	active := 0
	for _, r := range h.Runs() {
		if r.Status == types.RunRunning {
			active++
		}
	}
	if active != 1 {
		t.Fatalf("max_concurrent_runs: 1 but %d runs hold a slot; runs: %s", active, describeRuns(h))
	}
}

func cancelAndWait(t *testing.T, h *Harness, r ipc.RunInfo) {
	t.Helper()
	h.CancelRun(r.ID)
	deadline := time.Now().Add(time.Minute)
	for time.Now().Before(deadline) {
		if got := h.RunInfo(r.ID); got != nil && got.Status.Terminal() {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("run %s (%s) never finished after cancel", r.ID, r.Branch)
}

// The full operator journey on a machine capped at one run: an ordinary run
// parked at an approval gate holds the slot; two ordinary runs, a push-option
// priority run, and an `axi run --priority` run queue behind it. Freeing the
// slot admits by priority then submission order, the parked run is never
// displaced, axi status shows the priority, guards refuse bad or conflicting
// values, and a rerun inherits the priority it re-validates.
func TestRunPriorityJourney_UrgentRunStartsFirstUnderTheCap(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: writePriorityScenario(t), GlobalConfigExtra: "max_concurrent_runs: 1"})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	var transcript strings.Builder
	record := func(cmd, out string) {
		fmt.Fprintf(&transcript, "$ %s\n%s\n", cmd, strings.TrimRight(out, "\n"))
	}
	ctx := context.Background()

	h.CommitChange("feature/held", "work.txt", "held\n", "held work")
	h.PushToGate("feature/held")
	held := waitParkedAtReview(t, h, "feature/held")

	h.CommitChange("feature/ordinary-a", "work.txt", "a\n", "ordinary a")
	h.PushToGate("feature/ordinary-a")
	h.CommitChange("feature/ordinary-b", "work.txt", "b\n", "ordinary b")
	h.PushToGate("feature/ordinary-b")

	h.CommitChange("feature/red-main", "work.txt", "urgent\n", "repair red main")
	out, err := h.runGit(ctx, h.WorkDir, "push", "-o", "no-mistakes.priority=20", "no-mistakes", "feature/red-main")
	record("git push -o no-mistakes.priority=20 no-mistakes feature/red-main", string(out))
	if err != nil {
		t.Fatalf("priority push: %v\n%s", err, out)
	}

	// Adversarial: an out-of-range push option is refused, not queued as ordinary.
	h.CommitChange("feature/bad-option", "work.txt", "bad\n", "bad option")
	out, err = h.runGit(ctx, h.WorkDir, "push", "-o", "no-mistakes.priority=101", "no-mistakes", "feature/bad-option")
	record("git push -o no-mistakes.priority=101 no-mistakes feature/bad-option", string(out))
	if _, ok := latestRunsByBranch(h)["feature/bad-option"]; ok {
		t.Fatalf("an out-of-range priority push option created a run; push err=%v\n%s", err, out)
	}

	// Adversarial: an out-of-range --priority is refused before anything is pushed.
	h.CommitChange("feature/hotfix", "work.txt", "hotfix\n", "hotfix")
	out2, err := h.Run("axi", "run", "--intent", "repair the hotfix", "--priority", "101", "--wait", "20s")
	record("no-mistakes axi run --intent \"repair the hotfix\" --priority 101", out2)
	if err == nil || !strings.Contains(out2, "priority") {
		t.Fatalf("--priority 101 was not refused: %v\n%s", err, out2)
	}
	if _, ok := latestRunsByBranch(h)["feature/hotfix"]; ok {
		t.Fatalf("--priority 101 started a run")
	}

	out2, _ = h.Run("axi", "run", "--intent", "repair the hotfix", "--priority", "30", "--wait", "20s")
	record("no-mistakes axi run --intent \"repair the hotfix\" --priority 30 --wait 20s", out2)

	runs := latestRunsByBranch(h)
	for branch, want := range map[string]int{"feature/ordinary-a": 0, "feature/ordinary-b": 0, "feature/red-main": 20, "feature/hotfix": 30} {
		r, ok := runs[branch]
		if !ok || r.Priority != want || r.Status != types.RunPending {
			t.Fatalf("%s: run=%v priority=%d status=%s, want pending with priority %d; runs: %s", branch, ok, r.Priority, r.Status, want, describeRuns(h))
		}
	}
	// The parked run keeps its slot: nothing pre-empts, pauses or cancels it.
	assertQueue(t, h, "feature/held", "feature/ordinary-a", "feature/ordinary-b", "feature/red-main", "feature/hotfix")
	if got := stepStatus(latestRunsByBranch(h)["feature/held"], types.StepReview); got != types.StepStatusAwaitingApproval {
		t.Fatalf("parked run's gate changed to %s after priority arrivals", got)
	}

	// axi status on the priority branch shows the priority; an ordinary one does not.
	h.Checkout("feature/hotfix")
	status, err := h.Run("axi", "status")
	record("no-mistakes axi status   # on feature/hotfix", status)
	if err != nil || !strings.Contains(status, "priority: 30") {
		t.Fatalf("axi status does not show priority 30: %v\n%s", err, status)
	}
	// Adversarial: reattaching with a different --priority is refused.
	reattach, err := h.Run("axi", "run", "--intent", "repair the hotfix", "--priority", "5", "--wait", "10s")
	record("no-mistakes axi run --intent \"repair the hotfix\" --priority 5   # reattach", reattach)
	if err == nil || !strings.Contains(reattach, "submitted with priority 30") {
		t.Fatalf("conflicting reattach was not refused: %v\n%s", err, reattach)
	}
	if r := latestRunsByBranch(h)["feature/hotfix"]; r.Priority != 30 {
		t.Fatalf("reattach changed the priority to %d", r.Priority)
	}
	h.Checkout("feature/ordinary-a")
	ordStatus, err := h.Run("axi", "status")
	record("no-mistakes axi status   # on feature/ordinary-a", ordStatus)
	if err != nil || strings.Contains(ordStatus, "priority:") {
		t.Fatalf("ordinary run renders a priority: %v\n%s", err, ordStatus)
	}
	record("# queue before freeing the slot", describeRuns(h))

	// Free the slot: highest priority first, then priority 20, then FIFO.
	cancelAndWait(t, h, held)
	hotfix := waitParkedAtReview(t, h, "feature/hotfix")
	assertQueue(t, h, "feature/hotfix", "feature/ordinary-a", "feature/ordinary-b", "feature/red-main")
	record("# after cancelling feature/held", describeRuns(h))

	cancelAndWait(t, h, hotfix)
	redMain := waitParkedAtReview(t, h, "feature/red-main")
	assertQueue(t, h, "feature/red-main", "feature/ordinary-a", "feature/ordinary-b")
	record("# after cancelling feature/hotfix", describeRuns(h))

	cancelAndWait(t, h, redMain)
	ordA := waitParkedAtReview(t, h, "feature/ordinary-a")
	assertQueue(t, h, "feature/ordinary-a", "feature/ordinary-b")
	record("# after cancelling feature/red-main", describeRuns(h))

	// A rerun of the cancelled priority-20 run inherits its priority and so
	// queues ahead of the older ordinary-b.
	h.Checkout("feature/red-main")
	rerunOut, err := h.Run("rerun")
	record("no-mistakes rerun   # on feature/red-main", rerunOut)
	if err != nil {
		t.Fatalf("rerun: %v\n%s", err, rerunOut)
	}
	rerun := latestRunsByBranch(h)["feature/red-main"]
	if rerun.ID == redMain.ID || rerun.Priority != 20 || rerun.Status != types.RunPending {
		t.Fatalf("rerun = %s priority %d status %s, want a new pending run with inherited priority 20", rerun.ID, rerun.Priority, rerun.Status)
	}
	cancelAndWait(t, h, ordA)
	rerun = waitParkedAtReview(t, h, "feature/red-main")
	assertQueue(t, h, "feature/red-main", "feature/ordinary-b")
	record("# after cancelling feature/ordinary-a (rerun of red-main admitted before ordinary-b)", describeRuns(h))

	cancelAndWait(t, h, rerun)
	ordB := waitParkedAtReview(t, h, "feature/ordinary-b")
	assertQueue(t, h, "feature/ordinary-b")
	record("# after cancelling the red-main rerun", describeRuns(h))
	cancelAndWait(t, h, ordB)

	priorityEvidence(t, "01-priority-queue-journey.txt", transcript.String())
}

// Parked runs recovered after a daemon restart wait for a slot in priority
// order: with two runs parked under a cap of two, the cap drops to one and
// the daemon restarts; the newer priority run takes the slot, not the older
// ordinary one.
func TestRunPriorityJourney_RecoveredParkedRunsResumeByPriority(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: writePriorityScenario(t), GlobalConfigExtra: "max_concurrent_runs: 2"})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	var transcript strings.Builder
	record := func(cmd, out string) {
		fmt.Fprintf(&transcript, "$ %s\n%s\n", cmd, strings.TrimRight(out, "\n"))
	}
	h.CommitChange("feature/ordinary", "work.txt", "ordinary\n", "ordinary")
	h.PushToGate("feature/ordinary")
	ordinary := waitParkedAtReview(t, h, "feature/ordinary")
	h.CommitChange("feature/red-main", "work.txt", "urgent\n", "urgent")
	if out, err := h.runGit(context.Background(), h.WorkDir, "push", "-o", "no-mistakes.priority=20", "no-mistakes", "feature/red-main"); err != nil {
		t.Fatalf("priority push: %v\n%s", err, out)
	}
	urgent := waitParkedAtReview(t, h, "feature/red-main")
	if urgent.Priority != 20 {
		t.Fatalf("urgent priority = %d", urgent.Priority)
	}
	record("# both parked under max_concurrent_runs: 2", describeRuns(h))

	cfgPath := filepath.Join(h.NMHome, "config.yaml")
	cfg, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, []byte(strings.Replace(string(cfg), "max_concurrent_runs: 2", "max_concurrent_runs: 1", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	// --force: both runs are parked at a gate, which the daemon keeps for
	// resume after the restart.
	out, err := h.Run("daemon", "restart", "--force")
	record("# config: max_concurrent_runs: 1\nno-mistakes daemon restart --force", out)
	if err != nil {
		t.Fatalf("daemon restart: %v\n%s", err, out)
	}

	// The urgent run is recovered into the only slot; its gate answers.
	deadline := time.Now().Add(90 * time.Second)
	for {
		err := h.RespondError(urgent.ID, types.StepReview, types.ActionApprove)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			h.dumpDebugState()
			t.Fatalf("recovered urgent run never took the slot: %v; runs: %s", err, describeRuns(h))
		}
		time.Sleep(250 * time.Millisecond)
	}
	record("# respond approve to feature/red-main -> accepted (holds the slot)", "")
	// The older ordinary run still waits: its gate is not live.
	if err := h.RespondError(ordinary.ID, types.StepReview, types.ActionApprove); err == nil {
		t.Fatalf("older ordinary recovered run took the only slot ahead of the priority run; runs: %s", describeRuns(h))
	} else {
		record("# respond approve to feature/ordinary while red-main holds the slot -> refused", err.Error())
	}
	record("# after restart", describeRuns(h))
	// Once the urgent run ends, the ordinary run gets the slot.
	if r := h.RunInfo(urgent.ID); r != nil && !r.Status.Terminal() {
		cancelAndWait(t, h, *r)
	}
	deadline = time.Now().Add(90 * time.Second)
	for {
		err := h.RespondError(ordinary.ID, types.StepReview, types.ActionApprove)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("ordinary recovered run never resumed after the slot freed: %v", err)
		}
		time.Sleep(250 * time.Millisecond)
	}
	record("# after feature/red-main ended: respond approve to feature/ordinary -> accepted", describeRuns(h))
	if r := h.RunInfo(ordinary.ID); r != nil && !r.Status.Terminal() {
		cancelAndWait(t, h, *r)
	}
	priorityEvidence(t, "02-recovery-after-restart.txt", transcript.String())
}

// A database migrated by the previous release (no priority column) stays
// readable by this release's read-only surface: `axi rounds` opens the
// database read-only, so it cannot migrate it, and must still list the run.
func TestRunPriorityJourney_ReadOnlySurfaceOnAPreviousReleasesDatabase(t *testing.T) {
	oldBin := os.Getenv("NM_PRIORITY_OLD_BIN")
	if oldBin == "" {
		t.Skip("NM_PRIORITY_OLD_BIN (a binary built from the previous release) not set")
	}
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: writePriorityScenario(t)})
	newBin := h.NMBin
	h.NMBin = oldBin
	var transcript strings.Builder
	record := func(cmd, out string) {
		fmt.Fprintf(&transcript, "$ %s\n%s\n", cmd, strings.TrimRight(out, "\n"))
	}
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("old init: %v\n%s", err, out)
	}
	h.CommitChange("feature/old", "work.txt", "old\n", "old work")
	h.PushToGate("feature/old")
	run := waitParkedAtReview(t, h, "feature/old")
	if out, err := h.Run("daemon", "stop", "--force"); err != nil {
		t.Fatalf("old daemon stop: %v\n%s", err, out)
	}
	dbPath := filepath.Join(h.NMHome, "state.sqlite")
	columns := func() string {
		out, err := exec.Command("sqlite3", dbPath, "SELECT group_concat(name, ',') FROM pragma_table_info('runs') WHERE name = 'priority';").CombinedOutput()
		if err != nil {
			entries, _ := os.ReadDir(h.NMHome)
			var names []string
			for _, e := range entries {
				names = append(names, e.Name())
			}
			t.Fatalf("sqlite3 %s: %v\n%s\nNM_HOME: %v", dbPath, err, out, names)
		}
		return strings.TrimSpace(string(out))
	}
	if got := columns(); got != "" {
		t.Fatalf("previous release's database already has a priority column: %q", got)
	}
	record("# previous-release daemon created run "+run.ID+"; runs table has no priority column", "")

	h.NMBin = newBin
	out, err := h.Run("axi", "rounds", "--step", "review")
	record("no-mistakes axi rounds --step review   # new binary, read-only, previous release's database", out)
	if err != nil || !strings.Contains(out, run.ID) {
		t.Fatalf("new read-only surface failed on the previous schema: %v\n%s", err, out)
	}
	if got := columns(); got != "" {
		t.Fatalf("the read-only surface migrated the database: priority column = %q", got)
	}
	record("# runs table still has no priority column after the read-only read", "")
	priorityEvidence(t, "03-readonly-previous-schema.txt", transcript.String())
	h.NMBin = oldBin
}

// axi retry continues a run that failed on an agent invocation error, and
// the continuation keeps the failed run's admission priority.
func TestRunPriorityJourney_RetryInheritsPriority(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: writePriorityScenario(t), GlobalConfigExtra: "agent_crash_retries: 0\nmax_concurrent_runs: 1"})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	var transcript strings.Builder
	record := func(cmd, out string) {
		fmt.Fprintf(&transcript, "$ %s\n%s\n", cmd, strings.TrimRight(out, "\n"))
	}
	// The agent binary crashes on every invocation, so the run fails on an
	// agent invocation error - the one failure axi retry continues.
	agentLink := filepath.Join(h.BinDir, "claude")
	if err := os.Remove(agentLink); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(agentLink, []byte("#!/bin/sh\necho 'simulated agent crash' >&2\nexit 3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	h.CommitChange("feature/red-main", "work.txt", "urgent\n", "repair red main")
	out, err := h.runGit(context.Background(), h.WorkDir, "push", "-o", "no-mistakes.priority=20", "no-mistakes", "feature/red-main")
	record("git push -o no-mistakes.priority=20 no-mistakes feature/red-main   # agent crashes", string(out))
	if err != nil {
		t.Fatalf("priority push: %v\n%s", err, out)
	}
	failed := h.WaitForRun("feature/red-main", 2*time.Minute)
	if failed.Status != types.RunFailed || failed.Priority != 20 {
		t.Fatalf("source run status %s priority %d, want failed with priority 20", failed.Status, failed.Priority)
	}
	record("# source run", describeRuns(h))

	if err := os.Remove(agentLink); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(h.FakeAgent, agentLink); err != nil {
		t.Fatal(err)
	}
	retryOut, err := h.Run("axi", "retry", "--run", failed.ID, "--wait", "15s")
	record("no-mistakes axi retry --run "+failed.ID, retryOut)
	continuation := latestRunsByBranch(h)["feature/red-main"]
	if continuation.ID == failed.ID {
		t.Fatalf("axi retry started no continuation: %v\n%s", err, retryOut)
	}
	if continuation.Priority != 20 {
		t.Fatalf("continuation %s priority = %d, want inherited 20\n%s", continuation.ID, continuation.Priority, retryOut)
	}
	status, _ := h.Run("axi", "status")
	record("no-mistakes axi status", status)
	if !strings.Contains(status, "priority: 20") {
		t.Fatalf("axi status on the continuation does not show priority 20:\n%s", status)
	}
	record("# after retry", describeRuns(h))
	if r := h.RunInfo(continuation.ID); r != nil && !r.Status.Terminal() {
		cancelAndWait(t, h, *r)
	}
	priorityEvidence(t, "04-retry-inherits-priority.txt", transcript.String())
}

//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// staleMonitorFixture drives one full pipeline run that ends with pipeline fix
// commits pushed to the gate while the operator worktree stays at the head it
// submitted. That is exactly the shape a dead-monitor recovery starts from: the
// gate is ahead of the clone, so `rerun` would stamp a head the clone does not
// have. It returns the operator worktree and the pushed pipeline head.
func staleMonitorFixture(t *testing.T, branch string) (*Harness, string, string) {
	t.Helper()
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: branchSyncScenario(t)})
	h.CommitChange("init-stale", "seed.txt", "seed\n", "seed stale-monitor init")
	initWorktree := h.AddWorktree("init-stale")
	if out, err := h.RunInDir(initWorktree, "init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}

	submitted := h.CommitChange(branch, "feature.txt", "unsafe\n", "add unsafe feature")
	operator := h.AddWorktree(branch)
	gateOut, err := h.RunInDir(operator, "axi", "run", "--intent", "guard the feature before the monitor dies")
	if err != nil || !strings.Contains(gateOut, "sync-1") {
		t.Fatalf("initial review gate: %v\n%s", err, gateOut)
	}
	if fixOut, err := h.RunInDir(operator, "axi", "respond", "--action", "fix", "--findings", "sync-1"); err != nil {
		t.Fatalf("review fix: %v\n%s", err, fixOut)
	}
	doneOut, err := h.RunInDir(operator, "axi", "respond", "--action", "approve")
	if err != nil {
		t.Fatalf("approve fix review: %v\n%s", err, doneOut)
	}
	pushedHead := h.UpstreamBranchSHA(branch)
	if pushedHead == submitted {
		t.Fatal("pipeline did not create and push a fix commit")
	}
	if got := strings.TrimSpace(h.WorktreeRefSHA(branch)); got != submitted {
		t.Fatalf("operator branch moved on its own: %s, want %s", got, submitted)
	}
	return h, operator, pushedHead
}

// TestAxiStaleMonitorSyncBeforeRerunReattaches executes the prescribed
// dead-monitor recovery in the order the guidance now states: take the
// pipeline's commits with the offered sync action FIRST, then `rerun`, then
// `axi run` to answer the recovered run's gates. Each step is asserted against
// the real binary rather than reasoned about.
func TestAxiStaleMonitorSyncBeforeRerunReattaches(t *testing.T) {
	branch := "feature/stale-sync-first"
	h, operator, pushedHead := staleMonitorFixture(t, branch)

	// The refusal an agent actually hits first must itself offer the sync.
	noIntentOut, noIntentErr := h.RunInDir(operator, "axi", "run")
	if noIntentErr == nil {
		t.Fatalf("axi run without --intent should fail:\n%s", noIntentOut)
	}
	for _, want := range []string{
		"--intent is required to start a run",
		"branch_sync:",
		"state: behind",
		"code: sync",
		"command: no-mistakes axi sync",
	} {
		if !strings.Contains(noIntentOut, want) {
			t.Errorf("intent-required refusal missing %q:\n%s", want, noIntentOut)
		}
	}

	syncOut, err := h.RunInDir(operator, "axi", "sync")
	if err != nil {
		t.Fatalf("guarded sync before rerun: %v\n%s", err, syncOut)
	}
	if got := strings.TrimSpace(h.WorktreeRefSHA(branch)); got != pushedHead {
		t.Fatalf("operator HEAD after sync = %s, want pushed head %s", got, pushedHead)
	}

	rerunOut, err := h.RunInDir(operator, "rerun")
	if err != nil {
		t.Fatalf("rerun after sync: %v\n%s", err, rerunOut)
	}
	reran := h.ActiveRun(branch)
	if reran == nil {
		t.Fatalf("rerun left no active run:\n%s", rerunOut)
	}
	if reran.HeadSHA != pushedHead {
		t.Fatalf("reran run head = %s, want the synchronized head %s", reran.HeadSHA, pushedHead)
	}

	// The reattach: no --intent, no push, same run driven to its gate.
	runsBefore := len(h.Runs())
	reattachOut, err := h.RunInDir(operator, "axi", "run")
	if err != nil {
		t.Fatalf("reattach after sync+rerun: %v\n%s", err, reattachOut)
	}
	if !strings.Contains(reattachOut, reran.ID) {
		t.Errorf("reattach drove a different run than %s:\n%s", reran.ID, reattachOut)
	}
	for _, forbidden := range []string{"--intent is required", "non-fast-forward", "fetch first"} {
		if strings.Contains(reattachOut, forbidden) {
			t.Errorf("reattach output contains %q:\n%s", forbidden, reattachOut)
		}
	}
	if got := len(h.Runs()); got != runsBefore {
		t.Fatalf("reattach changed run count from %d to %d", runsBefore, got)
	}
	if !strings.Contains(reattachOut, "gate:") && !strings.Contains(reattachOut, "outcome:") {
		t.Fatalf("reattach neither parked at a gate nor produced an outcome:\n%s", reattachOut)
	}
}

// TestAxiStaleMonitorRerunBeforeSyncStrandsTheRecovery proves the claim the
// guidance makes about the WRONG order, which two earlier revisions shipped as
// the prescribed fallback: once `rerun` has created its pending run, that run
// is the newest one branchsync selects, it carries no push binding, so it owns
// the branch and the sync the agent was told to reach for is refused.
func TestAxiStaleMonitorRerunBeforeSyncStrandsTheRecovery(t *testing.T) {
	branch := "feature/stale-rerun-first"
	h, operator, pushedHead := staleMonitorFixture(t, branch)
	behindHead := strings.TrimSpace(h.WorktreeRefSHA(branch))

	rerunOut, err := h.RunInDir(operator, "rerun")
	if err != nil {
		t.Fatalf("rerun before sync: %v\n%s", err, rerunOut)
	}
	reran := h.ActiveRun(branch)
	if reran == nil {
		t.Fatalf("rerun left no active run:\n%s", rerunOut)
	}
	if reran.HeadSHA != pushedHead {
		t.Fatalf("reran run head = %s, want gate head %s", reran.HeadSHA, pushedHead)
	}

	syncOut, syncErr := h.RunInDir(operator, "axi", "sync")
	if syncErr == nil {
		t.Fatalf("sync after rerun should be refused:\n%s", syncOut)
	}
	// Exact lines, because `blocked_pipeline_owned_recoverable` - the terminal
	// custody classification of a different state - has the refused active
	// state's safety value as a prefix and would satisfy a substring match.
	for _, want := range []string{"state: pipeline_owned", "safety: blocked_pipeline_owned"} {
		if !containsExactLine(syncOut, want) {
			t.Fatalf("sync after rerun did not report the exact line %q:\n%s", want, syncOut)
		}
	}
	if got := strings.TrimSpace(h.WorktreeRefSHA(branch)); got != behindHead {
		t.Fatalf("refused sync moved the worktree to %s", got)
	}

	// And the run the agent was told to answer cannot be started from here
	// either: the clone is still behind, so the trigger push is rejected.
	runOut, runErr := h.RunInDir(operator, "axi", "run", "--intent", "answer the recovered run's gates")
	if runErr == nil {
		t.Fatalf("axi run from the stranded clone should fail:\n%s", runOut)
	}

	if out, err := h.RunInDir(operator, "axi", "abort", "--run", reran.ID); err != nil {
		t.Fatalf("abort stranded rerun: %v\n%s", err, out)
	}
	if run := h.WaitForRun(branch, 30*time.Second); run.Status != types.RunCancelled {
		t.Fatalf("stranded rerun status after abort = %s", run.Status)
	}
}

// containsExactLine matches a whole rendered field line, so a value that is a
// prefix of a sibling value cannot satisfy an assertion about the other.
func containsExactLine(out, want string) bool {
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == want {
			return true
		}
	}
	return false
}

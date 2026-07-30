//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestDraftUntilReadyJourney exercises the `--draft-until-ready` flag's
// process-crossing path end to end:
//
//   - real `no-mistakes init` + post-receive hook + daemon
//   - a real gate push carrying `--push-option
//     no-mistakes.draft-until-ready=true`
//   - the hook forwarding it to `daemon notify-push`
//   - HandlePushReceived stamping the policy through startRun
//   - the `runs.draft_until_ready` column
//
// and then the carry-forward rule: a second push on the same branch with no
// push option at all still runs under the policy, so the draft PR the first run
// opened cannot be orphaned unpublished by a plain re-push, a rerun, or the
// TUI's rerun action.
//
// A failure here means a wiring break between layers - the push option stops
// being advertised, the hook stops forwarding it, or the daemon stops stamping
// it - which the package-level unit tests cannot catch because each stubs out
// one boundary.
func TestDraftUntilReadyJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: writeDraftUntilReadyScenario(t)})

	if out, err := h.RunInDir(h.WorkDir, "init"); err != nil {
		t.Fatalf("nm init: %v\n%s", err, out)
	}

	branch := "feature/draft-until-ready"
	h.CommitChange(branch, "draft-target.txt", "first\n", "add draft target")
	h.PushToGateWithOptions(branch, "no-mistakes.draft-until-ready=true")

	first := h.WaitForRun(branch, 90*time.Second)
	if first.Status != types.RunCompleted {
		t.Fatalf("first run status = %q, want completed; error = %v", first.Status, first.Error)
	}
	if !readRunDraftUntilReady(t, h.NMHome, first.ID) {
		t.Fatal("runs.draft_until_ready is false; the push option never reached the run row")
	}

	// A plain re-push carries no push option. The policy belongs to the branch,
	// so this run must still be able to publish the draft the first one opened.
	h.CommitChange(branch, "draft-target.txt", "second\n", "amend draft target")
	h.PushToGate(branch)

	second := h.WaitForRun(branch, 90*time.Second)
	if second.Status != types.RunCompleted {
		t.Fatalf("second run status = %q, want completed; error = %v", second.Status, second.Error)
	}
	if second.ID == first.ID {
		t.Fatal("the second push did not create a new run")
	}
	if !readRunDraftUntilReady(t, h.NMHome, second.ID) {
		t.Fatal("a plain re-push lost the branch's draft-until-ready policy")
	}

	// A branch that never asked for the policy must not acquire it.
	other := "feature/ordinary"
	h.CommitChange(other, "ordinary.txt", "ordinary\n", "add ordinary target")
	h.PushToGate(other)

	ordinary := h.WaitForRun(other, 90*time.Second)
	if ordinary.Status != types.RunCompleted {
		t.Fatalf("ordinary run status = %q, want completed; error = %v", ordinary.Status, ordinary.Error)
	}
	if readRunDraftUntilReady(t, h.NMHome, ordinary.ID) {
		t.Fatal("an unrelated branch inherited the draft-until-ready policy")
	}
}

func readRunDraftUntilReady(t *testing.T, nmHome, runID string) bool {
	t.Helper()
	database, err := db.Open(paths.WithRoot(nmHome).DB())
	if err != nil {
		t.Fatalf("open e2e db: %v", err)
	}
	defer database.Close()
	run, err := database.GetRun(runID)
	if err != nil {
		t.Fatalf("get run %s: %v", runID, err)
	}
	if run == nil {
		t.Fatalf("run %s not in db", runID)
	}
	return run.DraftUntilReady
}

// writeDraftUntilReadyScenario returns the standard clean "no findings"
// response for every prompt, so the pipeline sails through to completion
// without needing approval.
func writeDraftUntilReadyScenario(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "draft_until_ready_scenario.yaml")
	content := `actions:
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
		t.Fatalf("write draft-until-ready scenario: %v", err)
	}
	return path
}

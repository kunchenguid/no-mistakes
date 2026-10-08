//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"

	toon "github.com/toon-format/toon-go"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestAxiBranchPublicationSnapshotJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: axiScenario(t)})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	branch := "feature/publication"
	h.CommitChange(branch, "feature.txt", "first change\n", "add feature")
	h.PushToGate(branch)
	first := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusAwaitingApproval, 90*time.Second)
	h.CommitChange(branch, "feature.txt", "second change\n", "update feature")
	h.PushToGate(branch)
	waitForRunIDStatus(t, h, first.ID, types.RunCancelled, 90*time.Second)
	second := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusAwaitingApproval, 90*time.Second)
	if second.ID == first.ID {
		t.Fatal("second push did not create a new run")
	}
	out, err := h.Run("axi", "status", "--branch", branch)
	if err != nil {
		t.Fatalf("parked snapshot: %v\n%s", err, out)
	}
	parked, err := toon.DecodeString(out)
	if err != nil {
		t.Fatal(err)
	}
	if parked.(map[string]any)["run"].(map[string]any)["current_step"] != "review" || strings.Contains(out, "command: no-mistakes axi respond") {
		t.Fatalf("explicit scope lost its phase or offered a mutation: %s", out)
	}
	h.Respond(second.ID, types.StepReview, types.ActionApprove)
	published := h.WaitForRun(branch, 90*time.Second)
	if published.Status != types.RunCompleted {
		t.Fatalf("publishing run: %s", published.Status)
	}
	pushed := h.UpstreamBranchSHA(branch)

	// The query must not depend on the branch currently checked out, or on a
	// branch-sync plan being different from the already synchronized state.
	h.Checkout("main")
	before := h.WorktreeRefSHA("HEAD")
	out, err = h.Run("axi", "status", "--branch", branch)
	if err != nil {
		t.Fatalf("publication snapshot: %v\n%s", err, out)
	}
	decoded, err := toon.DecodeString(out)
	if err != nil {
		t.Fatal(err)
	}
	doc := decoded.(map[string]any)
	run := doc["run"].(map[string]any)
	if doc["inventory_complete"] != true || doc["newest_run_id"] != second.ID || run["id"] != second.ID || run["branch"] != branch || run["outcome"] != "passed-with-skips" {
		t.Fatalf("wrong newest run or scope:\n%s", out)
	}
	if run["last_pushed_sha"] != pushed || run["reviewed_head_sha"] != pushed || run["push_ref"] != "refs/heads/"+branch || run["push_active"] != false || run["push_target_fingerprint"] == nil {
		t.Fatalf("missing publication provenance:\n%s", out)
	}
	inventory := doc["runs"].([]any)
	if len(inventory) != 2 || inventory[0].(map[string]any)["id"] != second.ID || inventory[1].(map[string]any)["id"] != first.ID || inventory[1].(map[string]any)["status"] != "cancelled" || len(doc["other_running_run_ids"].([]any)) != 0 {
		t.Fatalf("incomplete same-branch inventory:\n%s", out)
	}
	if rows := run["steps"].([]any); len(rows) != len(types.AllSteps()) {
		t.Fatalf("missing plan:\n%s", out)
	}
	if h.WorktreeRefSHA("HEAD") != before || h.UpstreamBranchSHA(branch) != pushed {
		t.Fatal("read-only snapshot changed a ref")
	}
}

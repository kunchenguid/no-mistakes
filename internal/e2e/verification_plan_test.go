//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The product under test is the real CLI/daemon/gate. The agent is a controlled
// process boundary: its invocation log proves precisely which bytes it received.
func TestVerificationPlanLaunchAndReattach(t *testing.T) {
	for _, proof := range []bool{false, true} {
		t.Run(fmt.Sprintf("proof=%t", proof), func(t *testing.T) {
			h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: writeReviewAgentsRoutingScenario(t)})
			if out, err := h.Run("init"); err != nil {
				t.Fatalf("isolated init: %v\n%s", err, out)
			}
			branch := "feature/verification-plan"
			head := h.CommitChange(branch, "feature.txt", "seed for review-agents routing\n", "add feature")
			source := filepath.Join(t.TempDir(), "plan.txt")
			original := []byte("  Plan café\r\nFailure: changed output.\r\nIndependent expected result: exact original bytes.\n\n")
			if err := os.WriteFile(source, original, 0600); err != nil {
				t.Fatal(err)
			}
			intent := "  Preserve this goal unchanged.\n\n"
			args := []string{"axi", "run", "--intent", intent, "--verification-plan", source}
			if proof {
				args = append(args, "--launch-nonce", "plan-launch", "--validation-generation", "generation")
			}
			if out, err := h.Run(args...); err != nil {
				t.Fatalf("launch: %v\n%s", err, out)
			}
			run := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusAwaitingApproval, 90*time.Second)
			plan := run.VerificationPlan
			if plan == nil || plan.SHA256 != fmt.Sprintf("%x", sha256.Sum256(original)) || plan.SourcePath != source || plan.CapturedAt == 0 {
				t.Fatalf("invalid attachment: %+v", plan)
			}
			database, err := db.OpenReadOnly(paths.WithRoot(h.NMHome).DB())
			if err != nil {
				t.Fatal(err)
			}
			stored, err := database.GetRun(run.ID)
			database.Close()
			if err != nil || stored.Intent == nil || *stored.Intent != intent || stored.HeadSHA != head {
				t.Fatalf("intent/head changed: %+v %v", stored, err)
			}
			if err := os.WriteFile(source, []byte("REPLACEMENT PLAN MUST NOT BE USED"), 0600); err != nil {
				t.Fatal(err)
			}
			if out, err := h.Run("axi", "run"); err != nil || !strings.Contains(out, run.ID) {
				t.Fatalf("reattach after edit: %v\n%s", err, out)
			}
			if out, err := h.Run(args...); err == nil || !strings.Contains(out, "accepted only when starting a new run") {
				t.Fatalf("replacement accepted: %v\n%s", err, out)
			}
			if err := os.Remove(source); err != nil {
				t.Fatal(err)
			}
			if out, err := h.Run("axi", "run"); err != nil || !strings.Contains(out, run.ID) {
				t.Fatalf("reattach after deletion: %v\n%s", err, out)
			}
			status, err := h.Run("axi", "status")
			if err != nil {
				t.Fatal(err)
			}
			for _, field := range []string{plan.Path, plan.SHA256, plan.SourcePath, "captured_at:"} {
				if !strings.Contains(status, field) {
					t.Fatalf("status lost %q:\n%s", field, status)
				}
			}
			h.RespondWithFindings(run.ID, types.StepReview, types.ActionFix, []string{"routing-check"})
			completed := h.WaitForRun(branch, 120*time.Second)
			if completed.Status != types.RunCompleted || completed.VerificationPlan == nil || *completed.VerificationPlan != *plan {
				t.Fatalf("snapshot lost after fix: %+v", completed)
			}
			snapshot, err := os.ReadFile(plan.Path)
			if err != nil || !bytes.Equal(snapshot, original) {
				t.Fatalf("snapshot changed: %q %v", snapshot, err)
			}
			review, test := false, false
			for _, inv := range h.AgentInvocations() {
				isReview := strings.Contains(inv.Prompt, reviewTurnMarker)
				isTest := strings.Contains(inv.Prompt, "You are validating a code change by driving the product itself")
				if !isReview && !isTest {
					continue
				}
				if !strings.Contains(inv.Prompt, string(original)) || strings.Contains(inv.Prompt, "REPLACEMENT PLAN MUST NOT BE USED") {
					t.Fatalf("consumer got different bytes: %s", inv.Prompt)
				}
				if !strings.Contains(inv.Prompt, "not user intent") || !strings.Contains(inv.Prompt, "independent expected result") {
					t.Fatal("attachment lacks evidence boundary")
				}
				review = review || isReview
				test = test || isTest
			}
			if !review || !test {
				t.Fatalf("missing consumer: review=%t test=%t", review, test)
			}
			t.Logf("run=%s sha256=%s exact intent and snapshot preserved through source edit, deletion, reattach and fix; Review/Test received original bytes", run.ID, plan.SHA256)
		})
	}
}

func TestVerificationPlanInvalidInputNeverTakesCustody(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("isolated init: %v\n%s", err, out)
	}
	branch := "feature/invalid-plan"
	head := h.CommitChange(branch, "feature.txt", "feature\n", "add feature")
	root := t.TempDir()
	empty := filepath.Join(root, "empty")
	denied := filepath.Join(root, "denied")
	if err := os.WriteFile(empty, []byte(" \n\t"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(denied, []byte("plan"), 0000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(denied, 0600) })
	inputs := []string{filepath.Join(root, "missing"), empty, root}
	if f, err := os.Open(denied); os.IsPermission(err) {
		inputs = append(inputs, denied)
	} else if err != nil {
		t.Fatal(err)
	} else {
		f.Close() // root and Windows can read mode-000 files.
	}
	for _, input := range inputs {
		out, err := h.Run("axi", "run", "--intent", "unchanged intent", "--verification-plan", input)
		if err == nil || !strings.Contains(out, "capture verification plan before push") {
			t.Fatalf("invalid source accepted: %s %v\n%s", input, err, out)
		}
		if runs := h.Runs(); len(runs) != 0 {
			t.Fatalf("invalid input created a run: %+v", runs)
		}
		gateDir := paths.WithRoot(h.NMHome).RepoDir(h.repoID())
		if out, err := h.runGit(context.Background(), gateDir, "show-ref", "--verify", "refs/heads/"+branch); err == nil {
			t.Fatalf("invalid input pushed branch: %s", out)
		}
		if got := h.WorktreeRefSHA("HEAD"); got != head {
			t.Fatalf("caller moved: %s", got)
		}
	}
	// Absence is explicit and opt-in guidance never reaches a plain run.
	if out, err := h.Run("axi", "run", "--intent", "validate without a plan"); err != nil {
		t.Fatalf("plain launch: %v\n%s", err, out)
	}
	run := h.WaitForRun(branch, 90*time.Second)
	if run.VerificationPlan != nil {
		t.Fatal("plain run inherited an attachment")
	}
	out, err := h.Run("axi", "status")
	if err != nil || !strings.Contains(out, "verification_plan: none") {
		t.Fatalf("absence: %v\n%s", err, out)
	}
	for _, inv := range h.AgentInvocations() {
		if strings.Contains(inv.Prompt, "Plan-aware verification guidance") {
			t.Fatal("plain run received plan-only guidance")
		}
	}
	// The gate already has this head, so push is a no-op and AXI must carry
	// the new attachment through its rerun RPC fallback.
	planFile := filepath.Join(root, "valid-plan")
	if err := os.WriteFile(planFile, []byte("fallback input bytes\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := h.Run("axi", "run", "--intent", "  exact fallback intent\n", "--verification-plan", planFile); err != nil {
		t.Fatalf("no-op fallback: %v\n%s", err, out)
	}
	attached := h.WaitForRun(branch, 90*time.Second)
	if attached.ID == run.ID || attached.VerificationPlan == nil {
		t.Fatalf("fallback reused run or lost attachment: %+v", attached)
	}
	data, err := os.ReadFile(attached.VerificationPlan.Path)
	if err != nil || string(data) != "fallback input bytes\n" {
		t.Fatalf("fallback snapshot: %q %v", data, err)
	}
}

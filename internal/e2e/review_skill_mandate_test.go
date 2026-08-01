//go:build e2e

package e2e

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestReviewSkillMandateFailsBeforeTheFirstReviewTurn is the counterpart of the
// harness seeding the mandated review skill: with the skill removed from an
// otherwise initialized HOME, the review step must fail with a setup error
// naming the skill, and must never spend a review turn to discover it.
//
// This is what keeps the harness seed honest. Without it, deleting
// writeMandatedReviewSkill would silently break every journey instead of this
// one test.
func TestReviewSkillMandateFailsBeforeTheFirstReviewTurn(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("nm init: %v\n%s", err, out)
	}
	if err := os.RemoveAll(h.MandatedReviewSkillDir()); err != nil {
		t.Fatalf("remove mandated review skill: %v", err)
	}

	branch := "review-skill-missing"
	h.CommitChange(branch, "internal/scm/github/github.go", "package github\n\n// changed\n", "touch scm")
	h.PushToGate(branch)

	run := h.WaitForRun(branch, 120*time.Second)
	if run.Status != types.RunFailed {
		t.Fatalf("run status = %s, want failed (error=%v)", run.Status, deref(run.Error))
	}
	if got := deref(run.Error); !strings.Contains(got, MandatedReviewSkill) || !strings.Contains(got, "not installed") {
		t.Errorf("run error = %q, want it to say the mandated review skill is not installed", got)
	}
	for _, inv := range h.AgentInvocations() {
		if strings.Contains(inv.Prompt, reviewStepPromptMarker) {
			t.Fatal("review turn ran despite the mandated skill being absent")
		}
	}
}

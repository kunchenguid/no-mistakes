//go:build e2e

package e2e

import (
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestRerunIntentProvenanceJourney drives the public CLI through both rerun
// defaults and both explicit overrides. The assertions inspect the persisted
// intent and the intent-step log so a rerun cannot silently switch from
// inheritance to transcript inference.
func TestRerunIntentProvenanceJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: writeIntentScenario(t)})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("nm init: %v\n%s", err, out)
	}

	explicit := "preserve the complete accepted requirements\nincluding exclusions and acceptance criteria\nwithout condensing them"
	explicitBranch := "feature/rerun-explicit"
	h.CommitChange(explicitBranch, "explicit.txt", "explicit\n", "add explicit fixture")
	explicitWT := h.AddWorktree(explicitBranch)
	h.PushToGate(explicitBranch)
	h.WaitForRun(explicitBranch, 90*time.Second)
	if out, err := h.RunInDir(explicitWT, "rerun", "--intent", " "); err == nil || !strings.Contains(out, "--intent must not be empty") {
		t.Fatalf("blank rerun intent should be rejected, err=%v output=%s", err, out)
	}
	originalExplicit := runCLIAndWait(t, h, explicitWT, explicitBranch, "rerun", "--intent", explicit)
	assertCompletedRerunFixture(t, h, originalExplicit, explicit, "agent")

	inherited := runCLIAndWait(t, h, explicitWT, explicitBranch, "rerun")
	assertCompletedRerunFixture(t, h, inherited, explicit, "rerun")
	if log := readStepLog(t, h, inherited.ID, "intent"); !strings.Contains(log, "using intent supplied by the agent") {
		t.Errorf("inherited explicit rerun should skip inference, log:\n%s", log)
	}

	overriddenExplicit := runCLIAndWait(t, h, explicitWT, explicitBranch, "rerun", "--intent", "replace the canonical requirements explicitly")
	assertCompletedRerunFixture(t, h, overriddenExplicit, "replace the canonical requirements explicitly", "agent")

	inferredBranch := "feature/rerun-inferred"
	seedClaudeTranscript(t, h.HomeDir, h.WorkDir, "inferred.txt")
	h.CommitChange(inferredBranch, "inferred.txt", "inferred\n", "add inferred fixture")
	inferredWT := h.AddWorktree(inferredBranch)
	h.PushToGate(inferredBranch)
	originalInferred := h.WaitForRun(inferredBranch, 90*time.Second)
	assertCompletedRerunFixture(t, h, originalInferred, "user wanted Bar() helper added", "claude")

	fresh := runCLIAndWait(t, h, inferredWT, inferredBranch, "rerun")
	originalInferredIntent := readRunIntent(t, h.NMHome, originalInferred.ID)
	if originalInferredIntent.summary == nil {
		t.Fatal("inferred original has no intent")
	}
	assertCompletedRerunFixture(t, h, fresh, *originalInferredIntent.summary, "claude")
	if log := readStepLog(t, h, fresh.ID, "intent"); !strings.Contains(log, "scanning recent agent transcripts") {
		t.Errorf("non-explicit rerun should perform fresh inference, log:\n%s", log)
	}

	overriddenInferred := runCLIAndWait(t, h, inferredWT, inferredBranch, "rerun", "--intent", "override inferred intent")
	assertCompletedRerunFixture(t, h, overriddenInferred, "override inferred intent", "agent")
}

func runCLIAndWait(t *testing.T, h *Harness, dir, branch string, args ...string) *ipc.RunInfo {
	t.Helper()
	out, err := h.RunInDir(dir, args...)
	if err != nil {
		t.Fatalf("%s: %v\n%s", strings.Join(args, " "), err, out)
	}
	if !strings.Contains(out, "Rerun started") {
		t.Fatalf("%s output missing rerun confirmation:\n%s", strings.Join(args, " "), out)
	}
	run := h.WaitForRun(branch, 90*time.Second)
	if run.Status != types.RunCompleted {
		t.Fatalf("%s status=%s error=%v", strings.Join(args, " "), run.Status, run.Error)
	}
	return run
}

func assertCompletedRerunFixture(t *testing.T, h *Harness, run *ipc.RunInfo, wantIntent, wantSource string) {
	t.Helper()
	if run.Status != types.RunCompleted {
		t.Fatalf("run %s status=%s, want completed", run.ID, run.Status)
	}
	intent := readRunIntent(t, h.NMHome, run.ID)
	if intent.summary == nil || *intent.summary != wantIntent {
		t.Fatalf("run %s intent=%v, want %q", run.ID, intent.summary, wantIntent)
	}
	if intent.source == nil || *intent.source != wantSource {
		t.Fatalf("run %s intent_source=%v, want %q", run.ID, intent.source, wantSource)
	}
}

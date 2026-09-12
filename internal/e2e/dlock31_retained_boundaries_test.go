//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestDLOCK31ExecutableRetainedRefusal(t *testing.T) {
	for _, variant := range []string{"malformed-binding", "unbound", "dirty", "moved-anchor", "moved-head", "divergent", "policy-changed"} {
		t.Run(variant, func(t *testing.T) {
			h, provider, database, parked, binding := parkedDLOCK31Repair(t)
			workdir := dlock31Workdir(t, database, parked.ID)
			beforeCalls := len(h.AgentInvocations())
			beforeEdits := dlock31ProviderEdits(t, provider)
			if beforeEdits == 0 { t.Fatal("provider edit counter did not observe original failure") }
			want := mutateDLOCK31Repair(t, h, database, binding, variant)
			beforeRounds := dlock31RoundCount(t, database, binding.StepID)
			beforeStatus := dlock31Git(t, h, workdir, "status", "--porcelain")
			beforeHead := dlock31Git(t, h, workdir, "rev-parse", "HEAD")
			respondDLOCK31Fix(t, h, parked)
			awaitDLOCK31(t, h, "settled refusal", func() bool {
				step, ok := findStep(h.RunInfo(parked.ID).Steps, types.StepCI)
				return ok && step.Status == types.StepStatusFixReview && dlock31RoundCount(t, database, binding.StepID) > beforeRounds && step.FindingsJSON != nil && strings.Contains(*step.FindingsJSON, want)
			})
			if len(h.AgentInvocations()) != beforeCalls || dlock31ProviderEdits(t, provider) != beforeEdits { t.Fatal("refusal invoked a fixer or attempted publication") }
			if h.UpstreamBranchSHA(binding.Branch) != binding.RecordedHead || dlock31Git(t, h, workdir, "rev-parse", "HEAD") != beforeHead { t.Fatal("refusal changed upstream or retained head") }
			if dlock31Git(t, h, workdir, "status", "--porcelain") != beforeStatus { t.Fatal("refusal altered owned worktree") }
		})
	}
}

func TestDLOCK31ExecutableAlreadyPublishedStaleMetadata(t *testing.T) {
	h, provider, database, parked, binding := parkedDLOCK31Repair(t)
	workdir := dlock31Workdir(t, database, parked.ID)
	dlock31Git(t, h, workdir, "push", h.UpstreamDir, binding.RetainedHead+":refs/heads/"+binding.Branch)
	if err := database.ClearRetainedCIRepair(parked.ID, binding.RetainedHead); err != nil { t.Fatal(err) }
	if err := os.Remove(filepath.Join(provider, "fail")); err != nil { t.Fatal(err) }
	setDLOCK31Actions(t, provider, strings.ReplaceAll(dlock31CIFixAction, "repaired\\n", "ordinary repair\\n"))
	stored, err := database.GetRun(parked.ID)
	if err != nil { t.Fatal(err) }
	if stored.HeadSHA != binding.RecordedHead || h.UpstreamBranchSHA(binding.Branch) != binding.RetainedHead { t.Fatal("did not establish published-head/stale-metadata precondition") }
	beforeCalls := len(h.AgentInvocations())
	respondDLOCK31Fix(t, h, parked)
	awaitDLOCK31(t, h, "ordinary repair published and settled", func() bool {
		current, err := database.GetRun(parked.ID)
		if err != nil { t.Fatal(err) }
		published := h.UpstreamBranchSHA(binding.Branch)
		return published != binding.RetainedHead && published != binding.RecordedHead && current.HeadSHA == published
	})
	head := h.UpstreamBranchSHA(binding.Branch)
	if len(h.AgentInvocations()) != beforeCalls+1 { t.Fatal("ordinary path did not invoke exactly one new fixer") }
	dlock31Git(t, h, workdir, "merge-base", "--is-ancestor", binding.RetainedHead, head)
	if got := dlock31Git(t, h, h.UpstreamDir, "show", head+":repair.txt"); got != "ordinary repair" { t.Fatalf("published content=%q", got) }
	if pending, err := database.RetainedCIRepair(parked.ID); err != nil || pending != nil { t.Fatalf("ordinary publication left binding: %+v %v", pending, err) }
}

func parkedDLOCK31Repair(t *testing.T) (*Harness, string, *db.DB, *ipc.RunInfo, *db.RetainedCIRepair) {
	t.Helper()
	h, provider := newDLOCK31Harness(t, dlock31CIFixAction)
	const branch = "feature/dlock31"
	h.CommitChange(branch, "repair.txt", "broken\n", "add fixture")
	h.PushToGate(branch)
	parked := waitForStepStatus(t, h, branch, types.StepCI, types.StepStatusAwaitingApproval, 60*time.Second)
	writeDLOCK31(t, filepath.Join(provider, "fail"), "fail edits only")
	respondDLOCK31Fix(t, h, parked)
	database, err := db.Open(paths.WithRoot(h.NMHome).DB())
	if err != nil { t.Fatal(err) }
	t.Cleanup(func() { _ = database.Close() })
	var binding *db.RetainedCIRepair
	awaitDLOCK31(t, h, "retained binding", func() bool {
		binding, err = database.RetainedCIRepair(parked.ID)
		if err != nil { t.Fatal(err) }
		return binding != nil
	})
	parked = waitForStepStatus(t, h, branch, types.StepCI, types.StepStatusFixReview, 30*time.Second)
	assertDLOCK31Retained(t, h, database, binding, parked)
	return h, provider, database, parked, binding
}

func awaitDLOCK31(t *testing.T, h *Harness, label string, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		if ready() { return }
		time.Sleep(100 * time.Millisecond)
	}
	h.dumpDebugState()
	t.Fatalf("timed out waiting for %s", label)
}

func dlock31Git(t *testing.T, h *Harness, dir string, args ...string) string {
	t.Helper()
	out, err := h.runGit(t.Context(), dir, args...)
	if err != nil { t.Fatalf("owned git %v: %v %s", args, err, out) }
	return strings.TrimSpace(string(out))
}

func dlock31RoundCount(t *testing.T, database *db.DB, step string) int {
	t.Helper()
	rounds, err := database.GetRoundsByStep(step)
	if err != nil { t.Fatal(err) }
	return len(rounds)
}

func dlock31ProviderEdits(t *testing.T, provider string) int {
	t.Helper()
	calls, err := os.ReadFile(filepath.Join(provider, "calls.jsonl"))
	if err != nil { t.Fatal(err) }
	return strings.Count(string(calls), `["pr","edit"`)
}

func dlock31Workdir(t *testing.T, database *db.DB, runID string) string {
	t.Helper()
	run, err := database.GetRun(runID)
	if err != nil || run == nil || run.WorktreePath() == "" { t.Fatalf("missing owned worktree: %+v %v", run, err) }
	return run.WorktreePath()
}

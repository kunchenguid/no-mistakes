//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestClosingIssueRefsSkippedPRIsRefused drives every way a run carrying
// --closes could finish without publishing a PR: the CLI rejects --skip pr up
// front, a push option pairing closes with skip=pr fails at the executor's
// pre-skip, and an empty diff after rebase fails instead of skipping the PR.
func TestClosingIssueRefsSkippedPRIsRefused(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude"})
	statePath := setupStatefulGitHub(t, h)
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}

	// axi run --closes with --skip pr is a usage error before any run starts.
	const cliBranch = "feature/closes-skip-cli"
	h.CommitChange(cliBranch, "c.txt", "c\n", "add cli skip fixture")
	wt := h.AddWorktree(cliBranch)
	out, err := h.RunInDir(wt, "axi", "run", "--intent", "skip", "--skip", "ci,pr", "--closes", "95")
	saveEvidence(t, "15-skip-pr-cli-rejected.txt", "$ no-mistakes axi run --skip ci,pr --closes 95\n"+out+"\nerr: "+errString(err)+"\n")
	if err == nil || !strings.Contains(out, "cannot be combined with --skip pr") {
		t.Fatalf("--closes with --skip pr should be refused, err=%v out:\n%s", err, out)
	}
	if runs := h.Runs(); len(runs) != 0 {
		t.Fatalf("refused --closes --skip pr started %d run(s)", len(runs))
	}
	h.RemoveWorktree(wt)

	// The push-option path reaches the executor's pre-skip branch.
	const optBranch = "feature/closes-skip-option"
	h.CommitChange(optBranch, "o.txt", "o\n", "add option skip fixture")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if out, err := h.runGit(ctx, h.WorkDir, "push", "-o", "no-mistakes.closes=95", "-o", "no-mistakes.skip=ci,pr", "no-mistakes", optBranch); err != nil {
		t.Fatalf("push: %v\n%s", err, out)
	}
	run := h.WaitForRun(optBranch, 3*time.Minute)
	pr, _ := findStep(run.Steps, types.StepPR)
	evidence := "$ git push -o no-mistakes.closes=95 -o no-mistakes.skip=ci,pr no-mistakes " + optBranch +
		"\nrun status: " + string(run.Status) + "\npr step status: " + string(pr.Status) + "\npr step error: " + deref(pr.Error) + "\n"
	saveEvidence(t, "16-skip-pr-push-option-fails.txt", evidence)
	if run.Status != types.RunFailed || pr.Status != types.StepStatusFailed || !strings.Contains(deref(pr.Error), "--closes requires publishing a pull request") {
		t.Fatalf("closes + skip=pr push option must fail the PR step; %s", evidence)
	}

	// Empty diff after rebase: the branch's change already landed on main.
	const emptyBranch = "feature/closes-empty-diff"
	h.CommitChange(emptyBranch, "e.txt", "e\n", "add empty-diff fixture")
	h.Checkout("main")
	if out, err := h.runGit(ctx, h.WorkDir, "cherry-pick", emptyBranch); err != nil {
		t.Fatalf("cherry-pick to main: %v\n%s", err, out)
	}
	if out, err := h.runGit(ctx, h.WorkDir, "push", "origin", "main"); err != nil {
		t.Fatalf("push main: %v\n%s", err, out)
	}
	ewt := h.AddWorktree(emptyBranch)
	out, _ = h.RunInDir(ewt, "axi", "run", "--intent", "already merged", "--skip", "ci", "--closes", "96")
	run = h.WaitForRun(emptyBranch, 3*time.Minute)
	pr, _ = findStep(run.Steps, types.StepPR)
	evidence = "$ no-mistakes axi run --skip ci --closes 96   # branch change already on main\n" + out +
		"\nrun status: " + string(run.Status) + "\npr step status: " + string(pr.Status) + "\npr step error: " + deref(pr.Error) + "\n"
	for _, s := range run.Steps {
		evidence += "step " + string(s.StepName) + ": " + string(s.Status) + "\n"
	}
	saveEvidence(t, "17-empty-diff-closes-fails.txt", evidence)
	if run.Status != types.RunFailed || pr.Status != types.StepStatusFailed || !strings.Contains(deref(pr.Error), "--closes requires publishing a pull request") {
		t.Fatalf("empty-diff run with --closes must fail the PR step; %s", evidence)
	}
	if strings.Contains(readStatefulGHRaw(statePath), "#96") {
		t.Errorf("an empty-diff run published a closing reference")
	}
}

func errString(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}

// readStatefulGHRaw returns every PR body the stub recorded, or "" when no PR
// was ever created.
func readStatefulGHRaw(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var state statefulGHState
	_ = json.Unmarshal(data, &state)
	var b strings.Builder
	for _, pr := range state.PRs {
		b.WriteString(pr.Body)
	}
	return b.String()
}

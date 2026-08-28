package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

// setupReviewBaseRepo creates the graph that exposed the review-base defect:
// main and feature/hardware diverge, while the run branch is based on the
// configured integration branch. The hardware file is inherited from the
// configured branch and must not be presented as a change to review.
func setupReviewBaseRepo(t *testing.T) (dir, integrationSHA, headSHA string) {
	t.Helper()

	dir = t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	gitCmd(t, dir, "remote", "add", "origin", dir)

	writeReviewBaseFile(t, dir, "root.txt", "root\n")
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "root")
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "feature/hardware")
	writeReviewBaseFile(t, dir, "hardware-base.txt", "hardware baseline\n")
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "hardware integration base")
	integrationSHA = gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "feature/hardware")

	gitCmd(t, dir, "checkout", "-b", "feature/review-base", "feature/hardware")
	writeReviewBaseFile(t, dir, "feature-change.txt", "feature change\n")
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature change")
	headSHA = gitCmd(t, dir, "rev-parse", "HEAD")

	gitCmd(t, dir, "checkout", "main")
	writeReviewBaseFile(t, dir, filepath.Join("application", "unrelated.go"), "package application\n")
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "unrelated application history")
	gitCmd(t, dir, "push", "origin", "main")
	gitCmd(t, dir, "checkout", "feature/review-base")

	return dir, integrationSHA, headSHA
}

func writeReviewBaseFile(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func cleanReviewFindings() *agent.Result {
	return &agent.Result{Output: json.RawMessage(`{"findings":[],"risk_level":"low","risk_rationale":"clean","risk_scope":"source-or-external"}`)}
}

func reviewBaseContext(t *testing.T, ag agent.Agent, dir, integrationSHA, headSHA string) *pipeline.StepContext {
	t.Helper()
	sctx := newTestContextWithDBRecords(t, ag, dir, integrationSHA, headSHA, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature/review-base"
	sctx.Repo.DefaultBranch = "main"
	return sctx
}

func reviewBaseRules() []config.PathInstruction {
	return []config.PathInstruction{
		{Path: "hardware-base.txt", Instructions: "hardware inherited rule must not be selected"},
		{Path: "feature-change.txt", Instructions: "feature changed rule is selected"},
	}
}

func TestReviewStep_UsesConfiguredIntegrationBaseForScopeAndWorkload(t *testing.T) {
	t.Parallel()
	dir, integrationSHA, headSHA := setupReviewBaseRepo(t)
	ag := &mockAgent{name: "review", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		return cleanReviewFindings(), nil
	}}
	sctx := reviewBaseContext(t, ag, dir, integrationSHA, headSHA)
	// A new branch has no prior push base. The review must still derive its
	// merge-base from the configured target rather than treating the zero SHA
	// as a usable commit.
	sctx.Run.BaseSHA = strings.Repeat("0", 40)
	sctx.Config.PR.BaseBranch = "feature/hardware"
	sctx.Config.Review.PathInstructions = reviewBaseRules()

	if _, err := (&ReviewStep{}).Execute(sctx); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(ag.calls) != 1 {
		t.Fatalf("agent calls = %d, want 1", len(ag.calls))
	}
	call := ag.calls[0]
	if !strings.Contains(call.Prompt, "base commit: "+integrationSHA) {
		t.Fatalf("review prompt does not use configured integration base %s:\n%s", integrationSHA, call.Prompt)
	}
	if !strings.Contains(call.Prompt, "integration base branch: feature/hardware") {
		t.Fatalf("review prompt does not name configured integration branch:\n%s", call.Prompt)
	}
	if !strings.Contains(call.Prompt, "feature changed rule is selected") {
		t.Fatalf("review prompt omitted the configured branch's changed path:\n%s", call.Prompt)
	}
	if strings.Contains(call.Prompt, "hardware inherited rule must not be selected") {
		t.Fatalf("review prompt included a path inherited from the configured base:\n%s", call.Prompt)
	}
	if call.Workload == nil {
		t.Fatal("review workload is nil")
	}
	if call.Workload.Files != 1 || call.Workload.Lines != 1 {
		t.Fatalf("review workload = %+v, want one changed file and line", *call.Workload)
	}
}

func TestReviewStep_DefaultsToRepositoryDefaultBranch(t *testing.T) {
	t.Parallel()
	dir, integrationSHA, headSHA := setupReviewBaseRepo(t)
	ag := &mockAgent{name: "review", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		return cleanReviewFindings(), nil
	}}
	sctx := reviewBaseContext(t, ag, dir, integrationSHA, headSHA)
	sctx.Config.Review.PathInstructions = reviewBaseRules()

	if _, err := (&ReviewStep{}).Execute(sctx); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if len(ag.calls) != 1 {
		t.Fatalf("agent calls = %d, want 1", len(ag.calls))
	}
	call := ag.calls[0]
	mainSHA := gitCmd(t, dir, "merge-base", "HEAD", "main")
	if !strings.Contains(call.Prompt, "base commit: "+mainSHA) {
		t.Fatalf("review prompt does not fall back to repository default merge-base %s:\n%s", mainSHA, call.Prompt)
	}
	if !strings.Contains(call.Prompt, "hardware inherited rule must not be selected") {
		t.Fatalf("default-branch review did not include the inherited hardware path:\n%s", call.Prompt)
	}
	if call.Workload == nil || call.Workload.Files != 2 {
		t.Fatalf("default-branch workload = %+v, want two changed files", call.Workload)
	}
}

func TestReviewStep_FixingAndRereviewUseConfiguredIntegrationBase(t *testing.T) {
	t.Parallel()
	dir, integrationSHA, headSHA := setupReviewBaseRepo(t)
	calls := 0
	ag := &mockAgent{name: "review", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		calls++
		if calls == 1 {
			writeReviewBaseFile(t, dir, "review-fix.txt", "fix\n")
			return &agent.Result{Output: json.RawMessage(`{"summary":"apply review fix"}`)}, nil
		}
		return cleanReviewFindings(), nil
	}}
	sctx := reviewBaseContext(t, ag, dir, integrationSHA, headSHA)
	sctx.Config.PR.BaseBranch = "feature/hardware"
	sctx.Config.Review.PathInstructions = reviewBaseRules()
	sctx.Fixing = true
	sctx.PreviousFindings = `{"findings":[{"severity":"warning","action":"auto-fix","file":"feature-change.txt","description":"fix it"}],"summary":"one finding"}`

	if _, err := (&ReviewStep{}).Execute(sctx); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if calls != 2 {
		t.Fatalf("agent calls = %d, want fix and rereview", calls)
	}
	for i, call := range ag.calls {
		if !strings.Contains(call.Prompt, "base commit: "+integrationSHA) {
			t.Errorf("call %d does not use configured integration base %s:\n%s", i+1, integrationSHA, call.Prompt)
		}
		if !strings.Contains(call.Prompt, "feature/hardware") {
			t.Errorf("call %d does not name configured integration branch:\n%s", i+1, call.Prompt)
		}
		if strings.Contains(call.Prompt, "hardware inherited rule must not be selected") {
			t.Errorf("call %d included inherited hardware scope:\n%s", i+1, call.Prompt)
		}
	}
	if ag.calls[1].Workload == nil || ag.calls[1].Workload.Files != 1 {
		t.Fatalf("rereview workload = %+v, want original one-file workload", ag.calls[1].Workload)
	}
}

func TestReviewStep_ConfiguredIntegrationBaseResolutionFailureIsActionable(t *testing.T) {
	t.Parallel()
	dir, integrationSHA, headSHA := setupReviewBaseRepo(t)
	ag := &mockAgent{name: "review", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		t.Fatal("review agent should not run when its integration base is unavailable")
		return nil, nil
	}}
	sctx := reviewBaseContext(t, ag, dir, integrationSHA, headSHA)
	sctx.Config.PR.BaseBranch = "feature/missing"

	_, err := (&ReviewStep{}).Execute(sctx)
	if err == nil {
		t.Fatal("expected configured integration base resolution to fail")
	}
	message := err.Error()
	if !strings.Contains(message, "feature/missing") || !strings.Contains(message, "review integration base") {
		t.Fatalf("error = %q, want actionable configured-base failure", message)
	}
	if strings.Contains(message, "reviewing against main") {
		t.Fatalf("error claims an unsafe fallback: %q", message)
	}
}

func TestEffectivePRBaseBranch_UsesTheSameSelectionAsRebaseAndPR(t *testing.T) {
	t.Parallel()
	sctx := &pipeline.StepContext{
		Repo:   &db.Repo{DefaultBranch: "main"},
		Config: &config.Config{PR: config.PR{BaseBranch: "feature/hardware"}},
	}
	if got := effectivePRBaseBranch(sctx); got != "feature/hardware" {
		t.Fatalf("effective base = %q, want feature/hardware", got)
	}
	sctx.Config.PR.BaseBranch = ""
	if got := effectivePRBaseBranch(sctx); got != "main" {
		t.Fatalf("fallback effective base = %q, want main", got)
	}
}

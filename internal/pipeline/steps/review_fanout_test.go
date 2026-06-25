package steps

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// reviewReturning builds a mockAgent runFn that returns the given findings as
// the agent's structured review output.
func reviewReturning(f Findings) func(context.Context, agent.RunOpts) (*agent.Result, error) {
	return func(_ context.Context, _ agent.RunOpts) (*agent.Result, error) {
		j, _ := json.Marshal(f)
		return &agent.Result{Output: j}, nil
	}
}

func findingBySource(items []Finding, source string) (Finding, bool) {
	for _, item := range items {
		if item.Source == source {
			return item, true
		}
	}
	return Finding{}, false
}

func TestReviewStep_FanOut_InitialReviewMergesBothReviewers(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	codex := &mockAgent{name: "codex", runFn: reviewReturning(Findings{
		Items:         []Finding{{Severity: "warning", Description: "codex issue", Action: "auto-fix"}},
		RiskLevel:     "medium",
		RiskRationale: "codex rationale",
		Summary:       "codex summary",
	})}
	claude := &mockAgent{name: "claude", runFn: reviewReturning(Findings{
		Items:         []Finding{{Severity: "error", Description: "claude issue", Action: "ask-user"}},
		RiskLevel:     "high",
		RiskRationale: "claude rationale",
		Summary:       "claude summary",
	})}
	// The fix/implementation agent must never run during an initial review.
	fixAgent := &mockAgent{name: "fixer", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		t.Fatal("fix agent must not run during the review pass")
		return nil, nil
	}}

	sctx := newTestContext(t, fixAgent, dir, baseSHA, headSHA, config.Commands{})
	sctx.Reviewers = []agent.Agent{codex, claude}

	step := &ReviewStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}

	merged, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if len(merged.Items) != 2 {
		t.Fatalf("expected 2 merged findings, got %d: %+v", len(merged.Items), merged.Items)
	}

	codexFinding, ok := findingBySource(merged.Items, "codex")
	if !ok {
		t.Fatal("expected a finding sourced from codex")
	}
	if codexFinding.ID != "review-codex-1" {
		t.Errorf("codex finding id = %q, want review-codex-1", codexFinding.ID)
	}
	claudeFinding, ok := findingBySource(merged.Items, "claude")
	if !ok {
		t.Fatal("expected a finding sourced from claude")
	}
	if claudeFinding.ID != "review-claude-1" {
		t.Errorf("claude finding id = %q, want review-claude-1", claudeFinding.ID)
	}

	// RiskLevel is the max across reviewers; an error finding needs approval.
	if merged.RiskLevel != "high" {
		t.Errorf("merged RiskLevel = %q, want high", merged.RiskLevel)
	}
	if !outcome.NeedsApproval {
		t.Error("expected NeedsApproval when a reviewer reports an error finding")
	}

	// Each reviewer ran exactly once, with streaming disabled in panel mode.
	if len(codex.calls) != 1 || len(claude.calls) != 1 {
		t.Fatalf("expected each reviewer to run once, got codex=%d claude=%d", len(codex.calls), len(claude.calls))
	}
	if codex.calls[0].OnChunk != nil || claude.calls[0].OnChunk != nil {
		t.Error("expected OnChunk to be nil in panel mode (not goroutine-safe)")
	}
}

func TestReviewStep_FanOut_SameFamilyReviewersHaveDistinctSourcesAndIDs(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	first := &mockAgent{name: "codex", runFn: reviewReturning(Findings{
		Items: []Finding{{ID: "same", Severity: "warning", Description: "first codex", Action: "auto-fix"}},
	})}
	second := &mockAgent{name: "codex", runFn: reviewReturning(Findings{
		Items: []Finding{{ID: "same", Severity: "warning", Description: "second codex", Action: "auto-fix"}},
	})}
	fixAgent := &mockAgent{name: "fixer"}

	sctx := newTestContext(t, fixAgent, dir, baseSHA, headSHA, config.Commands{})
	sctx.Reviewers = []agent.Agent{first, second}

	step := &ReviewStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}

	merged, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if len(merged.Items) != 2 {
		t.Fatalf("expected 2 merged findings, got %d: %+v", len(merged.Items), merged.Items)
	}
	wantIDs := []string{"review-codex-1", "review-codex-2-1"}
	wantSources := []string{"codex", "codex-2"}
	for i, item := range merged.Items {
		if item.ID != wantIDs[i] {
			t.Errorf("item %d ID = %q, want %q", i, item.ID, wantIDs[i])
		}
		if item.Source != wantSources[i] {
			t.Errorf("item %d Source = %q, want %q", i, item.Source, wantSources[i])
		}
	}
}

func TestReviewStep_ConfiguredSingleReviewerGetsPanelAttribution(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	codex := &mockAgent{name: "codex", runFn: reviewReturning(Findings{
		Items: []Finding{{ID: "model-id", Severity: "warning", Description: "codex issue", Action: "auto-fix"}},
	})}
	fixAgent := &mockAgent{name: "fixer"}

	sctx := newTestContext(t, fixAgent, dir, baseSHA, headSHA, config.Commands{})
	sctx.Reviewers = []agent.Agent{codex}
	sctx.Config.Review.Reviewers = []config.ReviewerSpec{{Agent: types.AgentCodex}}

	step := &ReviewStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}

	merged, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if len(merged.Items) != 1 {
		t.Fatalf("expected 1 finding, got %d: %+v", len(merged.Items), merged.Items)
	}
	if got := merged.Items[0].Source; got != "codex" {
		t.Errorf("Source = %q, want codex", got)
	}
	if got := merged.Items[0].ID; got != "review-codex-1" {
		t.Errorf("ID = %q, want review-codex-1", got)
	}
	if len(codex.calls) != 1 {
		t.Fatalf("expected codex reviewer to run once, got %d", len(codex.calls))
	}
	if codex.calls[0].OnChunk != nil {
		t.Error("expected configured one-reviewer panel to disable streaming callbacks")
	}
}

func TestReviewStep_FanOut_RunsInFixMode(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	fixAgent := &mockAgent{name: "fixer", runFn: func(_ context.Context, _ agent.RunOpts) (*agent.Result, error) {
		os.WriteFile(filepath.Join(dir, "fanout-fix.txt"), []byte("fixed"), 0o644)
		return &agent.Result{Output: json.RawMessage(`{"summary":"address findings"}`)}, nil
	}}
	codex := &mockAgent{name: "codex", runFn: reviewReturning(Findings{
		Items: []Finding{{Severity: "warning", Description: "codex issue", Action: "auto-fix"}},
	})}
	claude := &mockAgent{name: "claude", runFn: reviewReturning(Findings{
		Items: []Finding{{Severity: "info", Description: "claude note", Action: "no-op"}},
	})}

	sctx := newTestContextWithDBRecords(t, fixAgent, dir, baseSHA, headSHA, config.Commands{})
	sctx.Reviewers = []agent.Agent{codex, claude}
	sctx.Fixing = true
	sctx.PreviousFindings = `{"findings":[{"id":"review-1","severity":"warning","description":"earlier","action":"auto-fix"}],"summary":"1 issue"}`

	step := &ReviewStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}

	// The single fix agent ran once; the full panel re-reviewed the fixed code.
	if len(fixAgent.calls) != 1 {
		t.Errorf("expected fix agent to run once, got %d", len(fixAgent.calls))
	}
	if len(codex.calls) != 1 || len(claude.calls) != 1 {
		t.Fatalf("expected each reviewer to re-review once in fix mode, got codex=%d claude=%d", len(codex.calls), len(claude.calls))
	}
	if outcome.FixSummary != "address findings" {
		t.Errorf("FixSummary = %q, want 'address findings'", outcome.FixSummary)
	}

	merged, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := findingBySource(merged.Items, "codex"); !ok {
		t.Error("expected a codex-sourced finding after fix-mode re-review")
	}
	if _, ok := findingBySource(merged.Items, "claude"); !ok {
		t.Error("expected a claude-sourced finding after fix-mode re-review")
	}
}

func TestReviewStep_FanOut_FailClosedFailsStep(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	codex := &mockAgent{name: "codex", runFn: reviewReturning(Findings{
		Items: []Finding{{Severity: "warning", Description: "codex issue", Action: "auto-fix"}},
	})}
	claude := &mockAgent{name: "claude", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		return nil, errors.New("reviewer crashed")
	}}
	fixAgent := &mockAgent{name: "fixer"}

	sctx := newTestContext(t, fixAgent, dir, baseSHA, headSHA, config.Commands{})
	sctx.Reviewers = []agent.Agent{codex, claude}
	// Config.Review.FailOpen defaults to false (fail-closed).

	step := &ReviewStep{}
	_, err := step.Execute(sctx)
	if err == nil {
		t.Fatal("expected the step to fail closed when a reviewer errors")
	}
	if !strings.Contains(err.Error(), "claude") {
		t.Errorf("error should name the failed reviewer family, got %q", err)
	}
}

func TestReviewStep_FanOut_FailOpenContinues(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	codex := &mockAgent{name: "codex", runFn: reviewReturning(Findings{
		Items:     []Finding{{Severity: "warning", Description: "codex issue", Action: "auto-fix"}},
		RiskLevel: "medium",
	})}
	claude := &mockAgent{name: "claude", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		return nil, errors.New("reviewer crashed")
	}}
	fixAgent := &mockAgent{name: "fixer"}

	sctx := newTestContext(t, fixAgent, dir, baseSHA, headSHA, config.Commands{})
	sctx.Reviewers = []agent.Agent{codex, claude}
	sctx.Config.Review.FailOpen = true

	step := &ReviewStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("fail-open should survive a single reviewer error: %v", err)
	}
	merged, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if len(merged.Items) != 1 {
		t.Fatalf("expected only the surviving reviewer's finding, got %d", len(merged.Items))
	}
	if merged.Items[0].Source != "codex" {
		t.Errorf("surviving finding source = %q, want codex", merged.Items[0].Source)
	}
}

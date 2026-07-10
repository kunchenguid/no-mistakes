package steps

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// newHousekeepingContext builds a StepContext with a Shared scope, matching
// how the executor wires steps at runtime.
func newHousekeepingContext(t *testing.T, ag agent.Agent, workDir, baseSHA, headSHA string, cmds config.Commands) *pipeline.StepContext {
	t.Helper()
	sctx := newTestContextWithDBRecords(t, ag, workDir, baseSHA, headSHA, cmds)
	sctx.Shared = &pipeline.RunShared{}
	return sctx
}

// TestDocumentStep_CombinedPassCoversBothDutiesAndSplitsFindings proves the
// combined pass asks for both duties in ONE invocation and routes each
// finding category to its owning gate: documentation findings park the
// document step, lint findings are stashed for the lint step.
func TestDocumentStep_CombinedPassCoversBothDutiesAndSplitsFindings(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{
				"findings":[
					{"severity":"warning","description":"config docs conflict","action":"ask-user","category":"documentation"},
					{"severity":"warning","description":"unfixable vet warning","action":"ask-user","category":"lint"}
				],
				"summary":"housekeeping pass"
			}`)}, nil
		},
	}
	sctx := newHousekeepingContext(t, ag, dir, baseSHA, headSHA, config.Commands{})

	step := &DocumentStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}

	// One invocation covered both duties.
	if len(ag.calls) != 1 {
		t.Fatalf("combined pass must be one agent invocation, got %d", len(ag.calls))
	}
	prompt := ag.calls[0].Prompt
	if !strings.Contains(prompt, "Combined lint duty") {
		t.Fatalf("combined prompt missing the lint duty:\n%s", prompt)
	}
	if !strings.Contains(prompt, "Discover the configured linters and formatters") {
		t.Fatal("combined prompt lost the lint discovery responsibility")
	}
	if !strings.Contains(prompt, "exactly one authoritative owner document") {
		t.Fatal("combined prompt lost the documentation placement policy")
	}

	// Document gate sees only the documentation finding.
	var docFindings Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &docFindings); err != nil {
		t.Fatalf("unmarshal document findings: %v", err)
	}
	if len(docFindings.Items) != 1 || docFindings.Items[0].Description != "config docs conflict" {
		t.Fatalf("document gate findings = %+v, want only the documentation finding", docFindings.Items)
	}
	if !outcome.NeedsApproval {
		t.Fatal("documentation finding must park the document step")
	}

	// The lint half is stashed for the lint step.
	stash, ok := sctx.Shared.TakeHousekeepingLint()
	if !ok {
		t.Fatal("combined pass must stash the lint result")
	}
	lintFindings, err := types.ParseFindingsJSON(stash.FindingsJSON)
	if err != nil {
		t.Fatalf("parse stashed lint findings: %v", err)
	}
	if len(lintFindings.Items) != 1 || lintFindings.Items[0].Description != "unfixable vet warning" {
		t.Fatalf("stashed lint findings = %+v, want only the lint finding", lintFindings.Items)
	}
}

// TestDocumentStep_ConfiguredLintCommandKeepsDocOnlyPrompt proves the
// combined duty is only merged when lint would otherwise need its own agent
// pass: with commands.lint configured the document prompt stays doc-only and
// nothing is stashed.
func TestDocumentStep_ConfiguredLintCommandKeepsDocOnlyPrompt(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"docs current"}`)}, nil
		},
	}
	sctx := newHousekeepingContext(t, ag, dir, baseSHA, headSHA, config.Commands{Lint: "true"})

	step := &DocumentStep{}
	if _, err := step.Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ag.calls[0].Prompt, "Combined lint duty") {
		t.Fatal("configured lint command must keep the document prompt doc-only")
	}
	if _, ok := sctx.Shared.TakeHousekeepingLint(); ok {
		t.Fatal("nothing must be stashed when the lint command path is configured")
	}
}

func TestDocumentStep_ConfiguredLintCommandKeepsLintCategorizedFindingInDocumentGate(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{
				"findings":[{"severity":"warning","description":"documentation needs a decision","action":"ask-user","category":"lint"}],
				"summary":"documentation needs review"
			}`)}, nil
		},
	}
	sctx := newHousekeepingContext(t, ag, dir, baseSHA, headSHA, config.Commands{Lint: "true"})

	outcome, err := (&DocumentStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("a doc-only pass must keep every finding in the document gate")
	}
	var findings Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
		t.Fatalf("unmarshal document findings: %v", err)
	}
	if len(findings.Items) != 1 || findings.Items[0].Description != "documentation needs a decision" {
		t.Fatalf("document findings = %+v, want the categorized finding retained", findings.Items)
	}
	if _, ok := sctx.Shared.TakeHousekeepingLint(); ok {
		t.Fatal("a deterministic lint command must not receive a document-pass stash")
	}
}

// TestLintStep_ConsumesCombinedResultWithoutAgentPass proves the lint step
// reports the combined pass's lint findings with its own gate semantics and
// pays no second agent invocation.
func TestLintStep_ConsumesCombinedResultWithoutAgentPass(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			t.Error("lint step must not invoke the agent when a combined result exists")
			return &agent.Result{}, nil
		},
	}
	sctx := newHousekeepingContext(t, ag, dir, baseSHA, headSHA, config.Commands{})

	cases := []struct {
		name          string
		findings      string
		needsApproval bool
	}{
		{
			name:          "blocking lint finding parks",
			findings:      `{"findings":[{"severity":"warning","description":"vet warning","action":"ask-user","category":"lint"}],"summary":"1 lint issue"}`,
			needsApproval: true,
		},
		{
			name:          "clean lint result passes",
			findings:      `{"findings":[],"summary":"lint clean"}`,
			needsApproval: false,
		},
		{
			name:          "info-only lint finding passes",
			findings:      `{"findings":[{"severity":"info","description":"style note","action":"no-op","category":"lint"}],"summary":"note"}`,
			needsApproval: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sctx.Shared.SetHousekeepingLint(pipeline.HousekeepingLintResult{FindingsJSON: tc.findings, Summary: "housekeeping"})
			outcome, err := (&LintStep{}).Execute(sctx)
			if err != nil {
				t.Fatal(err)
			}
			if outcome.NeedsApproval != tc.needsApproval {
				t.Fatalf("NeedsApproval = %v, want %v", outcome.NeedsApproval, tc.needsApproval)
			}
			if outcome.AutoFixable {
				t.Fatal("combined lint result must not enter the auto-fix loop")
			}
			if outcome.Findings != tc.findings {
				t.Fatalf("lint findings = %s, want the stashed result", outcome.Findings)
			}
		})
	}
}

// TestLintStep_RunsOwnPassWithoutCombinedResult proves the lint duty is
// never silently dropped: with no stashed result (document step skipped or
// failed to produce trustworthy output) the lint step runs its own pass.
func TestLintStep_RunsOwnPassWithoutCombinedResult(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"lint clean"}`)}, nil
		},
	}
	sctx := newHousekeepingContext(t, ag, dir, baseSHA, headSHA, config.Commands{})

	outcome, err := (&LintStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) != 1 {
		t.Fatalf("lint step must run its own agent pass when nothing was stashed, got %d calls", len(ag.calls))
	}
	if outcome.NeedsApproval {
		t.Fatal("clean lint pass must not park")
	}
}

func TestDocumentStep_CombinedPassInvalidatesPriorLintResultWhenOutputIsUntrusted(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	calls := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			calls++
			if calls == 1 {
				return &agent.Result{Output: json.RawMessage(`{"findings":[{"severity":"warning","description":"docs need a decision","action":"ask-user","category":"documentation"}],"summary":"docs need review"}`)}, nil
			}
			return &agent.Result{Text: "untrusted output"}, nil
		},
	}
	sctx := newHousekeepingContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	step := &DocumentStep{}

	if _, err := step.Execute(sctx); err != nil {
		t.Fatal(err)
	}

	sctx.Fixing = true
	if _, err := step.Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := sctx.Shared.TakeHousekeepingLint(); ok {
		t.Fatal("untrusted combined rerun must not leave the prior lint result available")
	}
}

// TestLintStep_FixRoundReassessesWithOwnAgentPass proves a user-driven lint
// fix round does not trust the stale combined result: the fix turn runs the
// lint agent itself.
func TestLintStep_FixRoundReassessesWithOwnAgentPass(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"fixed lint"}`)}, nil
		},
	}
	sctx := newHousekeepingContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Shared.SetHousekeepingLint(pipeline.HousekeepingLintResult{FindingsJSON: `{"findings":[],"summary":"stale"}`, Summary: "stale"})
	sctx.Fixing = true
	sctx.PreviousFindings = `{"findings":[{"id":"l-1","severity":"warning","description":"vet warning","action":"auto-fix"}],"summary":"1 issue"}`

	if _, err := (&LintStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) != 1 {
		t.Fatalf("fix round must run the lint agent, got %d calls", len(ag.calls))
	}
}

// TestPipeline_DocumentPlusLintIsOneAgentInvocation is the cold-start
// regression: driving the real document and lint steps through the executor
// with agent-driven lint used to cost two cold agent passes; the combined
// pass must cost exactly one.
func TestPipeline_DocumentPlusLintIsOneAgentInvocation(t *testing.T) {
	workDir, baseSHA, headSHA := setupGitRepo(t)

	database, err := db.Open(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	repo, err := database.InsertRepo(workDir, "https://github.com/test/repo", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	run, err := database.InsertRun(repo.ID, "refs/heads/feature", headSHA, baseSHA)
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}

	calls := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			calls++
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"housekeeping clean"}`)}, nil
		},
	}

	cfg := &config.Config{Agent: types.AgentClaude}
	exec := pipeline.NewExecutor(database, paths.WithRoot(t.TempDir()), cfg, ag, []pipeline.Step{&DocumentStep{}, &LintStep{}}, nil)
	if err := exec.Execute(context.Background(), run, repo, workDir); err != nil {
		t.Fatalf("execute: %v", err)
	}

	if calls != 1 {
		t.Fatalf("document+lint cost %d agent invocations, want 1 (combined housekeeping pass)", calls)
	}

	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatalf("get steps: %v", err)
	}
	for _, s := range steps {
		if s.Status != types.StepStatusCompleted {
			t.Fatalf("step %s = %s, want completed", s.StepName, s.Status)
		}
	}
}

// TestPipeline_ConfiguredLintCommandStaysFirstClassGate proves a configured
// deterministic lint command still runs and its failure still parks the lint
// step, unchanged by the combined pass.
func TestPipeline_ConfiguredLintCommandStaysFirstClassGate(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"noop"}`)}, nil
		},
	}
	sctx := newHousekeepingContext(t, ag, dir, baseSHA, headSHA, config.Commands{Lint: "exit 3"})

	outcome, err := (&LintStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("failing configured lint command must stay a first-class gate failure")
	}
	if !outcome.AutoFixable {
		t.Fatal("failing configured lint command must stay auto-fixable")
	}
	if outcome.ExitCode != 3 {
		t.Fatalf("exit code = %d, want 3", outcome.ExitCode)
	}
}

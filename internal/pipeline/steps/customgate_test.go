package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func stepNames(steps []pipeline.Step) []string {
	names := make([]string, 0, len(steps))
	for _, step := range steps {
		names = append(names, string(step.Name()))
	}
	return names
}

// The core sequence must survive gate insertion intact: same members, same
// relative order. A gate can lengthen a run, never reshape it.
func TestWithCustomGates_InsertsAfterAnchorAndPreservesCore(t *testing.T) {
	got := stepNames(WithCustomGates(AllSteps(), []config.Gate{
		{Name: "mutation-budget", After: types.StepTest, Command: "make mutation"},
		{Name: "arch-fitness", After: types.StepReview, Instructions: "no cycles"},
	}))
	want := []string{
		"intent", "rebase", "review", "gate.review.arch-fitness",
		"test", "gate.test.mutation-budget", "document", "lint", "push", "pr", "ci",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("sequence =\n %v\nwant\n %v", got, want)
	}
}

func TestWithCustomGates_KeepsDeclarationOrderOnSharedAnchor(t *testing.T) {
	got := stepNames(WithCustomGates(AllSteps(), []config.Gate{
		{Name: "first", After: types.StepLint, Command: "a"},
		{Name: "second", After: types.StepLint, Command: "b"},
	}))
	joined := strings.Join(got, ",")
	if !strings.Contains(joined, "lint,gate.lint.first,gate.lint.second,push") {
		t.Fatalf("sequence = %v, want the two lint gates in declaration order", got)
	}
}

func TestWithCustomGates_NoGatesReturnsCore(t *testing.T) {
	if got, want := len(WithCustomGates(AllSteps(), nil)), len(AllSteps()); got != want {
		t.Fatalf("step count = %d, want the core %d", got, want)
	}
}

func TestCustomGateStep_CommandPassRunsClean(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContext(t, &mockAgent{name: "mock"}, dir, baseSHA, headSHA, config.Commands{})

	step := &CustomGateStep{Gate: config.Gate{Name: "always-pass", After: types.StepTest, Command: "exit 0"}}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("Execute() = %v", err)
	}
	if outcome.NeedsApproval || outcome.ExitCode != 0 || outcome.Findings != "" {
		t.Fatalf("outcome = %+v, want a clean pass", outcome)
	}
}

// A failing gate parks for a human rather than auto-fixing: a repair must be
// authorized, so the pipeline never starts one on its own initiative. The
// fix-round tests below cover the authorized path.
func TestCustomGateStep_CommandFailureParksForHuman(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContext(t, &mockAgent{name: "mock"}, dir, baseSHA, headSHA, config.Commands{})

	step := &CustomGateStep{Gate: config.Gate{Name: "always-fail", After: types.StepTest, Command: "exit 3"}}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("Execute() = %v", err)
	}
	if !outcome.NeedsApproval {
		t.Error("NeedsApproval = false, want the gate to park")
	}
	if outcome.AutoFixable {
		t.Error("AutoFixable = true, want a gate failure to stay the author's call")
	}
	if outcome.ExitCode != 3 {
		t.Errorf("ExitCode = %d, want 3", outcome.ExitCode)
	}
	var findings Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
		t.Fatalf("findings did not parse: %v", err)
	}
	if len(findings.Items) != 1 || findings.Items[0].Action != types.ActionAskUser {
		t.Fatalf("findings = %+v, want one ask-user finding", findings.Items)
	}
}

func TestCustomGateStep_AgentGateReportsFindingsAsAskUser(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{name: "mock", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: json.RawMessage(`{"findings":[{"severity":"error","description":"imports internal/cli","action":"auto-fix"}],"summary":"layering violation"}`)}, nil
	}}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})

	step := &CustomGateStep{Gate: config.Gate{Name: "arch-fitness", After: types.StepLint, Instructions: "No package under internal/ may import internal/cli."}}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("Execute() = %v", err)
	}
	if !outcome.NeedsApproval || outcome.AutoFixable {
		t.Fatalf("outcome = %+v, want a parked, non-auto-fixable gate", outcome)
	}
	var findings Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
		t.Fatalf("findings did not parse: %v", err)
	}
	// The gate states a repository rule; only a human can accept breaking it,
	// so the agent's own action classification is overridden.
	if len(findings.Items) != 1 || findings.Items[0].Action != types.ActionAskUser {
		t.Fatalf("findings = %+v, want the agent's action forced to ask-user", findings.Items)
	}
	if len(ag.calls) != 1 {
		t.Fatalf("agent calls = %d, want 1", len(ag.calls))
	}
	if !strings.Contains(ag.calls[0].Prompt, "No package under internal/ may import internal/cli.") {
		t.Error("gate instructions were not delivered to the agent")
	}
}

func TestCustomGateStep_AgentGateCleanRunPasses(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{name: "mock", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"clean"}`)}, nil
	}}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})

	step := &CustomGateStep{Gate: config.Gate{Name: "arch-fitness", After: types.StepLint, Instructions: "no cycles"}}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("Execute() = %v", err)
	}
	if outcome.NeedsApproval || outcome.Findings != "" {
		t.Fatalf("outcome = %+v, want a clean pass", outcome)
	}
}

// fileGateCommand builds a gate command that fails until name exists in the
// worktree, so a test can prove the gate re-ran against the repaired tree.
func fileGateCommand(name string) string {
	if runtime.GOOS == "windows" {
		return "if exist " + name + " (exit 0) else (exit 7)"
	}
	return "test -f " + name + " || exit 7"
}

// Answering a gate's park with `fix` used to re-run the identical check against
// an unchanged worktree, so the verdict could not move: one full extra
// execution for a guaranteed-identical outcome. The gate now runs a fix turn
// first and then re-checks, so an authorized repair actually clears it.
func TestCustomGateStep_CommandGateFixRoundRepairsThenReChecks(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{name: "mock", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if err := os.WriteFile(filepath.Join(dir, "gate-satisfied.txt"), []byte("ok"), 0o644); err != nil {
			return nil, err
		}
		return &agent.Result{Output: json.RawMessage(`{"summary":"satisfy mutation budget"}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Fixing = true
	sctx.PreviousFindings = `{"items":[{"id":"g-1","severity":"error","description":"gate \"mutation-budget\" failed with exit code 7"}],"summary":"mutation score 41% below the 60% budget"}`

	step := &CustomGateStep{Gate: config.Gate{
		Name:    "mutation-budget",
		After:   types.StepTest,
		Command: fileGateCommand("gate-satisfied.txt"),
	}}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("Execute() = %v", err)
	}
	if outcome.NeedsApproval {
		t.Fatalf("outcome = %+v, want the re-check to pass against the repaired worktree", outcome)
	}
	if outcome.FixSummary != "satisfy mutation budget" {
		t.Errorf("FixSummary = %q, want the agent's summary", outcome.FixSummary)
	}
	if len(ag.calls) != 1 {
		t.Fatalf("agent calls = %d, want exactly the fix turn", len(ag.calls))
	}
	prompt := ag.calls[0].Prompt
	if !strings.Contains(prompt, "mutation score 41%") {
		t.Error("fix prompt did not carry the previous gate findings")
	}
	if !strings.Contains(prompt, fileGateCommand("gate-satisfied.txt")) {
		t.Error("fix prompt did not carry the gate's own command as the requirement to satisfy")
	}
	if status := gitStatusPorcelain(t, dir); status != "" {
		t.Fatalf("worktree = %q, want the fix committed", status)
	}
	if got := lastCommitMessage(t, dir); got != "no-mistakes(gate.test.mutation-budget): satisfy mutation budget" {
		t.Fatalf("last commit message = %q", got)
	}
}

// A fix turn that does not actually satisfy the gate must re-park, and the
// re-parked verdict must come from re-running the check, not from replaying the
// stale findings.
func TestCustomGateStep_CommandGateFixRoundThatDoesNotSatisfyTheGateReParks(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{name: "mock", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if err := os.WriteFile(filepath.Join(dir, "unrelated.txt"), []byte("nope"), 0o644); err != nil {
			return nil, err
		}
		return &agent.Result{Output: json.RawMessage(`{"summary":"attempt gate repair"}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Fixing = true
	sctx.PreviousFindings = `{"items":[{"id":"g-1","severity":"error","description":"gate failed"}],"summary":"stale output"}`

	step := &CustomGateStep{Gate: config.Gate{
		Name:    "mutation-budget",
		After:   types.StepTest,
		Command: fileGateCommand("gate-satisfied.txt"),
	}}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("Execute() = %v", err)
	}
	if !outcome.NeedsApproval || outcome.AutoFixable {
		t.Fatalf("outcome = %+v, want a re-parked, non-auto-fixable gate", outcome)
	}
	if outcome.ExitCode != 7 {
		t.Errorf("ExitCode = %d, want the re-run command's 7", outcome.ExitCode)
	}
	if strings.Contains(outcome.Findings, "stale output") {
		t.Error("findings replayed the previous round instead of the re-run's own output")
	}
	if outcome.FixSummary != "attempt gate repair" {
		t.Errorf("FixSummary = %q, want the agent's summary recorded on the round", outcome.FixSummary)
	}
}

// An agent gate's fix turn is prescribed by the repository rule, and the gate
// must then re-judge the repaired change rather than trusting the fixer.
func TestCustomGateStep_AgentGateFixRoundRepairsThenReJudges(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{name: "mock", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if len(opts.JSONSchema) > 0 && strings.Contains(string(opts.JSONSchema), "risk_level") {
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"clean"}`)}, nil
		}
		if strings.Contains(opts.Prompt, "Fix the violations") {
			if err := os.WriteFile(filepath.Join(dir, "arch-fix.txt"), []byte("ok"), 0o644); err != nil {
				return nil, err
			}
			return &agent.Result{Output: json.RawMessage(`{"summary":"drop internal/cli import"}`)}, nil
		}
		return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"clean"}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Fixing = true
	sctx.PreviousFindings = `{"items":[{"id":"a-1","severity":"error","file":"internal/daemon/x.go","description":"imports internal/cli"}],"summary":"layering violation"}`

	step := &CustomGateStep{Gate: config.Gate{
		Name:         "arch-fitness",
		After:        types.StepLint,
		Instructions: "No package under internal/ may import internal/cli.",
	}}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("Execute() = %v", err)
	}
	if outcome.NeedsApproval {
		t.Fatalf("outcome = %+v, want the re-judged gate to pass", outcome)
	}
	if len(ag.calls) != 2 {
		t.Fatalf("agent calls = %d, want a fix turn followed by a re-judge", len(ag.calls))
	}
	fixPrompt := ag.calls[0].Prompt
	if !strings.Contains(fixPrompt, "No package under internal/ may import internal/cli.") {
		t.Error("fix prompt did not carry the repository rule the gate enforces")
	}
	if !strings.Contains(fixPrompt, "imports internal/cli") {
		t.Error("fix prompt did not carry the previous gate findings")
	}
	if status := gitStatusPorcelain(t, dir); status != "" {
		t.Fatalf("worktree = %q, want the fix committed", status)
	}
	if got := lastCommitMessage(t, dir); got != "no-mistakes(gate.lint.arch-fitness): drop internal/cli import" {
		t.Fatalf("last commit message = %q", got)
	}
}

// A gate must never repair on the pipeline's own initiative. Without an
// explicit `fix` authorization the step runs its check and nothing else.
func TestCustomGateStep_WithoutFixAuthorizationRunsNoFixTurn(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{name: "mock", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if err := os.WriteFile(filepath.Join(dir, "gate-satisfied.txt"), []byte("ok"), 0o644); err != nil {
			return nil, err
		}
		return &agent.Result{Output: json.RawMessage(`{"summary":"should not run"}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	step := &CustomGateStep{Gate: config.Gate{
		Name:    "mutation-budget",
		After:   types.StepTest,
		Command: fileGateCommand("gate-satisfied.txt"),
	}}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("Execute() = %v", err)
	}
	if len(ag.calls) != 0 {
		t.Fatalf("agent calls = %d, want none without an explicit fix authorization", len(ag.calls))
	}
	if !outcome.NeedsApproval || outcome.AutoFixable {
		t.Fatalf("outcome = %+v, want a parked, non-auto-fixable gate", outcome)
	}
}

// A gate runs immediately AFTER its anchor and shares its step order, so the
// published attestation has to place it there. A lexicographic tie-break put
// every gate ahead of the core step it actually ran after.
func TestBuildPipelineAttestation_ListsAGateAfterItsAnchor(t *testing.T) {
	t.Parallel()
	steps := []*db.StepResult{
		{StepName: types.StepReview, Status: types.StepStatusCompleted},
		{StepName: types.CustomGateStepName(types.StepReview, "arch-fitness"), Status: types.StepStatusCompleted},
		{StepName: types.CustomGateStepName(types.StepReview, "budget"), Status: types.StepStatusCompleted},
		{StepName: types.StepTest, Status: types.StepStatusCompleted},
		{StepName: types.CustomGateStepName(types.StepTest, "mutation"), Status: types.StepStatusCompleted},
		{StepName: types.StepDocument, Status: types.StepStatusCompleted},
	}

	raw := buildPipelineAttestation(steps, testPipelineHeadSHA)
	payload := strings.TrimSuffix(strings.TrimPrefix(raw, pipelineAttestationCommentPrefix), pipelineAttestationCommentClosingToken)
	var decoded pipelineAttestation
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		t.Fatalf("attestation payload did not parse: %v (%s)", err, payload)
	}

	got := make([]string, 0, len(decoded.Steps))
	for _, s := range decoded.Steps {
		got = append(got, string(s.Step))
	}
	want := []string{
		"review", "gate.review.arch-fitness", "gate.review.budget",
		"test", "gate.test.mutation", "document",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("attestation order =\n %v\nwant\n %v", got, want)
	}
}

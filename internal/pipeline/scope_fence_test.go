package pipeline

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestScopeDecisionSelectionRejectsExplicitOutsidePath(t *testing.T) {
	scope := NewDeclaredScope([]string{"apps/godot/src/table/table_shell.gd"})
	raw, err := types.MarshalFindingsJSON(types.Findings{
		Items: []types.Finding{{
			ID:          "ci-1",
			Severity:    "error",
			File:        "scripts/ci/emit-pr-receipt.mjs",
			Description: "missing PR-body receipt",
			Action:      types.ActionAutoFix,
		}},
		Summary: "CI repair requested",
	})
	if err != nil {
		t.Fatal(err)
	}

	fixable, gated := scopeAutoFixDecisionGate(raw, scope)
	if fixable != "" {
		t.Fatalf("out-of-scope repair remained auto-fixable: %s", fixable)
	}
	if !strings.Contains(gated, ScopeDecisionGateName) {
		t.Fatalf("decision gate %q missing from findings: %s", ScopeDecisionGateName, gated)
	}
	parsed, err := types.ParseFindingsJSON(gated)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Items) != 1 || parsed.Items[0].Action != types.ActionAskUser {
		t.Fatalf("out-of-scope repair did not become one ask-user gate: %#v", parsed.Items)
	}
	if got := parsed.Items[0].File; got != "scripts/ci/emit-pr-receipt.mjs" {
		t.Fatalf("gate lost proposed path: %q", got)
	}
}

type countingFenceAgent struct{ calls int }

func (a *countingFenceAgent) Name() string { return "counting" }
func (a *countingFenceAgent) Close() error { return nil }
func (a *countingFenceAgent) Run(_ context.Context, _ agent.RunOpts) (*agent.Result, error) {
	a.calls++
	return &agent.Result{}, nil
}

func TestAS440OutOfScopeRepairDecisionGateRunsBeforeAgentDispatch(t *testing.T) {
	database, paths, run, repo := setupTest(t)
	workDir := t.TempDir()
	bindRunToGitRepo(t, database, workDir, run)
	agentSpy := &countingFenceAgent{}
	findings := `{"findings":[{"id":"ci-1","severity":"error","file":"scripts/ci/emit-pr-receipt.mjs","description":"missing PR-body receipt","action":"auto-fix"}],"summary":"repair requested"}`
	stepCalls := 0
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			stepCalls++
			if sctx.Fixing {
				_, err := sctx.Agent.Run(sctx.Ctx, agent.RunOpts{Prompt: "repair"})
				return &StepOutcome{}, err
			}
			return &StepOutcome{NeedsApproval: true, AutoFixable: true, Findings: findings}, nil
		},
	}
	exec := NewExecutor(database, paths, &config.Config{AutoFix: config.AutoFix{Review: 1}}, agentSpy, []Step{step}, nil)
	done := make(chan error, 1)
	go func() { done <- exec.Execute(context.Background(), run, repo, workDir) }()
	waitForStepStatus(t, database, run.ID, types.StepReview, types.StepStatusAwaitingApproval)
	if agentSpy.calls != 0 || stepCalls != 1 {
		t.Fatalf("out-of-scope repair reached dispatch: agent_calls=%d step_calls=%d", agentSpy.calls, stepCalls)
	}
	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if steps[0].FindingsJSON == nil || !strings.Contains(*steps[0].FindingsJSON, ScopeDecisionGateName) {
		t.Fatalf("parked gate missing stable scope name: %#v", steps[0].FindingsJSON)
	}
	if err := exec.Respond(types.StepReview, types.ActionApprove, nil); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor did not finish after explicit decision")
	}
}

func TestFilelessRepairCannotBypassDeclaredScopeDecision(t *testing.T) {
	scope := NewDeclaredScope([]string{"apps/godot/src/table/table_shell.gd"})
	raw, err := types.MarshalFindingsJSON(types.Findings{Items: []types.Finding{{
		ID: "ci-1", Severity: "error", Description: "missing PR-body receipt", Action: types.ActionAutoFix,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	fixable, gated := scopeAutoFixDecisionGate(raw, scope)
	if fixable != "" || !strings.Contains(gated, ScopeDecisionGateName) {
		t.Fatalf("fileless repair bypassed named decision gate: fixable=%s gated=%s", fixable, gated)
	}
}

func TestExplicitScopeDecisionAuthorizesOnlySelectedNamedPath(t *testing.T) {
	scope := NewDeclaredScope([]string{"inside.go"})
	raw := `{"findings":[{"id":"scope-1","severity":"error","file":"outside.go","description":"PIPELINE_SCOPE_DECISION_REQUIRED: outside","action":"ask-user"},{"id":"other","severity":"error","file":"other.go","description":"ordinary","action":"ask-user"}]}`
	added := authorizeSelectedScopeDecisionPaths(scope, raw)
	if len(added) != 1 || added[0] != "outside.go" {
		t.Fatalf("authorized paths = %v, want outside.go", added)
	}
	if !scope.Contains("outside.go") || scope.Contains("other.go") {
		t.Fatalf("scope authorization escaped selected named gate: %v", scope.Paths())
	}
}

func TestOutOfScopeAgentEditIsPreservedButCannotBeCommitted(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	writeTestFile(t, dir, "outside.txt", "preserve me\n")
	sctx := &StepContext{
		Ctx:           context.Background(),
		WorkDir:       dir,
		DeclaredScope: NewDeclaredScope([]string{"README.md"}),
	}
	err := sctx.AssertDeclaredScope("commit agent repair")
	if err == nil || !strings.Contains(err.Error(), ScopeDecisionGateName) {
		t.Fatalf("AssertDeclaredScope() error = %v, want named refusal", err)
	}
	if data, readErr := os.ReadFile(filepath.Join(dir, "outside.txt")); readErr != nil || string(data) != "preserve me\n" {
		t.Fatalf("guard erased the agent edit instead of preserving it: data=%q err=%v", data, readErr)
	}
}

type capturingFenceAgent struct {
	opts agent.RunOpts
}

func (a *capturingFenceAgent) Name() string { return "capture" }
func (a *capturingFenceAgent) Close() error { return nil }
func (a *capturingFenceAgent) Run(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
	a.opts = opts
	return &agent.Result{}, nil
}

func TestEveryAgentReceivesDeclaredScopeAndInvocationIdentity(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	capture := &capturingFenceAgent{}
	wrapped := &scopeAndProofFenceAgent{inner: capture, workDir: dir, scope: NewDeclaredScope([]string{"README.md"})}
	result, err := wrapped.Run(context.Background(), agent.RunOpts{Prompt: "review"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"DECLARED TICKET SCOPE", "README.md", ScopeDecisionGateName} {
		if !strings.Contains(capture.opts.Prompt, want) {
			t.Fatalf("agent prompt missing %q:\n%s", want, capture.opts.Prompt)
		}
	}
	if capture.opts.InvocationHeadSHA == "" || capture.opts.InvocationTreeSHA == "" {
		t.Fatalf("invocation was not bound before dispatch: %#v", capture.opts)
	}
	if result.InvocationHeadSHA != capture.opts.InvocationHeadSHA || result.InvocationTreeSHA != capture.opts.InvocationTreeSHA {
		t.Fatalf("accepted result identity = %s/%s, invocation identity = %s/%s", result.InvocationHeadSHA, result.InvocationTreeSHA, capture.opts.InvocationHeadSHA, capture.opts.InvocationTreeSHA)
	}
}

type movingProofAgent struct {
	t       *testing.T
	workDir string
}

func (a *movingProofAgent) Name() string { return "moving-proof" }
func (a *movingProofAgent) Close() error { return nil }
func (a *movingProofAgent) Run(_ context.Context, _ agent.RunOpts) (*agent.Result, error) {
	execGit(a.t, a.workDir, "commit", "--allow-empty", "-m", "move after proof")
	return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"clean"}`)}, nil
}

func TestProofFenceRefusesDirectMixedHeadResult(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	wrapped := &scopeAndProofFenceAgent{
		inner:   &movingProofAgent{t: t, workDir: dir},
		workDir: dir,
		scope:   NewDeclaredScope([]string{"README.md"}),
	}

	result, err := wrapped.Run(context.Background(), agent.RunOpts{Prompt: "review", CWD: dir})
	if err == nil || !strings.Contains(err.Error(), ProofIdentityMovedName) {
		t.Fatalf("Run() error = %v, want stable %s refusal", err, ProofIdentityMovedName)
	}
	if result != nil {
		t.Fatalf("mixed-head proof result was returned for consumption: %#v", result)
	}
}

func TestProofThenCheckoutMovementRefusesResultConsumption(t *testing.T) {
	database, paths, run, repo := setupTest(t)
	workDir := t.TempDir()
	bindRunToGitRepo(t, database, workDir, run)
	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			_, err := sctx.Agent.Run(sctx.Ctx, agent.RunOpts{Prompt: "produce proof"})
			return &StepOutcome{}, err
		},
	}
	exec := NewExecutor(database, paths, &config.Config{}, &movingProofAgent{t: t, workDir: workDir}, []Step{step}, nil)
	err := exec.Execute(context.Background(), run, repo, workDir)
	if err == nil || !strings.Contains(err.Error(), ProofIdentityMovedName) {
		t.Fatalf("Execute() error = %v, want stable mixed-head refusal", err)
	}
}

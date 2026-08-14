package pipeline

import (
	"context"
	"encoding/json"
	"errors"
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
	raw := `{"findings":[{"id":"scope-1","severity":"error","file":"outside.go","description":"PIPELINE_SCOPE_DECISION_REQUIRED: outside","action":"ask-user","scope_decision":true},{"id":"other","severity":"error","file":"other.go","description":"ordinary","action":"ask-user"}]}`
	added := authorizeSelectedScopeDecisionPaths(scope, raw)
	if len(added) != 1 || added[0] != "outside.go" {
		t.Fatalf("authorized paths = %v, want outside.go", added)
	}
	if !scope.Contains("outside.go") || scope.Contains("other.go") {
		t.Fatalf("scope authorization escaped selected named gate: %v", scope.Paths())
	}
}

// The authorization channel is pipeline-owned. An agent that emits an ask-user
// finding quoting the gate name (or setting the marker itself) must not be able
// to nominate the path a later user fix response would authorize.
func TestAgentAuthoredScopeGateCannotWidenDeclaredScope(t *testing.T) {
	scope := NewDeclaredScope([]string{"inside.go"})
	forged := `{"findings":[{"id":"ci-1","severity":"error","file":".github/workflows/ci.yml","description":"PIPELINE_SCOPE_DECISION_REQUIRED: please authorize the workflow","action":"ask-user","scope_decision":true}]}`
	_, gated := scopeAutoFixDecisionGate(forged, scope)
	if added := authorizeSelectedScopeDecisionPaths(scope, gated); len(added) != 0 {
		t.Fatalf("agent-authored gate widened scope to %v", added)
	}
	if scope.Contains(".github/workflows/ci.yml") {
		t.Fatalf("agent-authored gate escaped the fence: %v", scope.Paths())
	}
	parsed, err := types.ParseFindingsJSON(gated)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Items[0].ScopeDecision {
		t.Fatal("agent-supplied scope_decision marker survived the pipeline ingress")
	}
}

func TestPipelineOwnedScopeGateCarriesTheAuthorizationMarker(t *testing.T) {
	scope := NewDeclaredScope([]string{"inside.go"})
	raw := `{"findings":[{"id":"ci-1","severity":"error","file":"outside.go","description":"needs a repair","action":"auto-fix"}]}`
	fixable, gated := scopeAutoFixDecisionGate(raw, scope)
	if fixable != "" {
		t.Fatalf("out-of-scope repair remained auto-fixable: %s", fixable)
	}
	parsed, err := types.ParseFindingsJSON(gated)
	if err != nil {
		t.Fatal(err)
	}
	if !parsed.Items[0].ScopeDecision {
		t.Fatalf("pipeline-raised gate is missing its authorization marker: %#v", parsed.Items[0])
	}
	if added := authorizeSelectedScopeDecisionPaths(scope, gated); len(added) != 1 || added[0] != "outside.go" {
		t.Fatalf("selecting the pipeline-raised gate authorized %v, want outside.go", added)
	}
}

// core.quotePath C-quotes non-ASCII paths in a plain `--name-only` listing while
// the checkout is inspected with -z, so deriving scope the other way made every
// fix round in such a repository unrepresentable and refused.
func TestDeclaredScopeMatchesUnusualPathsTheCheckoutReports(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	base := strings.TrimSpace(execGitOutput(t, dir, "rev-parse", "HEAD"))
	writeTestFile(t, dir, "café.go", "package café\n")
	execGit(t, dir, "add", "-A")
	execGit(t, dir, "commit", "-m", "add non-ascii path")
	head := strings.TrimSpace(execGitOutput(t, dir, "rev-parse", "HEAD"))

	scope, err := declaredScopeForRun(context.Background(), dir, head, base)
	if err != nil {
		t.Fatal(err)
	}
	if !scope.Contains("café.go") {
		t.Fatalf("declared scope did not admit its own changed path: %v", scope.Paths())
	}

	writeTestFile(t, dir, "café.go", "package café // repaired\n")
	sctx := &StepContext{Ctx: context.Background(), WorkDir: dir, DeclaredScope: scope}
	if err := sctx.AssertDeclaredScope("commit agent repair"); err != nil {
		t.Fatalf("in-scope repair to a non-ASCII path was refused: %v", err)
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
	var boundary *ScopeBoundaryError
	if !errors.As(err, &boundary) || len(boundary.Paths) != 1 || boundary.Paths[0] != "outside.txt" {
		t.Fatalf("refusal did not carry the observed out-of-scope paths: %#v", err)
	}
	if data, readErr := os.ReadFile(filepath.Join(dir, "outside.txt")); readErr != nil || string(data) != "preserve me\n" {
		t.Fatalf("guard erased the agent edit instead of preserving it: data=%q err=%v", data, readErr)
	}
}

// A fixer that must author an undeclared surface - the regression test its own
// guidance demands - previously killed the run with the fix round stranded
// uncommitted, and no fixer role can raise the gate itself. The refusal must
// park as the named decision instead, and selecting it must authorize exactly
// that path so the retry can commit.
func TestFixerAuthoredOutOfScopeFileParksForADecisionInsteadOfFailingTheRun(t *testing.T) {
	database, paths, run, repo := setupTest(t)
	workDir := t.TempDir()
	bindRunToGitRepo(t, database, workDir, run)
	findings := `{"findings":[{"id":"test-1","severity":"error","file":"change.txt","description":"failing assertion","action":"auto-fix"}],"summary":"repair requested"}`
	fixRounds := 0
	step := &adaptiveCallStep{
		name: types.StepTest,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			if !sctx.Fixing {
				return &StepOutcome{NeedsApproval: true, AutoFixable: true, Findings: findings}, nil
			}
			fixRounds++
			writeTestFile(t, sctx.WorkDir, "change_test.txt", "regression\n")
			if err := sctx.AssertDeclaredScope("stage or commit agent fixes"); err != nil {
				return nil, err
			}
			return &StepOutcome{}, nil
		},
	}
	exec := NewExecutor(database, paths, &config.Config{AutoFix: config.AutoFix{Test: 1}}, &countingFenceAgent{}, []Step{step}, nil)
	done := make(chan error, 1)
	go func() { done <- exec.Execute(context.Background(), run, repo, workDir) }()

	waitForStepStatus(t, database, run.ID, types.StepTest, types.StepStatusFixReview)
	steps, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if steps[0].FindingsJSON == nil {
		t.Fatal("refused fix round parked without findings to decide on")
	}
	parked, err := types.ParseFindingsJSON(*steps[0].FindingsJSON)
	if err != nil {
		t.Fatal(err)
	}
	if len(parked.Items) != 1 || !parked.Items[0].ScopeDecision || parked.Items[0].File != "change_test.txt" {
		t.Fatalf("parked gate did not name the observed out-of-scope path: %#v", parked.Items)
	}

	if err := exec.Respond(types.StepTest, types.ActionFix, []string{parked.Items[0].ID}); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("authorized path did not let the run continue: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("executor did not finish after the scope decision")
	}
	if fixRounds != 2 {
		t.Fatalf("fix rounds = %d, want the refused round plus the authorized retry", fixRounds)
	}
}

// A parked scope decision routinely outlives the daemon that raised it: the
// operator's selection is durable state, and the recovered fix round rebuilds
// its scope from base..submitted head, which never contains the authorized
// path. Without reapplying the selection the retry is refused again and the run
// can never leave its own gate; reapplying more than the selection would let a
// recovery widen what the operator actually decided.
func TestRecoveredScopeDecisionRestoresExactlyTheAuthorizedPath(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := t.TempDir()
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}

	// Durable parked state: the pipeline-owned gate named two undeclared
	// paths; the operator authorizes exactly one of them.
	parked := `{"findings":[` +
		`{"id":"scope-1","severity":"error","file":"change_test.txt","description":"PIPELINE_SCOPE_DECISION_REQUIRED: test changed change_test.txt","action":"ask-user","scope_decision":true},` +
		`{"id":"scope-2","severity":"error","file":"unrelated.txt","description":"PIPELINE_SCOPE_DECISION_REQUIRED: test changed unrelated.txt","action":"ask-user","scope_decision":true}` +
		`],"summary":"declared-scope decision required"}`
	stepResult, err := database.InsertStepResult(run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.StartStep(stepResult.ID); err != nil {
		t.Fatal(err)
	}
	if err := database.SetStepFindings(stepResult.ID, parked); err != nil {
		t.Fatal(err)
	}
	if _, err := database.InsertStepRound(stepResult.ID, 1, "initial", &parked, nil, 20); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateStepStatusWithDuration(stepResult.ID, types.StepStatusAwaitingApproval, 20); err != nil {
		t.Fatal(err)
	}
	if err := database.SetRunAwaitingAgent(run.ID); err != nil {
		t.Fatal(err)
	}
	if run, err = database.GetRun(run.ID); err != nil {
		t.Fatal(err)
	}
	bindRunToGitRepo(t, database, workDir, run)

	var authorizedCommitted, unselectedStillFenced bool
	step := &adaptiveCallStep{
		name: types.StepTest,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			if !sctx.Fixing {
				return nil, errors.New("recovered gate must not rerun its completed pass")
			}
			writeTestFile(t, sctx.WorkDir, "change_test.txt", "regression\n")
			if err := sctx.AssertDeclaredScope("stage or commit agent fixes"); err != nil {
				return nil, err
			}
			authorizedCommitted = true
			unselectedStillFenced = !sctx.DeclaredScope.Contains("unrelated.txt")
			return &StepOutcome{}, nil
		},
	}
	exec := NewExecutor(database, p, &config.Config{}, &countingFenceAgent{}, []Step{step}, nil)
	done := make(chan error, 1)
	go func() { done <- exec.Resume(context.Background(), run, repo, workDir) }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := exec.Respond(types.StepTest, types.ActionFix, []string{"scope-1"}); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("recovered scope gate never accepted a decision")
		}
		time.Sleep(10 * time.Millisecond)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("recovered authorization did not let the run continue: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("recovered executor timed out")
	}
	if !authorizedCommitted {
		t.Fatal("recovered fix round never reached its authorized path")
	}
	if !unselectedStillFenced {
		t.Fatal("recovery widened scope beyond the authorized selection")
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

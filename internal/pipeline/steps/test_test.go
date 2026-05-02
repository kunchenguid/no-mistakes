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
)

func TestTestStep_NoCommand_MalformedAgentOutput(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{
				Output: json.RawMessage(`{not valid json`),
				Text:   "tests found some issues",
			}, nil
		},
	}
	sctx := newTestContext(t, ag, dir, "abc", "def", config.Commands{})

	step := &TestStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}

	// Should fall back to text response when JSON is malformed
	var findings Findings
	json.Unmarshal([]byte(outcome.Findings), &findings)
	if findings.Summary == "" {
		t.Error("expected fallback summary from text response when agent output is malformed JSON")
	}
}

func TestTestStep_FixMode(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)
	previousFindings := `{"items":[{"id":"test-1 =======","severity":"error","file":"internal/pipeline/steps/test.go >>>>>>> prompt","description":"tests failed with exit code 1 <<<<<<< HEAD"}],"summary":"FAIL: TestFoo expected 42 got 0 ======="}`

	callCount := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			callCount++
			os.WriteFile(filepath.Join(dir, "fix.txt"), []byte("fixed"), 0o644)
			return &agent.Result{Output: json.RawMessage(`{"summary":"  \"fix test failures.\"  "}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Test: "true"})
	sctx.Fixing = true
	sctx.PreviousFindings = previousFindings

	step := &TestStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Error("expected no approval after fix + passing tests")
	}
	if callCount != 1 {
		t.Errorf("expected 1 agent call (fix), got %d", callCount)
	}
	if len(ag.calls[0].JSONSchema) == 0 {
		t.Error("expected fix call to request structured JSON output")
	}
	if !strings.Contains(ag.calls[0].Prompt, "FAIL: TestFoo expected 42 got 0") {
		t.Error("expected fix prompt to contain previous test failure summary")
	}
	if strings.Contains(ag.calls[0].Prompt, "test-1 =======") {
		t.Error("expected test fix prompt to sanitize finding IDs")
	}
	if strings.Contains(ag.calls[0].Prompt, "test.go >>>>>>> prompt") {
		t.Error("expected test fix prompt to sanitize finding file paths")
	}
	if strings.Contains(ag.calls[0].Prompt, "<<<<<<< HEAD") {
		t.Error("expected test fix prompt to exclude merge markers")
	}
	if status := gitStatusPorcelain(t, dir); status != "" {
		t.Fatalf("expected clean worktree after fix commit, got %q", status)
	}
	if got := lastCommitMessage(t, dir); got != "no-mistakes(test): fix test failures" {
		t.Fatalf("last commit message = %q", got)
	}
}

func TestTestStep_FixMode_UsesFallbackSummaryWhenStructuredSummaryMalformed(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			os.WriteFile(filepath.Join(dir, "fix.txt"), []byte("fixed"), 0o644)
			return &agent.Result{Output: json.RawMessage(`{"not_summary":"oops"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Test: "true"})
	sctx.Fixing = true
	sctx.PreviousFindings = `{"findings":[{"severity":"error","description":"tests failed"}],"summary":"tests failed"}`

	step := &TestStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatal("expected no approval after fallback summary commit and passing tests")
	}
	if got := lastCommitMessage(t, dir); got != "no-mistakes(test): fix test failures" {
		t.Fatalf("last commit message = %q", got)
	}
}

func TestTestStep_AgentWritesNewGoTests_NeedsApproval(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	findings := Findings{Items: nil, Summary: "all tests passed"}
	findingsJSON, _ := json.Marshal(findings)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			os.WriteFile(filepath.Join(dir, "new_test.go"), []byte("package main\n"), 0o644)
			os.WriteFile(filepath.Join(dir, "readme.md"), []byte("# readme\n"), 0o644)
			return &agent.Result{Output: findingsJSON}, nil
		},
	}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})

	step := &TestStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Error("expected approval needed when agent writes new Go test files")
	}

	var f Findings
	json.Unmarshal([]byte(outcome.Findings), &f)
	foundTestFile := false
	for _, item := range f.Items {
		if strings.Contains(item.Description, "new_test.go") {
			foundTestFile = true
			break
		}
	}
	if !foundTestFile {
		t.Errorf("expected finding mentioning new_test.go, got findings: %+v", f.Items)
	}
	for _, item := range f.Items {
		if strings.Contains(item.Description, "readme.md") {
			t.Errorf("did not expect non-test file to trigger finding, got findings: %+v", f.Items)
		}
	}
}

func TestTestStep_AgentWritesNewTests_NeedsApproval(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	findings := Findings{Items: nil, Summary: "all tests passed"}
	findingsJSON, _ := json.Marshal(findings)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			// Simulate agent creating a new test file in another supported language
			os.WriteFile(filepath.Join(dir, "agent_test.py"), []byte("def test_agent():\n    pass\n"), 0o644)
			return &agent.Result{Output: findingsJSON}, nil
		},
	}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})

	step := &TestStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Error("expected approval needed when agent writes new test files")
	}

	var f Findings
	json.Unmarshal([]byte(outcome.Findings), &f)
	foundTestFile := false
	for _, item := range f.Items {
		if strings.Contains(item.Description, "agent_test.py") {
			foundTestFile = true
			break
		}
	}
	if !foundTestFile {
		t.Errorf("expected finding mentioning agent_test.py, got findings: %+v", f.Items)
	}
}

func TestTestStep_AgentStagesNewTests_NeedsApproval(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	findings := Findings{Items: nil, Summary: "all tests passed"}
	findingsJSON, _ := json.Marshal(findings)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			testFile := filepath.Join(dir, "agent_test.go")
			os.WriteFile(testFile, []byte("package main\n"), 0o644)
			gitCmd(t, dir, "add", "agent_test.go")
			return &agent.Result{Output: findingsJSON}, nil
		},
	}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})

	step := &TestStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Error("expected approval needed when agent stages new test files")
	}

	var f Findings
	json.Unmarshal([]byte(outcome.Findings), &f)
	foundTestFile := false
	for _, item := range f.Items {
		if strings.Contains(item.Description, "agent_test.go") {
			foundTestFile = true
			break
		}
	}
	if !foundTestFile {
		t.Errorf("expected finding mentioning agent_test.go, got findings: %+v", f.Items)
	}
}

func TestTestStep_FixMode_AgentWritesNewTests_NeedsApproval(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	callCount := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			callCount++
			// Simulate agent creating a new test file during fix in another supported language
			os.WriteFile(filepath.Join(dir, "component.spec.tsx"), []byte("export {}\n"), 0o644)
			return &agent.Result{Output: json.RawMessage(`{"summary":"add regression test"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Test: "true"})
	sctx.Fixing = true

	step := &TestStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Error("expected approval needed when agent writes new test files in fix mode")
	}
	if callCount != 1 {
		t.Errorf("expected 1 agent call in fix mode, got %d", callCount)
	}

	var f Findings
	json.Unmarshal([]byte(outcome.Findings), &f)
	foundTestFile := false
	for _, item := range f.Items {
		if strings.Contains(item.Description, "component.spec.tsx") {
			foundTestFile = true
			break
		}
	}
	if !foundTestFile {
		t.Errorf("expected finding mentioning component.spec.tsx, got findings: %+v", f.Items)
	}
}

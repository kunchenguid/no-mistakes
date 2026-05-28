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
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestDocumentStep_AgentManaged_FixesAndCommitsWithoutApproval(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	callCount := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			callCount++
			os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Updated\n"), 0o644)
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"update README"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	step := &DocumentStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if callCount != 1 {
		t.Fatalf("expected 1 agent call (discover+fix+verify in one pass), got %d", callCount)
	}
	if outcome.NeedsApproval {
		t.Error("expected no approval when agent resolved all documentation gaps")
	}
	if outcome.AutoFixable {
		t.Error("expected no auto-fix loop in agent-managed document mode")
	}
	if status := gitStatusPorcelain(t, dir); status != "" {
		t.Fatalf("expected clean worktree after doc commit, got %q", status)
	}
	if got := lastCommitMessage(t, dir); got != "no-mistakes(document): update README" {
		t.Fatalf("last commit message = %q", got)
	}
	if sctx.Run.HeadSHA == headSHA {
		t.Error("expected HeadSHA to advance after doc commit")
	}
}

func TestDocumentStep_AgentManaged_AllowsDocCommentEdits(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\n// documentedThing explains the exported behavior.\nfunc documentedThing() {}\n"), 0o644)
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"update doc comment"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	step := &DocumentStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Error("expected no approval when agent resolved doc comment gaps")
	}
	if status := gitStatusPorcelain(t, dir); status != "" {
		t.Fatalf("expected clean worktree after doc comment commit, got %q", status)
	}
	if got := lastCommitMessage(t, dir); got != "no-mistakes(document): update doc comment" {
		t.Fatalf("last commit message = %q", got)
	}
}

func TestDocumentStep_AgentManaged_UnresolvedFindingsNeedApprovalWithoutAutoFixLoop(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"findings":[{"severity":"warning","description":"config docs conflict, needs human decision","action":"ask-user"}],"summary":"docs mostly updated"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	step := &DocumentStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Error("expected approval for unresolved documentation findings")
	}
	if outcome.AutoFixable {
		t.Error("expected unresolved documentation findings not to trigger an auto-fix round")
	}
	var findings Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
		t.Fatalf("unmarshal findings: %v", err)
	}
	if len(findings.Items) != 1 {
		t.Fatalf("expected 1 finding, got %+v", findings.Items)
	}
}

func TestDocumentStep_PromptEmphasizesExhaustiveFixing(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"docs current"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	step := &DocumentStep{}
	if _, err := step.Execute(sctx); err != nil {
		t.Fatal(err)
	}
	prompt := ag.calls[0].Prompt
	for _, want := range []string{
		"Be exhaustive",
		"Do not stop after the first documentation gap",
		"fix all of them yourself",
		"report only",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("expected document prompt to contain %q\nprompt:\n%s", want, prompt)
		}
	}
	// The fused prompt must not instruct read-only assessment.
	if strings.Contains(prompt, "Do NOT make any file changes") {
		t.Error("expected fused document prompt not to forbid file changes")
	}
}

func TestDocumentStep_UserFix_PassesPreviousFindingsIntoPrompt(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Fixed\n"), 0o644)
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"address config docs"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Fixing = true
	sctx.PreviousFindings = `{"items":[{"id":"doc-1 =======","severity":"warning","file":"docs/config.md >>>>>>> prompt","description":"config section stale <<<<<<< HEAD"}],"summary":"config docs stale"}`

	step := &DocumentStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Error("expected no approval after resolving the user-selected findings")
	}
	prompt := ag.calls[0].Prompt
	if !strings.Contains(prompt, "Previous documentation findings to address") {
		t.Error("expected user-fix prompt to include previous findings section")
	}
	if !strings.Contains(prompt, "config section stale") {
		t.Error("expected user-fix prompt to carry the previous finding description")
	}
	if strings.Contains(prompt, "doc-1 =======") || strings.Contains(prompt, "<<<<<<< HEAD") {
		t.Error("expected user-fix prompt to sanitize finding fields and merge markers")
	}
	if got := lastCommitMessage(t, dir); got != "no-mistakes(document): address config docs" {
		t.Fatalf("last commit message = %q", got)
	}
}

func TestDocumentStep_NoChanges_SkipsAgent(t *testing.T) {
	t.Parallel()
	dir, baseSHA, _ := setupGitRepo(t)

	callCount := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			callCount++
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"noop"}`)}, nil
		},
	}
	// Point head at base so there are no changed files.
	sctx := newTestContext(t, ag, dir, baseSHA, baseSHA, config.Commands{})

	step := &DocumentStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if callCount != 0 {
		t.Fatalf("expected no agent call when nothing changed, got %d", callCount)
	}
	if outcome.NeedsApproval || outcome.AutoFixable {
		t.Error("expected a clean no-op outcome when nothing changed")
	}
}

func TestDocumentStep_MalformedOutput_CommitsAndRequiresApproval(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Partial\n"), 0o644)
			return &agent.Result{
				Output: json.RawMessage(`{not valid json`),
				Text:   "I updated the docs",
			}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	step := &DocumentStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("expected malformed output to require approval")
	}
	if outcome.AutoFixable {
		t.Fatal("expected malformed output not to trigger an auto-fix loop")
	}
	var findings Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
		t.Fatalf("unmarshal findings: %v", err)
	}
	if len(findings.Items) != 1 {
		t.Fatalf("expected 1 finding, got %+v", findings.Items)
	}
	if findings.Items[0].Action != types.ActionAskUser {
		t.Error("expected malformed output finding to require human review")
	}
	// Any edits the agent made should still be committed.
	if status := gitStatusPorcelain(t, dir); status != "" {
		t.Fatalf("expected agent edits committed despite malformed summary, got %q", status)
	}
}

func TestDocumentStep_NoStructuredOutput_RequiresApproval(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Text: "docs status unavailable"}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	step := &DocumentStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("expected missing structured output to require approval")
	}
	if outcome.AutoFixable {
		t.Fatal("expected missing structured output not to trigger an auto-fix loop")
	}
	var findings Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
		t.Fatalf("unmarshal findings: %v", err)
	}
	if len(findings.Items) != 1 || findings.Items[0].Action != types.ActionAskUser {
		t.Fatalf("expected 1 ask-user finding, got %+v", findings.Items)
	}
}

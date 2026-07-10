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

// TestDocumentStep_PromptAppliesPlacementPolicy pins the placement-policy
// prompt contract from the 121-PR audit: each fact has one authoritative
// owner, stale duplicates are removed or reduced to pointers (not
// synchronized), AGENTS.md never receives incident narratives (invariant +
// regression-test pointer instead), no new surfaces for perceived gaps, and
// the scope stays on documentation this change made stale. The old
// exhaustive-corpus-synchronization incentives must be gone.
func TestDocumentStep_PromptAppliesPlacementPolicy(t *testing.T) {
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
		// One owner per fact; duplicates become pointers, never synced copies.
		"exactly one authoritative owner document",
		"remove the duplicate or reduce it to a short pointer to the owner",
		"never synchronize prose copies",
		// No new surfaces, no AGENTS.md postmortems; invariants + test pointers.
		"Do not create a new documentation surface merely to close a perceived gap",
		"Do not add incident narratives or postmortems to AGENTS.md",
		"point to the regression test or authoritative implementation",
		// Ownership map for the standard surfaces.
		"README.md owns the user-facing product introduction",
		"CONTRIBUTING.md owns contribution mechanics",
		"Code comments own non-obvious local intent",
		// Scope discipline: only what this change made stale.
		"Only touch documentation this change made stale",
		"Do not opportunistically rewrite, expand, or restructure unrelated documentation",
		"report one finding proposing the follow-up instead of multiplying edits",
		// Changed behavior must still land in its authoritative location.
		"Changed user-facing behavior must leave its authoritative user documentation accurate",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("expected document prompt to contain %q\nprompt:\n%s", want, prompt)
		}
	}
	// The exhaustive-synchronization incentives from the pre-audit prompt
	// must be gone: they are what produced doc commits in 90 of 121 PRs.
	for _, forbidden := range []string{
		"Be exhaustive",
		"resolve every gap you can in this run",
		"Enumerate all docs",
	} {
		if strings.Contains(prompt, forbidden) {
			t.Errorf("document prompt still carries corpus-sweep incentive %q", forbidden)
		}
	}
	// The fused prompt must not instruct read-only assessment.
	if strings.Contains(prompt, "Do NOT make any file changes") {
		t.Error("expected fused document prompt not to forbid file changes")
	}
}

// TestDocumentStep_TrustedPolicyInstructionsAugmentPrompt proves a
// repository's own ownership map (config document.instructions, loaded only
// from the trusted default branch) reaches the prompt as an augmentation of
// the built-in defaults, and that no-policy repositories keep the built-in
// policy alone.
func TestDocumentStep_TrustedPolicyInstructionsAugmentPrompt(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"docs current"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.Document.Instructions = "docs/architecture.md owns the daemon lifecycle facts."

	step := &DocumentStep{}
	if _, err := step.Execute(sctx); err != nil {
		t.Fatal(err)
	}
	prompt := ag.calls[0].Prompt
	if !strings.Contains(prompt, "docs/architecture.md owns the daemon lifecycle facts.") {
		t.Fatalf("expected trusted repo policy in prompt:\n%s", prompt)
	}
	if !strings.Contains(prompt, "augments the defaults above and cannot weaken them") {
		t.Fatal("expected the repo policy to be framed as augmenting, not replacing, the defaults")
	}
	// The built-in defaults remain active alongside the custom policy.
	if !strings.Contains(prompt, "exactly one authoritative owner document") {
		t.Fatal("expected built-in placement policy to remain with custom instructions present")
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
	if !strings.Contains(prompt, "Previous findings to address") {
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

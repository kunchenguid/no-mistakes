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

const testSkillBody = "---\n" +
	"name: security-review\n" +
	"description: SECRET-FRONTMATTER-MARKER audit for auth bugs\n" +
	"mode: review\n" +
	"---\n" +
	"# Security review\n\n" +
	"UNIQUE-SKILL-GUIDANCE-MARKER: focus on authentication and input validation.\n"

// TestSkillStep_PromptComposition_ThreeLayers proves the skill step builds its
// prompt from the three fixed layers — engine context header, repo skill body,
// engine output contract — and enforces the shared findings schema, exactly
// mirroring the built-in review step.
func TestSkillStep_PromptComposition_ThreeLayers(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			j, _ := json.Marshal(Findings{Summary: "clean"})
			return &agent.Result{Output: j}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.IgnorePatterns = []string{"vendor/**"}

	step := &SkillStep{StepName: "security-review", SkillBody: testSkillBody, Mode: SkillModeReview}
	if _, err := step.Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) != 1 {
		t.Fatalf("expected 1 agent call, got %d", len(ag.calls))
	}
	prompt := ag.calls[0].Prompt

	// Layer (a): engine-owned context header.
	for _, want := range []string{
		"security-review",
		"branch: " + sctx.Run.Branch,
		"base commit: " + baseSHA,
		"target commit: " + headSHA,
		"default branch: main",
		"ignore patterns: vendor/**",
		"review scope:",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("context header missing %q\n---\n%s", want, prompt)
		}
	}

	// Layer (b): repo-owned skill body, fenced and with frontmatter stripped.
	if !strings.Contains(prompt, "-----BEGIN SKILL-----") || !strings.Contains(prompt, "-----END SKILL-----") {
		t.Errorf("skill body not fenced with BEGIN/END markers\n---\n%s", prompt)
	}
	if !strings.Contains(prompt, "UNIQUE-SKILL-GUIDANCE-MARKER") {
		t.Errorf("skill body not inlined into prompt\n---\n%s", prompt)
	}
	if strings.Contains(prompt, "SECRET-FRONTMATTER-MARKER") {
		t.Errorf("skill frontmatter leaked into prompt (should be stripped)\n---\n%s", prompt)
	}

	// Layer (c): engine-owned read-only output contract with the review action
	// vocabulary.
	for _, want := range []string{
		`"ask-user"`, `"auto-fix"`, `"no-op"`,
		"challenges the author's deliberate intent",
		"read-only review",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("output contract missing %q\n---\n%s", want, prompt)
		}
	}

	// Findings schema is enforced.
	if len(ag.calls[0].JSONSchema) == 0 {
		t.Error("expected the skill review call to request structured JSON output")
	}
	if string(ag.calls[0].JSONSchema) != string(findingsSchema) {
		t.Error("expected the skill review call to enforce the shared findingsSchema")
	}
}

// TestSkillStep_WorktreeGuard_ResetsAndWarns proves the read-only contract is
// enforced, not hoped: if the skill agent dirties the worktree during a
// review-mode pass, the changes are discarded and a warning finding is added.
func TestSkillStep_WorktreeGuard_ResetsAndWarns(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			// The skill agent misbehaves: it edits a tracked file and creates a
			// new untracked file during a read-only review.
			if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("agent tampered\n"), 0o644); err != nil {
				return nil, err
			}
			if err := os.WriteFile(filepath.Join(dir, "sneaky.txt"), []byte("new file\n"), 0o644); err != nil {
				return nil, err
			}
			j, _ := json.Marshal(Findings{Summary: "reviewed"})
			return &agent.Result{Output: j}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	step := &SkillStep{StepName: "security-review", SkillBody: testSkillBody, Mode: SkillModeReview}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}

	// The worktree must be clean again — the agent's edits were discarded.
	if status := gitStatusPorcelain(t, dir); status != "" {
		t.Fatalf("expected clean worktree after read-only guard reset, got %q", status)
	}
	if _, err := os.Stat(filepath.Join(dir, "sneaky.txt")); !os.IsNotExist(err) {
		t.Fatalf("expected untracked file to be cleaned, stat err = %v", err)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "feature.txt")); string(data) != "feature code\n" {
		t.Fatalf("expected feature.txt reverted to committed content, got %q", data)
	}

	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatalf("parse findings: %v", err)
	}
	found := false
	for _, f := range findings.Items {
		if strings.Contains(f.Description, "skill modified the worktree during a review-mode step") {
			found = true
			if f.Severity != "warning" {
				t.Errorf("guard finding severity = %q, want warning", f.Severity)
			}
		}
	}
	if !found {
		t.Fatalf("expected a warning finding about the read-only violation, got %+v", findings.Items)
	}
	if !outcome.NeedsApproval {
		t.Error("expected the read-only violation warning to gate the step")
	}
}

// TestSkillStep_CleanReviewNoApproval proves a clean skill review parks nothing
// and leaves the worktree untouched.
func TestSkillStep_CleanReviewNoApproval(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			j, _ := json.Marshal(Findings{Items: nil, Summary: "no issues"})
			return &agent.Result{Output: j}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	step := &SkillStep{StepName: "security-review", SkillBody: testSkillBody, Mode: SkillModeReview}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Error("expected no approval needed for a clean review")
	}
	if outcome.AutoFixable {
		t.Error("expected no auto-fixable findings for a clean review")
	}
}

// TestSkillStep_EmptyBodyParks proves the fail-closed behavior: a skill step
// whose body could not be resolved from the trusted default branch parks with a
// misconfiguration finding rather than running an empty prompt.
func TestSkillStep_EmptyBodyParks(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			t.Fatal("agent must not be called when the skill body is empty")
			return nil, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	step := &SkillStep{StepName: "security-review", SkillBody: "", Mode: SkillModeReview}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval || outcome.AutoFixable {
		t.Errorf("expected a non-auto-fixable park, got NeedsApproval=%v AutoFixable=%v", outcome.NeedsApproval, outcome.AutoFixable)
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatalf("parse findings: %v", err)
	}
	if len(findings.Items) != 1 || !strings.Contains(findings.Items[0].Description, "could not be loaded from the trusted default branch") {
		t.Fatalf("expected a misconfiguration finding, got %+v", findings.Items)
	}
}

// TestSkillStep_FixMode proves a user "fix" round drives the agent with the
// skill body as domain guidance, commits its changes, then re-runs the
// read-only review — mirroring the built-in review fix loop.
func TestSkillStep_FixMode(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	callCount := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			callCount++
			if callCount == 1 {
				os.WriteFile(filepath.Join(dir, "skill-fix.txt"), []byte("fixed"), 0o644)
				return &agent.Result{Output: json.RawMessage(`{"summary":"harden auth check"}`)}, nil
			}
			j, _ := json.Marshal(Findings{Items: nil, Summary: "all clear"})
			return &agent.Result{Output: j}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Fixing = true
	sctx.PreviousFindings = `{"findings":[{"id":"skill-1","severity":"warning","file":"feature.txt","description":"missing auth guard","action":"ask-user"}],"summary":"1 issue"}`

	step := &SkillStep{StepName: "security-review", SkillBody: testSkillBody, Mode: SkillModeReview}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Error("expected no approval needed after a clean re-review")
	}
	if callCount != 2 {
		t.Errorf("expected 2 agent calls (fix + review), got %d", callCount)
	}
	// The fix prompt includes the skill body and the previous findings.
	if !strings.Contains(ag.calls[0].Prompt, "UNIQUE-SKILL-GUIDANCE-MARKER") {
		t.Error("expected fix prompt to include the skill body as domain guidance")
	}
	if !strings.Contains(ag.calls[0].Prompt, "missing auth guard") {
		t.Error("expected fix prompt to include the previous findings")
	}
	// The agent's edits were committed and the worktree is clean.
	if status := gitStatusPorcelain(t, dir); status != "" {
		t.Fatalf("expected clean worktree after fix commit, got %q", status)
	}
	if got := lastCommitMessage(t, dir); got != "no-mistakes(security-review): harden auth check" {
		t.Fatalf("last commit message = %q", got)
	}
}

// TestSkillStep_FixMode_RequiresPreviousFindings proves the fix loop refuses to
// run without previous findings, matching the built-in review step.
func TestSkillStep_FixMode_RequiresPreviousFindings(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			t.Fatal("agent should not be called when fix mode has no previous findings")
			return nil, nil
		},
	}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Fixing = true

	step := &SkillStep{StepName: "security-review", SkillBody: testSkillBody, Mode: SkillModeReview}
	if _, err := step.Execute(sctx); err == nil {
		t.Fatal("expected an error when fix mode has no previous findings")
	}
}

func TestSkillPromptBody_StripsFrontmatter(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"with frontmatter", "---\nname: x\nmode: review\n---\nbody line\n", "body line\n"},
		{"no frontmatter", "just a body\n", "just a body\n"},
		{"unterminated frontmatter left intact", "---\nname: x\nno close", "---\nname: x\nno close"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := skillPromptBody(tt.in); got != tt.want {
				t.Errorf("skillPromptBody(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

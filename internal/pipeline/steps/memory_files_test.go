package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The agent-memory-file contract these tests pin: AGENTS.md and CLAUDE.md are
// loaded into every future agent session, so what they contain stays a
// deliberate human decision. Every pipeline step whose agent can write to the
// worktree carries the hands-off rule in its prompt; the rebase and merge
// conflict resolvers carry a scoped variant because a conflicted memory file
// still has to be resolved for the integration to conclude; the document step
// carries a correction-only exception instead. These are prompt-contract
// tests: they assert on the rendered prompt delivered to the agent, not on
// source text.
const (
	memoryHandsOffProbe        = "Agent memory files (AGENTS.md and CLAUDE.md) are hands-off"
	memoryHandsOffNoEdit       = "Do not create, modify, rename, or delete them"
	memoryHandsOffNoFix        = "not even to correct or add content that looks stale, wrong, or missing"
	memoryConflictProbe        = "stay hands-off beyond the conflict itself"
	memoryConflictResolution   = "resolve their conflicts yourself whether they have conflict markers or are modify/delete or add/add conflicts"
	memoryConflictNoOtherEdits = "Make no other edits to their content"
	memoryDocCorrectOnly       = "Edit them only to correct or remove information that is factually wrong"
	memoryDocNoAdditions       = "never add content because something is missing"
	memoryDocNoCreate          = "never create them when absent"
	memoryDocIncidentScope     = "Do not add incident narratives or postmortems to AGENTS.md or CLAUDE.md"
)

func requirePromptContains(t *testing.T, prompt string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing memory-file rule fragment %q\nprompt:\n%s", want, prompt)
		}
	}
}

func requirePromptOmits(t *testing.T, prompt string, forbiddens ...string) {
	t.Helper()
	for _, forbidden := range forbiddens {
		if strings.Contains(prompt, forbidden) {
			t.Errorf("prompt carries forbidden fragment %q\nprompt:\n%s", forbidden, prompt)
		}
	}
}

// The review analyzer is a read-and-report turn, but its agent holds write
// tools in the worktree, so its prompt carries the generic hands-off rule.
func TestReviewStep_PromptKeepsMemoryFilesHandsOff(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	findingsJSON, _ := json.Marshal(cleanReviewFindings())
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: findingsJSON}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	if _, err := (&ReviewStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) == 0 {
		t.Fatal("expected the review analyzer to run")
	}
	requirePromptContains(t, ag.calls[0].Prompt, memoryHandsOffProbe, memoryHandsOffNoEdit, memoryHandsOffNoFix)
}

// The shared fixer wrapper covers every fix turn (review, test, lint, custom
// gates, CI repair). This pins it through the review fix path, where the
// agent definitely writes.
func TestReviewStep_FixPromptKeepsMemoryFilesHandsOff(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	callCount := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			callCount++
			if callCount == 1 {
				return &agent.Result{Output: json.RawMessage(`{"summary":"address findings"}`)}, nil
			}
			j, _ := json.Marshal(cleanReviewFindings())
			return &agent.Result{Output: j}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Fixing = true
	sctx.PreviousFindings = `{"findings":[{"id":"review-1","severity":"warning","file":"main.go","description":"possible nil deref","action":"auto-fix"}],"summary":"1 issue"}`

	if _, err := (&ReviewStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	requirePromptContains(t, ag.calls[0].Prompt, memoryHandsOffProbe, memoryHandsOffNoEdit, memoryHandsOffNoFix)
}

// The test evidence turn can write to the worktree (building scratch surfaces
// to drive scenarios), so its prompt carries the rule.
func TestTestStep_PromptKeepsMemoryFilesHandsOff(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	ag := &mockAgent{
		name: "test",
		runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(passingScenarioFindingsJSON)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	if _, err := (&TestStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) == 0 {
		t.Fatal("expected the test evidence turn to run")
	}
	requirePromptContains(t, ag.calls[0].Prompt, memoryHandsOffProbe, memoryHandsOffNoEdit, memoryHandsOffNoFix)
}

// With no configured lint command the agent-managed pass fixes lint and
// formatting itself, so its prompt carries the rule through fixerPrompt.
func TestLintStep_AgentPassPromptKeepsMemoryFilesHandsOff(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"format code"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	if _, err := (&LintStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) == 0 {
		t.Fatal("expected the lint agent pass to run")
	}
	requirePromptContains(t, ag.calls[0].Prompt, memoryHandsOffProbe, memoryHandsOffNoEdit, memoryHandsOffNoFix)
}

// A configured-lint fix turn also goes through the shared wrapper.
func TestLintStep_FixPromptKeepsMemoryFilesHandsOff(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"summary":"fix lint issues"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Lint: "exit 0"})
	sctx.Fixing = true
	sctx.PreviousFindings = `{"items":[{"id":"lint-1","severity":"warning","file":"main.go","description":"unused variable"}],"summary":"lint issues"}`

	if _, err := (&LintStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) == 0 {
		t.Fatal("expected the lint fix turn to run")
	}
	requirePromptContains(t, ag.calls[0].Prompt, memoryHandsOffProbe, memoryHandsOffNoEdit, memoryHandsOffNoFix)
}

// The CI repair turn composes the shared wrapper after LateRepairPrompt, so
// the rule survives its extra prompt layers.
func TestCIStep_RepairPromptKeepsMemoryFilesHandsOff(t *testing.T) {
	t.Parallel()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	os.WriteFile(filepath.Join(dir, "init.txt"), []byte("init"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "initial")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "feature")

	checksJSON := `[{"name":"test","status":"COMPLETED","conclusion":"failure","bucket":"fail"}]`
	env := fakeCIGHMergeable(t, "OPEN", checksJSON, "CONFLICTING")

	var capturedPrompt string
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			capturedPrompt = opts.Prompt
			os.WriteFile(filepath.Join(opts.CWD, "fix.txt"), []byte("fixed"), 0o644)
			return &agent.Result{}, nil
		},
	}

	prURL := "https://github.com/test/repo/pull/42"
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	sctx.Repo.UpstreamURL = upstream
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Config.CITimeout = 30 * time.Second
	sctx.Config.AutoFix = config.AutoFix{CI: 3}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx
	sctx.Log = func(s string) {}

	step := &CIStep{
		waitForNextPoll: func(ctx context.Context, interval time.Duration) error {
			cancel()
			return ctx.Err()
		},
	}
	driveCI(t, step, sctx)

	if capturedPrompt == "" {
		t.Fatal("expected the CI repair turn to run")
	}
	requirePromptContains(t, capturedPrompt, memoryHandsOffProbe, memoryHandsOffNoEdit, memoryHandsOffNoFix)
}

// The rebase conflict resolver gets the scoped variant: it must still be able
// to resolve a conflicted AGENTS.md, but may not touch the content otherwise.
func TestRebaseStep_ConflictPromptResolvesMemoryFileMarkers(t *testing.T) {
	t.Parallel()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")

	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("base content\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "base commit")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "main")

	gitCmd(t, dir, "checkout", "-b", "feature")
	os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("feature change\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature change")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	gitCmd(t, dir, "checkout", "main")
	os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("main change\n"), 0o644)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "main conflict")
	gitCmd(t, dir, "push", "origin", "main")
	gitCmd(t, dir, "checkout", "feature")

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("resolved content\n"), 0o644)
			cmd := exec.Command("git", "add", "AGENTS.md")
			cmd.Dir = dir
			cmd.Env = append(os.Environ(),
				"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.com",
				"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.com",
			)
			if out, err := cmd.CombinedOutput(); err != nil {
				return nil, fmt.Errorf("git add: %s: %w", out, err)
			}
			cmd = exec.Command("git", "rebase", "--continue")
			cmd.Dir = dir
			cmd.Env = append(os.Environ(),
				"GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.com",
				"GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.com",
				"GIT_EDITOR=true",
			)
			if out, err := cmd.CombinedOutput(); err != nil {
				return nil, fmt.Errorf("git rebase --continue: %s: %w", out, err)
			}
			return &agent.Result{Output: json.RawMessage(`{"summary":"resolve conflict"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Repo.UpstreamURL = upstream
	sctx.Fixing = true
	sctx.PreviousFindings = `{"findings":[{"severity":"warning","file":"AGENTS.md","description":"merge conflict rebasing onto origin/main"}]}`

	if _, err := (&RebaseStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) != 1 {
		t.Fatalf("expected 1 conflict-resolution call, got %d", len(ag.calls))
	}
	prompt := ag.calls[0].Prompt
	requirePromptContains(t, prompt, "- AGENTS.md", memoryConflictProbe, memoryConflictResolution, memoryConflictNoOtherEdits)
	// The conflict prompt must not carry the generic never-touch wording: a
	// conflicted AGENTS.md has to be resolvable for the rebase to conclude.
	requirePromptOmits(t, prompt, memoryHandsOffNoEdit, memoryHandsOffNoFix)
}

// A modify/delete conflict has no conflict markers. The merge resolver must
// still ask the agent to settle the conflicted memory file itself.
func TestRebaseStep_MergeStrategyConflictPromptResolvesMemoryFileModifyDelete(t *testing.T) {
	t.Parallel()
	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")
	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	writeFixtureFile(t, dir, "CLAUDE.md", "base\n")
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "base")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "push", "origin", "main")
	gitCmd(t, dir, "checkout", "-b", "feature")
	writeFixtureFile(t, dir, "CLAUDE.md", "feature change\n")
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "feature")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "checkout", "main")
	gitCmd(t, dir, "rm", "CLAUDE.md")
	gitCmd(t, dir, "commit", "-m", "remove memory file")
	gitCmd(t, dir, "push", "origin", "main")
	gitCmd(t, dir, "checkout", "feature")

	var prompt string
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			prompt = opts.Prompt
			if got := gitCmd(t, dir, "ls-files", "-u", "--", "CLAUDE.md"); got == "" {
				t.Fatal("expected an unresolved modify/delete conflict in CLAUDE.md")
			}
			if content, err := os.ReadFile(filepath.Join(dir, "CLAUDE.md")); err != nil || strings.Contains(string(content), "<<<<<<<") {
				t.Fatalf("expected marker-less modify/delete conflict, content %q: %v", content, err)
			}
			gitCmd(t, dir, "add", "CLAUDE.md")
			gitCmd(t, dir, "commit", "--no-edit")
			return &agent.Result{Output: json.RawMessage(`{"summary":"kept both sides"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Run.Branch = "refs/heads/feature"
	sctx.Repo.UpstreamURL = upstream
	sctx.Config.Rebase.Strategy = config.RebaseStrategyMerge
	sctx.Fixing = true

	if _, err := (&RebaseStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) != 1 {
		t.Fatalf("expected 1 conflict-resolution call, got %d", len(ag.calls))
	}
	requirePromptContains(t, prompt, "- CLAUDE.md", memoryConflictProbe, memoryConflictResolution, memoryConflictNoOtherEdits)
	requirePromptOmits(t, prompt, memoryHandsOffNoEdit, memoryHandsOffNoFix)
}

// PR drafting is output-oriented, but the same agent can still write to the
// worktree while drafting, and the push step would commit the leftovers.
func TestPRStep_DraftPromptKeepsMemoryFilesHandsOff(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	env, _ := fakeGH(t, "")

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			payload := json.RawMessage(`{"title":"feat: add bar","body":"## What Changed\n\n- add Bar()"}`)
			return &agent.Result{Output: payload}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env

	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) == 0 {
		t.Fatal("expected the PR draft turn to run")
	}
	requirePromptContains(t, ag.calls[0].Prompt, memoryHandsOffProbe, memoryHandsOffNoEdit, memoryHandsOffNoFix)
}

// The configured-title draft on an existing PR runs its own prompt path.
func TestPRStep_ConfiguredTitlePromptKeepsMemoryFilesHandsOff(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	env, _ := fakeGH(t, "https://github.com/test/repo/pull/42")

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			data, _ := json.Marshal(map[string]string{"title": "add widget"})
			return &agent.Result{Output: data}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = append(env, "FAKE_CLI_PR_BODY=existing author body", "FAKE_CLI_PR_TITLE=old title")
	sctx.Config.PR.TitleFormat = "{{.Branch}}: {{.Title}}"
	reviewStep, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.UpdateStepStatus(reviewStep.ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}

	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) != 1 {
		t.Fatalf("expected one configured-title draft call, got %d", len(ag.calls))
	}
	requirePromptContains(t, ag.calls[0].Prompt, memoryHandsOffProbe, memoryHandsOffNoEdit, memoryHandsOffNoFix)
}

// The template narrative draft runs yet another prompt path.
func TestPRStep_TemplateNarrativePromptKeepsMemoryFilesHandsOff(t *testing.T) {
	t.Parallel()
	sctx, ag, _ := templateTestContext(t)
	env, _ := fakeGH(t, "")
	bodyFile := filepath.Join(t.TempDir(), "body.md")
	sctx.Env = append(env, "FAKE_CLI_PR_BODY_FILE="+bodyFile)

	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) != 1 {
		t.Fatalf("expected one template narrative call, got %d", len(ag.calls))
	}
	requirePromptContains(t, ag.calls[0].Prompt, memoryHandsOffProbe, memoryHandsOffNoEdit, memoryHandsOffNoFix)
}

// The document step is the single exception: it may edit the memory files to
// correct factually wrong content, but must never add to them because
// something is missing - so its prompt carries the correction-only wording
// and must not carry the generic never-touch rule, which would block
// legitimate corrections.
func TestDocumentStep_PromptScopesMemoryFilesToCorrections(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"docs current"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	if _, err := (&DocumentStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) == 0 {
		t.Fatal("expected the document turn to run")
	}
	prompt := ag.calls[0].Prompt
	requirePromptContains(t, prompt,
		memoryDocCorrectOnly,
		memoryDocNoAdditions,
		memoryDocNoCreate,
		memoryDocIncidentScope,
	)
	requirePromptOmits(t, prompt,
		// The generic rule would forbid the corrections the step is allowed
		// to make.
		memoryHandsOffNoEdit,
		memoryHandsOffNoFix,
		// The pre-change framing invited proactive additions.
		"high-value project-intrinsic knowledge useful to almost every future session",
	)
}

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

type documentRecordingInvoker struct {
	requests []agent.InvocationRequest
	run      func(agent.InvocationRequest) (*agent.Result, error)
}

func (i *documentRecordingInvoker) Invoke(_ context.Context, request agent.InvocationRequest) (*agent.Result, error) {
	i.requests = append(i.requests, request)
	return i.run(request)
}

func TestDocumentStep_AgentManaged_FixesAndCommitsWithoutApproval(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	invoker := &documentRecordingInvoker{}
	invoker.run = func(request agent.InvocationRequest) (*agent.Result, error) {
		switch request.Purpose {
		case types.PurposeDocumentationAuthoring:
			if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Updated\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"update README"}`)}, nil
		case types.PurposeDocumentationVerification:
			if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != headSHA {
				t.Fatalf("documentation was committed before verification: HEAD = %s, want %s", got, headSHA)
			}
			if status := gitStatusPorcelain(t, dir); status == "" {
				t.Fatal("expected verifier to inspect the uncommitted documentation candidate")
			}
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"documentation verified"}`)}, nil
		default:
			t.Fatalf("unexpected documentation purpose %q", request.Purpose)
			return nil, nil
		}
	}
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "unused"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Invoker = invoker

	step := &DocumentStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(invoker.requests) != 2 {
		t.Fatalf("expected author then independent verifier, got %d invocations", len(invoker.requests))
	}
	if invoker.requests[0].Purpose != types.PurposeDocumentationAuthoring || invoker.requests[1].Purpose != types.PurposeDocumentationVerification {
		t.Fatalf("documentation purposes = %q then %q", invoker.requests[0].Purpose, invoker.requests[1].Purpose)
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

func TestDocumentStep_RejectsInconclusiveVerificationBeforeCommit(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)
	invoker := &documentRecordingInvoker{}
	invoker.run = func(request agent.InvocationRequest) (*agent.Result, error) {
		if request.Purpose == types.PurposeDocumentationAuthoring {
			if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Updated\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"update README"}`)}, nil
		}
		return &agent.Result{Output: json.RawMessage(`{"findings":[]}`)}, nil
	}
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "unused"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Invoker = invoker

	if _, err := (&DocumentStep{}).Execute(sctx); err == nil {
		t.Fatal("expected schema-incomplete documentation verification to fail closed")
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != headSHA {
		t.Fatalf("inconclusive verification committed HEAD %s, want %s", got, headSHA)
	}
	if got := lastCommitMessage(t, dir); got == "no-mistakes(document): update README" {
		t.Fatal("inconclusive documentation verification created a documentation commit")
	}
}

func TestDocumentStep_MutatingVerifierErrorCannotBypassIntegrityCheck(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)
	invoker := &documentRecordingInvoker{}
	invoker.run = func(request agent.InvocationRequest) (*agent.Result, error) {
		if request.Purpose == types.PurposeDocumentationAuthoring {
			if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Updated\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"update README"}`)}, nil
		}
		if err := os.WriteFile(filepath.Join(dir, "verifier-mutation.txt"), []byte("not verified\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return nil, errors.New("documentation verifier transport failed")
	}
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "unused"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Invoker = invoker

	_, err := (&DocumentStep{}).Execute(sctx)
	if err == nil || !strings.Contains(err.Error(), "documentation verifier mutated the candidate") {
		t.Fatalf("document error = %v, want candidate-integrity failure", err)
	}
	if !strings.Contains(err.Error(), "documentation verifier transport failed") {
		t.Fatalf("document error = %v, want underlying invocation failure retained", err)
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != headSHA {
		t.Fatalf("mutating verifier error committed HEAD %s, want %s", got, headSHA)
	}
	if got := lastCommitMessage(t, dir); got == "no-mistakes(document): update README" {
		t.Fatal("mutating verifier error accepted the documentation candidate")
	}
}

func TestDocumentStep_MutationTakesPrecedenceOverParseError(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)
	invoker := &documentRecordingInvoker{}
	invoker.run = func(request agent.InvocationRequest) (*agent.Result, error) {
		if request.Purpose == types.PurposeDocumentationAuthoring {
			if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Updated\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"update README"}`)}, nil
		}
		if err := os.WriteFile(filepath.Join(dir, "verifier-mutation.txt"), []byte("not verified\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return &agent.Result{Output: json.RawMessage(`{"findings":`)}, nil
	}
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "unused"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Invoker = invoker

	if _, err := (&DocumentStep{}).Execute(sctx); err == nil || !strings.Contains(err.Error(), "documentation verifier mutated the candidate") {
		t.Fatalf("document error = %v, want candidate-integrity failure before parse error", err)
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != headSHA {
		t.Fatalf("mutating parse error committed HEAD %s, want %s", got, headSHA)
	}
}

func TestDocumentStep_RejectsAuthorCommitBeforeVerification(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)
	invoker := &documentRecordingInvoker{}
	invoker.run = func(request agent.InvocationRequest) (*agent.Result, error) {
		if request.Purpose == types.PurposeDocumentationAuthoring {
			if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Updated\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, dir, "add", "--", "README.md")
			gitCmd(t, dir, "commit", "-m", "author committed before verification")
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"update README"}`)}, nil
		}
		return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"documentation verified"}`)}, nil
	}
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "unused"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Invoker = invoker

	if _, err := (&DocumentStep{}).Execute(sctx); err == nil {
		t.Fatal("expected an author-created commit to be rejected before verification")
	}
	if len(invoker.requests) != 1 {
		t.Fatalf("expected rejection before verifier launch, got %d invocations", len(invoker.requests))
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

func TestDocumentStep_MalformedAuthorOutputFailsBeforeCommit(t *testing.T) {
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

	if _, err := (&DocumentStep{}).Execute(sctx); err == nil {
		t.Fatal("expected malformed author output to fail closed")
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != headSHA {
		t.Fatalf("malformed author output committed HEAD %s, want %s", got, headSHA)
	}
}

func TestDocumentStep_NoStructuredAuthorOutputFailsClosed(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Text: "docs status unavailable"}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	if _, err := (&DocumentStep{}).Execute(sctx); err == nil {
		t.Fatal("expected missing structured author output to fail closed")
	}
}

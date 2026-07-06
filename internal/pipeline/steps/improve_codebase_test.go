package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestImproveCodebaseStep_ModeOffSkips(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		t.Fatal("agent should not run when improve-codebase is off")
		return nil, nil
	}}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.ImproveCodebase.Mode = config.ImproveCodebaseModeOff

	outcome, err := (&ImproveCodebaseStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Skipped {
		t.Fatal("expected improve-codebase off mode to skip")
	}
}

func TestImproveCodebaseStep_ModeAlwaysRunsAgent(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	findingsJSON, _ := json.Marshal(Findings{Summary: "clear"})
	ag := &mockAgent{runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if !opts.ReadOnly {
			t.Fatal("expected improve-codebase agent invocation to request read-only mode")
		}
		if !strings.Contains(opts.Prompt, "Run the local improve-codebase skill") {
			t.Fatalf("prompt did not invoke improve-codebase skill: %s", opts.Prompt)
		}
		if !strings.Contains(opts.Prompt, "no-mistakes pipeline gate mode") {
			t.Fatalf("prompt did not request pipeline gate mode: %s", opts.Prompt)
		}
		if !strings.Contains(opts.Prompt, "Do not edit files") {
			t.Fatal("expected read-only guardrail in prompt")
		}
		return &agent.Result{Output: findingsJSON}, nil
	}}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.ImproveCodebase.Mode = config.ImproveCodebaseModeAlways

	outcome, err := (&ImproveCodebaseStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Skipped {
		t.Fatal("expected always mode to run")
	}
	if outcome.NeedsApproval {
		t.Fatal("expected clean findings not to need approval")
	}
	if len(ag.calls) != 1 {
		t.Fatalf("agent calls = %d, want 1", len(ag.calls))
	}
}

func TestImproveCodebaseStep_PromptIncludesIgnorePatterns(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	findingsJSON, _ := json.Marshal(Findings{Summary: "clear"})
	ag := &mockAgent{runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		for _, want := range []string{
			"ignore patterns: vendor/**, generated/**",
			"Exclude files and paths matched by ignore_patterns from findings.",
		} {
			if !strings.Contains(opts.Prompt, want) {
				t.Fatalf("prompt missing %q:\n%s", want, opts.Prompt)
			}
		}
		return &agent.Result{Output: findingsJSON}, nil
	}}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.ImproveCodebase.Mode = config.ImproveCodebaseModeAlways
	sctx.Config.IgnorePatterns = []string{"vendor/**", "generated/**"}

	if _, err := (&ImproveCodebaseStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
}

func TestImproveCodebaseStep_RunsAgentInDisposableCheckout(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)
	originalGitFile, err := os.ReadFile(filepath.Join(dir, ".git"))
	if err == nil {
		t.Fatalf("test repo .git = %q, want git directory", originalGitFile)
	}
	beforeStatus := gitCmd(t, dir, "status", "--porcelain", "--ignored")
	var auditDir string

	ag := &mockAgent{runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if opts.CWD == dir {
			t.Fatal("agent CWD = original worktree, want disposable checkout")
		}
		auditDir = opts.CWD
		if got := gitCmd(t, opts.CWD, "rev-parse", "HEAD"); got != headSHA {
			t.Fatalf("disposable HEAD = %s, want %s", got, headSHA)
		}
		hooksDir := gitCmd(t, opts.CWD, "rev-parse", "--git-path", "hooks")
		mustWriteFile(t, filepath.Join(hooksDir, "pre-commit"), "#!/bin/sh\nexit 1\n")
		gitCmd(t, opts.CWD, "update-ref", "refs/tags/agent-tag", headSHA)
		gitCmd(t, opts.CWD, "update-index", "--skip-worktree", "feature.txt")
		gitCmd(t, opts.CWD, "config", "--local", "core.fsmonitor", "agent-hook")
		if err := os.RemoveAll(filepath.Join(opts.CWD, ".git")); err != nil {
			t.Fatal(err)
		}
		mustWriteFile(t, filepath.Join(opts.CWD, ".git"), "poisoned\n")
		return &agent.Result{Output: []byte(`{"findings":[],"summary":"clear"}`)}, nil
	}}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.ImproveCodebase.Mode = config.ImproveCodebaseModeAlways

	outcome, err := (&ImproveCodebaseStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatal("expected clean disposable audit findings not to need approval")
	}
	if _, err := os.Stat(auditDir); !os.IsNotExist(err) {
		t.Fatalf("audit checkout stat err = %v, want removed", err)
	}
	if status := gitCmd(t, dir, "status", "--porcelain", "--ignored"); status != beforeStatus {
		t.Fatalf("original status = %q, want %q", status, beforeStatus)
	}
	if _, err := git.Run(context.Background(), dir, "rev-parse", "refs/tags/agent-tag"); err == nil {
		t.Fatal("agent tag exists in original repo, want isolated disposable refs")
	}
	if got := gitCmd(t, dir, "ls-files", "-v", "--", "feature.txt"); strings.HasPrefix(got, "S ") {
		t.Fatalf("original index flag = %q, want skip-worktree absent", got)
	}
	if got, err := git.Run(context.Background(), dir, "config", "--local", "--get", "core.fsmonitor"); err == nil {
		t.Fatalf("original core.fsmonitor = %q, want unset", got)
	}
}

func TestImproveCodebaseStep_AutoSkipsSmallChange(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		t.Fatal("agent should not run for a small isolated text change")
		return nil, nil
	}}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})

	outcome, err := (&ImproveCodebaseStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.Skipped {
		t.Fatal("expected small auto-mode change to skip")
	}
	if !strings.Contains(outcome.Findings, "small and not structurally risky") {
		t.Fatalf("findings = %q, want skip reason", outcome.Findings)
	}
}

func TestImproveCodebaseStep_AutoRunsForCrossDirectoryMove(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	mustWriteFile(t, filepath.Join(dir, "internal", "api", "client.go"), strings.Repeat("package api\n\nfunc clientMarker() string { return \"client\" }\n", 4))
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "add api client")
	baseSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "checkout", "-b", "feature")
	if err := os.MkdirAll(filepath.Join(dir, "pkg", "client"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "mv", "internal/api/client.go", "pkg/client/client.go")
	gitCmd(t, dir, "commit", "-m", "move client")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	findingsJSON, _ := json.Marshal(Findings{Summary: "clear"})
	ag := &mockAgent{runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if !strings.Contains(opts.Prompt, "file moved across directories") {
			t.Fatalf("prompt missing trigger reason: %s", opts.Prompt)
		}
		return &agent.Result{Output: findingsJSON}, nil
	}}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})

	outcome, err := (&ImproveCodebaseStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Skipped {
		t.Fatal("expected cross-directory move to run")
	}
	if len(ag.calls) != 1 {
		t.Fatalf("agent calls = %d, want 1", len(ag.calls))
	}
}

func TestImproveCodebaseStep_AutoRunsForManySourceFiles(t *testing.T) {
	t.Parallel()
	dir, baseSHA, _ := setupGitRepo(t)
	for i := 0; i < improveCodebaseSourceFileThreshold+1; i++ {
		mustWriteFile(t, filepath.Join(dir, "pkg", "many", fmt.Sprintf("file%02d.go", i)), "package many\n")
	}
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-m", "touch many files")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	findingsJSON, _ := json.Marshal(Findings{Summary: "clear"})
	ag := &mockAgent{runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if !strings.Contains(opts.Prompt, "source files changed") {
			t.Fatalf("prompt missing source-file trigger reason: %s", opts.Prompt)
		}
		return &agent.Result{Output: findingsJSON}, nil
	}}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})

	outcome, err := (&ImproveCodebaseStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Skipped {
		t.Fatal("expected many source files to run")
	}
}

func TestImproveCodebaseStep_BlockingFindingsNeedApproval(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	findings := Findings{
		Items: []Finding{{
			ID:          "ic-1",
			Severity:    "warning",
			File:        "internal/api/client.go",
			Description: "new adapter boundary duplicates existing provider mechanics",
			Action:      "ask-user",
		}},
		Summary: "1 structural warning",
	}
	findingsJSON, _ := json.Marshal(findings)
	ag := &mockAgent{runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: findingsJSON}, nil
	}}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.ImproveCodebase.Mode = config.ImproveCodebaseModeAlways

	outcome, err := (&ImproveCodebaseStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("expected warning finding to need approval")
	}
	if outcome.AutoFixable {
		t.Fatal("expected improve-codebase gate not to be auto-fixable")
	}
	if !outcome.DisableFix {
		t.Fatal("expected improve-codebase gate to disable manual fix")
	}
}

func TestImproveCodebaseStep_NormalizesAuditOnlyActions(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	findings := Findings{
		Items: []Finding{
			{ID: "ic-1", Severity: "warning", Description: "structural issue", Action: types.ActionAutoFix},
			{ID: "ic-2", Severity: "info", Description: "note", Action: types.ActionAutoFix},
		},
		Summary: "mixed actions",
	}
	findingsJSON, _ := json.Marshal(findings)
	ag := &mockAgent{runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		if !strings.Contains(string(opts.JSONSchema), `"enum": ["no-op", "ask-user"]`) {
			t.Fatalf("expected audit-only findings schema, got %s", opts.JSONSchema)
		}
		return &agent.Result{Output: findingsJSON}, nil
	}}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.ImproveCodebase.Mode = config.ImproveCodebaseModeAlways

	outcome, err := (&ImproveCodebaseStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	var got Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &got); err != nil {
		t.Fatal(err)
	}
	if got.Items[0].Action != types.ActionAskUser {
		t.Fatalf("warning action = %q, want %q", got.Items[0].Action, types.ActionAskUser)
	}
	if got.Items[1].Action != types.ActionNoOp {
		t.Fatalf("info action = %q, want %q", got.Items[1].Action, types.ActionNoOp)
	}
}

func TestImproveCodebaseSourceFileIgnoresGeneratedAndVendoredPaths(t *testing.T) {
	t.Parallel()
	for _, path := range []string{
		"vendor/example/client.go",
		"node_modules/example/index.ts",
		"internal/vendor/example/client.go",
		"web/node_modules/example/index.ts",
		"internal/api/client_generated.go",
		"internal/api/client.pb.go",
	} {
		if isImproveCodebaseSourceFile(path) {
			t.Errorf("%s should not count as an improve-codebase source file", path)
		}
	}
	if !isImproveCodebaseSourceFile("internal/api/client.go") {
		t.Error("ordinary source file should count")
	}
}

func TestImproveCodebaseHighRiskPathIncludesNoMistakesConfig(t *testing.T) {
	t.Parallel()
	if !isImproveCodebaseHighRiskPath(".no-mistakes.yaml") {
		t.Fatal(".no-mistakes.yaml should trigger improve-codebase auto mode")
	}
}

func TestAllStepsIncludesImproveCodebaseAfterReview(t *testing.T) {
	t.Parallel()
	steps := AllSteps()
	var got []types.StepName
	for _, step := range steps {
		got = append(got, step.Name())
	}
	want := []types.StepName{
		types.StepIntent,
		types.StepRebase,
		types.StepReview,
		types.StepImproveCodebase,
		types.StepTest,
		types.StepDocument,
		types.StepLint,
		types.StepPush,
		types.StepPR,
		types.StepCI,
	}
	if len(got) != len(want) {
		t.Fatalf("steps = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("steps = %v, want %v", got, want)
		}
	}
}

func mustWriteFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

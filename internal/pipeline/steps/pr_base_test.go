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
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
)

func TestPipelineBaseBranch_DefaultAndConfigured(t *testing.T) {
	sctx := &pipeline.StepContext{
		Repo:   &db.Repo{DefaultBranch: "main"},
		Config: &config.Config{},
	}
	if got := pipelineBaseBranch(sctx); got != "main" {
		t.Fatalf("unset pr.base_branch = %q, want repository default main", got)
	}

	sctx.Config.PR.BaseBranch = "quality-assurance"
	if got := pipelineBaseBranch(sctx); got != "quality-assurance" {
		t.Fatalf("configured pipeline base = %q, want quality-assurance", got)
	}
}

func TestResolveBranchBaseSHA_UsesConfiguredPRBase(t *testing.T) {
	dir := t.TempDir()
	gitCmd(t, dir, "init")
	gitCmd(t, dir, "config", "user.name", "test")
	gitCmd(t, dir, "config", "user.email", "test@test.com")
	gitCmd(t, dir, "checkout", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "main.txt"), []byte("main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", ".")
	gitCmd(t, dir, "commit", "-m", "main")
	mainSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	gitCmd(t, dir, "checkout", "-b", "quality-assurance")
	if err := os.WriteFile(filepath.Join(dir, "qa.txt"), []byte("qa\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", ".")
	gitCmd(t, dir, "commit", "-m", "qa")
	qaSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	gitCmd(t, dir, "checkout", "-b", "feature")
	if err := os.WriteFile(filepath.Join(dir, "feature.txt"), []byte("feature\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", ".")
	gitCmd(t, dir, "commit", "-m", "feature")
	featureSHA := gitCmd(t, dir, "rev-parse", "HEAD")

	sctx := &pipeline.StepContext{
		Ctx:     context.Background(),
		Repo:    &db.Repo{DefaultBranch: "main"},
		Config:  &config.Config{PR: config.PR{BaseBranch: "quality-assurance"}},
		WorkDir: dir,
	}
	got := resolveBranchBaseSHA(sctx.Ctx, dir, mainSHA, pipelineBaseBranch(sctx))
	if got != qaSHA {
		t.Fatalf("branch delta base = %s, want configured PR base tip %s", got, qaSHA)
	}
	if got == mainSHA {
		t.Fatal("configured branch delta fell back to repository default branch")
	}

	// Every local branch-delta step must receive the same configured base, not
	// just PR creation. Their prompts are a generated interface that exposes
	// the exact commit each step scoped itself against.
	steps := []struct {
		name     string
		step     pipeline.Step
		commands config.Commands
	}{
		{name: "review", step: &ReviewStep{}},
		{name: "test", step: &TestStep{}},
		{name: "document", step: &DocumentStep{}, commands: config.Commands{Lint: "true"}},
		{name: "lint", step: &LintStep{}},
	}
	for _, tc := range steps {
		t.Run(tc.name, func(t *testing.T) {
			mock := &mockAgent{
				name: "test",
				runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
					return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"clean","tested":["focused"],"testing_summary":"clean","artifacts":[],"risk_level":"low","risk_rationale":"focused change","risk_scope":"source-or-external"}`)}, nil
				},
			}
			stepCtx := newTestContextWithDBRecords(t, mock, dir, mainSHA, featureSHA, tc.commands)
			stepCtx.Config.PR.BaseBranch = "quality-assurance"
			if _, err := tc.step.Execute(stepCtx); err != nil {
				t.Fatal(err)
			}
			if len(mock.calls) != 1 {
				t.Fatalf("agent calls = %d, want one prompt", len(mock.calls))
			}
			prompt := mock.calls[0].Prompt
			if !strings.Contains(prompt, "base commit: "+qaSHA) {
				t.Fatalf("prompt did not use configured PR base %s:\n%s", qaSHA, prompt)
			}
			if strings.Contains(prompt, "base commit: "+mainSHA) {
				t.Fatalf("prompt used repository default branch instead of configured PR base:\n%s", prompt)
			}
		})
	}

	t.Run("pr summary and routing", func(t *testing.T) {
		env, logFile := fakeGH(t, "")
		mock := &mockAgent{
			name: "test",
			runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				return &agent.Result{Output: json.RawMessage(`{"title":"feat: target qa","body":"## What Changed\n\n- add feature"}`)}, nil
			},
		}
		stepCtx := newTestContextWithDBRecords(t, mock, dir, mainSHA, featureSHA, config.Commands{})
		stepCtx.Config.PR.BaseBranch = "quality-assurance"
		stepCtx.Env = env
		if _, err := (&PRStep{}).Execute(stepCtx); err != nil {
			t.Fatal(err)
		}
		if len(mock.calls) != 1 || !strings.Contains(mock.calls[0].Prompt, "base commit: "+qaSHA) || !strings.Contains(mock.calls[0].Prompt, "PR base branch: quality-assurance") {
			t.Fatalf("PR summary prompt did not use configured base: %#v", mock.calls)
		}
		logBytes, err := os.ReadFile(logFile)
		if err != nil {
			t.Fatal(err)
		}
		if logText := string(logBytes); !strings.Contains(logText, "--base quality-assurance") {
			t.Fatalf("PR discovery/creation did not use configured base:\n%s", logText)
		}
	})
}

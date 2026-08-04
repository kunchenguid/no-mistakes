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

	upstream := t.TempDir()
	gitCmd(t, upstream, "init", "--bare")
	gitCmd(t, dir, "remote", "add", "origin", upstream)
	gitCmd(t, dir, "push", "origin", "main", "quality-assurance", "feature")

	sctx := &pipeline.StepContext{
		Ctx:     context.Background(),
		Repo:    &db.Repo{DefaultBranch: "main"},
		Config:  &config.Config{PR: config.PR{BaseBranch: "quality-assurance", ResolvedBaseSHA: qaSHA}},
		WorkDir: dir,
	}
	got := resolveBranchBaseSHA(sctx.Ctx, dir, mainSHA, pipelineBaseTarget(sctx))
	if got != qaSHA {
		t.Fatalf("branch delta base = %s, want configured PR base tip %s", got, qaSHA)
	}
	if got == mainSHA {
		t.Fatal("configured branch delta fell back to repository default branch")
	}
	gitCmd(t, dir, "branch", "-f", "quality-assurance", "main")
	if moved := resolveBranchBaseSHA(sctx.Ctx, dir, mainSHA, pipelineBaseTarget(sctx)); moved != qaSHA {
		t.Fatalf("branch delta followed mutable base ref to %s, want pinned %s", moved, qaSHA)
	}
	gitCmd(t, dir, "branch", "-f", "quality-assurance", qaSHA)

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
			stepCtx.Config.PR.ResolvedBaseSHA = qaSHA
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

	t.Run("intent", func(t *testing.T) {
		stepCtx := newTestContextWithDBRecords(t, &fakeIntentAgent{}, dir, strings.Repeat("0", 40), qaSHA, config.Commands{})
		stepCtx.Config.Intent = config.Intent{Enabled: true, Threshold: 0.1, SlackDays: 3}
		stepCtx.Config.PR.BaseBranch = "quality-assurance"
		stepCtx.Config.PR.ResolvedBaseSHA = qaSHA
		var logs []string
		stepCtx.Log = func(line string) { logs = append(logs, line) }
		outcome, err := (&IntentStep{}).Execute(stepCtx)
		if err != nil {
			t.Fatal(err)
		}
		if outcome == nil || !outcome.Skipped {
			t.Fatalf("configured-base empty intent delta outcome = %#v, want skipped", outcome)
		}
		if !strings.Contains(strings.Join(logs, "\n"), "no diff between base and head") {
			t.Fatalf("intent step did not scope its delta to configured base: %v", logs)
		}
	})

	t.Run("empty diff skips remaining", func(t *testing.T) {
		gitCmd(t, dir, "checkout", "quality-assurance")
		t.Cleanup(func() { gitCmd(t, dir, "checkout", "feature") })
		stepCtx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, mainSHA, qaSHA, config.Commands{})
		stepCtx.Config.PR.BaseBranch = "quality-assurance"
		stepCtx.Config.PR.ResolvedBaseSHA = qaSHA
		outcome, err := updateHeadSHA(stepCtx.Ctx, stepCtx)
		if err != nil {
			t.Fatal(err)
		}
		if outcome == nil || !outcome.SkipRemaining {
			t.Fatalf("configured-base empty diff outcome = %#v, want SkipRemaining", outcome)
		}
		gitCmd(t, dir, "checkout", "feature")
	})

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
		stepCtx.Config.PR.ResolvedBaseSHA = qaSHA
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
		logText := string(logBytes)
		if !strings.Contains(logText, "--base quality-assurance") {
			t.Fatalf("PR discovery/creation did not use configured base:\n%s", logText)
		}
		t.Logf("provider CLI transcript:\n%s", logText)
	})
}

func TestDeliveryRejectsConfiguredBaseMovingBehindSnapshot(t *testing.T) {
	for _, stepName := range []string{"push", "pr"} {
		t.Run(stepName, func(t *testing.T) {
			dir, _, baseSHA, headSHA, snapshotSHA := setupConfiguredBaseRewrite(t, "backward")
			sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
			sctx.Config.PR.BaseBranch = "quality-assurance"
			sctx.Config.PR.ResolvedBaseSHA = snapshotSHA

			var err error
			switch stepName {
			case "push":
				recordReviewApproval(t, sctx, headSHA)
				_, err = (&PushStep{}).Execute(sctx)
			case "pr":
				env, logFile := fakeGH(t, "")
				sctx.Env = env
				_, err = (&PRStep{}).Execute(sctx)
				logBytes, readErr := os.ReadFile(logFile)
				if readErr != nil {
					t.Fatal(readErr)
				}
				if strings.Contains(string(logBytes), "pr create") {
					t.Fatalf("PR delivery proceeded after base reset:\n%s", logBytes)
				}
			}
			if err == nil || !strings.Contains(err.Error(), "immutable run snapshot") {
				t.Fatalf("%s step did not reject reset configured base: %v", stepName, err)
			}
		})
	}
}

func TestPRStep_RevalidatesConfiguredBaseAfterContentGeneration(t *testing.T) {
	dir, _, baseSHA, headSHA, snapshotSHA := setupConfiguredBaseRewrite(t, "")
	env, logFile := fakeGH(t, "")
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		gitCmd(t, dir, "push", "--force", "origin", baseSHA+":refs/heads/quality-assurance")
		return &agent.Result{Output: json.RawMessage(`{"title":"feat: update","body":"## What Changed\n\n- update"}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.PR.BaseBranch = "quality-assurance"
	sctx.Config.PR.ResolvedBaseSHA = snapshotSHA
	sctx.Env = env

	_, err := (&PRStep{}).Execute(sctx)
	if err == nil || !strings.Contains(err.Error(), "immutable run snapshot") {
		t.Fatalf("PR step did not reject base reset during content generation: %v", err)
	}
	logBytes, readErr := os.ReadFile(logFile)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if strings.Contains(string(logBytes), "pr list") || strings.Contains(string(logBytes), "pr create") {
		t.Fatalf("PR provider delivery proceeded after content-generation reset:\n%s", logBytes)
	}
}

func TestPRStep_RevalidatesConfiguredBaseImmediatelyBeforeCreate(t *testing.T) {
	dir, _, baseSHA, headSHA, snapshotSHA := setupConfiguredBaseRewrite(t, "")
	env, logFile := fakeGH(t, "")
	env = append(env,
		"FAKE_CLI_PR_LIST_RESET_SOURCE="+baseSHA,
		"FAKE_CLI_PR_LIST_RESET_BRANCH=quality-assurance",
	)
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: json.RawMessage(`{"title":"feat: update","body":"## What Changed\n\n- update"}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.PR.BaseBranch = "quality-assurance"
	sctx.Config.PR.ResolvedBaseSHA = snapshotSHA
	sctx.Env = env

	_, err := (&PRStep{}).Execute(sctx)
	if err == nil || !strings.Contains(err.Error(), "immutable run snapshot") {
		t.Fatalf("PR step did not reject base reset during discovery: %v", err)
	}
	logBytes, readErr := os.ReadFile(logFile)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if strings.Contains(string(logBytes), "pr create") {
		t.Fatalf("PR creation proceeded after discovery-time reset:\n%s", logBytes)
	}
}

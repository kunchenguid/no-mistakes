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

func TestBuildStepRunsUnconfiguredBuildThroughAgent(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	marker := filepath.Join(dir, ".git", "agent-build-ran")
	ag := &mockAgent{
		name: "builder",
		runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
			for _, required := range []string{"run the smallest relevant build commands yourself", `non-empty "tested" array`, "Do not run tests"} {
				if !strings.Contains(opts.Prompt, required) {
					t.Fatalf("build prompt missing %q:\n%s", required, opts.Prompt)
				}
			}
			if err := os.WriteFile(marker, []byte("ran\n"), 0o644); err != nil {
				return nil, err
			}
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"frontend build passed","tested":["npm run build"]}`)}, nil
		},
	}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})

	outcome, err := (&BuildStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval || outcome.ExitCode != 0 {
		t.Fatalf("outcome = %#v, want successful build", outcome)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("agent did not run build probe: %v", err)
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings.Tested) != 1 || findings.Tested[0] != "npm run build" {
		t.Fatalf("tested = %#v, want agent build evidence", findings.Tested)
	}
}

func TestBuildStepWithoutCommandParksWhenAgentCannotRunBuild(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{
		name: "builder",
		runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"findings":[{"severity":"warning","description":"no build metadata exists","action":"ask-user"}],"summary":"no build command","tested":[]}`)}, nil
		},
	}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})

	outcome, err := (&BuildStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval || outcome.AutoFixable {
		t.Fatalf("outcome = %#v, want ask-user gate", outcome)
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if !types.HasAskUserFindings(findings) {
		t.Fatalf("findings = %#v, want ask-user finding", findings.Items)
	}
}

func TestBuildStepBlankOnlyEvidenceParksInsteadOfPassing(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{
		name: "builder",
		runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"","tested":["  ",""]}`)}, nil
		},
	}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})

	outcome, err := (&BuildStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval || outcome.AutoFixable {
		t.Fatalf("outcome = %#v, want ask-user gate for blank-only evidence", outcome)
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if !types.HasAskUserFindings(findings) || len(types.AutoFixableFindings(findings).Items) != 0 {
		t.Fatalf("findings = %#v, want only ask-user actions", findings.Items)
	}
}

func TestBuildStepAgentReportedFailureIsAutoFixable(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{
		name: "builder",
		runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"findings":[{"severity":"error","description":"TypeScript compilation failed","action":"auto-fix"}],"summary":"frontend does not compile","tested":["npm run build"]}`)}, nil
		},
	}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})

	outcome, err := (&BuildStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval || !outcome.AutoFixable {
		t.Fatalf("outcome = %#v, want auto-fixable build failure", outcome)
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if len(types.AutoFixableFindings(findings).Items) != 1 {
		t.Fatalf("findings = %#v, want one auto-fixable failure", findings.Items)
	}
}

func TestBuildStepMissingExecutionEvidenceIsNotAutoFixable(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{
		name: "builder",
		runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"findings":[{"severity":"error","description":"compile failed","action":"auto-fix"}],"summary":"compile failed","tested":[]}`)}, nil
		},
	}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})

	outcome, err := (&BuildStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval || outcome.AutoFixable {
		t.Fatalf("outcome = %#v, want non-auto-fixable missing-evidence gate", outcome)
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if !types.HasAskUserFindings(findings) || len(types.AutoFixableFindings(findings).Items) != 0 {
		t.Fatalf("findings = %#v, want only ask-user actions", findings.Items)
	}
}

func TestBuildStepRefusesAgentBuildSideEffects(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{
		name: "builder",
		runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
			if err := os.WriteFile(filepath.Join(opts.CWD, "agent-output.txt"), []byte("unexpected\n"), 0o644); err != nil {
				return nil, err
			}
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"build passed","tested":["make build"]}`)}, nil
		},
	}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})

	_, err := (&BuildStep{}).Execute(sctx)
	if err == nil || !strings.Contains(err.Error(), "side-effect free") || !strings.Contains(err.Error(), "agent-output.txt") {
		t.Fatalf("Execute() error = %v, want agent-side-effect refusal", err)
	}
}

func TestBuildStepConfiguredCommandRunsWithoutAgent(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	command := "go env GOVERSION"
	ag := &mockAgent{name: "unused"}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{Build: command})
	var logs []string
	sctx.Log = func(message string) { logs = append(logs, message) }

	outcome, err := (&BuildStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval || outcome.ExitCode != 0 {
		t.Fatalf("outcome = %#v, want successful build", outcome)
	}
	if got := strings.Join(logs, "\n"); !strings.Contains(got, "running build: "+command) || !strings.Contains(got, "go1.") {
		t.Fatalf("build logs = %q, want command and Go version", got)
	}
	if len(ag.calls) != 0 {
		t.Fatalf("agent calls = %d, want 0", len(ag.calls))
	}
}

func TestBuildStepConfiguredCommandRefusesSideEffectsEvenOnFailure(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContext(t, &mockAgent{name: "unused"}, dir, baseSHA, headSHA, config.Commands{
		Build: "go env GOVERSION > build-output.txt && exit 17",
	})

	_, err := (&BuildStep{}).Execute(sctx)
	if err == nil || !strings.Contains(err.Error(), "exit code 17") || !strings.Contains(err.Error(), "side-effect free") || !strings.Contains(err.Error(), "build-output.txt") {
		t.Fatalf("Execute() error = %v, want build failure plus side-effect refusal", err)
	}
}

func TestBuildStepConfiguredCommandAllowsIgnoredOutput(t *testing.T) {
	dir, baseSHA, _ := setupGitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("build-output.txt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", ".gitignore")
	gitCmd(t, dir, "commit", "-m", "ignore build output")
	headSHA := gitCmd(t, dir, "rev-parse", "HEAD")
	sctx := newTestContext(t, &mockAgent{name: "unused"}, dir, baseSHA, headSHA, config.Commands{
		Build: "go env GOVERSION > build-output.txt",
	})

	outcome, err := (&BuildStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval || outcome.ExitCode != 0 {
		t.Fatalf("outcome = %#v, want successful build with ignored output", outcome)
	}
	if _, err := os.Stat(filepath.Join(dir, "build-output.txt")); err != nil {
		t.Fatalf("ignored build output: %v", err)
	}
}

func TestBuildStepConfiguredCommandFailureIsActionable(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContext(t, &mockAgent{name: "unused"}, dir, baseSHA, headSHA, config.Commands{
		Build: "go env GOVERSION && exit 17",
	})

	outcome, err := (&BuildStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval || !outcome.AutoFixable || outcome.ExitCode != 17 {
		t.Fatalf("outcome = %#v, want auto-fixable build failure", outcome)
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings.Items) != 1 || findings.Items[0].Action != types.ActionAutoFix || !strings.Contains(findings.Summary, "go1.") {
		t.Fatalf("findings = %#v, want compiler output and auto-fix action", findings)
	}
}

func TestBuildStepFixModeRepairsThenRebuilds(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/buildtest\n\ngo 1.25\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mainPath := filepath.Join(dir, "main.go")
	if err := os.WriteFile(mainPath, []byte("package main\nfunc main() { syntax error }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ag := &mockAgent{
		name: "builder",
		runFn: func(_ context.Context, _ agent.RunOpts) (*agent.Result, error) {
			if err := os.WriteFile(mainPath, []byte("package main\nfunc main() {}\n"), 0o644); err != nil {
				return nil, err
			}
			return &agent.Result{Output: json.RawMessage(`{"summary":"fix compile syntax"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Build: "go build -o .git/buildtest ./..."})
	sctx.Fixing = true
	sctx.PreviousFindings = `{"findings":[{"severity":"error","description":"main.go does not compile","action":"auto-fix"}]}`

	outcome, err := (&BuildStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval || outcome.ExitCode != 0 || outcome.FixSummary != "fix compile syntax" {
		t.Fatalf("outcome = %#v, want repaired successful build", outcome)
	}
	if len(ag.calls) != 1 {
		t.Fatalf("agent calls = %d, want one repair call", len(ag.calls))
	}
	repairedHead := gitCmd(t, dir, "rev-parse", "HEAD")
	if sctx.Run.HeadSHA != repairedHead {
		t.Fatalf("run head = %s, want agent repair commit %s", sctx.Run.HeadSHA, repairedHead)
	}
	stored, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.HeadSHA != repairedHead {
		t.Fatalf("stored head = %s, want agent repair commit %s", stored.HeadSHA, repairedHead)
	}
}

func TestReviewStepRefusesUnrecordedCommitAfterBuild(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	if err := os.WriteFile(filepath.Join(dir, "unrecorded.go"), []byte("package unrecorded\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, dir, "add", "unrecorded.go")
	gitCmd(t, dir, "commit", "-m", "unrecorded forward commit")
	sctx := newTestContext(t, &mockAgent{name: "reviewer"}, dir, baseSHA, headSHA, config.Commands{})

	_, err := (&ReviewStep{}).Execute(sctx)
	if err == nil || !strings.Contains(err.Error(), "does not match the pipeline's recorded head") {
		t.Fatalf("ReviewStep.Execute() error = %v, want unrecorded-head refusal", err)
	}
}

package steps

import (
	"context"
	"encoding/json"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func persistTestStepFindings(t *testing.T, sctx *pipeline.StepContext, findings string) {
	t.Helper()
	sr, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	sctx.StepResultID = sr.ID
	if err := sctx.DB.SetStepFindings(sr.ID, findings); err != nil {
		t.Fatal(err)
	}
}

func TestTestStep_VerifyApprovalOverride_FailingConfiguredCommand(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: json.RawMessage(passingScenarioFindingsJSON)}, nil
	}}
	testCmd := "printf 'configured command broke'; exit 7"
	if runtime.GOOS == "windows" {
		testCmd = "echo configured command broke && exit /b 7"
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Test: testCmd})
	var logs []string
	sctx.Log = func(line string) { logs = append(logs, line) }

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.ExitCode != 7 || !outcome.NeedsApproval {
		t.Fatalf("outcome = %+v, want a parked failing configured command", outcome)
	}
	if !containsLog(logs, "configured test command failed, asking agent to gather live evidence...") {
		t.Fatalf("missing configured-command failure log, got %q", logs)
	}
	if containsLog(logs, "baseline tests failed") {
		t.Fatalf("failure log still used baseline wording: %q", logs)
	}
	persistTestStepFindings(t, sctx, outcome.Findings)

	unresolved, err := (&TestStep{}).VerifyApprovalOverride(sctx)
	if err != nil {
		t.Fatalf("VerifyApprovalOverride() error = %v", err)
	}
	if !strings.Contains(unresolved, "configured test command failed with exit code 7") {
		t.Fatalf("unresolved = %q, want the configured-command failure reason", unresolved)
	}
}

func TestTestStep_VerifyApprovalOverride_PassingCommandLeavesNoMark(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: json.RawMessage(`{
  "findings": [{"severity":"error","description":"scenario failed","action":"ask-user"}],
  "summary": "live scenario failed",
  "tested": ["npm run e2e -- checkout"],
  "testing_summary": "drove checkout",
  "artifacts": [],
  "scenarios": [{"name":"user reaches the success screen","result":"fail","live":true,"evidence":"checkout.png","reason":""}],
  "verdict": "no-go"
}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Test: "exit 0"})

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0 from the passing configured command", outcome.ExitCode)
	}
	persistTestStepFindings(t, sctx, outcome.Findings)

	unresolved, err := (&TestStep{}).VerifyApprovalOverride(sctx)
	if err != nil {
		t.Fatalf("VerifyApprovalOverride() error = %v", err)
	}
	if unresolved != "" {
		t.Fatalf("unresolved = %q, want \"\" when the configured command passed", unresolved)
	}
}

func TestTestStep_VerifyApprovalOverride_NoConfiguredCommand(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	persistTestStepFindings(t, sctx, `{"findings":[{"severity":"error","description":"scenario failed"}]}`)

	unresolved, err := (&TestStep{}).VerifyApprovalOverride(sctx)
	if err != nil {
		t.Fatalf("VerifyApprovalOverride() error = %v", err)
	}
	if unresolved != "" {
		t.Fatalf("unresolved = %q, want \"\" when no commands.test is configured", unresolved)
	}
}

func TestTestStep_VerifyApprovalOverride_MissingStepResultFailsClosed(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{Test: "exit 1"})

	unresolved, err := (&TestStep{}).VerifyApprovalOverride(sctx)
	if err != nil {
		t.Fatalf("VerifyApprovalOverride() error = %v", err)
	}
	if unresolved == "" {
		t.Fatal("unresolved = \"\", want a fail-closed reason when the parked findings cannot be read")
	}
}

func containsLog(logs []string, want string) bool {
	for _, line := range logs {
		if strings.Contains(line, want) {
			return true
		}
	}
	return false
}

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
)

// The test step's evidence-agent decision is the contract test_command_sufficient
// changes, so these tests drive the real TestStep.Execute against real temporary
// git repositories and real shell commands rather than asserting on the
// predicate in isolation.
//
// The matrix they pin, row by row:
//
//	declaration | commands.test | user intent | evidence agent
//	------------|---------------|-------------|---------------
//	absent      | set, passes   | present     | runs   (backward compatible)
//	absent      | empty         | absent      | runs   (backward compatible)
//	true        | set, passes   | present     | skipped (the whole point)
//	true        | set, passes   | absent      | skipped (unchanged from today)
//	true        | set, FAILS    | present     | never reached; existing failure path
//	true        | empty         | present     | runs   (never a silent skip)

// TestTestStep_CommandSufficientSkipsEvidenceAgentWithUserIntent is the goal
// case: the measured waste is an evidence agent launched after a deterministic
// command already passed.
func TestTestStep_CommandSufficientSkipsEvidenceAgentWithUserIntent(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	baselineLog := filepath.Join(dir, "baseline.log")
	// The command records the commit it actually ran against, so the assertion
	// below is direct evidence rather than an inference from step ordering.
	testCmd := "git rev-parse HEAD > baseline.log"

	callCount := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			callCount++
			return &agent.Result{Output: json.RawMessage(`{"findings":[]}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Test: testCmd})
	sctx.UserIntent = "Show users a success screen after checkout"
	sctx.Config.TestCommandSufficient = true

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if callCount != 0 {
		t.Fatalf("expected no agent invocation when the trusted declaration covers a passing command, got %d", callCount)
	}
	if outcome.NeedsApproval {
		t.Fatal("expected a passing declared-sufficient command not to need approval")
	}

	// The command must actually have run, against this run's candidate head:
	// "no agent" is only acceptable because deterministic validation of THIS
	// commit happened, never as a skipped step.
	ranAt, err := os.ReadFile(baselineLog)
	if err != nil {
		t.Fatalf("expected the configured test command to have run: %v", err)
	}
	if got := strings.TrimSpace(string(ranAt)); got != headSHA {
		t.Fatalf("command ran against %s, want the run's candidate head %s", got, headSHA)
	}
	if sctx.Run.HeadSHA != headSHA {
		t.Fatalf("run head = %s, want %s", sctx.Run.HeadSHA, headSHA)
	}

	// The exact command is recorded against this head as what was tested.
	var findings Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
		t.Fatal(err)
	}
	if len(findings.Tested) != 1 || findings.Tested[0] != testCmd {
		t.Fatalf("expected the configured command recorded in tested, got %+v", findings.Tested)
	}
}

// TestTestStep_CommandSufficientIsLoggedNotSilent keeps a command-only pass
// inspectable. Without the declaration named in the log it is indistinguishable
// from a run that simply had no user intent to demonstrate.
func TestTestStep_CommandSufficientIsLoggedNotSilent(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Test: "true"})
	sctx.UserIntent = "Add a retry to the upload path"
	sctx.Config.TestCommandSufficient = true

	var logged []string
	prev := sctx.Log
	sctx.Log = func(msg string) {
		logged = append(logged, msg)
		if prev != nil {
			prev(msg)
		}
	}

	if _, err := (&TestStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(logged, "\n")
	if !strings.Contains(joined, "test_command_sufficient") {
		t.Fatalf("expected the declaration named in the step log, got:\n%s", joined)
	}
}

// TestTestStep_CommandSufficientWithoutCommandStillRunsEvidenceAgent is the
// fail-closed case. Config validation rejects this combination, so reaching the
// step with it means something upstream failed open; the step must still refuse
// to treat the declaration as permission to skip Test.
func TestTestStep_CommandSufficientWithoutCommandStillRunsEvidenceAgent(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	callCount := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			callCount++
			return &agent.Result{Output: json.RawMessage(`{"findings":[]}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Test: ""})
	sctx.UserIntent = "Add a retry to the upload path"
	sctx.Config.TestCommandSufficient = true

	if _, err := (&TestStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if callCount != 1 {
		t.Fatalf("a declaration with no command must never skip Test; expected 1 agent invocation, got %d", callCount)
	}
}

// TestTestStep_CommandSufficientWhitespaceCommandStillRunsEvidenceAgent covers
// the same fail-closed rule for a command that is only whitespace. runStepShellCommand
// would happily "succeed" at running nothing.
func TestTestStep_CommandSufficientWhitespaceCommandStillRunsEvidenceAgent(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	callCount := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			callCount++
			return &agent.Result{Output: json.RawMessage(`{"findings":[]}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Test: "   "})
	sctx.UserIntent = "Add a retry to the upload path"
	sctx.Config.TestCommandSufficient = true

	if _, err := (&TestStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if callCount != 1 {
		t.Fatalf("a whitespace-only command must not satisfy the declaration; expected 1 agent invocation, got %d", callCount)
	}
}

// TestTestStep_CommandSufficientFailingCommandKeepsActionableFailurePath proves
// the declaration cannot convert a red command into a pass. The non-zero exit
// returns before the evidence decision is ever reached.
func TestTestStep_CommandSufficientFailingCommandKeepsActionableFailurePath(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	callCount := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			callCount++
			return &agent.Result{Output: json.RawMessage(`{"findings":[]}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Test: "exit 3"})
	sctx.UserIntent = "Add a retry to the upload path"
	sctx.Config.TestCommandSufficient = true

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if callCount != 0 {
		t.Fatalf("expected no evidence agent on a failing command, got %d", callCount)
	}
	if !outcome.NeedsApproval || !outcome.AutoFixable {
		t.Fatalf("expected the existing actionable failure path, got NeedsApproval=%v AutoFixable=%v", outcome.NeedsApproval, outcome.AutoFixable)
	}
	if outcome.ExitCode != 3 {
		t.Fatalf("expected the command exit code preserved, got %d", outcome.ExitCode)
	}
	var findings Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
		t.Fatal(err)
	}
	if len(findings.Items) != 1 || findings.Items[0].Severity != "error" {
		t.Fatalf("expected one error finding, got %+v", findings.Items)
	}
}

// TestTestStep_CommandSufficientDoesNotSuppressFixAgent guards the repair pass.
// Fix mode answers a prior failure; it is not evidence gathering, so a
// sufficiency declaration must leave it alone.
func TestTestStep_CommandSufficientDoesNotSuppressFixAgent(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	callCount := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			callCount++
			return &agent.Result{Output: json.RawMessage(`{"summary":"fix flaky retry test"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Test: "true"})
	sctx.UserIntent = "Add a retry to the upload path"
	sctx.Config.TestCommandSufficient = true
	sctx.Fixing = true
	sctx.PreviousFindings = `{"findings":[{"severity":"error","description":"retry test failed"}]}`

	if _, err := (&TestStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if callCount != 1 {
		t.Fatalf("expected the fix agent to still run under the declaration, got %d invocations", callCount)
	}
	if !strings.Contains(ag.calls[0].Prompt, "Fix the failing tests in this repository") {
		t.Fatalf("expected the fix prompt, got:\n%s", ag.calls[0].Prompt)
	}
}

// TestTestStep_WithoutDeclarationBehaviorIsUnchanged pins backward
// compatibility: absent the field, both pre-existing agent triggers still fire.
func TestTestStep_WithoutDeclarationBehaviorIsUnchanged(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		cmd    string
		intent string
	}{
		{name: "passing_command_with_user_intent", cmd: "true", intent: "Add a retry to the upload path"},
		{name: "no_command_no_user_intent", cmd: "", intent: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, headSHA := setupGitRepo(t)
			callCount := 0
			ag := &mockAgent{
				name: "test",
				runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
					callCount++
					return &agent.Result{Output: json.RawMessage(`{"findings":[]}`)}, nil
				},
			}
			sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Test: tc.cmd})
			sctx.UserIntent = tc.intent
			// TestCommandSufficient deliberately left at its false zero value.

			if _, err := (&TestStep{}).Execute(sctx); err != nil {
				t.Fatal(err)
			}
			if callCount != 1 {
				t.Fatalf("expected unchanged evidence-agent behavior (1 invocation), got %d", callCount)
			}
		})
	}
}

// TestTestStep_CommandSufficientWithoutUserIntentIsUnchanged confirms the
// declaration does not alter the case that already skipped the agent, so the
// only behavior it changes is the intent-present one.
func TestTestStep_CommandSufficientWithoutUserIntentIsUnchanged(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	callCount := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			callCount++
			return &agent.Result{Output: json.RawMessage(`{"findings":[]}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Test: "true"})
	sctx.UserIntent = ""
	sctx.Config.TestCommandSufficient = true

	if _, err := (&TestStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if callCount != 0 {
		t.Fatalf("expected no agent (unchanged from today), got %d", callCount)
	}
}

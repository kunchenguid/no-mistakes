package steps

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestTestStep_HangingEvidenceAgentParksForADecision(t *testing.T) {
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{
		name: "hanging-evidence-agent",
		runFn: func(ctx context.Context, _ agent.RunOpts) (*agent.Result, error) {
			<-ctx.Done()
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"","tested":["ok"],"testing_summary":"ok","artifacts":[],"scenarios":[{"name":"user runs the command","result":"pass","live":true,"evidence":"ok","reason":""}],"verdict":"go"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.TestAgentTimeout = 20 * time.Millisecond

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("Execute() error = %v, want a parked budget cut rather than a failed run", err)
	}
	if outcome == nil || !outcome.NeedsApproval {
		t.Fatalf("outcome = %#v, want the Test step parked for a decision", outcome)
	}
	findings, parseErr := types.ParseFindingsJSON(outcome.Findings)
	if parseErr != nil {
		t.Fatalf("parse findings: %v", parseErr)
	}
	if len(findings.Items) != 1 || findings.Items[0].ID != types.FindingIDTestAgentTimeout {
		t.Fatalf("findings = %#v, want the test-agent-timeout park", findings.Items)
	}
	if findings.Items[0].Action != types.ActionAskUser {
		t.Fatalf("finding action = %q, want %q", findings.Items[0].Action, types.ActionAskUser)
	}
	got := findings.Items[0].Description
	if !strings.Contains(got, "timed out after 20ms") {
		t.Fatalf("finding = %q, want the expired test budget named", got)
	}
	if !strings.Contains(got, "produced no output at all") {
		t.Fatalf("finding = %q, want the measured silence of a never-emitting agent", got)
	}
	if strings.Contains(got, "silent for 20ms") {
		t.Fatalf("finding = %q, must not restate the budget as if it were a measurement", got)
	}
	if findings.Verdict == types.TestVerdictGo {
		t.Fatal("late structured output after the deadline must not complete Test as a pass")
	}
}

func TestTestStep_EvidenceAgentCallIsDeadlineBounded(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	var sawDeadline bool
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, _ agent.RunOpts) (*agent.Result, error) {
			_, sawDeadline = ctx.Deadline()
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"","tested":["ok"],"testing_summary":"ok","artifacts":[],"scenarios":[{"name":"user runs the command","result":"pass","live":true,"evidence":"ok","reason":""}],"verdict":"go"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !sawDeadline {
		t.Fatal("evidence agent ran without a deadline")
	}
	if outcome == nil || outcome.NeedsApproval {
		t.Fatalf("successful evidence gathering should still complete, got %+v", outcome)
	}
}

func TestTestStep_NoStructuredOutputFails(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{
		name: "test",
		runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Text: "tests unavailable"}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	outcome, err := (&TestStep{}).Execute(sctx)
	if err == nil || !strings.Contains(err.Error(), "test analyzer") {
		t.Fatalf("Execute() error = %v, want missing test analyzer output", err)
	}
	if outcome != nil {
		t.Fatalf("Execute() outcome = %+v, want no outcome", outcome)
	}
}

func TestTestStep_IncompleteStructuredOutputFails(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{
		name: "test",
		runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":""}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	outcome, err := (&TestStep{}).Execute(sctx)
	if err == nil || !strings.Contains(err.Error(), "missing tested array") {
		t.Fatalf("Execute() error = %v, want missing test evidence fields", err)
	}
	if outcome != nil {
		t.Fatalf("Execute() outcome = %+v, want no outcome", outcome)
	}
}

func TestTestStep_EmptyEvidenceFails(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		output json.RawMessage
	}{
		{name: "empty array", output: json.RawMessage(`{"findings":[],"summary":"","tested":[],"testing_summary":"  ","artifacts":[]}`)},
		{name: "whitespace entries", output: json.RawMessage(`{"findings":[],"summary":"","tested":[" \t"],"testing_summary":"tests passed","artifacts":[]}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, baseSHA, headSHA := setupGitRepo(t)
			ag := &mockAgent{
				name: "test",
				runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
					return &agent.Result{Output: tc.output}, nil
				},
			}
			sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

			outcome, err := (&TestStep{}).Execute(sctx)
			if err == nil || !strings.Contains(err.Error(), "empty tested array") {
				t.Fatalf("Execute() error = %v, want empty tested array rejected", err)
			}
			if outcome != nil {
				t.Fatalf("Execute() outcome = %+v, want no outcome", outcome)
			}
		})
	}
}

func TestTestStep_BlankTestingSummaryFails(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{
		name: "test",
		runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"","tested":["go test ./..."],"testing_summary":"","artifacts":[]}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	outcome, err := (&TestStep{}).Execute(sctx)
	if err == nil || !strings.Contains(err.Error(), "empty testing summary") {
		t.Fatalf("Execute() error = %v, want blank testing summary rejected", err)
	}
	if outcome != nil {
		t.Fatalf("Execute() outcome = %+v, want no outcome", outcome)
	}
}

func TestTestStep_FixAgentTimeoutDoesNotCancelPostProcessing(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)
	ag := &mockAgent{
		name: "test",
		runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			if err := os.WriteFile(filepath.Join(dir, "fix.txt"), []byte("fixed"), 0o644); err != nil {
				return nil, err
			}
			return &agent.Result{Output: json.RawMessage(`{"summary":"fix tests","findings":[],"tested":["go test ./..."],"testing_summary":"re-verified the repaired behaviour","artifacts":[],"scenarios":[{"name":"the repaired behaviour works for a user","result":"pass","live":true,"evidence":"go test ./...","reason":""}],"verdict":"go"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Test: "exit 0"})
	sctx.Fixing = true
	sctx.Config.TestAgentTimeout = time.Second

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("post-agent processing used agent context: %v", err)
	}
	if outcome == nil || outcome.NeedsApproval {
		t.Fatalf("successful fix should complete, got %+v", outcome)
	}
	if got := gitCmd(t, dir, "status", "--porcelain"); got != "" {
		t.Fatalf("fix was not committed: %s", got)
	}
	if got := gitCmd(t, dir, "show", "HEAD:fix.txt"); got != "fixed" {
		t.Fatalf("committed fix = %q, want fixed", got)
	}
}

func TestTestStep_FixAgentTimeoutParksWithoutCommit(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, _ agent.RunOpts) (*agent.Result, error) {
			if err := os.WriteFile(filepath.Join(dir, "fix.txt"), []byte("fixed"), 0o644); err != nil {
				return nil, err
			}
			<-ctx.Done()
			return &agent.Result{Output: json.RawMessage(`{"summary":"fix tests","findings":[],"tested":["go test ./..."],"testing_summary":"re-verified the repaired behaviour","artifacts":[],"scenarios":[{"name":"the repaired behaviour works for a user","result":"pass","live":true,"evidence":"go test ./...","reason":""}],"verdict":"go"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Test: "exit 0"})
	sctx.Fixing = true
	sctx.Config.TestAgentTimeout = 20 * time.Millisecond

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("Execute() error = %v, want a parked budget cut", err)
	}
	if outcome == nil || !outcome.NeedsApproval {
		t.Fatalf("outcome = %#v, want the Test step parked for a decision", outcome)
	}
	if got := testFindingByID(t, outcome.Findings, types.FindingIDTestAgentTimeout).Description; !strings.Contains(got, "agent fix tests timed out after 20ms") || strings.Count(got, "agent fix tests") != 1 {
		t.Fatalf("finding = %q, want the timeout named once", got)
	}
	work := testFindingByID(t, outcome.Findings, types.FindingIDTestAgentUnvalidatedWork).Description
	if !strings.Contains(work, "uncommitted changes to fix.txt") || !strings.Contains(work, "git -C "+dir+" diff") {
		t.Fatalf("finding = %q, want the leftover file named with how to inspect it", work)
	}
	if got := gitCmd(t, dir, "rev-parse", "HEAD"); got != headSHA {
		t.Fatalf("HEAD = %s, want unchanged %s", got, headSHA)
	}
	if got := gitCmd(t, dir, "status", "--porcelain", "--", "fix.txt"); got != "?? fix.txt" {
		t.Fatalf("fix.txt status = %q, want uncommitted", got)
	}
}

func TestTestStep_FixAgentTimeoutRecordsCommittedHead(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, _ agent.RunOpts) (*agent.Result, error) {
			if err := os.WriteFile(filepath.Join(dir, "fix.txt"), []byte("fixed"), 0o644); err != nil {
				return nil, err
			}
			gitCmd(t, dir, "add", "fix.txt")
			gitCmd(t, dir, "commit", "-m", "timed-out repair")
			<-ctx.Done()
			return nil, ctx.Err()
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Test: "exit 0"})
	sctx.Fixing = true
	sctx.Config.TestAgentTimeout = 20 * time.Millisecond

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("Execute() error = %v, want a parked budget cut", err)
	}
	if outcome == nil || !outcome.NeedsApproval {
		t.Fatalf("outcome = %#v, want the Test step parked for a decision", outcome)
	}
	committed := gitCmd(t, dir, "rev-parse", "HEAD")
	if committed == headSHA {
		t.Fatal("timed-out agent commit was lost")
	}
	if sctx.Run.HeadSHA != committed {
		t.Fatalf("run head = %s, want recorded committed head %s", sctx.Run.HeadSHA, committed)
	}
	work := testFindingByID(t, outcome.Findings, types.FindingIDTestAgentUnvalidatedWork).Description
	if !strings.Contains(work, "recorded locally") || !strings.Contains(work, "log -p "+headSHA+".."+committed) {
		t.Fatalf("finding = %q, want the committed head retained with how to inspect it", work)
	}
}

func TestTestStep_EvidenceCutKeepsTheFailingConfiguredCommand(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{name: "test", runFn: func(ctx context.Context, _ agent.RunOpts) (*agent.Result, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	testCmd := "printf 'TestCheckout failed'; exit 3"
	if runtime.GOOS == "windows" {
		testCmd = "echo TestCheckout failed && exit /b 3"
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Test: testCmd})
	sctx.Config.TestAgentTimeout = 20 * time.Millisecond

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("Execute() error = %v, want a parked budget cut", err)
	}
	if outcome.ExitCode != 3 {
		t.Fatalf("ExitCode = %d, want the configured command's 3", outcome.ExitCode)
	}
	if parked, parseErr := types.ParseFindingsJSON(outcome.Findings); parseErr != nil || !strings.Contains(parked.Summary, "TestCheckout failed") {
		t.Fatalf("park summary = %q (%v), want the failing command's output kept for the next repair", parked.Summary, parseErr)
	}
	if got := testFindingByID(t, outcome.Findings, types.FindingIDTestAgentTimeout).Description; strings.Contains(got, "not a code failure") {
		t.Fatalf("finding = %q, must not call the cut harmless while the configured command failed", got)
	}
	persistTestStepFindings(t, sctx, outcome.ExitCode, outcome.Findings)
	unresolved, err := (&TestStep{}).VerifyApprovalOverride(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if unresolved != "configured test command failed with exit code 3" {
		t.Fatalf("unresolved = %q, want approval to stay a configured-command waiver", unresolved)
	}
}

func TestTestStep_RepairCutRecordsTheConfiguredCommandResult(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	calls := 0
	ag := &mockAgent{name: "test", runFn: func(ctx context.Context, _ agent.RunOpts) (*agent.Result, error) {
		calls++
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Test: "exit 4"})
	sctx.Fixing = true
	sctx.PreviousFindings = `{"findings":[{"id":"test-1","severity":"error","category":"test-command","description":"configured test command failed with exit code 4"}]}`
	sctx.Config.TestAgentTimeout = 20 * time.Millisecond

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("Execute() error = %v, want a parked budget cut", err)
	}
	if calls != 1 {
		t.Fatalf("agent calls = %d, want only the cut repair turn", calls)
	}
	if outcome.ExitCode != 4 {
		t.Fatalf("ExitCode = %d, want the configured command's 4", outcome.ExitCode)
	}
	persistTestStepFindings(t, sctx, outcome.ExitCode, outcome.Findings)
	unresolved, err := (&TestStep{}).VerifyApprovalOverride(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if unresolved != "configured test command failed with exit code 4" {
		t.Fatalf("unresolved = %q, want approval to stay a configured-command waiver", unresolved)
	}
	if pipeline.HasUnvalidatedWorkRefusal(outcome.Findings) {
		t.Fatalf("findings = %s, a cut that changed nothing must stay approvable", outcome.Findings)
	}
}

func TestTestStep_FixOfABudgetCutAloneRerunsOnlyValidation(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	var prompts []string
	ag := &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		prompts = append(prompts, opts.Prompt)
		return &agent.Result{Output: json.RawMessage(passingScenarioFindingsJSON)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Fixing = true
	sctx.PreviousFindings = `{"findings":[{"id":"test-agent-timeout","severity":"warning","action":"ask-user","description":"budget cut"},{"id":"test-agent-unvalidated-work","severity":"error","action":"ask-user","description":"uncommitted changes"}]}`

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatalf("outcome = %+v, want the re-run validation to pass", outcome)
	}
	if len(prompts) != 1 || strings.Contains(prompts[0], "Fix the failing tests") || !strings.Contains(prompts[0], "Derive the scenarios") {
		t.Fatalf("prompts = %q, want exactly one evidence turn and no repair turn", prompts)
	}
	if outcome.FixSummary != NoChangesAppliedSummary {
		t.Fatalf("fix summary = %q, want %q: a validation-only round fixes nothing", outcome.FixSummary, NoChangesAppliedSummary)
	}
}

func TestTestStep_CutMidRebaseRecordsNoPartialHead(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{name: "test", runFn: func(ctx context.Context, _ agent.RunOpts) (*agent.Result, error) {
		leaveConflictedRebase(t, dir)
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Fixing = true
	sctx.Config.TestAgentTimeout = 20 * time.Millisecond

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("Execute() error = %v, want a parked budget cut", err)
	}
	if sctx.Run.HeadSHA != headSHA {
		t.Fatalf("run head = %s, want the partial rebase head left unrecorded", sctx.Run.HeadSHA)
	}
	if work := testFindingByID(t, outcome.Findings, types.FindingIDTestAgentUnvalidatedWork).Description; !strings.Contains(work, "unfinished rebase or merge") {
		t.Fatalf("finding = %q, want the unfinished rebase named", work)
	}
}

func noGoTestGateJSON(testedHead string) string {
	return `{"findings":[{"id":"test-2","severity":"error","action":"auto-fix","description":"live validation verdict: no-go (1 of 1 scenarios were driven live against the product); failed: checkout"}],"summary":"checkout failed","verdict":"no-go","scenarios":[{"name":"checkout","result":"fail","live":true,"evidence":"checkout.png","reason":""}],"tested_head_sha":"` + testedHead + `"}`
}

func TestTestStep_RepeatedCutKeepsRefusingUnvalidatedWork(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	calls := 0
	ag := &mockAgent{name: "test", runFn: func(ctx context.Context, _ agent.RunOpts) (*agent.Result, error) {
		calls++
		if calls == 1 {
			if err := os.WriteFile(filepath.Join(dir, "fix.txt"), []byte("fixed"), 0o644); err != nil {
				return nil, err
			}
			return &agent.Result{Output: json.RawMessage(`{"summary":"fix checkout"}`)}, nil
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Fixing = true
	sctx.PreviousFindings = noGoTestGateJSON(headSHA)
	sctx.Config.TestAgentTimeout = 20 * time.Millisecond

	first, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("repair round error = %v", err)
	}
	repaired := sctx.Run.HeadSHA
	if repaired == headSHA || !pipeline.HasUnvalidatedWorkRefusal(first.Findings) {
		t.Fatalf("repair round findings = %s, want the unvalidated repair commit refused", first.Findings)
	}

	parked, err := types.ParseFindingsJSON(first.Findings)
	if err != nil {
		t.Fatal(err)
	}
	budgetCut, err := types.MarshalFindingsJSON(types.FilterFindings(parked, []string{types.FindingIDTestAgentTimeout, types.FindingIDTestAgentUnvalidatedWork}))
	if err != nil {
		t.Fatal(err)
	}
	sctx.PreviousFindings = budgetCut
	second, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("validation-only round error = %v", err)
	}
	if calls != 3 {
		t.Fatalf("agent calls = %d, want the second round to run only the evidence turn", calls)
	}
	if !pipeline.HasUnvalidatedWorkRefusal(second.Findings) {
		t.Fatalf("findings = %s, want the repeated cut to keep refusing the repair no evidence turn validated", second.Findings)
	}
	if work := testFindingByID(t, second.Findings, types.FindingIDTestAgentUnvalidatedWork).Description; !strings.Contains(work, "log -p "+headSHA+".."+repaired) {
		t.Fatalf("finding = %q, want the range since the last validated head", work)
	}
}

func TestTestStep_RepairCutKeepsTheFindingsItWasFixing(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{name: "test", runFn: func(ctx context.Context, _ agent.RunOpts) (*agent.Result, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Fixing = true
	sctx.PreviousFindings = noGoTestGateJSON(headSHA)
	sctx.Config.TestAgentTimeout = 20 * time.Millisecond

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("Execute() error = %v, want a parked budget cut", err)
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if findings.Verdict != types.TestVerdictNoGo || len(findings.Scenarios) != 1 || !strings.Contains(outcome.Findings, "failed: checkout") {
		t.Fatalf("findings = %s, want the no-go the repair was fixing kept on the park", outcome.Findings)
	}
	if got := testFindingByID(t, outcome.Findings, types.FindingIDTestAgentTimeout).Description; strings.Contains(got, "not a code failure") {
		t.Fatalf("finding = %q, must not call the cut harmless next to a no-go", got)
	}
	if pipeline.HasUnvalidatedWorkRefusal(outcome.Findings) {
		t.Fatalf("findings = %s, a cut that changed nothing must stay approvable", outcome.Findings)
	}

	persistTestStepFindings(t, sctx, outcome.ExitCode, outcome.Findings)
	if err := sctx.DB.SetTestApprovalReason(sctx.StepResultID, "accepted"); err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.CompleteStep(sctx.StepResultID, 0, 1, "test.log"); err != nil {
		t.Fatal(err)
	}
	sr, err := sctx.DB.GetStepResult(sctx.StepResultID)
	if err != nil {
		t.Fatal(err)
	}
	if got := sr.TestOverrideReason(); !strings.Contains(got, "live validation verdict: no-go") {
		t.Fatalf("TestOverrideReason = %q, want approval recorded against the no-go", got)
	}
}

// answerTestPark splits a parked Test gate the way the executor does for a fix
// response selecting ids: the selection and the deferred rest.
func answerTestPark(t *testing.T, raw string, ids ...string) (string, string) {
	t.Helper()
	parked, err := types.ParseFindingsJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	parked = types.NormalizeFindings(parked, string(types.StepTest))
	selected, err := types.MarshalFindingsJSON(types.FilterFindings(parked, ids))
	if err != nil {
		t.Fatal(err)
	}
	deferred, err := types.MarshalFindingsJSON(types.ExcludeFindings(parked, ids))
	if err != nil {
		t.Fatal(err)
	}
	return selected, deferred
}

func TestTestStep_CutAfterAnEarlierStepCommittedStaysApprovable(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{name: "test", runFn: func(ctx context.Context, _ agent.RunOpts) (*agent.Result, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	reviewed := baseSHA
	sctx.Run.ReviewApprovedHeadSHA = &reviewed
	sctx.Config.TestAgentTimeout = 20 * time.Millisecond

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("Execute() error = %v, want a parked budget cut", err)
	}
	if pipeline.HasUnvalidatedWorkRefusal(outcome.Findings) {
		t.Fatalf("findings = %s, commits made before Test started are not leftovers of the cut", outcome.Findings)
	}
}

func TestTestStep_RepeatedCutKeepsRefusingBeforeAnyEvidenceCompletes(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	calls := 0
	ag := &mockAgent{name: "test", runFn: func(ctx context.Context, _ agent.RunOpts) (*agent.Result, error) {
		calls++
		if calls == 1 {
			if err := os.WriteFile(filepath.Join(dir, "setup.txt"), []byte("setup"), 0o644); err != nil {
				return nil, err
			}
			gitCmd(t, dir, "add", "setup.txt")
			gitCmd(t, dir, "commit", "-m", "evidence agent setup")
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.TestAgentTimeout = 20 * time.Millisecond

	first, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("first round error = %v", err)
	}
	committed := sctx.Run.HeadSHA
	if committed == headSHA || !pipeline.HasUnvalidatedWorkRefusal(first.Findings) {
		t.Fatalf("first round findings = %s, want the agent's commit refused", first.Findings)
	}

	sctx.Fixing = true
	sctx.PreviousFindings, sctx.DeferredFindings = answerTestPark(t, first.Findings, types.FindingIDTestAgentTimeout, types.FindingIDTestAgentUnvalidatedWork)
	second, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("validation-only round error = %v", err)
	}
	if work := testFindingByID(t, second.Findings, types.FindingIDTestAgentUnvalidatedWork).Description; !strings.Contains(work, "log -p "+headSHA+".."+committed) {
		t.Fatalf("finding = %q, want the earlier cut's unvalidated commit still refused", work)
	}
}

func TestTestStep_RepeatedCutReMeasuresLeftoversInsteadOfRepeatingThem(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	calls := 0
	ag := &mockAgent{name: "test", runFn: func(ctx context.Context, _ agent.RunOpts) (*agent.Result, error) {
		calls++
		if calls == 1 {
			if err := os.WriteFile(filepath.Join(dir, "foo_test.go"), []byte("package foo"), 0o644); err != nil {
				return nil, err
			}
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.TestAgentTimeout = 20 * time.Millisecond
	cutAgain := func(answered string) string {
		t.Helper()
		sctx.Fixing = true
		sctx.PreviousFindings, sctx.DeferredFindings = answerTestPark(t, answered, types.FindingIDTestAgentTimeout, types.FindingIDTestAgentUnvalidatedWork)
		outcome, err := (&TestStep{}).Execute(sctx)
		if err != nil {
			t.Fatalf("validation-only round error = %v", err)
		}
		return outcome.Findings
	}

	first, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("first round error = %v", err)
	}
	second := cutAgain(first.Findings)
	work := testFindingByID(t, second, types.FindingIDTestAgentUnvalidatedWork).Description
	if strings.Count(work, "uncommitted changes to foo_test.go") != 1 || strings.Contains(work, "Since then") {
		t.Fatalf("finding = %q, want the same leftover named once, not repeated as new work", work)
	}

	if err := os.Remove(filepath.Join(dir, "foo_test.go")); err != nil {
		t.Fatal(err)
	}
	third := cutAgain(second)
	if pipeline.HasUnvalidatedWorkRefusal(third) {
		t.Fatalf("findings = %s, a worktree with the leftover gone and HEAD unmoved must be approvable again", third)
	}
}

func TestTestStep_ValidationOnlyCutKeepsTheDeferredNoGo(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{name: "test", runFn: func(ctx context.Context, _ agent.RunOpts) (*agent.Result, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Fixing = true
	sctx.PreviousFindings = noGoTestGateJSON(headSHA)
	sctx.Config.TestAgentTimeout = 20 * time.Millisecond

	first, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("repair round error = %v", err)
	}
	sctx.PreviousFindings, sctx.DeferredFindings = answerTestPark(t, first.Findings, types.FindingIDTestAgentTimeout)
	second, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("validation-only round error = %v", err)
	}
	if !strings.Contains(second.Findings, "failed: checkout") {
		t.Fatalf("findings = %s, want the deferred no-go kept on the park", second.Findings)
	}
	if second.FixSummary != NoChangesAppliedSummary {
		t.Fatalf("fix summary = %q, want %q: a cut validation-only round fixes nothing", second.FixSummary, NoChangesAppliedSummary)
	}
	if got := testFindingByID(t, second.Findings, types.FindingIDTestAgentTimeout).Description; strings.Contains(got, "not a code failure") {
		t.Fatalf("finding = %q, must not call the cut harmless next to a no-go", got)
	}
}

func TestTestStep_BudgetCutInstructionsReachTheEvidenceTurn(t *testing.T) {
	t.Parallel()
	const guidance = "drive only the checkout scenario; do not run the full suite"
	for _, tt := range []struct {
		name      string
		selection []string
		repair    bool
	}{
		{"validation only", []string{types.FindingIDTestAgentTimeout}, false},
		{"both budget-cut findings carry one note", []string{types.FindingIDTestAgentTimeout, types.FindingIDTestAgentUnvalidatedWork}, false},
		{"with a repair finding", []string{types.FindingIDTestAgentTimeout, "test-2"}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, headSHA := setupGitRepo(t)
			var prompts []string
			ag := &mockAgent{name: "test", runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
				prompts = append(prompts, opts.Prompt)
				if len(prompts) == 1 && tt.repair {
					return &agent.Result{Output: json.RawMessage(`{"summary":"fix checkout"}`)}, nil
				}
				<-ctx.Done()
				return nil, ctx.Err()
			}}
			sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
			sctx.Config.TestAgentTimeout = 20 * time.Millisecond
			sctx.Fixing = true
			parked := `{"findings":[{"id":"` + types.FindingIDTestAgentTimeout + `","severity":"warning","action":"ask-user","description":"budget cut"},` +
				`{"id":"` + types.FindingIDTestAgentUnvalidatedWork + `","severity":"warning","action":"ask-user","description":"approval is refused"},` +
				strings.TrimPrefix(noGoTestGateJSON(headSHA), `{"findings":[`)
			selected, deferred := answerTestPark(t, parked, tt.selection...)
			answered, err := types.ParseFindingsJSON(selected)
			if err != nil {
				t.Fatal(err)
			}
			for i := range answered.Items {
				if slices.Contains(testBudgetCutIDs, answered.Items[i].ID) {
					answered.Items[i].UserInstructions = guidance
				}
			}
			if sctx.PreviousFindings, err = types.MarshalFindingsJSON(answered); err != nil {
				t.Fatal(err)
			}
			sctx.DeferredFindings = deferred

			if _, err := (&TestStep{}).Execute(sctx); err != nil {
				t.Fatalf("Execute() error = %v", err)
			}
			wantCalls := 1
			if tt.repair {
				wantCalls = 2
			}
			if len(prompts) != wantCalls {
				t.Fatalf("agent calls = %d, want %d", len(prompts), wantCalls)
			}
			evidence := prompts[len(prompts)-1]
			if !strings.Contains(evidence, "Operator guidance for this validation (from the decision on the Test agent budget cut):\n"+guidance+"\n") {
				t.Fatalf("evidence prompt lacks the operator's budget-cut guidance:\n%s", evidence)
			}
			if n := strings.Count(evidence, guidance); n != 1 {
				t.Fatalf("evidence prompt carries the operator's guidance %d times, want once:\n%s", n, evidence)
			}
		})
	}
}

func TestTestStep_CutParkDoesNotRenderAnEarlierCyclesEvidence(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{name: "test", runFn: func(ctx context.Context, _ agent.RunOpts) (*agent.Result, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.TestAgentTimeout = 20 * time.Millisecond

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("Execute() error = %v, want a parked budget cut", err)
	}
	park := outcome.Findings
	earlierCycle := liveValidatedFindingsJSON(t, []types.TestScenario{
		{Name: "user reaches the success screen", Result: types.ScenarioResultPass, Live: true, Evidence: "checkout.png"},
	}, types.TestVerdictGo, baseSHA)
	steps := []*db.StepResult{{ID: "s1", StepName: types.StepTest, Status: types.StepStatusCompleted, FindingsJSON: &park}}
	rounds := map[string][]*db.StepRound{"s1": {
		{Round: 1, Trigger: "initial", FindingsJSON: &earlierCycle},
		{Round: 2, Trigger: "initial", FindingsJSON: &park},
	}}

	md := BuildTestingSummary(steps, rounds)
	for _, stale := range []string{"drove the checkout scenarios", "Live validation", "user reaches the success screen"} {
		if strings.Contains(md, stale) {
			t.Fatalf("Testing section renders an earlier cycle's evidence %q for a head it never validated:\n%s", stale, md)
		}
	}
	if !strings.Contains(md, "before live validation completed") {
		t.Fatalf("Testing section = %q, want it to say the cut left no evidence for this head", md)
	}
}

func TestTestStep_RepairPromptLeavesOutTheBudgetCutFindings(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	var prompts []string
	ag := &mockAgent{name: "test", runFn: func(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
		prompts = append(prompts, opts.Prompt)
		if len(prompts) == 1 {
			return &agent.Result{Output: json.RawMessage(`{"summary":"fix checkout"}`)}, nil
		}
		return &agent.Result{Output: json.RawMessage(passingScenarioFindingsJSON)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Fixing = true
	sctx.PreviousFindings = `{"findings":[` +
		`{"id":"test-agent-timeout","severity":"warning","action":"ask-user","description":"raise test_agent_timeout in global config"},` +
		`{"id":"test-agent-unvalidated-work","severity":"error","action":"ask-user","description":"Approval is refused: leftover.txt"},` +
		`{"id":"test-1","severity":"error","category":"test-command","description":"configured test command failed with exit code 1"}]}`

	if _, err := (&TestStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if len(prompts) == 0 || !strings.Contains(prompts[0], "Fix the failing tests") {
		t.Fatalf("prompts = %q, want a repair turn first", prompts)
	}
	repair := prompts[0]
	if !strings.Contains(repair, "configured test command failed with exit code 1") {
		t.Fatalf("repair prompt = %q, want the command failure to address", repair)
	}
	for _, operatorOnly := range []string{"raise test_agent_timeout in global config", "Approval is refused"} {
		if strings.Contains(repair, operatorOnly) {
			t.Fatalf("repair prompt carries the operator-only budget-cut text %q:\n%s", operatorOnly, repair)
		}
	}
}

func TestTestStep_EvidenceAgentCannotClaimAReservedBudgetCutID(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: json.RawMessage(`{"findings":[{"id":"test-agent-unvalidated-work","severity":"warning","action":"ask-user","description":"leftover build output"},{"id":"test-agent-timeout","severity":"warning","action":"ask-user","description":"slow suite"}],"summary":"ok","tested":["go test ./x"],"testing_summary":"drove it","artifacts":[],"scenarios":[{"name":"user runs it","result":"pass","live":true,"evidence":"ok","reason":""}],"verdict":"go","unvalidated_since_sha":"deadbeef"}`)}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if findings.UnvalidatedSinceSHA != "" {
		t.Fatalf("findings = %s, an agent payload must not set the park's measured head", outcome.Findings)
	}
	for _, item := range findings.Items {
		if item.ID == types.FindingIDTestAgentUnvalidatedWork || item.ID == types.FindingIDTestAgentTimeout {
			t.Fatalf("findings = %s, an agent finding must not claim a step-owned budget-cut ID", outcome.Findings)
		}
	}
	if pipeline.HasUnvalidatedWorkRefusal(outcome.Findings) || !strings.Contains(outcome.Findings, "leftover build output") {
		t.Fatalf("findings = %s, want the agent finding kept without refusing approval", outcome.Findings)
	}
}

// leaveConflictedRebase stops a rebase on a conflict, leaving HEAD detached at
// a partial result the way an agent cut mid-rebase does.
func leaveConflictedRebase(t *testing.T, dir string) {
	t.Helper()
	start := gitCmd(t, dir, "rev-parse", "HEAD")
	commitFile := func(content string) {
		if err := os.WriteFile(filepath.Join(dir, "conflict.txt"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		gitCmd(t, dir, "add", "conflict.txt")
		gitCmd(t, dir, "commit", "-m", content)
	}
	commitFile("theirs")
	onto := gitCmd(t, dir, "rev-parse", "HEAD")
	gitCmd(t, dir, "reset", "--hard", start)
	commitFile("ours")
	cmd := exec.Command("git", "rebase", onto)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=test", "GIT_AUTHOR_EMAIL=test@test.com", "GIT_COMMITTER_NAME=test", "GIT_COMMITTER_EMAIL=test@test.com")
	if out, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("rebase unexpectedly applied cleanly: %s", out)
	}
}

func testFindingByID(t *testing.T, raw, id string) types.Finding {
	t.Helper()
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		t.Fatalf("parse findings: %v", err)
	}
	for _, item := range findings.Items {
		if item.ID == id {
			return item
		}
	}
	t.Fatalf("findings = %#v, want %s", findings.Items, id)
	return types.Finding{}
}

func TestTestStep_FixMode(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)
	previousFindings := `{"items":[{"id":"test-1 =======","severity":"error","file":"internal/pipeline/steps/test.go >>>>>>> prompt","description":"tests failed with exit code 1 <<<<<<< HEAD"}],"summary":"FAIL: TestFoo expected 42 got 0 ======="}`

	callCount := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			callCount++
			os.WriteFile(filepath.Join(dir, "fix.txt"), []byte("fixed"), 0o644)
			return &agent.Result{Output: json.RawMessage(`{"summary":"  \"fix test failures.\"  ","findings":[],"tested":["go test ./..."],"testing_summary":"re-verified the repaired behaviour","artifacts":[],"scenarios":[{"name":"the repaired behaviour works for a user","result":"pass","live":true,"evidence":"go test ./...","reason":""}],"verdict":"go"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Test: "exit 0"})
	sctx.Fixing = true
	sctx.PreviousFindings = previousFindings

	step := &TestStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Error("expected no approval after fix + passing tests")
	}
	if callCount != 2 {
		t.Errorf("expected 2 agent calls (fix, then the unconditional evidence turn), got %d", callCount)
	}
	if len(ag.calls[0].JSONSchema) == 0 {
		t.Error("expected fix call to request structured JSON output")
	}
	if !strings.Contains(ag.calls[0].Prompt, "FAIL: TestFoo expected 42 got 0") {
		t.Error("expected fix prompt to contain previous test failure summary")
	}
	if strings.Contains(ag.calls[0].Prompt, "test-1 =======") {
		t.Error("expected test fix prompt to sanitize finding IDs")
	}
	if strings.Contains(ag.calls[0].Prompt, "test.go >>>>>>> prompt") {
		t.Error("expected test fix prompt to sanitize finding file paths")
	}
	if strings.Contains(ag.calls[0].Prompt, "<<<<<<< HEAD") {
		t.Error("expected test fix prompt to exclude merge markers")
	}
	if !strings.Contains(ag.calls[0].Prompt, "smallest correct root-cause fix") {
		t.Error("expected test fix prompt to prefer root-cause fixes over bandaids")
	}
	if !strings.Contains(ag.calls[0].Prompt, "When a problem can be solved by removing a code path that is not strictly required to satisfy the intent") ||
		!strings.Contains(ag.calls[0].Prompt, "fix it by removing that path, not by validating, hardening, or documenting it") {
		t.Error("expected test fix prompt to prefer removing unrequired paths")
	}
	assertTestQualityRulePrompt(t, ag.calls[0].Prompt)
	if !strings.Contains(ag.calls[0].Prompt, "remove any transient artifacts your testing created in the working tree") {
		t.Error("expected test fix prompt to ask the agent to clean up transient testing artifacts before finishing")
	}
	if strings.Contains(ag.calls[0].Prompt, "Make the minimal change needed") {
		t.Error("expected test fix prompt not to prefer narrow minimal changes")
	}
	if status := gitStatusPorcelain(t, dir); status != "" {
		t.Fatalf("expected clean worktree after fix commit, got %q", status)
	}
	if got := lastCommitMessage(t, dir); got != "no-mistakes(test): fix test failures" {
		t.Fatalf("last commit message = %q", got)
	}
}

func TestTestStep_FixMode_UsesConfiguredCommitMessage(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			os.WriteFile(filepath.Join(dir, "fix.txt"), []byte("fixed"), 0o644)
			return &agent.Result{Output: json.RawMessage(`{"summary":"fix test failures","findings":[],"tested":["go test ./..."],"testing_summary":"re-verified the repaired behaviour","artifacts":[],"scenarios":[{"name":"the repaired behaviour works for a user","result":"pass","live":true,"evidence":"go test ./...","reason":""}],"verdict":"go"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Test: "exit 0"})
	sctx.Config.Commit = config.Commit{FixMessage: "fix({{.Step}}): {{.Summary}}"}
	sctx.Fixing = true
	sctx.PreviousFindings = `{"findings":[{"severity":"error","description":"tests failed"}],"summary":"tests failed"}`

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatal("expected no approval after fix and passing tests")
	}
	if got := lastCommitMessage(t, dir); got != "fix(test): fix test failures" {
		t.Fatalf("last commit message = %q", got)
	}
}

func TestTestStep_FixMode_UsesFallbackSummaryWhenStructuredSummaryMalformed(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	// Only the FIX turn returns the malformed payload this test is about; the
	// unconditional evidence turn that follows answers the scenario contract
	// normally, so the fallback-summary behaviour is what the test isolates.
	fixTurnDone := false
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			if !fixTurnDone {
				fixTurnDone = true
				os.WriteFile(filepath.Join(dir, "fix.txt"), []byte("fixed"), 0o644)
				return &agent.Result{Output: json.RawMessage(`{"not_summary":"oops"}`)}, nil
			}
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"","tested":["go test ./..."],"testing_summary":"re-verified the repaired behaviour","artifacts":[],"scenarios":[{"name":"the repaired behaviour works for a user","result":"pass","live":true,"evidence":"go test ./...","reason":""}],"verdict":"go"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Test: "exit 0"})
	sctx.Fixing = true
	sctx.PreviousFindings = `{"findings":[{"severity":"error","description":"tests failed"}],"summary":"tests failed"}`

	step := &TestStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatal("expected no approval after fallback summary commit and passing tests")
	}
	if got := lastCommitMessage(t, dir); got != "no-mistakes(test): fix test failures" {
		t.Fatalf("last commit message = %q", got)
	}
}

func TestTestStep_FixMode_AgentWritesNewTests_ProceedsAutomatically(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	callCount := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			callCount++
			// Simulate agent creating a new test file during fix in another supported language
			os.WriteFile(filepath.Join(dir, "component.spec.tsx"), []byte("export {}\n"), 0o644)
			return &agent.Result{Output: json.RawMessage(`{"summary":"add regression test","findings":[],"tested":["go test ./..."],"testing_summary":"re-verified the repaired behaviour","artifacts":[],"scenarios":[{"name":"the repaired behaviour works for a user","result":"pass","live":true,"evidence":"go test ./...","reason":""}],"verdict":"go"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Test: "exit 0"})
	sctx.Fixing = true

	step := &TestStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	// Issue #140: a passing test run whose only finding is an informational
	// "new test file written by agent" note must not require approval.
	if outcome.NeedsApproval {
		t.Error("expected no approval for an informational new-test-file finding when tests pass")
	}
	if callCount != 2 {
		t.Errorf("expected 2 agent calls in fix mode (fix, then the unconditional evidence turn), got %d", callCount)
	}

	var f Findings
	json.Unmarshal([]byte(outcome.Findings), &f)
	foundTestFile := false
	for _, item := range f.Items {
		if strings.Contains(item.Description, "component.spec.tsx") {
			foundTestFile = true
			if item.Action != types.ActionNoOp {
				t.Errorf("expected new-test-file finding action %q, got %q", types.ActionNoOp, item.Action)
			}
		}
	}
	if !foundTestFile {
		t.Errorf("expected finding mentioning component.spec.tsx, got findings: %+v", f.Items)
	}
}

func TestTestStep_ConfiguredCommandRunsEvidenceWithoutExtractedIntent(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	calls := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
			calls++
			return &agent.Result{Output: json.RawMessage(passingScenarioFindingsJSON)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Test: "exit 0"})

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("evidence agent calls = %d, want 1", calls)
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings.Scenarios) == 0 || findings.Verdict != types.TestVerdictGo {
		t.Fatalf("live findings = %+v, want scenarios and go verdict", findings)
	}
}

func TestTestStep_UserIntentRunsConfiguredCommandThenEvidenceAgent(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	baselineLog := filepath.Join(dir, "baseline.log")
	testCmd := "go env GOOS > baseline.log"

	callCount := 0
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			callCount++
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"evidence demonstrates intent","tested":["manual screenshot review"],"testing_summary":"captured screenshot evidence","artifacts":[],"scenarios":[{"name":"reviewer sees the new screen","result":"pass","live":true,"evidence":"manual screenshot review","reason":""}],"verdict":"go"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Test: testCmd})
	sctx.UserIntent = "Show users a success screen after checkout"

	step := &TestStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatal("expected no approval when evidence-oriented agent testing passes")
	}
	if callCount != 1 {
		t.Fatalf("expected evidence agent to run after configured test command, got %d calls", callCount)
	}
	data, err := os.ReadFile(baselineLog)
	if err != nil {
		t.Fatalf("expected configured test command to run: %v", err)
	}
	if strings.TrimSpace(string(data)) != runtime.GOOS {
		t.Fatalf("configured test command output = %q, want %s", string(data), runtime.GOOS)
	}
	prompt := ag.calls[0].Prompt
	for _, want := range []string{
		"Show users a success screen after checkout",
		"Decide what evidence or artifacts would clearly demonstrate each scenario's result",
		"Unit tests passing is not sufficient evidence by itself",
		"drive each scenario end-to-end against that running product",
		"Prefer product-level artifacts",
		"Only use command output as an artifact when that output directly demonstrates the end-user experience or requested behavior",
		"Configured test command already ran successfully as baseline",
		testCmd,
		"The \"testing_summary\" must account for the complete test step: baseline commands that already ran, scenarios driven, manual or evidence-producing checks, artifacts gathered, and the overall result",
		"screenshots, GIFs, videos, rendered UI, CLI transcripts",
		"For UI, HTML, CSS, Electron renderer, browser, visual layout, or copy-placement changes, attempt to capture reviewer-visible visual evidence",
		"DOM snapshots, selector assertions, and text-only render summaries are not substitutes for visual evidence when a rendered surface is available",
		"If a UI-facing change has no screenshot, image, video, GIF, or rendered HTML artifact, state why in testing_summary",
		"Write new evidence files into this evidence directory, never into the worktree:",
		sctx.EvidenceDir,
		"Do not move, commit, or modify source files only to make evidence linkable",
		"if no existing check drives a scenario, write or improve a focused test",
		"perform manual verification with evidence",
		"Always include an \"artifacts\" array",
		"If sufficient evidence is not possible, report a warning finding",
		"When the blocker is a host capability or OS permission the agent's own process lacks",
		"name the specific capability or permission and how to grant it",
		"remove any transient artifacts your testing created in the working tree",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("expected prompt to contain %q, got:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "will be available from the pushed commit") || strings.Contains(prompt, "files that already exist in the repository") {
		t.Fatalf("expected prompt not to make the testing agent worry about committed evidence files, got:\n%s", prompt)
	}
	if _, err := os.Stat(sctx.EvidenceDir); err != nil {
		t.Fatalf("expected temporary evidence directory to exist: %v", err)
	}

	var findings Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
		t.Fatal(err)
	}
	t.Logf("evidence findings JSON: %s", outcome.Findings)
	if len(findings.Tested) != 2 || findings.Tested[0] != testCmd || findings.Tested[1] != "manual screenshot review" {
		t.Fatalf("expected baseline command and agent-tested evidence to be recorded, got %+v", findings.Tested)
	}
}

func TestTestStep_EvidenceDirectoryIsAlwaysOutsideTheWorktree(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"","tested":["manual evidence check"],"testing_summary":"checked evidence","artifacts":[],"scenarios":[{"name":"user sees the change","result":"pass","live":true,"evidence":"manual evidence check","reason":""}],"verdict":"go"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.UserIntent = "Show users a success screen after checkout"

	step := &TestStep{}
	if _, err := step.Execute(sctx); err != nil {
		t.Fatal(err)
	}

	prompt := ag.calls[0].Prompt
	wantDir := sctx.EvidenceDir
	if !strings.Contains(prompt, "Write new evidence files into this evidence directory, never into the worktree: "+wantDir) {
		t.Fatalf("expected evidence guidance to point outside the worktree, got:\n%s", prompt)
	}
	if _, err := os.Stat(filepath.Join(dir, ".no-mistakes")); err == nil {
		t.Fatal("test step created an in-repo evidence directory")
	}
}

func TestTestStep_PublishedEvidenceGuidanceNamesTheEvidenceBranch(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"","tested":["manual evidence check"],"testing_summary":"checked evidence","artifacts":[],"scenarios":[{"name":"user sees the change","result":"pass","live":true,"evidence":"manual evidence check","reason":""}],"verdict":"go"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.UserIntent = "Show users a success screen after checkout"
	sctx.Config.Test.Evidence = config.Evidence{StoreInRepo: true, Dir: ".no-mistakes/evidence", Branch: "team/ci/evidence"}

	step := &TestStep{}
	if _, err := step.Execute(sctx); err != nil {
		t.Fatal(err)
	}

	prompt := ag.calls[0].Prompt
	wantDir := sctx.EvidenceDir
	if !strings.Contains(prompt, "published to the repository's team/ci/evidence branch automatically and linked from the PR: "+wantDir) {
		t.Fatalf("expected evidence-branch publishing guidance, got:\n%s", prompt)
	}
	if strings.Contains(prompt, "committed and pushed automatically") {
		t.Fatalf("evidence must not be promised as a commit on the pushed branch, got:\n%s", prompt)
	}
}

// Local Test is targeted validation of the requested intent, never a complete
// repository-suite walk. Broad regression belongs to remote CI. Pins the
// normal evidence-agent contract wording so a soft "run the appropriate tests"
// regression is caught.
func TestTestStep_InitialAgent_TargetedValidationContract(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"summary":"","tested":["go test ./internal/cli -run TestDoctor -count=1"],"testing_summary":"targeted check passed","artifacts":[],"scenarios":[{"name":"doctor reports the new row","result":"pass","live":true,"evidence":"go test ./internal/cli -run TestDoctor -count=1","reason":""}],"verdict":"go"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.UserIntent = "Keep doctor checks green for CLI users"

	if _, err := (&TestStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) != 1 {
		t.Fatalf("expected 1 evidence agent call, got %d", len(ag.calls))
	}
	prompt := ag.calls[0].Prompt

	assertTestQualityRulePrompt(t, prompt)
	for _, want := range []string{
		"Derive the scenarios this change must satisfy, then run each one against the real running product",
		"Do NOT run the complete repository test suite",
		"Local Test is targeted validation of the requested intent",
		"remote CI owns broad regression and remains mandatory before a PR is ready",
		"Never treat \"do not run everything\" as permission to run nothing",
		"report a warning finding that sufficient targeted evidence is not possible",
		"A generic driver or user instruction asking for broad or full-suite confirmation does NOT override the targeted-validation product boundary",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("expected initial test prompt to contain %q, got:\n%s", want, prompt)
		}
	}
	for _, forbid := range []string{
		"run the appropriate tests yourself",
		"Run the tests, identify failures",
		// Replaced by scenario derivation: "the smallest relevant tests" is
		// what let a green unit-test run stand in for driving the product.
		"run the smallest relevant tests yourself",
	} {
		if strings.Contains(prompt, forbid) {
			t.Errorf("initial test prompt still carries open-ended suite language %q:\n%s", forbid, prompt)
		}
	}
}

// Test repair must reproduce the specific failure, fix its root cause, and
// re-verify only with focused checks. Soft "Run the tests" / "relevant tests"
// wording invited complete-suite walks after a one-line fix.
func TestTestStep_FixMode_TargetedVerificationContract(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			os.WriteFile(filepath.Join(dir, "fix.txt"), []byte("fixed"), 0o644)
			return &agent.Result{Output: json.RawMessage(`{"summary":"fix targeted failure","findings":[],"tested":["go test ./..."],"testing_summary":"re-verified the repaired behaviour","artifacts":[],"scenarios":[{"name":"the repaired behaviour works for a user","result":"pass","live":true,"evidence":"go test ./...","reason":""}],"verdict":"go"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Test: "exit 0"})
	sctx.Fixing = true
	sctx.PreviousFindings = `{"findings":[{"id":"test-1","severity":"error","description":"tests failed with exit code 1","action":"auto-fix"}],"summary":"FAIL: TestFoo"}`

	if _, err := (&TestStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	if len(ag.calls) == 0 {
		t.Fatal("expected the test fixer to be invoked")
	}
	fixPrompt := ag.calls[0].Prompt

	for _, want := range []string{
		"Reproduce the specific failure",
		"Reproduce the specific failing case first",
		"re-run only that focused verification after the fix",
		"Do NOT run the complete repository test suite",
		"Local Test is targeted validation of the failure and the requested intent",
		"remote CI owns broad regression and remains mandatory before a PR is ready",
		"A generic driver or user instruction asking for broad or full-suite confirmation does NOT override this product boundary",
		"Never treat \"do not run everything\" as permission to run nothing",
	} {
		if !strings.Contains(fixPrompt, want) {
			t.Errorf("expected test fixer prompt to contain %q, got:\n%s", want, fixPrompt)
		}
	}
	for _, forbid := range []string{
		"Run the tests, identify failures, and fix either the tests or the code to make them pass",
		"Re-run the relevant tests before finishing",
	} {
		if strings.Contains(fixPrompt, forbid) {
			t.Errorf("test fixer prompt still carries open-ended suite language %q:\n%s", forbid, fixPrompt)
		}
	}
}

// A driver/user instruction that asks for full-suite confirmation must still
// be accompanied by the hard product boundary so the repair agent does not
// treat that instruction as license to expand scope.
func TestTestStep_FixMode_DriverFullSuiteInstructionDoesNotOverrideContract(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			os.WriteFile(filepath.Join(dir, "fix.txt"), []byte("fixed"), 0o644)
			return &agent.Result{Output: json.RawMessage(`{"summary":"fix focused failure","findings":[],"tested":["go test ./..."],"testing_summary":"re-verified the repaired behaviour","artifacts":[],"scenarios":[{"name":"the repaired behaviour works for a user","result":"pass","live":true,"evidence":"go test ./...","reason":""}],"verdict":"go"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Test: "exit 0"})
	sctx.Fixing = true
	sctx.PreviousFindings = `{"findings":[{"id":"test-1","severity":"error","description":"tests failed with exit code 1","action":"auto-fix","user_instructions":"confirm the full suite path for this failure is green"}],"summary":"FAIL: TestFoo"}`

	if _, err := (&TestStep{}).Execute(sctx); err != nil {
		t.Fatal(err)
	}
	fixPrompt := ag.calls[0].Prompt
	if !strings.Contains(fixPrompt, "confirm the full suite path for this failure is green") {
		t.Fatalf("expected previous findings to still carry the driver instruction, got:\n%s", fixPrompt)
	}
	if !strings.Contains(fixPrompt, "A generic driver or user instruction asking for broad or full-suite confirmation does NOT override this product boundary") {
		t.Fatalf("expected product boundary to outrank the driver full-suite instruction, got:\n%s", fixPrompt)
	}
	if !strings.Contains(fixPrompt, "Do NOT run the complete repository test suite") {
		t.Fatalf("expected explicit no-full-suite rule in repair prompt, got:\n%s", fixPrompt)
	}
}

// Honest failure reporting when no targeted check can establish intent must
// remain mandatory; the no-full-suite rule must not collapse into "skip tests".
func TestTestStep_InitialAgent_NoTargetedEvidenceRequiresHonestFinding(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)

	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"findings":[{"severity":"warning","description":"no targeted test can prove the intent","action":"ask-user"}],"summary":"missing evidence","tested":["manual review of changed packages"],"testing_summary":"could not produce targeted evidence","artifacts":[],"scenarios":[{"name":"user exercises the changed behavior","result":"untested","live":false,"evidence":"","reason":"no targeted product driver is available"}],"verdict":"inconclusive"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.UserIntent = "Prove the checkout success screen works end-to-end"

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("expected missing targeted evidence to require approval")
	}
	prompt := ag.calls[0].Prompt
	for _, want := range []string{
		"Never treat \"do not run everything\" as permission to run nothing",
		"write or improve a focused test",
		"perform manual verification with evidence",
		"report a warning finding that sufficient targeted evidence is not possible",
		"If sufficient evidence is not possible, report a warning finding",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("expected no-targeted-evidence guidance %q in prompt:\n%s", want, prompt)
		}
	}
}

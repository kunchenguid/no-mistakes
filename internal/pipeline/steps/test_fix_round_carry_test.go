package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const (
	breach            = "apply wrote outside the worktree"
	breachFinding     = `{"id":"test-isolation-breach","severity":"error","action":"no-op","description":"` + breach + `"}`
	cleanTestEvidence = `{"summary":"fixed","findings":%s,"tested":["x"],"testing_summary":"ok","artifacts":[],"scenarios":[{"name":"user runs the command","result":"pass","live":true,"evidence":"x","reason":""}],"verdict":"go"}`
)

// fixRoundTestContext is a Test step on an automatic fix round whose
// evidence turn comes back clean: verdict go, reporting only reported.
func fixRoundTestContext(t *testing.T, deferred, reported string) *pipeline.StepContext {
	t.Helper()
	sctx := humanFixRoundTestContext(t, deferred, reported)
	sctx.AutoFixRound = true
	return sctx
}

func humanFixRoundTestContext(t *testing.T, deferred, reported string) *pipeline.StepContext {
	t.Helper()
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		if err := os.WriteFile(filepath.Join(dir, "fix.txt"), []byte("fixed"), 0o644); err != nil {
			return nil, err
		}
		return &agent.Result{Output: json.RawMessage(fmt.Sprintf(cleanTestEvidence, reported))}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Fixing = true
	sctx.PreviousFindings = `{"findings":[{"id":"test-2","severity":"error","action":"auto-fix","category":"test-verdict","description":"live validation verdict: no-go"}],"summary":"no-go"}`
	sctx.DeferredFindings = deferred
	return sctx
}

func descriptionCount(t *testing.T, raw, description string) int {
	t.Helper()
	findings, err := types.ParseFindingsJSON(raw)
	if err != nil {
		t.Fatalf("parse findings %q: %v", raw, err)
	}
	count := 0
	for _, item := range findings.Items {
		if item.Description == description {
			count++
		}
	}
	return count
}

// A finding the auto-fix round did not select records what an earlier turn
// observed (here a write outside the workspace), so a clean fix round cannot
// clear it: it must survive the round and keep the step parked.
func TestTestStep_FixRoundKeepsUnselectedFindings(t *testing.T) {
	t.Parallel()
	sctx := fixRoundTestContext(t, `{"findings":[`+breachFinding+`],"summary":"no-go"}`, `[]`)

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Fatalf("NeedsApproval = false, want the unselected finding to park; findings = %s", outcome.Findings)
	}
	if got := descriptionCount(t, outcome.Findings, breach); got != 1 {
		t.Fatalf("unselected finding appears %d times, want 1; findings = %s", got, outcome.Findings)
	}
}

// A human fix response accounts for every finding it leaves unselected as an
// explicit decline, so a fix round a human started carries none of them.
func TestTestStep_HumanFixRoundDoesNotCarryDeclinedFindings(t *testing.T) {
	t.Parallel()
	sctx := humanFixRoundTestContext(t, `{"findings":[`+breachFinding+`],"summary":"no-go"}`, `[]`)

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatalf("NeedsApproval = true, want the declined finding left decided; findings = %s", outcome.Findings)
	}
	if got := descriptionCount(t, outcome.Findings, breach); got != 0 {
		t.Fatalf("declined finding appears %d times, want 0; findings = %s", got, outcome.Findings)
	}
}

func TestTestStep_FixRoundDoesNotDuplicateAReReportedFinding(t *testing.T) {
	t.Parallel()
	sctx := fixRoundTestContext(t,
		`{"findings":[`+breachFinding+`],"summary":"no-go"}`,
		`[`+breachFinding+`]`)

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := descriptionCount(t, outcome.Findings, breach); got != 1 {
		t.Fatalf("re-reported finding appears %d times, want 1; findings = %s", got, outcome.Findings)
	}
}

func TestTestStep_FixRoundCarriedFindingGetsADistinctID(t *testing.T) {
	t.Parallel()
	const currentDescription = "current observation"
	sctx := fixRoundTestContext(t,
		`{"findings":[`+breachFinding+`],"summary":"no-go"}`,
		`[{"id":"test-2","severity":"info","action":"no-op","description":"`+currentDescription+`"}]`)

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Fatalf("NeedsApproval = false, want the carried finding to park; findings = %s", outcome.Findings)
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	findings = types.NormalizeFindings(findings, string(types.StepTest))
	selected := types.FilterFindings(findings, []string{"test-2"})
	if len(selected.Items) != 1 || selected.Items[0].Description != currentDescription {
		t.Fatalf("test-2 selected %+v, want only the current finding", selected.Items)
	}
	deferred := types.ExcludeFindings(findings, []string{"test-2"})
	if len(deferred.Items) != 1 || deferred.Items[0].Description != breach || deferred.Items[0].ID != "test-3" {
		t.Fatalf("deferred findings = %+v, want the carried finding as test-3", deferred.Items)
	}
}

// Every evidence turn derives its own verdict finding, so an earlier turn's
// inconclusive verdict is superseded by this turn's go rather than carried.
func TestTestStep_FixRoundDoesNotCarryAnEarlierVerdict(t *testing.T) {
	t.Parallel()
	sctx := fixRoundTestContext(t, `{"findings":[{"id":"test-3","severity":"warning","action":"ask-user","category":"test-verdict","description":"live validation verdict: inconclusive"}],"summary":"inconclusive"}`, `[]`)

	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatalf("NeedsApproval = true, want the earlier verdict superseded; findings = %s", outcome.Findings)
	}
	if got := descriptionCount(t, outcome.Findings, "live validation verdict: inconclusive"); got != 0 {
		t.Fatalf("earlier verdict appears %d times, want 0; findings = %s", got, outcome.Findings)
	}
}

func TestTestStep_AutoFixCarryPolicy(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		finding  types.Finding
		retained bool
	}{
		{"warning observation", types.Finding{Severity: types.FindingSeverityWarning, Action: types.ActionNoOp}, true},
		{"error observation", types.Finding{Severity: types.FindingSeverityError, Action: types.ActionNoOp}, true},
		{"informational question", types.Finding{Severity: types.FindingSeverityInfo, Action: types.ActionAskUser}, true},
		{"informational observation", types.Finding{Severity: types.FindingSeverityInfo, Action: types.ActionNoOp}, false},
		{"configured command", types.Finding{Severity: types.FindingSeverityError, Category: types.FindingCategoryTestCommand}, false},
		{"agent timeout", types.Finding{ID: types.FindingIDTestAgentTimeout, Severity: types.FindingSeverityWarning, Action: types.ActionAskUser}, false},
		{"unvalidated work", types.Finding{ID: types.FindingIDTestAgentUnvalidatedWork, Severity: types.FindingSeverityError, Action: types.ActionAskUser}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.finding.Description = "earlier round: " + tc.name
			deferred, err := types.MarshalFindingsJSON(types.Findings{Items: []types.Finding{tc.finding}})
			if err != nil {
				t.Fatal(err)
			}
			outcome, err := (&TestStep{}).Execute(fixRoundTestContext(t, deferred, `[]`))
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			if tc.retained {
				want = 1
			}
			if got := descriptionCount(t, outcome.Findings, tc.finding.Description); got != want {
				t.Fatalf("deferred finding appears %d times, want %d; findings = %s", got, want, outcome.Findings)
			}
			wantApproval := tc.retained && (tc.finding.Severity == types.FindingSeverityError || tc.finding.Severity == types.FindingSeverityWarning)
			if outcome.NeedsApproval != wantApproval {
				t.Fatalf("NeedsApproval = %t, want %t; findings = %s", outcome.NeedsApproval, wantApproval, outcome.Findings)
			}
		})
	}
}

// End to end through the executor: a no-go verdict starts an automatic fix
// round, which defers the agent's ask-user finding. The fix round's clean
// result must still park on that finding, and only a human decision on it
// lets the step complete.
func TestTestStep_DeferredAskUserFindingParksUntilAHumanDecidesIt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		severity string
		action   string
	}{
		{"clean rerun", types.FindingSeverityWarning, ""},
		{"info re-reported as no-op", types.FindingSeverityInfo, types.ActionNoOp},
		{"warning re-reported as auto-fix", types.FindingSeverityWarning, types.ActionAutoFix},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			testDeferredAskUserFindingParksUntilAHumanDecidesIt(t, tc.severity, tc.action)
		})
	}
}

func testDeferredAskUserFindingParksUntilAHumanDecidesIt(t *testing.T, severity, reportedAction string) {
	t.Helper()
	const question = "this failing check looks intentional; confirm it should exist"
	dir, baseSHA, headSHA := setupGitRepo(t)
	gitCmd(t, dir, "checkout", "--detach", headSHA)
	var mu sync.Mutex
	calls := 0
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		mu.Lock()
		calls++
		call := calls
		mu.Unlock()
		if call == 1 {
			return &agent.Result{Output: json.RawMessage(`{"summary":"broken","findings":[{"id":"intent-check","severity":"` + severity + `","action":"ask-user","description":"` + question + `"}],"tested":["x"],"testing_summary":"failed","artifacts":[],"scenarios":[{"name":"user runs the command","result":"fail","live":true,"evidence":"x","reason":""}],"verdict":"no-go"}`)}, nil
		}
		if err := os.WriteFile(filepath.Join(dir, "fix.txt"), []byte(fmt.Sprint("fix ", call)), 0o644); err != nil {
			return nil, err
		}
		if call == 3 && reportedAction != "" {
			reported := `[{"id":"intent-check","severity":"` + severity + `","action":"` + reportedAction + `","description":"` + question + `"}]`
			return &agent.Result{Output: json.RawMessage(fmt.Sprintf(cleanTestEvidence, reported))}, nil
		}
		return &agent.Result{Output: json.RawMessage(fmt.Sprintf(cleanTestEvidence, "[]"))}, nil
	}}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.AutoFix.Test = 3

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	parked := make(chan string, 2)
	exec := pipeline.NewExecutor(sctx.DB, paths.WithRoot(t.TempDir()), sctx.Config, ag, []pipeline.Step{&TestStep{}}, func(event ipc.Event) {
		if event.Type == ipc.EventStepCompleted && event.Status != nil && (*event.Status == string(types.StepStatusAwaitingApproval) || *event.Status == string(types.StepStatusFixReview)) && event.Findings != nil {
			parked <- *event.Findings
		}
	})
	done := make(chan error, 1)
	go func() { done <- exec.Execute(ctx, sctx.Run, sctx.Repo, dir) }()

	var gate string
	select {
	case gate = <-parked:
	case err := <-done:
		run, getErr := sctx.DB.GetRun(sctx.Run.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		t.Fatalf("run finished as %s (err %v) without parking on the deferred ask-user finding", run.Status, err)
	case <-time.After(30 * time.Second):
		t.Fatal("Test step neither parked nor finished")
	}
	findings, err := types.ParseFindingsJSON(gate)
	if err != nil {
		t.Fatal(err)
	}
	var questionID string
	var ignoredIDs []string
	for _, item := range findings.Items {
		ignoredIDs = append(ignoredIDs, item.ID)
		if item.Description == question && item.ActionOrDefault() == types.ActionAskUser {
			if questionID != "" {
				t.Fatalf("parked gate carries the deferred ask-user finding twice: %s", gate)
			}
			questionID = item.ID
		}
	}
	if questionID == "" {
		t.Fatalf("parked gate lacks the deferred ask-user finding: %s", gate)
	}

	// A human fix response that declines it is that decision.
	added := []types.Finding{{Severity: types.FindingSeverityInfo, Description: "rename the fixture", Action: types.ActionAutoFix}}
	if _, err := exec.RespondWithOverrides(types.StepTest, types.ActionFix, []string{}, ignoredIDs, nil, added, ""); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run failed after the human decision: %v", err)
		}
	case gate := <-parked:
		t.Fatalf("declined finding parked the step again: %s", gate)
	case <-time.After(30 * time.Second):
		t.Fatal("Test step did not finish after the human decision")
	}
	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != types.RunCompleted {
		t.Fatalf("run status = %s, want %s", run.Status, types.RunCompleted)
	}
}

package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const reportedOperatorStop = `{
	"summary":"Validation stopped",
	"findings":[{"id":"validation-refused","severity":"error","action":"ask-user","description":"The checker refused to proceed without an operator decision.","requires_operator_stop":true}],
	"tested":["run-check"],
	"testing_summary":"The checker refused. The dependent scenario was not run.",
	"artifacts":[],
	"scenarios":[
		{"name":"Start validation","result":"fail","live":true,"evidence":"run-check exited with a refusal","reason":""},
		{"name":"Verify the output","result":"untested","live":false,"evidence":"","reason":"The checker refused to proceed."}
	],
	"verdict":"no-go"
}`

func TestTestStep_OperatorStopParksBeforeAnotherProviderCall(t *testing.T) {
	for _, tc := range []struct {
		name   string
		report string
		valid  bool
	}{
		{"valid evidence", reportedOperatorStop, true},
		{"invalid verdict", strings.Replace(reportedOperatorStop, `"no-go"`, `"go"`, 1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, baseSHA, headSHA := setupGitRepo(t)
			gitCmd(t, dir, "checkout", "--detach", headSHA)
			var primaryCalls, fallbackCalls atomic.Int32
			primary := &mockAgent{name: "primary", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				if primaryCalls.Add(1) == 1 {
					return &agent.Result{Output: json.RawMessage(tc.report)}, nil
				}
				return &agent.Result{Output: json.RawMessage(fmt.Sprintf(cleanTestEvidence, "[]"))}, nil
			}}
			fallback := &mockAgent{name: "fallback", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				fallbackCalls.Add(1)
				return nil, fmt.Errorf("unexpected fallback invocation")
			}}
			ag := agent.NewFallback([]agent.Agent{primary, fallback})
			sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
			sctx.Config.AutoFix.Test = 3
			sctx.Config.AgentCrashRetries = 2

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			parked := make(chan ipc.Event, 1)
			exec := pipeline.NewExecutor(sctx.DB, paths.WithRoot(t.TempDir()), sctx.Config, ag, []pipeline.Step{&TestStep{}}, func(event ipc.Event) {
				if event.Type == ipc.EventStepCompleted && event.Status != nil &&
					(*event.Status == string(types.StepStatusAwaitingApproval) || *event.Status == string(types.StepStatusFixReview)) {
					parked <- event
				}
			})
			done := make(chan struct{})
			var executeErr error
			go func() {
				defer close(done)
				executeErr = exec.Execute(ctx, sctx.Run, sctx.Repo, dir)
			}()
			t.Cleanup(func() {
				cancel()
				<-done
			})
			select {
			case event := <-parked:
				if primaryCalls.Load() != 1 || fallbackCalls.Load() != 0 || *event.Status != string(types.StepStatusAwaitingApproval) {
					t.Fatalf("operator stop parked as %s after %d primary calls and %d fallback calls", *event.Status, primaryCalls.Load(), fallbackCalls.Load())
				}
			case <-done:
				t.Fatalf("Test finished after %d primary calls and %d fallback calls without parking on the operator stop: %v", primaryCalls.Load(), fallbackCalls.Load(), executeErr)
			case <-ctx.Done():
				t.Fatal("Test did not park on the operator stop")
			}

			run, err := sctx.DB.GetRun(sctx.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if run.Status != types.RunRunning || run.HeadSHA != headSHA {
				t.Fatalf("parked run status = %s, head = %s, want running at %s", run.Status, run.HeadSHA, headSHA)
			}
			stepResults, err := sctx.DB.GetStepsByRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(stepResults) != 1 || stepResults[0].Status != types.StepStatusAwaitingApproval || stepResults[0].FindingsJSON == nil {
				t.Fatalf("Test did not persist an approval gate: %+v", stepResults)
			}
			step := stepResults[0]
			rounds, err := sctx.DB.GetRoundsByStep(step.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(rounds) != 1 || rounds[0].Trigger != "initial" || rounds[0].SelectedFindingIDs != nil ||
				rounds[0].FindingsJSON == nil || *rounds[0].FindingsJSON != *step.FindingsJSON {
				t.Fatalf("operator stop must persist in the initial unselected round: %+v", rounds)
			}
			findings, err := types.ParseFindingsJSON(*step.FindingsJSON)
			if err != nil {
				t.Fatal(err)
			}
			if findings.TestedHeadSHA != headSHA || !strings.Contains(*step.FindingsJSON, `"requires_operator_stop":true`) ||
				len(findings.Items) == 0 || findings.Items[0].ID != "validation-refused" || findings.Items[0].Action != types.ActionAskUser {
				t.Fatalf("parked Test lost the reported stop or its head: %s", *step.FindingsJSON)
			}
			if tc.valid {
				if findings.Verdict != types.TestVerdictNoGo || len(findings.Scenarios) != 2 ||
					findings.Scenarios[0].Result != types.ScenarioResultFail || findings.Scenarios[0].Evidence != "run-check exited with a refusal" ||
					findings.Scenarios[1].Result != types.ScenarioResultUntested || findings.Scenarios[1].Reason != "The checker refused to proceed." {
					t.Fatalf("parked Test lost the scenario evidence: %s", *step.FindingsJSON)
				}
			} else if findings.Verdict == types.TestVerdictGo || !strings.Contains(findings.TestingSummary, "failed scenario") {
				t.Fatalf("parked Test did not report the validation limit: %s", *step.FindingsJSON)
			}

			if _, err := exec.RespondWithOverrides(types.StepTest, types.ActionAbort, nil, nil, nil, nil, "Validation was refused"); err != nil {
				t.Fatal(err)
			}
			select {
			case <-done:
				if executeErr == nil || !strings.Contains(executeErr.Error(), "Validation was refused") {
					t.Fatalf("Test did not honor the operator abort: %v", executeErr)
				}
			case <-ctx.Done():
				t.Fatal("Test did not finish after the operator abort")
			}
			if primaryCalls.Load() != 1 || fallbackCalls.Load() != 0 {
				t.Fatalf("operator abort reached a provider: primary=%d fallback=%d", primaryCalls.Load(), fallbackCalls.Load())
			}
		})
	}
}

package steps

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
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
	legacyReport := strings.Replace(reportedOperatorStop, `"findings":`, `"items":`, 1)
	for _, tc := range []struct {
		name      string
		report    string
		rejection string
		fix       bool
	}{
		{"valid evidence", reportedOperatorStop, "", false},
		{"valid legacy evidence", legacyReport, "", false},
		{"canonical repair and legacy stop", strings.Replace(legacyReport, `"summary":`, `"findings":[{"id":"repair","severity":"error","action":"auto-fix","description":"Repair the synthetic defect."}],"summary":`, 1), "", false},
		{"invalid verdict", strings.Replace(reportedOperatorStop, `"no-go"`, `"go"`, 1), "failed scenario", false},
		{"invalid unused legacy field", strings.Replace(reportedOperatorStop, `"summary":`, `"items":false,"summary":`, 1), "cannot unmarshal bool", false},
		{"invalid canonical field", strings.Replace(legacyReport, `"summary":`, `"findings":false,"summary":`, 1), "cannot unmarshal bool", false},
		{"invalid sibling finding", strings.Replace(reportedOperatorStop, `"findings":[`, `"findings":[false,`, 1), "cannot unmarshal bool", false},
		{"invalid canonical finding", strings.Replace(legacyReport, `"summary":`, `"findings":[false],"summary":`, 1), "cannot unmarshal bool", false},
		{"operator authorizes a repair", reportedOperatorStop, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, baseSHA, headSHA := setupGitRepo(t)
			gitCmd(t, dir, "checkout", "--detach", headSHA)
			var primaryCalls, fallbackCalls atomic.Int32
			primary := &mockAgent{name: "primary", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				switch primaryCalls.Add(1) {
				case 1:
					return &agent.Result{Output: json.RawMessage(tc.report)}, nil
				case 2:
					if tc.fix {
						if err := os.WriteFile(filepath.Join(dir, "checker.conf"), []byte("approved=true\n"), 0o644); err != nil {
							return nil, err
						}
						return &agent.Result{Output: json.RawMessage(`{"summary":"Set the approved checker configuration"}`)}, nil
					}
				}
				return &agent.Result{Output: json.RawMessage(fmt.Sprintf(cleanTestEvidence, "[]"))}, nil
			}}
			fallback := &mockAgent{name: "fallback", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				fallbackCalls.Add(1)
				return nil, fmt.Errorf("unexpected fallback invocation")
			}}
			ag := agent.NewFallback([]agent.Agent{primary, fallback})
			sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{Lint: "exit 0"})
			sctx.Config.AutoFix.Test = 3
			sctx.Config.AgentCrashRetries = 2

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			parked := make(chan ipc.Event, 1)
			exec := pipeline.NewExecutor(sctx.DB, paths.WithRoot(t.TempDir()), sctx.Config, ag, []pipeline.Step{&TestStep{}, &LintStep{}}, func(event ipc.Event) {
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
			if len(stepResults) != 2 || stepResults[0].Status != types.StepStatusAwaitingApproval ||
				stepResults[0].FindingsJSON == nil || stepResults[1].Status != types.StepStatusPending {
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
				!types.HasTestOperatorStop(findings) {
				t.Fatalf("parked Test lost the reported stop or its head: %s", *step.FindingsJSON)
			}
			if tc.rejection == "" {
				original, err := types.ParseFindingsJSON(tc.report)
				if err != nil {
					t.Fatal(err)
				}
				for _, want := range original.Items {
					found := false
					for _, got := range findings.Items {
						if reflect.DeepEqual(got, want) {
							found = true
						}
					}
					if !found {
						t.Fatalf("parked Test lost an accepted finding: %+v", want)
					}
				}
				if findings.Verdict != types.TestVerdictNoGo || len(findings.Scenarios) != 2 ||
					findings.Scenarios[0].Result != types.ScenarioResultFail || findings.Scenarios[0].Evidence != "run-check exited with a refusal" ||
					findings.Scenarios[1].Result != types.ScenarioResultUntested || findings.Scenarios[1].Reason != "The checker refused to proceed." {
					t.Fatalf("parked Test lost the scenario evidence: %s", *step.FindingsJSON)
				}
			} else if findings.Verdict != types.TestVerdictInconclusive || !strings.Contains(findings.TestingSummary, tc.rejection) {
				t.Fatalf("parked Test did not report the validation limit: %s", *step.FindingsJSON)
			}

			if tc.fix {
				ids := make([]string, 0, len(findings.Items))
				for _, item := range findings.Items {
					ids = append(ids, item.ID)
				}
				if _, err := exec.RespondWithOverrides(types.StepTest, types.ActionFix, ids, nil, nil, nil, "",
					pipeline.ResponseOptions{Origin: types.ResponseOriginInteractive}); err != nil {
					t.Fatal(err)
				}
			} else if _, err := exec.RespondWithOverrides(types.StepTest, types.ActionAbort, nil, nil, nil, nil, "Validation was refused"); err != nil {
				t.Fatal(err)
			}
			select {
			case <-done:
			case event := <-parked:
				t.Fatalf("operator response left Test parked: %s", *event.Status)
			case <-ctx.Done():
				t.Fatal("run did not finish after the operator response")
			}
			if !tc.fix {
				if executeErr == nil || !strings.Contains(executeErr.Error(), "Validation was refused") {
					t.Fatalf("Test did not honor the operator abort: %v", executeErr)
				}
				if primaryCalls.Load() != 1 || fallbackCalls.Load() != 0 {
					t.Fatalf("operator abort reached a provider: primary=%d fallback=%d", primaryCalls.Load(), fallbackCalls.Load())
				}
				return
			}
			if executeErr != nil {
				t.Fatalf("run did not finish after the authorized fix: %v", executeErr)
			}
			if primaryCalls.Load() != 3 || fallbackCalls.Load() != 0 {
				t.Fatalf("authorized fix ran %d primary calls and %d fallback calls", primaryCalls.Load(), fallbackCalls.Load())
			}
			run, err = sctx.DB.GetRun(run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if run.Status != types.RunCompleted || run.HeadSHA == headSHA {
				t.Fatalf("authorized repair did not complete on a new head: %+v", run)
			}
			rounds, err = sctx.DB.GetRoundsByStep(step.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(rounds) != 2 || rounds[0].FindingsJSON == nil || *rounds[0].FindingsJSON != *step.FindingsJSON ||
				rounds[0].SelectionSource == nil || *rounds[0].SelectionSource != db.RoundSelectionSourceUser ||
				rounds[1].FindingsJSON == nil {
				t.Fatalf("authorized fix did not retain the stopped round and its decision: %+v", rounds)
			}
			fresh, err := types.ParseFindingsJSON(*rounds[1].FindingsJSON)
			if err != nil {
				t.Fatal(err)
			}
			if fresh.Verdict != types.TestVerdictGo || fresh.TestedHeadSHA != run.HeadSHA || types.HasTestOperatorStop(fresh) {
				t.Fatalf("fresh passing Test result is missing for the repaired head: %s", *rounds[1].FindingsJSON)
			}
		})
	}
}

func TestTestAnalyzer_OperatorStopRequiresExplicitAskUser(t *testing.T) {
	for _, tc := range []struct {
		name   string
		fields string
		stop   bool
		reject bool
	}{
		{"ask-user", `"action":"ask-user","requires_operator_stop":true`, true, false},
		{"omitted marker", `"action":"ask-user"`, false, false},
		{"null marker", `"action":"ask-user","requires_operator_stop":null`, false, false},
		{"false marker", `"action":"ask-user","requires_operator_stop":false`, false, false},
		{"missing action", `"requires_operator_stop":true`, false, true},
		{"legacy action", `"requires_human_review":true,"requires_operator_stop":true`, false, true},
		{"auto-fix", `"action":"auto-fix","requires_operator_stop":true`, false, true},
		{"no-op", `"action":"no-op","requires_operator_stop":true`, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			item := `[{"severity":"info","description":"Validation refused",` + tc.fields + `}]`
			output := json.RawMessage(fmt.Sprintf(cleanTestEvidence, item))
			findings, err := parseTestAnalyzerOutput(&agent.Result{Output: output})
			if stopped := types.HasTestOperatorStop(findings); (err != nil) != tc.reject || stopped != tc.stop {
				t.Fatalf("operator stop = %v, error = %v. Want stop = %v, rejected = %v", stopped, err, tc.stop, tc.reject)
			}
		})
	}
}

func TestTestAnalyzer_OperatorStopAliasPrecedence(t *testing.T) {
	stop := `{"id":"canonical-stop","severity":"warning","action":"ask-user","description":"Operator decision required.","requires_operator_stop":true}`
	legacyStop := strings.Replace(stop, "canonical-stop", "legacy-stop", 1)
	repair := `{"id":"repair","severity":"error","action":"auto-fix","description":"Repair the synthetic defect."}`
	for _, tc := range []struct {
		name      string
		canonical string
		legacy    string
		wantIDs   []string
	}{
		{"canonical wins", stop, legacyStop, []string{"canonical-stop"}},
		{"legacy supplements canonical", repair, legacyStop, []string{"repair", "legacy-stop"}},
		{"legacy supplies empty canonical", "", legacyStop, []string{"legacy-stop"}},
		{"ordinary legacy stays unselected", repair, repair, []string{"repair"}},
		{"invalid legacy stop is not promoted", repair, strings.Replace(legacyStop, `"ask-user"`, `"auto-fix"`, 1), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := fmt.Sprintf(cleanTestEvidence, "["+tc.canonical+"]")
			raw = strings.Replace(raw, `"summary":`, `"items":[`+tc.legacy+`],"summary":`, 1)
			got, err := parseTestAnalyzerOutput(&agent.Result{Output: json.RawMessage(raw)})
			if tc.wantIDs == nil {
				if err == nil {
					t.Fatal("invalid stop action must be rejected")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			var ids []string
			for _, item := range got.Items {
				ids = append(ids, item.ID)
			}
			if !reflect.DeepEqual(ids, tc.wantIDs) {
				t.Fatalf("finding IDs = %v, want %v", ids, tc.wantIDs)
			}
			if got.Verdict != types.TestVerdictGo || len(got.Scenarios) == 0 || len(got.Tested) == 0 {
				t.Fatalf("accepted evidence was lost: %+v", got)
			}
		})
	}
}

package citest

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps/internal/stepstest"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestCIStep_BillingRefusalParksWithoutRepairOrBudgetSpend(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		reruns     int
		pending    bool
		failedRead string
	}{{0, false, ""}, {1, false, ""}, {0, true, ""}, {1, true, "job"}, {1, true, "annotations"}} {
		t.Run(fmt.Sprintf("rerun_budget_%d_pending_%v_failed_read_%s", tc.reruns, tc.pending, tc.failedRead), func(t *testing.T) {
			t.Parallel()
			dir, base, head := stepstest.SetupGitRepo(t)
			ag := &stepstest.MockAgent{AgentName: "test"}
			sctx := stepstest.NewTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
			const billing = "The job was not started because recent account payments have failed or your spending limit needs to be increased."
			const jobLink = "https://github.com/test/repo/actions/runs/1/job/2"
			checks := `[{"id":2,"name":"build","state":"FAILURE","bucket":"fail","app":"github-actions","link":"` + jobLink + `"}]`
			if tc.pending {
				checks = strings.TrimSuffix(checks, "]") + `,{"id":3,"name":"unit","state":"PENDING","bucket":"pending"}]`
			}
			logPath := filepath.Join(t.TempDir(), "gh.log")
			sctx.Env = append(stepstest.FakeCIGH(t, "OPEN", checks),
				"FAKE_CLI_PR_HEAD_SHA="+head, "FAKE_CLI_LOG="+logPath,
				`FAKE_CLI_ACTIONS_JOB={"id":2,"run_id":1,"head_sha":"`+head+`","status":"completed","conclusion":"failure","steps":[],"check_run_url":"https://api.github.com/repos/test/repo/check-runs/2"}`,
				`FAKE_CLI_CHECK_ANNOTATIONS=[[{"annotation_level":"failure","message":"`+billing+`","raw_details":"private-marker","path":"/Users/private-marker"}]]`)
			url := "https://github.com/test/repo/pull/42"
			sctx.Run.PRURL = &url
			sctx.Config.AutoFix = config.AutoFix{CI: 3}
			sctx.Config.CI.RerunTransient = tc.reruns
			current := time.Now()
			sctx.Config.CITimeout = time.Second
			var logs []string
			sctx.Log = func(s string) { logs = append(logs, s) }
			polls := 1
			step := (&steps.CIStep{}).SetNow(func() time.Time { return current }).SetBaseBranchTip(func(context.Context) (string, bool) { return base, true }).SetWaitForNextPoll(func(context.Context, time.Duration) error {
				if !tc.pending {
					t.Fatal("a settled billing refusal must park immediately")
				}
				if tc.failedRead != "" {
					if polls >= 3 {
						t.Fatal("billing refusal must park when the companion check finishes")
					}
					// Poll 2 loses the evidence read while the companion is still
					// pending. Poll 3 settles it with the evidence still unreadable.
					for i, value := range sctx.Env {
						if tc.failedRead == "job" && strings.HasPrefix(value, "FAKE_CLI_ACTIONS_JOB=") {
							sctx.Env[i] = "FAKE_CLI_ACTIONS_JOB="
						}
						if tc.failedRead == "annotations" && strings.HasPrefix(value, "FAKE_CLI_CHECK_ANNOTATIONS=") {
							sctx.Env[i] = "FAKE_CLI_CHECK_ANNOTATIONS="
						}
						if polls == 2 && strings.HasPrefix(value, "FAKE_CLI_CHECKS=") {
							sctx.Env[i] = "FAKE_CLI_CHECKS=" + strings.ReplaceAll(strings.ReplaceAll(checks, "PENDING", "SUCCESS"), "pending", "pass")
						}
					}
					polls++
					return nil
				}
				current = current.Add(2 * time.Second)
				return nil
			})
			outcome, err := stepstest.ExecuteWithAutoFix(t, step, sctx, 0)
			if err != nil || outcome == nil || !outcome.NeedsApproval || outcome.AutoFixable {
				t.Fatalf("outcome=%+v err=%v logs=%v", outcome, err, logs)
			}
			if len(ag.Calls) != 0 || sctx.Fixing {
				t.Fatalf("agent calls=%d fixing=%v, want no repair round despite available budget", len(ag.Calls), sctx.Fixing)
			}
			if tc.failedRead != "" && polls != 3 {
				t.Fatalf("observed %d polls, want confirmed, unreadable/pending, unreadable/settled", polls)
			}
			findings, err := types.ParseFindingsJSON(outcome.Findings)
			if err != nil {
				t.Fatal(err)
			}
			if len(findings.Items) != 1 || len(types.AutoFixableFindings(findings).Items) != 0 {
				t.Fatalf("findings=%+v", findings)
			}
			item := findings.Items[0]
			if item.Category != "ci-infrastructure" || item.Action != types.ActionAskUser || item.CheckID != "github-check-run:2" {
				t.Fatalf("finding=%+v, want infrastructure decision bound to check 2", item)
			}
			for _, evidence := range []string{billing, jobLink, "local-validation", "CI-override"} {
				if !strings.Contains(item.Description, evidence) {
					t.Fatalf("finding=%+v, missing evidence or next action %q", item, evidence)
				}
			}
			if strings.Contains(outcome.Findings, "private-marker") {
				t.Fatal("unrelated annotation data leaked into retained findings")
			}
			run, err := sctx.DB.GetRun(sctx.Run.ID)
			if err != nil || run.CIReadyAt != nil {
				t.Fatalf("billing refusal became ready: run=%+v err=%v", run, err)
			}
			if got := stepstest.GitCmd(t, dir, "rev-parse", "HEAD"); got != head {
				t.Fatalf("head moved from %s to %s", head, got)
			}
			commands, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(commands), "run rerun") || step.TransientRerunRecorded("build") {
				t.Fatalf("billing refusal spent a workflow rerun: %s", commands)
			}
		})
	}
}

func TestCIStep_UnprovenBillingEvidenceStillReachesCodeRepair(t *testing.T) {
	t.Parallel()
	const jobLink = "https://github.com/test/repo/actions/runs/1/job/2"
	const billing = "The job was not started because recent account payments have failed or your spending limit needs to be increased."
	for _, tc := range []struct {
		name, job, annotations, link string
		mixed                        bool
	}{
		{name: "one second real failure", job: `{"id":2,"run_id":1,"status":"completed","conclusion":"failure","started_at":"2026-10-28T00:00:00Z","completed_at":"2026-10-28T00:00:01Z","steps":[{"number":1,"conclusion":"success"},{"number":2,"conclusion":"failure"}],"check_run_url":"https://api.github.com/repos/test/repo/check-runs/2"}`, annotations: `[[{"annotation_level":"failure","message":"` + billing + `"}]]`, link: jobLink},
		{name: "missing annotations", job: `{"id":2,"run_id":1,"status":"completed","conclusion":"failure","steps":[],"check_run_url":"https://api.github.com/repos/test/repo/check-runs/2"}`, annotations: `[[]]`, link: jobLink},
		{name: "annotation read fails", job: `{"id":2,"run_id":1,"status":"completed","conclusion":"failure","steps":[],"check_run_url":"https://api.github.com/repos/test/repo/check-runs/2"}`, link: jobLink},
		{name: "job read fails", annotations: `[[{"annotation_level":"failure","message":"` + billing + `"}]]`, link: jobLink},
		{name: "ambiguous empty job list", annotations: `[[{"annotation_level":"failure","message":"` + billing + `"}]]`, link: "https://github.com/test/repo/actions/runs/1"},
		{name: "billing alongside real failure", job: `{"id":2,"run_id":1,"status":"completed","conclusion":"failure","steps":[],"check_run_url":"https://api.github.com/repos/test/repo/check-runs/2"}`, annotations: `[[{"annotation_level":"failure","message":"` + billing + `"}]]`, link: jobLink, mixed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, base, head := stepstest.SetupGitRepo(t)
			ag := &stepstest.MockAgent{AgentName: "test", RunFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				return &agent.Result{Output: json.RawMessage(`{"summary":"operator decision needed","code_change_needed":false}`)}, nil
			}}
			sctx := stepstest.NewTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
			checks := `[{"id":2,"name":"build","state":"FAILURE","bucket":"fail","app":"github-actions","link":"` + tc.link + `"}]`
			wantCheckID := "github-check-run:2"
			if tc.mixed {
				checks = strings.TrimSuffix(checks, "]") + `,{"id":3,"name":"unit","state":"FAILURE","bucket":"fail","app":"github-actions","link":"https://github.com/test/repo/actions/runs/1/job/3"}]`
				wantCheckID = "github-check-run:3"
			}
			job := tc.job
			if job != "" {
				job = strings.Replace(job, `"id":2`, `"head_sha":"`+head+`","id":2`, 1)
			}
			sctx.Env = append(stepstest.FakeCIGH(t, "OPEN", checks), "FAKE_CLI_PR_HEAD_SHA="+head,
				"FAKE_CLI_ACTIONS_JOB="+job, "FAKE_CLI_CHECK_ANNOTATIONS="+tc.annotations, `FAKE_CLI_RUN_JOBS=[]`)
			url := "https://github.com/test/repo/pull/42"
			sctx.Run.PRURL = &url
			sctx.Config.AutoFix = config.AutoFix{CI: 3}
			step := (&steps.CIStep{}).SetBaseBranchTip(func(context.Context) (string, bool) { return base, true }).SetWaitForNextPoll(func(context.Context, time.Duration) error {
				t.Fatal("settled failure must reach repair")
				return nil
			})
			outcome, err := stepstest.ExecuteWithAutoFix(t, step, sctx, 0)
			if err != nil || outcome == nil || !outcome.NeedsApproval || len(ag.Calls) != 1 || !sctx.Fixing {
				t.Fatalf("outcome=%+v err=%v calls=%d fixing=%v", outcome, err, len(ag.Calls), sctx.Fixing)
			}
			selected, err := types.ParseFindingsJSON(sctx.PreviousFindings)
			if err != nil || len(selected.Items) != 1 || selected.Items[0].Category != types.FindingCategoryCICheck || selected.Items[0].CheckID != wantCheckID {
				t.Fatalf("selected=%+v err=%v, want ordinary CI failure selected for repair", selected, err)
			}
			if strings.Contains(outcome.Findings, "ci-infrastructure") != tc.mixed {
				t.Fatalf("unproven evidence fabricated an infrastructure exemption: %s", outcome.Findings)
			}
		})
	}
}

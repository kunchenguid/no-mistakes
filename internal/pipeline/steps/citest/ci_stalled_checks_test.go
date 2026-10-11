package citest

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps/internal/stepstest"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestCIStep_StalledChecksParkWithoutReadiness(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, concluded string
		count           int
		autoFix         bool
	}{
		{name: "all pending"},
		{name: "successful legacy status", concluded: `,{"__typename":"StatusContext","id":"passed","context":"passed","state":"SUCCESS"}`, count: 1},
		{name: "failure preserved", concluded: `,{"__typename":"CheckRun","id":"failed","name":"build","status":"COMPLETED","conclusion":"FAILURE"}`, count: 1, autoFix: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, headSHA := stepstest.SetupGitRepo(t)
			checks := fmt.Sprintf(`{"data":{"repository":{"object":{"statusCheckRollup":{"contexts":{"nodes":[{"__typename":"StatusContext","id":"legacy","context":"legacy","state":"PENDING"}%s],"pageInfo":{"hasNextPage":false}}}}}}}`, tc.concluded)
			sctx := stepstest.NewTestContextWithDBRecords(t, nil, dir, baseSHA, headSHA, config.Commands{})
			sctx.Env = stepstest.FakeCIGHMergeable(t, "OPEN", checks, "MERGEABLE")
			prURL := "https://github.com/test/repo/pull/42"
			sctx.Run.PRURL = &prURL
			sctx.Config.CITimeout = -1
			sctx.Config.NoCI = true
			sctx.DeferredFindings = `{"findings":[{"id":"deferred","severity":"warning","action":"ask-user","description":"Existing decision"}]}`
			var logs []string
			sctx.Log = func(s string) { logs = append(logs, s) }
			start := time.Unix(1700000000, 0)
			now := start
			step := (&steps.CIStep{}).SetNow(func() time.Time { return now }).SetPollIntervalOverride(5 * time.Minute).SetWaitForNextPoll(func(context.Context, time.Duration) error {
				now = now.Add(5 * time.Minute)
				if now.Sub(start) > 20*time.Minute {
					t.Fatal("stalled check did not park")
				}
				return nil
			})
			outcome, err := step.Execute(sctx)
			if err != nil {
				t.Fatal(err)
			}
			if !outcome.NeedsApproval || outcome.AutoFixable != tc.autoFix {
				t.Fatalf("unexpected outcome: %+v", outcome)
			}
			if now.Sub(start) != 20*time.Minute {
				t.Fatalf("parked at %v", now.Sub(start))
			}
			findings, err := types.ParseFindingsJSON(outcome.Findings)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(findings.Summary, fmt.Sprintf("%d concluded", tc.count)) {
				t.Fatalf("missing concluded count: %s", findings.Summary)
			}
			var stalled, deferred bool
			for _, item := range findings.Items {
				if item.Check == "legacy" && item.CheckID == "github-status:legacy" && item.Action == types.ActionAskUser {
					stalled = true
				}
				if item.ID == "deferred" {
					deferred = true
				}
			}
			if !stalled || !deferred {
				t.Fatalf("missing stalled or deferred finding: %s", outcome.Findings)
			}
			run, err := sctx.DB.GetRun(sctx.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if run.CIReadyAt != nil || strings.Contains(strings.Join(logs, "\n"), "all CI checks passed") {
				t.Fatal("stall reported ready")
			}
			reason, err := step.VerifyApprovalOverride(sctx)
			if err != nil || !strings.Contains(reason, "legacy") {
				t.Fatalf("raw pending check missing from override: %q, %v", reason, err)
			}
		})
	}
}

func TestCIStep_StallWindowRequiresContinuousUnchangedEvidence(t *testing.T) {
	t.Parallel()
	const queued = `{"__typename":"CheckRun","name":"build","status":"QUEUED","detailsUrl":"https://github.com/test/repo/actions/runs/1/job/1","startedAt":"2026-10-10T00:00:00Z"}`
	for _, tc := range []struct {
		name, second          string
		minutes               int
		readError, headChange bool
	}{
		{name: "unchanged", second: queued, minutes: 20},
		{name: "state transition", second: strings.ReplaceAll(queued, "QUEUED", "IN_PROGRESS"), minutes: 30},
		{name: "new execution", second: strings.ReplaceAll(queued, "runs/1/", "runs/2/"), minutes: 30},
		{name: "changed start", second: strings.ReplaceAll(queued, "00:00:00Z", "00:01:00Z"), minutes: 30},
		{name: "independent same name status", second: queued + `,{"__typename":"StatusContext","id":"other","context":"build","state":"PENDING"}`, minutes: 20},
		{name: "disappeared", minutes: 40},
		{name: "read discontinuity", second: queued, readError: true, minutes: 40},
		{name: "head changed", second: queued, headChange: true, minutes: 30},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, headSHA := stepstest.SetupGitRepo(t)
			sctx := stepstest.NewTestContext(t, nil, dir, baseSHA, headSHA, config.Commands{})
			var sequence []string
			for _, nodes := range []string{queued, tc.second, tc.second} {
				if len(sequence) == 2 && tc.second == "" {
					nodes = queued
				}
				sequence = append(sequence, fmt.Sprintf(`{"data":{"repository":{"object":{"statusCheckRollup":{"contexts":{"nodes":[%s],"pageInfo":{"hasNextPage":false}}}}}}}`, nodes))
			}
			sctx.Env = stepstest.FakeCIGHSequenceMergeable(t, "OPEN", sequence, "MERGEABLE")
			prURL := "https://github.com/test/repo/pull/42"
			sctx.Run.PRURL = &prURL
			sctx.Config.CITimeout = time.Hour
			start := time.Unix(1700000000, 0)
			now := start
			polls := 0
			step := (&steps.CIStep{}).SetNow(func() time.Time { return now }).SetBaseBranchTip(func(context.Context) (string, bool) { return fmt.Sprint(polls), true }).SetPollIntervalOverride(10 * time.Minute).SetWaitForNextPoll(func(context.Context, time.Duration) error {
				polls++
				now = now.Add(10 * time.Minute)
				if now.Sub(start) > time.Duration(tc.minutes)*time.Minute {
					t.Fatal("stall window did not expire")
				}
				if tc.readError {
					// The fake CLI supports read errors on its non-sequenced path.
					if polls == 1 {
						sctx.Env = stepstest.FakeCIGHChecksError(t, "OPEN", "MERGEABLE", "unavailable")
					} else {
						sctx.Env = stepstest.FakeCIGHMergeable(t, "OPEN", sequence[0], "MERGEABLE")
					}
				}
				if tc.headChange && polls == 1 {
					for i, entry := range sctx.Env {
						if strings.HasPrefix(entry, "FAKE_CLI_PR_HEAD_SHA=") {
							sctx.Env[i] = "FAKE_CLI_PR_HEAD_SHA=new-head"
						}
					}
				}
				return nil
			})
			outcome, err := step.Execute(sctx)
			if err != nil {
				t.Fatal(err)
			}
			if !outcome.NeedsApproval || !strings.Contains(outcome.Findings, "stalled") || now.Sub(start) != time.Duration(tc.minutes)*time.Minute {
				t.Fatalf("unexpected park after %v: %+v", now.Sub(start), outcome)
			}
		})
	}
}

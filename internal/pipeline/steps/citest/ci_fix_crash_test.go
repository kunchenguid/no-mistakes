package citest

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps/internal/stepstest"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type crashTestCIStep struct {
	*steps.CIStep
	env []string
}

func (s crashTestCIStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	sctx.Env = s.env
	return s.CIStep.Execute(sctx)
}

func TestCIStep_FixAgentCrashesSurviveGreenChecks(t *testing.T) {
	t.Parallel()
	for _, crashes := range []int{0, 2} {
		t.Run(fmt.Sprintf("crashes_%d", crashes), func(t *testing.T) {
			dir, upstream, baseSHA, headSHA := setupCIRerunRepo(t)
			const red = `[{"name":"build","state":"FAILURE","bucket":"fail"}]`
			const green = `[{"name":"build","state":"SUCCESS","bucket":"pass"}]`
			var observations []string
			for i := 0; i < crashes; i++ {
				// One settled observation and one pre-repair snapshot per crash.
				observations = append(observations, red, red)
			}
			observations = append(observations, green)
			env, _ := stepstest.FakeCIGHLoggedSequence(t, "OPEN", observations, "MERGEABLE", "")
			calls := 0
			ag := &stepstest.MockAgent{AgentName: "codex", RunFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				calls++
				return nil, fmt.Errorf("crash %d: exit status 1 https://user:private-token@example.com API_KEY=abcdefghijklmnop\n\x1b[31m%s", calls, strings.Repeat("x", 4096))
			}}
			sctx := stepstest.NewTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
			prURL := "https://github.com/test/repo/pull/42"
			sctx.Run.PRURL = &prURL
			sctx.Repo.UpstreamURL = upstream
			sctx.Config.Agent = types.AgentCodex
			sctx.Config.CITimeout = time.Minute
			sctx.Config.CI.ReleaseAfterChecksPass = true
			sctx.Config.AutoFix = config.AutoFix{CI: 2}
			step := (&steps.CIStep{}).SetWaitForNextPoll(func(context.Context, time.Duration) error {
				t.Fatal("scripted observations must settle without waiting")
				return nil
			})
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			executor := pipeline.NewExecutor(sctx.DB, paths.WithRoot(t.TempDir()), sctx.Config, ag,
				[]pipeline.Step{crashTestCIStep{CIStep: step, env: env}}, func(event ipc.Event) {
					if event.Status != nil && (*event.Status == string(types.StepStatusAwaitingApproval) || *event.Status == string(types.StepStatusFixReview)) {
						t.Errorf("green CI must not park: %+v", event)
						cancel()
					}
				})
			if err := executor.Execute(ctx, sctx.Run, sctx.Repo, dir); err != nil {
				t.Fatalf("green CI must complete: %v", err)
			}
			run, err := sctx.DB.GetRun(sctx.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if run.Status != types.RunChecksPassed || run.CIReleasedAt == nil {
				t.Fatalf("green CI must record checks-passed and release: %+v", run)
			}
			results, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
			if err != nil || len(results) != 1 {
				t.Fatalf("want one recorded CI step: results=%+v err=%v", results, err)
			}
			result := results[0]
			if result.StepName != types.StepCI || result.Status != types.StepStatusCompleted {
				t.Fatalf("CI step must complete: %+v", result)
			}
			rawFindings := ""
			if result.FindingsJSON != nil {
				rawFindings = *result.FindingsJSON
			}
			if calls != crashes {
				t.Fatalf("fixer calls = %d, want %d", calls, crashes)
			}
			if crashes == 0 {
				if rawFindings != "" {
					t.Fatalf("no-crash control has findings: %s", rawFindings)
				}
				return
			}
			findings, err := types.ParseFindingsJSON(rawFindings)
			if err != nil || len(findings.Items) != 1 {
				t.Fatalf("want one inspectable crash diagnostic, got %q err=%v", rawFindings, err)
			}
			diagnostic := findings.Items[0]
			if diagnostic.Severity != types.FindingSeverityInfo || diagnostic.Action != types.ActionNoOp {
				t.Fatalf("crash evidence must be informational: %+v", diagnostic)
			}
			if !strings.Contains(diagnostic.Description, "2 CI fix-agent crashes") || !strings.Contains(diagnostic.Description, "crash 2: exit status 1") {
				t.Fatalf("missing count or last cause: %s", diagnostic.Description)
			}
			for _, secret := range []string{"private-token", "abcdefghijklmnop", "\x1b", "\n"} {
				if strings.Contains(diagnostic.Description, secret) {
					t.Errorf("diagnostic contains unsanitized %q", secret)
				}
			}
			if len(diagnostic.Description) > 2048 {
				t.Fatalf("diagnostic is unbounded: %d bytes", len(diagnostic.Description))
			}
		})
	}
}

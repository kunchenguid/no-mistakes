package citest

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps/internal/stepstest"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Drive the real executor decision path and provider subprocess adapter without
// a shared daemon, network, or writable agent. Only the external provider is fake.
type recheckEnvStep struct {
	*steps.CIStep
	env    []string
	legacy bool
}

func (s recheckEnvStep) Execute(ctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	ctx.Env = s.env
	outcome, err := s.CIStep.Execute(ctx)
	if s.legacy && outcome != nil {
		outcome.Findings = strings.ReplaceAll(outcome.Findings, `,"category":"ci-provider-read"`, "")
	}
	return outcome, err
}
func (s recheckEnvStep) ReconcileApprovalGate(ctx *pipeline.StepContext) (bool, error) {
	ctx.Env = s.env
	return s.CIStep.ReconcileApprovalGate(ctx)
}
func (s recheckEnvStep) RecheckApprovalGate(ctx *pipeline.StepContext, findings string) (string, error) {
	ctx.Env = s.env
	return s.CIStep.RecheckApprovalGate(ctx, findings)
}

func TestCIProviderRecheck_RecoveredGreen(t *testing.T) {
	for _, recovered := range []bool{false, true} {
		name := "live"
		if recovered {
			name = "restart_with_legacy_finding"
		}
		t.Run(name, func(t *testing.T) { testCIProviderRecheck(t, recovered) })
	}
}

func testCIProviderRecheck(t *testing.T, recovered bool) {
	dir, base, head := stepstest.SetupGitRepo(t)
	ag := &stepstest.MockAgent{AgentName: "test"}
	sctx := stepstest.NewTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
	// A no-CI declaration must not make an empty explicit recheck green.
	sctx.Config.NoCI = true
	url := "https://github.com/test/repo/pull/42"
	sctx.Run.PRURL = &url
	if err := sctx.DB.UpdateRunPRURL(sctx.Run.ID, url); err != nil {
		t.Fatal(err)
	}
	sequence := make([]string, steps.ConsecutiveCheckErrorLimit())
	for i := range sequence {
		sequence[i] = `not-json`
	}
	green := `[{"name":"build","state":"SUCCESS","bucket":"pass"}]`
	env, logPath := stepstest.FakeCIGHLoggedSequence(t, "OPEN", sequence, "MERGEABLE", "")
	checksPath := ""
	for _, entry := range env {
		if strings.HasPrefix(entry, "FAKE_CLI_CHECKS_PATH=") {
			checksPath = strings.TrimPrefix(entry, "FAKE_CLI_CHECKS_PATH=")
		}
	}
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	parked := make(chan struct{}, 4)
	newExecutor := func() *pipeline.Executor {
		ci := (&steps.CIStep{}).SetBaseBranchTip(func(context.Context) (string, bool) { return base, true }).SetWaitForNextPoll(func(context.Context, time.Duration) error { return nil })
		exec := pipeline.NewExecutor(sctx.DB, p, sctx.Config, ag, []pipeline.Step{recheckEnvStep{ci, env, recovered}}, func(ev ipc.Event) {
			if ev.Status != nil && *ev.Status == string(types.StepStatusAwaitingApproval) {
				parked <- struct{}{}
			}
		})
		exec.SetGateReconcileTimings(time.Hour, 5*time.Second)
		return exec
	}
	waitPark := func() {
		t.Helper()
		select {
		case <-parked:
		case <-time.After(20 * time.Second):
			t.Fatal("CI did not park")
		}
	}
	exec := newExecutor()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- exec.Execute(ctx, sctx.Run, sctx.Repo, dir) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("executor did not stop")
		}
	})
	waitPark()
	rows, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
	if err != nil || len(rows) != 1 || rows[0].FindingsJSON == nil || !strings.Contains(*rows[0].FindingsJSON, "could not be read") {
		t.Fatalf("not the provider-read gate: %v %v", rows, err)
	}
	original := *rows[0].FindingsJSON
	if recovered {
		if strings.Contains(original, "ci-provider-read") {
			t.Fatal("fixture did not retain the legacy finding shape")
		}
		// Model a daemon restart using an isolated durable gate fixture. Never
		// mutate any serving run: this database was created by this test.
		cancel()
		<-done
		if err := sctx.DB.UpdateRunStatus(sctx.Run.ID, types.RunRunning); err != nil {
			t.Fatal(err)
		}
		if err := sctx.DB.ParkStepForApproval(sctx.Run.ID, rows[0].ID, types.StepStatusAwaitingApproval, 0, 0, &original); err != nil {
			t.Fatal(err)
		}
		sctx.Run, err = sctx.DB.GetRun(sctx.Run.ID)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel = context.WithCancel(context.Background())
		defer cancel()
		exec = newExecutor()
		go func() { done <- exec.Resume(ctx, sctx.Run, sctx.Repo, dir) }()
		waitPark()
	}
	before, _ := sctx.DB.GetRun(sctx.Run.ID)
	statsBefore, _ := sctx.DB.StepRoundStats(rows[0].ID)
	for _, tc := range []struct{ name, checks string }{
		{"pending", `[{"name":"build","state":"PENDING","bucket":"pending"}]`},
		{"failed", `[{"name":"build","state":"FAILURE","bucket":"fail"}]`},
		{"empty", `[]`},
		{"unreadable", `not-json`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(checksPath, []byte(tc.checks), 0600); err != nil {
				t.Fatal(err)
			}
			if err := exec.Respond(types.StepCI, types.ActionRecheck, nil); err == nil {
				t.Fatal("unverified recheck accepted")
			}
			current, _ := sctx.DB.GetStepsByRun(sctx.Run.ID)
			run, _ := sctx.DB.GetRun(sctx.Run.ID)
			if current[0].Status != types.StepStatusAwaitingApproval || current[0].FindingsJSON == nil || *current[0].FindingsJSON != original || current[0].OverrideReason != nil || run.AwaitingAgentSince == nil || *run.AwaitingAgentSince != *before.AwaitingAgentSince {
				t.Fatalf("failed recheck altered the parked decision: %+v %+v", current[0], run)
			}
		})
	}
	if err := os.WriteFile(checksPath, []byte(green), 0600); err != nil {
		t.Fatal(err)
	}
	if err := exec.Respond(types.StepCI, types.ActionRecheck, nil); err != nil {
		t.Fatalf("provider-only recovery refused: %v", err)
	}
	select {
	case err := <-done:
		done <- err
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("green recheck did not complete")
	}
	rows, _ = sctx.DB.GetStepsByRun(sctx.Run.ID)
	after, _ := sctx.DB.GetRun(sctx.Run.ID)
	statsAfter, _ := sctx.DB.StepRoundStats(rows[0].ID)
	if rows[0].Status != types.StepStatusCompleted || rows[0].OverrideReason != nil || rows[0].FindingsJSON == nil || after.Status != types.RunCompleted || after.AwaitingAgentSince != nil {
		t.Fatalf("not a clean completion: %+v %+v", rows[0], after)
	}
	receipt, err := types.ParseFindingsJSON(*rows[0].FindingsJSON)
	if err != nil || len(receipt.Items) != 0 || !strings.Contains(receipt.Summary, head) || !strings.Contains(receipt.Summary, "no override or repair") {
		t.Fatalf("missing durable verification receipt: %+v %v", receipt, err)
	}
	if statsBefore != statsAfter {
		t.Fatalf("recheck changed round accounting: %+v -> %+v", statsBefore, statsAfter)
	}
	if len(ag.Calls) != 0 {
		t.Fatalf("recheck launched %d agent calls", len(ag.Calls))
	}
	log, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"run rerun", "pr edit", "git push"} {
		if strings.Contains(string(log), forbidden) {
			t.Fatalf("recheck performed %q: %s", forbidden, log)
		}
	}
	if got := stepstest.GitCmd(t, dir, "rev-parse", "HEAD"); got != head {
		t.Fatalf("head changed: %s", got)
	}
}

func TestCIProviderRecheck_RefusesOtherDecisionsAndChangedHeads(t *testing.T) {
	for _, tc := range []string{"provider head moved", "worktree head moved", "dirty worktree", "provider unavailable", "merge conflict", "unresolved mergeability", "other finding", "mixed findings", "unsupported provider"} {
		t.Run(tc, func(t *testing.T) {
			dir, base, head := stepstest.SetupGitRepo(t)
			ag := &stepstest.MockAgent{AgentName: "test"}
			sctx := stepstest.NewTestContext(t, ag, dir, base, head, config.Commands{})
			url := "https://github.com/test/repo/pull/42"
			sctx.Run.PRURL = &url
			sctx.Env = stepstest.FakeCIGH(t, "OPEN", `[{"name":"build","bucket":"pass","state":"SUCCESS"}]`)
			findings := `{"findings":[{"severity":"warning","description":"read failed","action":"ask-user","category":"ci-provider-read"}]}`
			switch tc {
			case "provider head moved":
				sctx.Env = append(sctx.Env, "FAKE_CLI_PR_HEAD_SHA="+base)
			case "worktree head moved":
				stepstest.GitCmd(t, dir, "commit", "--allow-empty", "-m", "other head")
			case "dirty worktree":
				if err := os.WriteFile(dir+"/unvalidated.txt", []byte("unvalidated"), 0600); err != nil {
					t.Fatal(err)
				}
			case "provider unavailable":
				sctx.Env = append(sctx.Env, "FAKE_CLI_MODE=unavailable")
			case "merge conflict":
				sctx.Env = append(sctx.Env, "FAKE_CLI_MERGEABLE=CONFLICTING")
			case "unresolved mergeability":
				sctx.Env = append(sctx.Env, "FAKE_CLI_MERGEABLE=UNKNOWN")
			case "other finding":
				findings = `{"findings":[{"description":"review bot question","action":"ask-user","category":"ci-review-bot"}]}`
			case "mixed findings":
				findings = strings.Replace(findings, "]}", `,{"description":"needs human decision","action":"ask-user"}]}`, 1)
			case "unsupported provider":
				sctx.Repo.UpstreamURL = "https://bitbucket.org/test/repo"
			}
			if _, err := (&steps.CIStep{}).RecheckApprovalGate(sctx, findings); err == nil {
				t.Fatal("recheck did not fail closed")
			}
			if len(ag.Calls) != 0 {
				t.Fatal("recheck entered repair")
			}
		})
	}
}

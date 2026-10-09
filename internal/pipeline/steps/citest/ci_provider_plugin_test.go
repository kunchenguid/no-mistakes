package citest

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps/internal/stepstest"
	"github.com/kunchenguid/no-mistakes/internal/scm/plugin"
	"github.com/kunchenguid/no-mistakes/internal/scm/plugin/fakeplugin"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const pluginPRURL = "https://git.example.com/team/repo/pulls/7"

// pluginCIContext wires the reference fake provider plugin as provider
// "plugin:ssm" for git.example.com, with one open PR #7 for the run branch.
func pluginCIContext(t *testing.T, state fakeplugin.State) (*pipeline.StepContext, string) {
	t.Helper()
	dir, baseSHA, headSHA := stepstest.SetupGitRepo(t)
	if len(state.PRs) == 0 {
		state.PRs = []fakeplugin.PR{{Number: "7", URL: pluginPRURL, HeadBranch: "feature", BaseBranch: "main", HeadSHA: headSHA, Title: "t", Body: "b", State: "open"}}
	}
	for i := range state.PRs {
		if state.PRs[i].HeadSHA == "" {
			state.PRs[i].HeadSHA = headSHA
		}
	}
	binDir := stepstest.FakeCLIBinDir(t)
	stepstest.LinkFakeCLI(t, binDir, fakeplugin.ExecutableName)
	statePath := filepath.Join(t.TempDir(), "plugin-state.json")
	if err := fakeplugin.Save(statePath, state); err != nil {
		t.Fatal(err)
	}

	sctx := stepstest.NewTestContext(t, &stepstest.MockAgent{AgentName: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = stepstest.FakeCLIEnv(binDir, map[string]string{fakeplugin.EnvState: statePath})
	sctx.Repo.UpstreamURL = "https://git.example.com/team/repo.git"
	prURL := pluginPRURL
	sctx.Run.PRURL = &prURL
	sctx.Config.CITimeout = 5 * time.Second
	sctx.Config.ProviderPlugins = config.ProviderPlugins{
		"ssm": {Command: fakeplugin.ExecutableName, Hosts: []string{"git.example.com"}, Timeout: 30 * time.Second},
	}
	return sctx, statePath
}

func TestCIStep_ProviderPluginMergedPRExitsEarly(t *testing.T) {
	t.Parallel()
	sctx, _ := pluginCIContext(t, fakeplugin.State{PRs: []fakeplugin.PR{{Number: "7", URL: pluginPRURL, HeadBranch: "feature", BaseBranch: "main", State: "merged"}}})
	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	step := (&steps.CIStep{}).SetWaitForNextPoll(failOnExtraPoll)
	pinCIMonitorClock(step)
	outcome, err := stepstest.ExecuteWithAutoFix(t, step, sctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatalf("merged PR needs approval: %+v", outcome)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "merged") {
		t.Fatalf("expected a merged log, got: %v", logs)
	}
}

func TestCIStep_ProviderPluginPassingChecksKeepMonitoring(t *testing.T) {
	t.Parallel()
	sctx, _ := pluginCIContext(t, fakeplugin.State{Checks: []fakeplugin.Check{{Name: "build", Bucket: "pass"}, {Name: "lint", Bucket: "skipping"}}})
	stepstest.MonitorUntilMerged(sctx)
	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	step := (&steps.CIStep{}).SetWaitForNextPoll(func(ctx context.Context, _ time.Duration) error {
		cancel()
		return ctx.Err()
	})
	pinCIMonitorClock(step)
	_, err := stepstest.ExecuteWithAutoFix(t, step, sctx, 0)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected passing plugin CI to keep monitoring, got %v", err)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "all CI checks passed - still monitoring until merged or closed") {
		t.Fatalf("expected passing CI log, got: %v", logs)
	}
}

func TestCIStep_ProviderPluginMergesAfterChecksPass(t *testing.T) {
	t.Parallel()
	sctx, statePath := pluginCIContext(t, fakeplugin.State{
		Checks:              []fakeplugin.Check{{Name: "build", Bucket: "pass"}},
		MergeWhenChecksPass: true,
	})
	polls := 0
	step := (&steps.CIStep{}).SetWaitForNextPoll(func(ctx context.Context, _ time.Duration) error {
		polls++
		if polls > 3 {
			return errors.New("CI monitor never observed the merge")
		}
		return nil
	})
	pinCIMonitorClock(step)
	outcome, err := stepstest.ExecuteWithAutoFix(t, step, sctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatalf("merged PR needs approval: %+v", outcome)
	}
	state, err := fakeplugin.Load(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if state.PRs[0].State != "merged" {
		t.Fatalf("fake forge PR state = %q", state.PRs[0].State)
	}
}

func TestCIStep_ProviderPluginFailingCheckNeedsApproval(t *testing.T) {
	t.Parallel()
	sctx, _ := pluginCIContext(t, fakeplugin.State{
		Checks:     []fakeplugin.Check{{Name: "build", Bucket: "pass"}, {Name: "unit-tests", Bucket: "fail", Link: "https://ci.example.com/run/1"}},
		FailedLogs: "FAIL TestSomething",
	})
	sctx.Config.AutoFix = config.AutoFix{CI: 0}

	step := (&steps.CIStep{}).SetWaitForNextPoll(failOnExtraPoll)
	pinCIMonitorClock(step)
	outcome, err := stepstest.ExecuteWithAutoFix(t, step, sctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("expected a failing plugin check to require approval when auto-fix is disabled")
	}
	var findings types.Findings
	if err := json.Unmarshal([]byte(outcome.Findings), &findings); err != nil {
		t.Fatalf("unmarshal findings: %v", err)
	}
	if len(findings.Items) == 0 || !strings.Contains(findings.Items[0].Description, "unit-tests") {
		t.Fatalf("expected a failing unit-tests finding, got %+v", findings.Items)
	}
}

// A fork URL and an unauthenticated plugin skip the CI step with a reason,
// exactly like a built-in provider; the executor persists it and axi reports
// it under run.automatic_skips. A broken status handshake fails the step, and
// so does a violation in a later call: polling a malformed answer again could
// only spin to ci_timeout.
func TestCIStep_ProviderPluginSkipsWithReasonAndFailsClosedOnProtocolViolation(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name     string
		state    fakeplugin.State
		forkURL  string
		wantSkip string
		wantErr  string
		timeout  time.Duration
	}{
		{name: "fork routing", forkURL: "https://git.example.com/me/repo", wantSkip: `fork PR routing for provider plugin "ssm" is not implemented`},
		{name: "unauthenticated", state: fakeplugin.State{Unauthenticated: true}, wantSkip: "not authenticated for git.example.com"},
		{name: "protocol violation", state: fakeplugin.State{ProtocolVersion: 2}, wantErr: "speaks protocol version 2"},
		{name: "malformed checks after the handshake", state: fakeplugin.State{Checks: []fakeplugin.Check{{Name: "build", Bucket: "green"}}}, wantErr: `unknown bucket "green"`},
		{
			name:    "PR url drift after the handshake",
			state:   fakeplugin.State{PRs: []fakeplugin.PR{{Number: "7", URL: "https://git.example.com/team/repo/pull/7", HeadBranch: "feature", BaseBranch: "main", Title: "t", Body: "b", State: "open"}}},
			wantErr: "returned PR url",
		},
		// The live-base read is one-shot, so unlike a poll even a timeout
		// fails: the configured base may no longer be the PR's target.
		{name: "live base read times out", state: fakeplugin.State{HangOn: "pr view"}, timeout: time.Second, wantErr: "timed out after 1s"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			sctx, _ := pluginCIContext(t, tt.state)
			sctx.Repo.ForkURL = tt.forkURL
			if tt.timeout > 0 {
				cfg := sctx.Config.ProviderPlugins["ssm"]
				cfg.Timeout = tt.timeout
				sctx.Config.ProviderPlugins["ssm"] = cfg
			}
			step := (&steps.CIStep{}).SetWaitForNextPoll(failOnExtraPoll)
			pinCIMonitorClock(step)
			outcome, err := step.Execute(sctx)
			if tt.wantErr != "" {
				if err == nil || !errors.Is(err, plugin.ErrProtocol) || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("CI step = %+v, %v; want protocol error %q", outcome, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !outcome.Skipped || !strings.Contains(outcome.SkipReason, tt.wantSkip) {
				t.Fatalf("outcome = %+v, want skip containing %q", outcome, tt.wantSkip)
			}
		})
	}
}

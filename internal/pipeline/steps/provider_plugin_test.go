package steps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/scm/plugin"
	"github.com/kunchenguid/no-mistakes/internal/scm/plugin/fakeplugin"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// fakeProviderPlugin puts the reference fake plugin on the step's PATH and
// configures it as provider plugin "ssm" for git.example.com.
func fakeProviderPlugin(t *testing.T, sctx *pipeline.StepContext, state fakeplugin.State) (statePath, logPath string) {
	t.Helper()
	binDir := fakeCLIBinDir(t)
	linkTestBinary(t, binDir, fakeplugin.ExecutableName)
	tmp := t.TempDir()
	statePath = filepath.Join(tmp, "plugin-state.json")
	logPath = filepath.Join(tmp, "plugin-requests.ndjson")
	if err := fakeplugin.Save(statePath, state); err != nil {
		t.Fatal(err)
	}
	sctx.Env = fakeCLIEnv(binDir, map[string]string{
		fakeplugin.EnvState: statePath,
		fakeplugin.EnvLog:   logPath,
	})
	sctx.Repo.UpstreamURL = "https://git.example.com/team/repo"
	sctx.Config.ProviderPlugins = config.ProviderPlugins{
		"ssm": {
			Command:           fakeplugin.ExecutableName,
			Hosts:             []string{"git.example.com"},
			Timeout:           30 * time.Second,
			DraftPullRequests: true,
		},
	}
	return statePath, logPath
}

func pluginOperations(t *testing.T, logPath string) []string {
	t.Helper()
	calls, err := fakeplugin.ReadLog(logPath)
	if err != nil {
		t.Fatal(err)
	}
	ops := make([]string, 0, len(calls))
	for _, call := range calls {
		ops = append(ops, call.Command)
	}
	return ops
}

func completeCleanReview(t *testing.T, sctx *pipeline.StepContext) {
	t.Helper()
	reviewStep, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.UpdateStepStatus(reviewStep.ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.SetStepFindings(reviewStep.ID, `{"findings":[],"summary":"clean","risk_level":"low","risk_rationale":"docs only"}`); err != nil {
		t.Fatal(err)
	}
}

func TestProviderPlugin_ClaimsHostBeforeBuiltinDetection(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.ProviderPlugins = config.ProviderPlugins{"ssm": {Command: "nm-ssm", Hosts: []string{"github.com"}}}

	if got := resolvedProvider(sctx); got != scm.PluginProvider("ssm") {
		t.Fatalf("resolvedProvider = %q, want plugin:ssm", got)
	}
	sctx.Config.ProviderPlugins = config.ProviderPlugins{"ssm": {Command: "nm-ssm", Hosts: []string{"*.sourcemanager.dev"}}}
	if got := resolvedProvider(sctx); got != scm.ProviderGitHub {
		t.Fatalf("resolvedProvider for an unclaimed host = %q, want github", got)
	}
}

func TestProviderPlugin_PRStepCreatesPRThroughPlugin(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	statePath, logPath := fakeProviderPlugin(t, sctx, fakeplugin.State{})
	completeCleanReview(t, sctx)

	outcome, err := (&PRStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("PR step: %v", err)
	}
	if outcome.Skipped {
		t.Fatalf("PR step skipped: %s", outcome.SkipReason)
	}

	state, err := fakeplugin.Load(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.PRs) != 1 {
		t.Fatalf("plugin PRs = %+v", state.PRs)
	}
	pr := state.PRs[0]
	if pr.HeadBranch != "feature" || pr.BaseBranch != "main" || !pr.Draft || pr.Title == "" || !strings.Contains(pr.Body, "feature.txt") {
		t.Fatalf("created PR = %+v", pr)
	}
	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.PRURL == nil || *run.PRURL != "https://git.example.com/team/repo/pulls/1" {
		t.Fatalf("persisted PR URL = %v", run.PRURL)
	}
	ops := strings.Join(pluginOperations(t, logPath), ",")
	if !strings.HasPrefix(ops, "status,pr find,pr create") {
		t.Fatalf("plugin operations = %s", ops)
	}
	t.Logf("plugin operations: %s\npersisted PR URL: %s", ops, *run.PRURL)
}

func TestProviderPlugin_PRStepUpdatesExistingPR(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	statePath, logPath := fakeProviderPlugin(t, sctx, fakeplugin.State{
		NextNumber: 8,
		PRs: []fakeplugin.PR{{
			Number: "7", URL: "https://git.example.com/team/repo/pulls/7",
			HeadBranch: "feature", BaseBranch: "main", Title: "existing title", Body: "old body", State: "open",
		}},
	})
	completeCleanReview(t, sctx)

	outcome, err := (&PRStep{}).Execute(sctx)
	if err != nil {
		t.Fatalf("PR step: %v", err)
	}
	if outcome.Skipped {
		t.Fatalf("PR step skipped: %s", outcome.SkipReason)
	}
	state, err := fakeplugin.Load(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.PRs) != 1 || state.PRs[0].Body == "old body" {
		t.Fatalf("plugin PRs = %+v", state.PRs)
	}
	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.PRURL == nil || *run.PRURL != "https://git.example.com/team/repo/pulls/7" {
		t.Fatalf("persisted PR URL = %v", run.PRURL)
	}
	ops := pluginOperations(t, logPath)
	for _, op := range ops {
		if op == "pr create" {
			t.Fatalf("existing PR was duplicated: %v", ops)
		}
	}
}

// A plugin that says it cannot serve the repository, a missing command, and
// a fork URL (which no non-GitHub provider routes) skip exactly like a
// built-in provider: Skipped with a SkipReason, which the executor persists
// and axi reports under run.automatic_skips as passed-with-skips.
func TestProviderPlugin_PRStepSkipsWithPluginReason(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		state   fakeplugin.State
		mutate  func(*pipeline.StepContext)
		wantMsg string
	}{
		{name: "unauthenticated", state: fakeplugin.State{Unauthenticated: true}, wantMsg: "not authenticated for git.example.com"},
		{
			name:    "fork routing",
			mutate:  func(sctx *pipeline.StepContext) { sctx.Repo.ForkURL = "https://git.example.com/me/repo" },
			wantMsg: `fork PR routing for provider plugin "ssm" is not implemented`,
		},
		{
			name: "missing command",
			mutate: func(sctx *pipeline.StepContext) {
				cfg := sctx.Config.ProviderPlugins["ssm"]
				cfg.Command = "nm-provider-plugin-that-does-not-exist"
				sctx.Config.ProviderPlugins["ssm"] = cfg
			},
			wantMsg: `command "nm-provider-plugin-that-does-not-exist" not found`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir, baseSHA, headSHA := setupGitRepo(t)
			sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
			statePath, _ := fakeProviderPlugin(t, sctx, tt.state)
			if tt.mutate != nil {
				tt.mutate(sctx)
			}
			outcome, err := (&PRStep{}).Execute(sctx)
			if err != nil {
				t.Fatalf("PR step: %v", err)
			}
			if !outcome.Skipped || !strings.Contains(outcome.SkipReason, tt.wantMsg) {
				t.Fatalf("outcome = %+v, want skip containing %q", outcome, tt.wantMsg)
			}
			state, err := fakeplugin.Load(statePath)
			if err != nil {
				t.Fatal(err)
			}
			if len(state.PRs) != 0 {
				t.Fatalf("skipped step created PRs: %+v", state.PRs)
			}
		})
	}
}

// A plugin whose status handshake breaks the contract fails the PR step and
// the pre-push attestation instead of skipping: a skip would let the run read
// as passed-with-skips off an answer nothing could validate.
func TestProviderPlugin_ProtocolViolationFailsClosed(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	statePath, _ := fakeProviderPlugin(t, sctx, fakeplugin.State{ProtocolVersion: 99})
	completeCleanReview(t, sctx)

	outcome, err := (&PRStep{}).Execute(sctx)
	if err == nil || !errors.Is(err, plugin.ErrProtocol) || !strings.Contains(err.Error(), "speaks protocol version 99") {
		t.Fatalf("PR step = %+v, %v; want a protocol-violation error", outcome, err)
	}
	if err := attestHeadBeforePush(sctx, headSHA, nil); err == nil || !errors.Is(err, errAttestationWriteFailed) || !errors.Is(err, plugin.ErrProtocol) {
		t.Fatalf("attestHeadBeforePush = %v; want a failed attestation write", err)
	}
	state, err := fakeplugin.Load(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.PRs) != 0 {
		t.Fatalf("failed step created PRs: %+v", state.PRs)
	}
}

func TestProviderPlugin_PRStepClampsBodyToDeclaredLimit(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	statePath, _ := fakeProviderPlugin(t, sctx, fakeplugin.State{MaxPRBodyChars: 600})
	completeCleanReview(t, sctx)

	if _, err := (&PRStep{}).Execute(sctx); err != nil {
		t.Fatalf("PR step: %v", err)
	}
	state, err := fakeplugin.Load(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.PRs) != 1 {
		t.Fatalf("plugin PRs = %+v", state.PRs)
	}
	if got := scm.PRBodyLen(state.PRs[0].Body); got > 600 {
		t.Fatalf("PR body is %d characters, over the declared limit 600", got)
	}
}

// Every PR URL a plugin returns is checked against the repository path, so a
// remote that has none can never be served and skips with a reason instead.
func TestProviderPlugin_RemoteWithoutRepositoryPathSkips(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	sctx.Config.ProviderPlugins = config.ProviderPlugins{"ssm": {Command: "nm-ssm", Hosts: []string{"git.example.com"}}}
	sctx.Repo.UpstreamURL = "https://git.example.com/"
	host, reason := buildHost(sctx, scm.PluginProvider("ssm"))
	if host != nil || !strings.Contains(reason, "could not resolve the repository path") {
		t.Fatalf("buildHost = %v, %q", host, reason)
	}
}

// The CI monitor fails on a deterministic contract violation but retries a
// timed-out read like any failed read; nothing else fails a poll.
func TestProviderPlugin_PollFailsStepOnlyOnDeterministicViolations(t *testing.T) {
	t.Parallel()
	violation := fmt.Errorf("pr checks: %w", plugin.ErrProtocol)
	timeout := fmt.Errorf("pr checks: %w: %w", plugin.ErrProtocol, plugin.ErrTimeout)
	if !pluginPollFailsStep(violation) {
		t.Fatal("a malformed answer must fail the poll")
	}
	if pluginPollFailsStep(timeout) || !pluginContractBroken(timeout) {
		t.Fatal("a timeout is a violation but must be retried by the poll")
	}
	if pluginPollFailsStep(errors.New("exit status 1")) {
		t.Fatal("an ordinary failed read must be retried")
	}
}

// A plugin that breaks its contract while a repair fetches failed-check logs
// would break it identically on every retry, so the repair fails the step
// instead of being treated as an ordinary, retryable fix failure.
func TestProviderPlugin_RepairFailsOnLogProtocolViolation(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{name: "test", runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		t.Error("the fix agent ran after a plugin contract violation")
		return &agent.Result{Output: json.RawMessage(`{"summary":"x","code_change_needed":false}`)}, nil
	}}
	sctx := newTestContext(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.PreviousFindings = `{"findings":[{"id":"ci-1","severity":"error","description":"failed","action":"auto-fix","category":"ci-check","check":"test"}]}`
	host := &pluginLogViolationHost{
		completionSnapshotHost: completionSnapshotHost{checks: []scm.Check{{Name: "test", Bucket: scm.CheckBucketFail}}},
		err:                    fmt.Errorf("pr check-logs: %w", plugin.ErrProtocol),
	}
	outcome, err := (&CIStep{}).repairFromFindings(sctx, host, &scm.PR{Number: "42"})
	if outcome != nil || !errors.Is(err, plugin.ErrProtocol) {
		t.Fatalf("repair = %#v, %v; want the plugin protocol error", outcome, err)
	}
}

// A plugin whose status handshake breaks while a CI repair attests the
// repaired head fails the step. The attestation failure is wrapped as an
// unsettled repair push, which parks for approval; the plugin violation must
// win, or a broken integration would read as a repair awaiting approval.
func TestProviderPlugin_RepairFailsOnAttestationHandshakeViolation(t *testing.T) {
	t.Parallel()
	f := newCIRepairFixture(t, false, writeCIFix)
	fakeProviderPlugin(t, f.sctx, fakeplugin.State{ProtocolVersion: 99})
	f.sctx.PreviousFindings = `{"findings":[{"id":"ci-1","severity":"error","description":"failed","action":"auto-fix","category":"ci-check","check":"test"}]}`
	f.sctx.Ctx = context.Background()
	host := &completionSnapshotHost{checks: []scm.Check{{Name: "test", Bucket: scm.CheckBucketFail}}}

	outcome, err := (&CIStep{}).repairFromFindings(f.sctx, host, &scm.PR{Number: "42"})
	if outcome != nil || !errors.Is(err, plugin.ErrProtocol) || !errors.Is(err, errCIAttestationUnsettled) {
		t.Fatalf("repair = %#v, %v; want the plugin protocol error, not a park\nlog:\n%s", outcome, err, f.log())
	}
	if f.remoteHead(t) != f.headSHA {
		t.Fatal("the repair was pushed although its attestation failed")
	}
}

type pluginLogViolationHost struct {
	completionSnapshotHost
	err error
}

func (h *pluginLogViolationHost) Capabilities() scm.Capabilities {
	return scm.Capabilities{FailedCheckLogs: true}
}

func (h *pluginLogViolationHost) FetchFailedCheckLogs(context.Context, *scm.PR, string, string, []string) (string, error) {
	return "", h.err
}

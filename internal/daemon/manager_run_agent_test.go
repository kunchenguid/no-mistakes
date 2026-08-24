package daemon

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestRunAgentOverrideIsAppliedAfterRepositoryTrustFiltering(t *testing.T) {
	global := config.DefaultGlobalConfig()
	global.Agent = types.AgentClaude
	trusted, err := config.LoadRepoFromBytes([]byte("agent: claude\n"))
	if err != nil {
		t.Fatal(err)
	}
	untrusted, err := config.LoadRepoFromBytes([]byte("agent: antigravity\nagent_config:\n  antigravity:\n    model: attacker-model\n"))
	if err != nil {
		t.Fatal(err)
	}
	effective := config.EffectiveRepoConfig(untrusted, trusted, false)
	cfg := config.Merge(global, effective)
	if err := applyRunAgentOverride(cfg, types.AgentCodex, "gpt-5.6-codex"); err != nil {
		t.Fatal(err)
	}
	if cfg.Agent != types.AgentCodex || len(cfg.Agents) != 1 || cfg.Agents[0] != types.AgentCodex {
		t.Fatalf("resolved agents = %q/%v, want exact codex", cfg.Agent, cfg.Agents)
	}
	if got := cfg.AgentProfileFor(types.AgentCodex).Model; got != "gpt-5.6-codex" {
		t.Fatalf("resolved model = %q, want gpt-5.6-codex", got)
	}
	if got := cfg.AgentProfileFor(types.AgentAntigravity); !got.IsZero() {
		t.Fatalf("untrusted repository model escaped trust filtering: %#v", got)
	}
}

func TestRunAgentOverrideRefusesRawModelConflict(t *testing.T) {
	cfg := &config.Config{AgentArgsOverride: map[string][]string{"codex": {"-m", "global-model"}}}
	err := applyRunAgentOverride(cfg, types.AgentCodex, "per-run-model")
	if err == nil || !strings.Contains(err.Error(), "already pins a model") {
		t.Fatalf("conflicting model error = %v", err)
	}
}

func TestPushReceivedPerRunSelectionsStayIsolatedAcrossBranches(t *testing.T) {
	started := make(chan string, 2)
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{&notifyBlockStep{name: types.StepReview, started: started}}
	})
	mockCodex, _ := writeCapturingCodex(t)
	appendAgentPathOverride(t, p.ConfigFile(), types.AgentCodex, mockCodex)
	_, headSHA := setupTestGitRepo(t, p, d, "selection-isolation-repo")

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	runs := make(map[string]string)
	for _, tc := range []struct{ branch, model string }{{"feature/one", "model-one"}, {"feature/two", "model-two"}} {
		var result ipc.PushReceivedResult
		if err := client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
			Gate: p.RepoDir("selection-isolation-repo"), Ref: "refs/heads/" + tc.branch,
			Old: strings.Repeat("0", 40), New: headSHA, Agent: types.AgentCodex, Model: tc.model,
		}, &result); err != nil {
			t.Fatal(err)
		}
		runs[tc.branch] = result.RunID
		waitForStartedBranch(t, started, tc.branch)
	}
	for branch, runID := range runs {
		run, err := d.GetRun(runID)
		if err != nil {
			t.Fatal(err)
		}
		name, profile, ok := run.RunAgentSelection()
		if !ok || name != types.AgentCodex || profile.Model != "model-"+strings.TrimPrefix(branch, "feature/") {
			t.Fatalf("%s selection = %q/%#v/%v", branch, name, profile, ok)
		}
	}
}

type invokeAgentStep struct{ name types.StepName }

func (s *invokeAgentStep) Name() types.StepName { return s.name }
func (s *invokeAgentStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	_, err := sctx.Agent.Run(sctx.Ctx, agent.RunOpts{Prompt: "validate selection", CWD: sctx.WorkDir})
	return &pipeline.StepOutcome{}, err
}

func TestPushReceivedRunsSelectedNativeCodexModel(t *testing.T) {
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{&invokeAgentStep{name: types.StepReview}}
	})
	mockCodex, capture := writeCapturingCodex(t)
	t.Setenv("NM_TEST_CODEX_ARGS", capture)
	appendAgentPathOverride(t, p.ConfigFile(), types.AgentCodex, mockCodex)
	_, headSHA := setupTestGitRepo(t, p, d, "native-codex-selection-repo")

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var result ipc.PushReceivedResult
	if err := client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir("native-codex-selection-repo"), Ref: "refs/heads/main",
		Old: strings.Repeat("0", 40), New: headSHA, Agent: types.AgentCodex, Model: "gpt-5.6-codex",
	}, &result); err != nil {
		t.Fatal(err)
	}
	run := waitForRunTerminalState(t, d, result.RunID)
	if run.Status != types.RunCompleted {
		t.Fatalf("run status = %s, error = %v", run.Status, run.Error)
	}
	args, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(args), "-m\ngpt-5.6-codex\n") {
		t.Fatalf("codex argv did not receive selected model:\n%s", args)
	}
	name, profile, ok := run.RunAgentSelection()
	if !ok || name != types.AgentCodex || profile.Model != "gpt-5.6-codex" {
		t.Fatalf("persisted selection = %q/%#v/%v", name, profile, ok)
	}
}

func TestRerunDoesNotInheritPerRunAgentSelection(t *testing.T) {
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{&mockPassStep{name: types.StepReview}}
	})
	mockCodex, _ := writeCapturingCodex(t)
	appendAgentPathOverride(t, p.ConfigFile(), types.AgentCodex, mockCodex)
	_, headSHA := setupTestGitRepo(t, p, d, "selection-rerun-repo")
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var first ipc.PushReceivedResult
	if err := client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir("selection-rerun-repo"), Ref: "refs/heads/main", Old: strings.Repeat("0", 40), New: headSHA,
		Agent: types.AgentCodex, Model: "one-run-model",
	}, &first); err != nil {
		t.Fatal(err)
	}
	waitForRunTerminalState(t, d, first.RunID)

	var second ipc.RerunResult
	if err := client.Call(ipc.MethodRerun, &ipc.RerunParams{RepoID: "selection-rerun-repo", Branch: "main"}, &second); err != nil {
		t.Fatal(err)
	}
	rerun := waitForRunTerminalState(t, d, second.RunID)
	if name, profile, ok := rerun.RunAgentSelection(); ok {
		t.Fatalf("rerun inherited prior selection %q/%#v", name, profile)
	}
}

func TestPersistedRunAgentSelectionOverridesLaterGlobalConfig(t *testing.T) {
	cfg := &config.Config{
		Agent:       types.AgentClaude,
		Agents:      []types.AgentName{types.AgentClaude},
		AgentConfig: map[string]agentcfg.Profile{"codex": {Model: "later-model"}},
	}
	persisted := agentcfg.Profile{Model: "bound-model", Effort: agentcfg.EffortHigh}
	if err := applyPersistedRunAgentSelection(cfg, types.AgentCodex, persisted); err != nil {
		t.Fatal(err)
	}
	if cfg.Agent != types.AgentCodex || cfg.AgentProfileFor(types.AgentCodex) != persisted {
		t.Fatalf("recovered selection = %q/%#v, want codex/%#v", cfg.Agent, cfg.AgentProfileFor(types.AgentCodex), persisted)
	}
}

func appendAgentPathOverride(t *testing.T, configPath string, name types.AgentName, path string) {
	t.Helper()
	content, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	content = append(content, []byte(fmt.Sprintf("  %s: %s\n", name, path))...)
	if err := os.WriteFile(configPath, content, 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeCapturingCodex(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "codex")
	capture := filepath.Join(dir, "args.log")
	script := `#!/bin/sh
if [ -n "$NM_TEST_CODEX_ARGS" ]; then
  printf '%s\n' "$@" > "$NM_TEST_CODEX_ARGS"
fi
printf '%s\n' '{"type":"thread.started","thread_id":"test-thread"}'
printf '%s\n' '{"type":"item.completed","item":{"type":"agent_message","text":"done"}}'
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path, capture
}

func TestUnavailablePerRunAgentFailsBeforeExecution(t *testing.T) {
	cfg := &config.Config{
		AgentPathOverride: map[string]string{"codex": filepath.Join(t.TempDir(), "missing-codex")},
	}
	if err := applyRunAgentOverride(cfg, types.AgentCodex, "gpt-5.6-codex"); err != nil {
		t.Fatal(err)
	}
	if _, err := newPipelineAgent(context.Background(), cfg, t.TempDir(), exec.LookPath); err == nil || !strings.Contains(err.Error(), "no runnable agent found") {
		t.Fatalf("unavailable selected agent error = %v", err)
	}
}

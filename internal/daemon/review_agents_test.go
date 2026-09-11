package daemon

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/runenv"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestPipelineReviewRolesUseIndependentPiProfiles(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "pi")
	const response = `{"type":"agent_end","messages":[{"role":"assistant","content":[{"type":"text","text":"ok"}]}]}`
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" > pi-argv.txt\ncat >/dev/null\nprintf '%s\\n' '" + response + "'\n"
	if runtime.GOOS == "windows" {
		bin += ".cmd"
		script = "@echo off\r\necho %* > pi-argv.txt\r\nmore > nul\r\necho " + response + "\r\n"
	}
	if err := os.WriteFile(bin, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	global, err := config.LoadGlobalFromBytes([]byte(`agent: pi
agent_config:
  pi: {model: default-model, effort: high}
review_agents:
  reviewer: {agent: pi, model: anthropic-vertex/claude-opus-4-8, effort: max}
  fixer: {agent: pi, model: google-vertex/gemini-3.8-flash, effort: max}
`))
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.Merge(global, &config.RepoConfig{})
	cfg.AgentPathOverride = map[string]string{"pi": bin}
	cfg.DisableProjectSettings = true
	ag, err := newPipelineAgent(context.Background(), cfg, t.TempDir(), fakeLookPath, runenv.Overlay{})
	if err != nil {
		t.Fatal(err)
	}
	defer ag.Close()
	for _, tc := range []struct{ purpose, model, effort string }{
		{"review", "anthropic-vertex/claude-opus-4-8", "max"},
		{"review-fix", "google-vertex/gemini-3.8-flash", "max"},
		{"review-correction", "anthropic-vertex/claude-opus-4-8", "max"},
		{"test-evidence", "default-model", "high"},
	} {
		_, err := ag.Run(context.Background(), agent.RunOpts{Purpose: tc.purpose, Prompt: "hello", CWD: dir})
		if err != nil {
			t.Fatal(err)
		}
		args, err := os.ReadFile(filepath.Join(dir, "pi-argv.txt"))
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"--model " + tc.model, "--thinking " + tc.effort, "--no-context-files"} {
			if !strings.Contains(string(args), want) {
				t.Fatalf("%s args %q missing %q", tc.purpose, args, want)
			}
		}
	}
}

func TestPipelineReviewRoleFailsClosed(t *testing.T) {
	cfg := &config.Config{Agent: types.AgentPi, DisableProjectSettings: true,
		ReviewAgents: map[string]config.ReviewAgent{"reviewer": {Agent: types.AgentAntigravity}}}
	_, err := newPipelineAgent(context.Background(), cfg, t.TempDir(), fakeLookPath, runenv.Overlay{})
	if err == nil || !strings.Contains(err.Error(), "review_agents.reviewer") || !strings.Contains(err.Error(), "does not neutralize") {
		t.Fatalf("unsafe reviewer error = %v", err)
	}
}

// reviewRoleProbeStep drives one review turn and one review-fix turn through the
// pipeline agent the executor was given, so a test can observe which harness
// profile each role resolved to. Sessions are deliberately not used: pi requires
// a session-id header when resuming, which the fake binary does not emit.
type reviewRoleProbeStep struct{}

func (s *reviewRoleProbeStep) Name() types.StepName { return types.StepReview }

func (s *reviewRoleProbeStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	if _, err := sctx.RunAgentContext(sctx.Ctx, agent.RunOpts{Purpose: "review", Prompt: "review", CWD: sctx.WorkDir}); err != nil {
		return nil, err
	}
	if _, err := sctx.RunAgentContext(sctx.Ctx, agent.RunOpts{Purpose: "review-fix", Prompt: "fix", CWD: sctx.WorkDir}); err != nil {
		return nil, err
	}
	return &pipeline.StepOutcome{}, nil
}

// writeCapturingPiAgent writes a fake pi binary that appends its argv to
// capturePath on every invocation and returns a minimal agent_end payload.
func writeCapturingPiAgent(t *testing.T, dir, capturePath string) string {
	t.Helper()
	const response = `{"type":"agent_end","messages":[{"role":"assistant","content":[{"type":"text","text":"ok"}]}]}`
	bin := filepath.Join(dir, "pi")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + shellQuoteForTest(capturePath) + "\ncat >/dev/null\nprintf '%s\\n' '" + response + "'\n"
	if runtime.GOOS == "windows" {
		bin += ".cmd"
		script = "@echo off\r\necho %* >> \"" + capturePath + "\"\r\nmore > nul\r\necho " + response + "\r\n"
	}
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// TestPushReceivedRoutesReviewRolesToIndependentProfiles is the behavioral
// regression for review-role routing on the NORMAL push start path
// (HandlePushReceived -> startRun -> startRunWithIntentSourceLocked), which used
// to build the pipeline agent inline and never apply WithReviewAgents. Both role
// models can only appear in the captured argv if that wrapper is applied on this
// path; before the fix both review-loop turns ran on the default profile.
func TestPushReceivedRoutesReviewRolesToIndependentProfiles(t *testing.T) {
	capturePath := filepath.Join(t.TempDir(), "pi-argv.log")
	piBin := writeCapturingPiAgent(t, t.TempDir(), capturePath)

	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step {
		return []pipeline.Step{&reviewRoleProbeStep{}}
	})

	configYAML := "agent: pi\n" +
		"agent_path_override:\n  pi: " + piBin + "\n" +
		"agent_config:\n  pi: {model: default-model, effort: high}\n" +
		"review_agents:\n" +
		"  reviewer: {agent: pi, model: anthropic-vertex/claude-opus-4-8, effort: max}\n" +
		"  fixer: {agent: pi, model: google-vertex/gemini-3.8-flash, effort: max}\n"
	if err := os.WriteFile(p.ConfigFile(), []byte(configYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	_, headSHA := setupTestGitRepo(t, p, d, "review-roles-run-repo")

	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	var result ipc.PushReceivedResult
	if err := client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
		Gate: p.RepoDir("review-roles-run-repo"),
		Ref:  "refs/heads/main",
		Old:  "0000000000000000000000000000000000000000",
		New:  headSHA,
	}, &result); err != nil {
		t.Fatal(err)
	}

	run := waitForRunTerminalState(t, d, result.RunID)
	if run.Status != types.RunCompleted {
		var runErr string
		if run.Error != nil {
			runErr = *run.Error
		}
		t.Fatalf("run status = %q, want %q (error: %s)", run.Status, types.RunCompleted, runErr)
	}

	argv, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatal(err)
	}
	got := string(argv)
	for role, want := range map[string]string{
		"reviewer": "--model anthropic-vertex/claude-opus-4-8",
		"fixer":    "--model google-vertex/gemini-3.8-flash",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("%s profile not routed on push start path; argv = %q missing %q", role, got, want)
		}
	}
}

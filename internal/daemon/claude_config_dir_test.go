package daemon

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/forgecontext"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/runenv"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

type agentRunStep struct{}

func (agentRunStep) Name() types.StepName { return types.StepReview }
func (agentRunStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	if _, err := sctx.Agent.Run(sctx.Ctx, agent.RunOpts{Prompt: "review", CWD: sctx.WorkDir}); err != nil {
		return nil, err
	}
	return &pipeline.StepOutcome{}, nil
}

func writeClaudeConfigDirRecorder(t *testing.T, dir, capture string) string {
	t.Helper()
	path := filepath.Join(dir, "claude")
	script := "#!/bin/sh\nprintf '%s' \"$CLAUDE_CONFIG_DIR\" > '" + capture + "'\n" +
		`printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"result":"ok"}'` + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPushReceivedRunsClaudeUnderTheCallersConfigDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell mock agent")
	}
	const daemonProfile = "/daemon/.claude"
	t.Setenv(runenv.ClaudeConfigDirEnvVar, daemonProfile)
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step { return []pipeline.Step{agentRunStep{}} })
	capture := filepath.Join(t.TempDir(), "claude-config-dir")
	recorder := writeClaudeConfigDirRecorder(t, t.TempDir(), capture)
	if err := os.WriteFile(p.ConfigFile(), []byte("agent: claude\nagent_path_override:\n  claude: "+recorder+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	repo, head := setupTestGitRepo(t, p, d, "claude-profile-repo")
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	for _, tc := range []struct {
		name, callerDir, want string
	}{
		{"caller profile", "/caller/.claude1", "/caller/.claude1"},
		{"no caller profile", "", daemonProfile},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_ = os.Remove(capture)
			var result ipc.PushReceivedResult
			if err := client.Call(ipc.MethodPushReceived, &ipc.PushReceivedParams{
				Gate: p.RepoDir(repo.ID), Ref: "refs/heads/main", New: head, ClaudeConfigDir: tc.callerDir,
			}, &result); err != nil {
				t.Fatal(err)
			}
			run := waitForRunTerminalState(t, d, result.RunID)
			if run.Status != types.RunCompleted {
				t.Fatalf("run status = %s, error = %v", run.Status, run.Error)
			}
			if got := run.ClaudeConfigDir; (tc.callerDir == "") != (got == nil) || (got != nil && *got != tc.callerDir) {
				t.Fatalf("persisted ClaudeConfigDir = %v, want %q", got, tc.callerDir)
			}
			data, err := os.ReadFile(capture)
			if err != nil {
				t.Fatal(err)
			}
			if string(data) != tc.want {
				t.Fatalf("claude ran with CLAUDE_CONFIG_DIR=%q, want %q", data, tc.want)
			}
		})
	}
}

func TestRelativeClaudeConfigDirDoesNotSupersedeActiveRun(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	repo, head := setupTestGitRepo(t, p, d, "relative-claude-profile")
	active, err := d.InsertRun(repo.ID, "feature", head, head)
	if err != nil {
		t.Fatal(err)
	}
	m := NewRunManager(d, p, nil)
	cancelled := false
	m.cancels[active.ID] = func(error) { cancelled = true }
	_, err = m.startRun(context.Background(), repo, "feature", head, head, "test", nil, "", "", false, "", nil, ".claude1")
	if err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("relative CLAUDE_CONFIG_DIR error = %v", err)
	}
	runs, err := d.GetRunsByRepo(repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled || len(runs) != 1 {
		t.Fatalf("refused request changed active validation: cancelled=%v runs=%d", cancelled, len(runs))
	}
}

func TestAgentEnvironmentAddsTheRunsClaudeConfigDirToTheForgeOverlay(t *testing.T) {
	forge := &forgecontext.Context{Environment: runenv.Overlay{Set: map[string]string{"GH_CONFIG_DIR": "/gh"}, Unset: []string{"GH_TOKEN"}}}
	dir := "/caller/.claude1"

	got := agentEnvironment(forge, &db.Run{ClaudeConfigDir: &dir})
	if got.Set[runenv.ClaudeConfigDirEnvVar] != dir || got.Set["GH_CONFIG_DIR"] != "/gh" || len(got.Unset) != 1 {
		t.Fatalf("overlay = %+v", got)
	}
	if _, leaked := forge.Environment.Set[runenv.ClaudeConfigDirEnvVar]; leaked {
		t.Fatal("run overlay mutated the shared forge overlay")
	}
	if got := agentEnvironment(nil, &db.Run{ClaudeConfigDir: &dir}); got.Set[runenv.ClaudeConfigDirEnvVar] != dir {
		t.Fatalf("overlay without forge context = %+v", got)
	}
	if got := agentEnvironment(forge, &db.Run{}); len(got.Set) != 1 {
		t.Fatalf("unset run changed the forge overlay: %+v", got)
	}
}

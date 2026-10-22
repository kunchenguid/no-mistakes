package daemon

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestRunAgentInvalidSelectionPreservesActiveValidation(t *testing.T) {
	for _, requested := range []types.AgentName{types.AgentAuto, types.AgentCodex} {
		t.Run(string(requested), func(t *testing.T) {
			p := paths.WithRoot(t.TempDir())
			if err := p.EnsureDirs(); err != nil {
				t.Fatal(err)
			}
			d, err := db.Open(p.DB())
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			repo, head := setupTestGitRepo(t, p, d, "caller-agent")
			if err := os.WriteFile(p.ConfigFile(), []byte("agent: claude\nagent_path_override:\n  codex: "+filepath.Join(t.TempDir(), "missing-codex")+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			active, err := d.InsertRun(repo.ID, "feature", head, head)
			if err != nil {
				t.Fatal(err)
			}
			m := NewRunManager(d, p, nil)
			cancelled := false
			m.cancels[active.ID] = func(error) { cancelled = true }
			if _, err := m.startRun(context.Background(), repo, "feature", head, head, "test", nil, "caller selection", "", false, "", nil, requested); err == nil {
				t.Fatal("invalid or unavailable agent accepted")
			}
			runs, err := d.GetRunsByRepo(repo.ID)
			if err != nil {
				t.Fatal(err)
			}
			if cancelled || len(runs) != 1 || runs[0].ID != active.ID {
				t.Fatalf("invalid request changed active validation: cancelled=%v runs=%d", cancelled, len(runs))
			}
		})
	}
}

func TestRunAgentNonceReplayKeepsItsBackend(t *testing.T) {
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step { return []pipeline.Step{&mockPassStep{name: types.StepReview}} })
	repo, head := setupTestGitRepo(t, p, d, "agent-nonce")
	fake := writeMockClaude(t, t.TempDir())
	if err := os.WriteFile(p.ConfigFile(), []byte("agent: claude\nagent_path_override:\n  codex: "+fake+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	params := &ipc.StartFreshRunParams{RepoID: repo.ID, Branch: "main", HeadSHA: head, Intent: "caller engine", LaunchNonce: "caller-engine", ValidationGeneration: "gen", RequestedAgent: types.AgentCodex}
	var result ipc.StartFreshRunResult
	if err := client.Call(ipc.MethodStartFreshRun, params, &result); err != nil {
		t.Fatal(err)
	}
	run := waitForRunTerminalState(t, d, result.Receipt.RunID)
	if run.RequestedAgent != types.AgentCodex || result.Receipt.RequestedAgent != types.AgentCodex {
		t.Fatalf("lost selection: %+v %+v", run, result.Receipt)
	}
	params.RequestedAgent = types.AgentClaude
	if err := client.Call(ipc.MethodStartFreshRun, params, &result); err == nil {
		t.Fatal("nonce replay changed backend")
	}
	params.RequestedAgent = types.AgentCodex
	if err := client.Call(ipc.MethodStartFreshRun, params, &result); err != nil {
		t.Fatal(err)
	}
	if result.Receipt.RunID != run.ID {
		t.Fatal("matching replay created a different run")
	}
}

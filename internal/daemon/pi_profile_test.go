package daemon

import (
	"context"
	"os"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestPiProfileLaunchRPCAndNonceReplay(t *testing.T) {
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step { return []pipeline.Step{&mockPassStep{name: types.StepReview}} })
	repo, head := setupTestGitRepo(t, p, d, "profile-repo")
	fake := writeMockClaude(t, t.TempDir())
	writeCfg := func(extra string) {
		t.Helper()
		if err := os.WriteFile(p.ConfigFile(), []byte("agent: pi\nagent_path_override:\n  pi: "+fake+"\n"+extra), 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeCfg("agent_config:\n  pi: {model: openai-codex/gpt-5.4, effort: high}\n")
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var resolved agentcfg.PiProfile
	if err := client.Call(ipc.MethodResolvePiProfile, &agentcfg.PiProfile{Effort: agentcfg.EffortMedium}, &resolved); err != nil {
		t.Fatal(err)
	}
	if resolved.Model != "openai-codex/gpt-5.4" || resolved.Effort != agentcfg.EffortMedium {
		t.Fatalf("resolve: %+v", resolved)
	}
	params := &ipc.StartFreshRunParams{RepoID: repo.ID, Branch: "main", HeadSHA: head, Intent: "pin profile", LaunchNonce: "profile-nonce", ValidationGeneration: "gen", PiProfile: &resolved}
	var result ipc.StartFreshRunResult
	if err := client.Call(ipc.MethodStartFreshRun, params, &result); err != nil {
		t.Fatal(err)
	}
	run := waitForRunTerminalState(t, d, result.Receipt.RunID)
	if run.Status != types.RunCompleted || run.PiProfile == nil || *run.PiProfile != resolved {
		t.Fatalf("run: %+v", run)
	}
	writeCfg("agent_config:\n  pi: {model: anthropic/other, effort: low}\nagent_args_override:\n  pi: [--model, later]\n")
	// A nonce replay uses its run, not newly conflicting live defaults.
	if err := client.Call(ipc.MethodStartFreshRun, params, &result); err != nil {
		t.Fatal(err)
	}
	params.PiProfile = &agentcfg.PiProfile{Effort: agentcfg.EffortLow}
	if err := client.Call(ipc.MethodStartFreshRun, params, &result); err == nil {
		t.Fatal("conflicting nonce profile accepted")
	}
	var status ipc.GetRunResult
	if err := client.Call(ipc.MethodGetRun, &ipc.GetRunParams{RunID: run.ID}, &status); err != nil {
		t.Fatal(err)
	}
	if status.Run.PiProfile == nil || *status.Run.PiProfile != resolved {
		t.Fatalf("IPC lost profile: %+v", status)
	}
	// A rerun is a new run. Old callers neither inherit a prior pin nor
	// reject the raw native flags that remain valid for unpinned runs.
	var rerun ipc.RerunResult
	if err := client.Call(ipc.MethodRerun, &ipc.RerunParams{RepoID: repo.ID, Branch: "main"}, &rerun); err != nil {
		t.Fatal(err)
	}
	latest := waitForRunTerminalState(t, d, rerun.RunID)
	if latest.ID == run.ID || latest.PiProfile != nil || latest.Status != types.RunCompleted {
		t.Fatalf("legacy rerun changed: %+v", latest)
	}
}

func TestPiProfileInvalidLaunchDoesNotSupersedeActiveRun(t *testing.T) {
	for _, tc := range []struct {
		name, config string
	}{
		{"raw selection flags", "agent: pi\nagent_args_override:\n  pi: [--thinking, low]\n"},
		{"non-Pi agent", "agent: claude\n"},
		{"mixed fallbacks", "agent: [pi, claude]\n"},
		{"non-Pi review_agents", "agent: pi\nreview_agents:\n  reviewer: {agent: claude}\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := paths.WithRoot(t.TempDir())
			if err := p.EnsureDirs(); err != nil {
				t.Fatal(err)
			}
			d, err := db.Open(p.DB())
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			repo, head := setupTestGitRepo(t, p, d, "invalid-profile")
			active, err := d.InsertRun(repo.ID, "feature", head, head)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p.ConfigFile(), []byte(tc.config), 0600); err != nil {
				t.Fatal(err)
			}
			m := NewRunManager(d, p, nil)
			cancelled := false
			m.cancels[active.ID] = func(error) { cancelled = true }
			pin := &agentcfg.PiProfile{Model: "openai-codex/gpt-5.4", Effort: agentcfg.EffortHigh}
			if _, err := m.startRun(context.Background(), repo, "feature", head, head, "test", nil, "pin", "", false, pin); err == nil {
				t.Fatal("invalid pin accepted")
			}
			runs, err := d.GetRunsByRepo(repo.ID)
			if err != nil {
				t.Fatal(err)
			}
			if cancelled || len(runs) != 1 || runs[0].ID != active.ID {
				t.Fatalf("bad request changed active validation: cancelled=%v runs=%d", cancelled, len(runs))
			}
		})
	}
}

func TestPiProfileTrustedRepoAgentOverrideDoesNotSupersedeActiveRun(t *testing.T) {
	for _, tc := range []struct {
		name, yaml string
	}{
		{"non-Pi agent", "agent: claude\n"},
		{"mixed fallbacks", "agent: [pi, claude]\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := paths.WithRoot(t.TempDir())
			if err := p.EnsureDirs(); err != nil {
				t.Fatal(err)
			}
			d, err := db.Open(p.DB())
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			repo, _ := setupTestGitRepo(t, p, d, "trusted-profile")
			head := commitDefaultBranchConfig(t, repo.WorkingPath, tc.yaml)
			active, err := d.InsertRun(repo.ID, "feature", head, head)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p.ConfigFile(), []byte("agent: pi\n"), 0600); err != nil {
				t.Fatal(err)
			}
			m := NewRunManager(d, p, nil)
			cancelled := false
			m.cancels[active.ID] = func(error) { cancelled = true }
			pin := &agentcfg.PiProfile{Model: "openai-codex/gpt-5.4", Effort: agentcfg.EffortHigh}
			if _, err := m.startRun(context.Background(), repo, "feature", head, head, "test", nil, "pin", "", false, pin); err == nil {
				t.Fatal("trusted-repo override accepted")
			}
			runs, err := d.GetRunsByRepo(repo.ID)
			if err != nil {
				t.Fatal(err)
			}
			if cancelled || len(runs) != 1 || runs[0].ID != active.ID {
				t.Fatalf("trusted-repo override changed active validation: cancelled=%v runs=%d", cancelled, len(runs))
			}
		})
	}
}

func TestPiProfileRecoveryUsesPersistedPinAndLegacyUsesLiveConfig(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	repo, head := setupTestGitRepo(t, p, d, "recovery-profile")
	pin := &agentcfg.PiProfile{Model: "openai-codex/gpt-5.4", Effort: agentcfg.EffortHigh}
	pinned, err := d.InsertRunWithIntentAndLaunchNonce(repo.ID, "pinned", head, head, nil, "", "", "", "", false, pin)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := d.InsertRun(repo.ID, "legacy", head, head)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.ConfigFile(), []byte("agent: claude\nagent_config:\n  pi: {model: anthropic/later, effort: low}\nagent_args_override:\n  pi: [--model, later, --thinking, low]\nreview_agents:\n  reviewer: {agent: claude}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, r := range []*db.Run{pinned, legacy} {
		dir := p.WorktreeDir(repo.ID, r.ID)
		gitCmd(t, p.RepoDir(repo.ID), "worktree", "add", "--detach", dir, head)
		reloaded, err := d.GetRun(r.ID)
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := NewRunManager(d, p, nil).loadRecoveredConfig(context.Background(), reloaded, repo, dir)
		if err != nil {
			t.Fatal(err)
		}
		if r == pinned {
			if cfg.Agent != types.AgentPi || cfg.AgentProfile().Model != pin.Model || cfg.AgentProfile().Effort != pin.Effort || cfg.ReviewAgents != nil {
				t.Fatalf("recovered drift: %+v", cfg)
			}
		} else if cfg.Agent != types.AgentClaude {
			t.Fatal("legacy stopped following config")
		}
	}
}

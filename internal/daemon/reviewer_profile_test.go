package daemon

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestReviewerSelectionConcurrentLaunchAndNonceReplay(t *testing.T) {
	capture := filepath.Join(t.TempDir(), "argv.log")
	bin := writeCapturingPiAgent(t, t.TempDir(), capture)
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step { return []pipeline.Step{&reviewRoleProbeStep{}} })
	writeConfig := func(model string) {
		t.Helper()
		if err := os.WriteFile(p.ConfigFile(), []byte("agent: pi\nagent_path_override:\n  pi: "+bin+"\nagent_config:\n  pi: {model: "+model+", effort: low}\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeConfig("openai/default")
	requests := make([]*ipc.StartFreshRunParams, 2)
	for i, name := range []string{"a", "b"} {
		repo, head := setupTestGitRepo(t, p, d, "reviewer-"+name)
		requests[i] = &ipc.StartFreshRunParams{RepoID: repo.ID, Branch: "main", HeadSHA: head, Intent: "independent reviewer", LaunchNonce: "reviewer-" + name, ValidationGeneration: "gen",
			PiProfile:       &agentcfg.PiProfile{Model: "openai/writer", Effort: agentcfg.EffortMedium},
			ReviewerProfile: &agentcfg.ReviewerProfile{Agent: types.AgentPi, Model: "openai/review-" + name, Effort: agentcfg.EffortHigh}}
	}
	var wg sync.WaitGroup
	results := make([]ipc.StartFreshRunResult, 2)
	for i := range requests {
		wg.Go(func() {
			client, err := ipc.Dial(p.Socket())
			if err != nil {
				t.Error(err)
				return
			}
			defer client.Close()
			if err := client.Call(ipc.MethodStartFreshRun, requests[i], &results[i]); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	for i := range requests {
		run := waitForRunTerminalState(t, d, results[i].Receipt.RunID)
		if run.Status != types.RunCompleted || run.ReviewerProfile == nil || *run.ReviewerProfile != *requests[i].ReviewerProfile {
			t.Fatalf("launch lost selection: %+v", run)
		}
		args, err := os.ReadFile(capture + "." + run.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, model := range []string{requests[i].ReviewerProfile.Model, "openai/writer"} {
			if got := strings.Count(string(args), "--model "+model+" "); got != 1 {
				t.Fatalf("run %s: %s invocations=%d want=1: %s", run.ID, model, got, args)
			}
		}
	}
	writeConfig("openai/later")
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var replay ipc.StartFreshRunResult
	if err := client.Call(ipc.MethodStartFreshRun, requests[0], &replay); err != nil {
		t.Fatal(err)
	}
	if replay.Receipt.RunID != results[0].Receipt.RunID || replay.Receipt.ReviewerProfile == nil || *replay.Receipt.ReviewerProfile != *requests[0].ReviewerProfile {
		t.Fatalf("nonce replay drifted: %+v", replay)
	}
	changed := *requests[0].ReviewerProfile
	changed.Model = "openai/wrong"
	requests[0].ReviewerProfile = &changed
	if err := client.Call(ipc.MethodStartFreshRun, requests[0], &replay); err == nil {
		t.Fatal("nonce replay replaced reviewer")
	}
	var rerun ipc.RerunResult
	if err := client.Call(ipc.MethodRerun, &ipc.RerunParams{RepoID: requests[1].RepoID, Branch: "main"}, &rerun); err != nil {
		t.Fatal(err)
	}
	inherited := waitForRunTerminalState(t, d, rerun.RunID)
	if inherited.Status != types.RunCompleted || inherited.ReviewerProfile == nil || *inherited.ReviewerProfile != *requests[1].ReviewerProfile || inherited.PiProfile != nil {
		t.Fatalf("rerun inheritance: %+v", inherited)
	}
}

func TestReviewerSelectionUnavailableDoesNotSupersedeActiveRun(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	repo, head := setupTestGitRepo(t, p, d, "unavailable-reviewer")
	active, err := d.InsertRun(repo.ID, "feature", head, head)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.ConfigFile(), []byte("agent: codex\nagent_path_override:\n  codex: "+filepath.Join(t.TempDir(), "missing-codex")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	m := NewRunManager(d, p, nil)
	cancelled := false
	m.cancels[active.ID] = func(error) { cancelled = true }
	pin := &agentcfg.ReviewerProfile{Agent: types.AgentCodex, Model: "reviewer", Effort: agentcfg.EffortHigh}
	if _, err := m.startRun(context.Background(), repo, "feature", head, head, "test", nil, "review", "", false, "", nil, pin); err == nil || !strings.Contains(err.Error(), "reviewer unavailable") {
		t.Fatalf("unavailable reviewer accepted: %v", err)
	}
	runs, err := d.GetRunsByRepo(repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled || len(runs) != 1 || runs[0].ID != active.ID {
		t.Fatal("invalid reviewer superseded active run")
	}
}

func TestReviewerSelectionRecoveryUsesPersistedPin(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	repo, head := setupTestGitRepo(t, p, d, "reviewer-recovery")
	pin := &agentcfg.ReviewerProfile{Agent: types.AgentPi, Model: "openai/reviewer", Effort: agentcfg.EffortHigh}
	run, err := d.InsertRunWithIntentAndLaunchNonce(repo.ID, "feature", head, head, nil, "", "", "", "", false, nil, pin)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p.ConfigFile(), []byte("agent: codex\nagent_config:\n  pi: {model: openai/later, effort: low}\nagent_args_override:\n  pi: [--model, later]\nreview_agents:\n  reviewer: {agent: codex}\n  reviewer_after_round: {agent: codex}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	dir := p.WorktreeDir(repo.ID, run.ID)
	gitCmd(t, p.RepoDir(repo.ID), "worktree", "add", "--detach", dir, head)
	run, err = d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := NewRunManager(d, p, nil).loadRecoveredConfig(context.Background(), run, repo, dir)
	if err != nil {
		t.Fatal(err)
	}
	role := cfg.ForReviewAgent(cfg.ReviewAgents["reviewer"])
	if cfg.Agent != types.AgentCodex || role.Agent != pin.Agent || role.AgentProfile().Model != pin.Model || role.AgentProfile().Effort != pin.Effort || strings.Join(role.AgentArgsFor(pin.Agent), " ") != "--provider openai" || cfg.ReviewAgentTakeoverRound("reviewer_after_round") != 0 {
		t.Fatalf("recovery drifted: %+v", cfg)
	}
}

func TestReviewerSelectionRetryKeepsResolvedProfile(t *testing.T) {
	pin := &agentcfg.ReviewerProfile{Agent: types.AgentCodex, Model: "reviewer", Effort: agentcfg.EffortHigh}
	f := newRetryFixture(t, retryFixtureOptions{reviewer: pin})
	bin := writeMockClaude(t, t.TempDir())
	if err := os.WriteFile(f.p.ConfigFile(), []byte("agent: codex\nagent_path_override:\n  codex: "+bin+"\nagent_config:\n  codex: {model: later-default, effort: low}\nagent_args_override:\n  codex: [--model, different-reviewer]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	client, err := ipc.Dial(f.p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var result ipc.RetryRunResult
	if err := client.Call(ipc.MethodRetryRun, &ipc.RetryRunParams{RepoID: f.repo.ID, Branch: "main", RunID: f.source.ID, CallerHeadSHA: f.callerHead}, &result); err != nil {
		t.Fatal(err)
	}
	run := waitForRunTerminalState(t, f.d, result.RunID)
	if run.Status != types.RunCompleted || run.ReviewerProfile == nil || *run.ReviewerProfile != *pin {
		t.Fatalf("retry replaced reviewer: %+v", run)
	}
}

func TestReviewerSelectionFreshLaunchDoesNotInherit(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fresh bool
		pi    *agentcfg.PiProfile
		want  string
	}{
		{name: "explicit rerun", want: "openai/old-reviewer"},
		{name: "fresh global", fresh: true, want: "openai/default"},
		{name: "fresh Pi", fresh: true, pi: &agentcfg.PiProfile{Model: "openai/new-writer", Effort: agentcfg.EffortHigh}, want: "openai/new-writer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			capture := filepath.Join(t.TempDir(), "argv.log")
			bin := writeCapturingPiAgent(t, t.TempDir(), capture)
			p, d := startTestDaemonWithSteps(t, func() []pipeline.Step { return []pipeline.Step{&reviewRoleProbeStep{}} })
			if err := os.WriteFile(p.ConfigFile(), []byte("agent: pi\nagent_path_override:\n  pi: "+bin+"\nagent_config:\n  pi: {model: openai/default, effort: low}\n"), 0600); err != nil {
				t.Fatal(err)
			}
			repo, head := setupTestGitRepo(t, p, d, "fresh-reviewer")
			pin := &agentcfg.ReviewerProfile{Agent: types.AgentPi, Model: "openai/old-reviewer", Effort: agentcfg.EffortHigh}
			previous, err := d.InsertRunWithIntentAndLaunchNonce(repo.ID, "main", head, head, nil, "", "", "", "", false, nil, pin)
			if err != nil {
				t.Fatal(err)
			}
			if err := d.UpdateRunStatus(previous.ID, types.RunCompleted); err != nil {
				t.Fatal(err)
			}
			client, err := ipc.Dial(p.Socket())
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			var result ipc.RerunResult
			if err := client.Call(ipc.MethodRerun, &ipc.RerunParams{RepoID: repo.ID, Branch: "main", CallerHeadSHA: head, FreshLaunch: tc.fresh, PiProfile: tc.pi}, &result); err != nil {
				t.Fatal(err)
			}
			run := waitForRunTerminalState(t, d, result.RunID)
			if run.Status != types.RunCompleted || tc.fresh && run.ReviewerProfile != nil || !tc.fresh && (run.ReviewerProfile == nil || *run.ReviewerProfile != *pin) {
				t.Fatalf("reviewer inheritance: %+v", run)
			}
			args, err := os.ReadFile(capture + "." + run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(args), "--model "+tc.want+" ") || tc.fresh && strings.Contains(string(args), pin.Model) {
				t.Fatalf("reviewer invocation: %s", args)
			}
		})
	}
}

func TestReviewerSelectionPrePushResolutionAndAdmission(t *testing.T) {
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step { return []pipeline.Step{&mockPassStep{name: types.StepReview}} })
	repo, _ := setupTestGitRepo(t, p, d, "reviewer-admission")
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	pin := &agentcfg.ReviewerProfile{Agent: types.AgentCodex, Model: "reviewer", Effort: agentcfg.EffortHigh}
	for _, tc := range []struct {
		name, trusted, binary, wantErr string
		brokenFetch                    bool
	}{
		{name: "missing binary", binary: filepath.Join(t.TempDir(), "missing-codex"), wantErr: "reviewer unavailable"},
		{name: "available binary", binary: writeMockClaude(t, t.TempDir())},
		{name: "trusted policy", trusted: "disable_project_settings: true\n", binary: writeMockClaude(t, t.TempDir())},
		{name: "broken trusted policy", trusted: "disable_project_settings: invalid\n", binary: writeMockClaude(t, t.TempDir()), wantErr: "config"},
		{name: "unreadable trusted branch", binary: writeMockClaude(t, t.TempDir()), brokenFetch: true, wantErr: "failed to fetch trusted default branch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.brokenFetch {
				gitCmd(t, p.RepoDir(repo.ID), "remote", "set-url", "origin", filepath.Join(t.TempDir(), "missing-origin"))
			}
			if tc.trusted != "" {
				commitDefaultBranchConfig(t, repo.WorkingPath, tc.trusted)
			}
			if err := os.WriteFile(p.ConfigFile(), []byte("agent: codex\nagent_path_override:\n  codex: "+tc.binary+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			before := gitOutput(t, p.RepoDir(repo.ID), "show-ref")
			for _, admission := range []bool{false, true} {
				var callErr error
				if admission {
					var result ipc.AdmitPushResult
					callErr = client.Call(ipc.MethodAdmitPush, &ipc.AdmitPushParams{Gate: p.RepoDir(repo.ID), ReviewerProfile: pin}, &result)
				} else {
					var resolved agentcfg.ReviewerProfile
					callErr = client.Call(ipc.MethodResolveReviewerProfile, &ipc.ResolveReviewerProfileParams{RepoID: repo.ID, ReviewerProfile: pin}, &resolved)
					if callErr == nil && resolved != *pin {
						t.Fatalf("resolution changed pin: %+v", resolved)
					}
				}
				if tc.wantErr == "" && callErr != nil || tc.wantErr != "" && (callErr == nil || !strings.Contains(callErr.Error(), tc.wantErr)) {
					t.Fatalf("admission=%t: %v, want %q", admission, callErr, tc.wantErr)
				}
			}
			if after := gitOutput(t, p.RepoDir(repo.ID), "show-ref"); after != before {
				t.Fatalf("reviewer preflight changed refs: before=%s after=%s", before, after)
			}
			runs, err := d.GetRunsByRepo(repo.ID)
			if err != nil || len(runs) != 0 {
				t.Fatalf("reviewer preflight created runs: %+v %v", runs, err)
			}
		})
	}
}

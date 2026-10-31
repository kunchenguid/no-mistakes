package daemon

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/agentcfg"
	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestUnavailableSelectedAgentDoesNotSupersedeActiveRun(t *testing.T) {
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step { return []pipeline.Step{&allRolesProbeStep{}} })
	repo, head := setupTestGitRepo(t, p, d, "missing-agent")
	missing := filepath.Join(t.TempDir(), "missing-codex")
	if err := os.WriteFile(p.ConfigFile(), []byte("agent: claude\nagent_path_override:\n  codex: "+missing+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	active, err := d.InsertRun(repo.ID, "main", head, head)
	if err != nil {
		t.Fatal(err)
	}
	m := NewRunManager(d, p, nil)
	cancelled := false
	m.cancels[active.ID] = func(error) { cancelled = true }
	if _, err := m.startRun(context.Background(), repo, "main", head, head, "test", nil, "selected", "", false, "", nil, agentcfg.RunAgentCodex); err == nil {
		t.Fatal("missing Codex accepted")
	}
	if cancelled {
		t.Fatal("active run was cancelled")
	}
	latest, err := d.GetActiveRun(repo.ID, "main")
	if err != nil || latest == nil || latest.ID != active.ID {
		t.Fatalf("active run changed: %+v %v", latest, err)
	}
}

func TestSelectedRerunCannotReplaceActiveRun(t *testing.T) {
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step { return []pipeline.Step{&allRolesProbeStep{}} })
	repo, head := setupTestGitRepo(t, p, d, "active-rerun")
	active, err := d.InsertRun(repo.ID, "main", head, head)
	if err != nil {
		t.Fatal(err)
	}
	m := NewRunManager(d, p, nil)
	cancelled := false
	m.cancels[active.ID] = func(error) { cancelled = true }
	if _, err := m.HandleRerun(context.Background(), repo.ID, "main", "", nil, "", "", false, head, "", nil, agentcfg.RunAgentCodex); err == nil {
		t.Fatal("selected rerun replaced active run")
	}
	if _, err := m.startRun(context.Background(), repo, "main", head, head, "rerun", nil, "selected", "", false, "", nil, agentcfg.RunAgentCodex); err == nil {
		t.Fatal("selected rerun displaced a run after its first active-run check")
	}
	if cancelled {
		t.Fatal("active run was cancelled")
	}
	latest, err := d.GetActiveRun(repo.ID, "main")
	if err != nil || latest == nil || latest.ID != active.ID {
		t.Fatalf("active run changed: %+v %v", latest, err)
	}
}

func TestSelectedRerunAfterUnchangedTerminalRunNeedsNoRecovery(t *testing.T) {
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step { return []pipeline.Step{&allRolesProbeStep{}} })
	repo, head := setupTestGitRepo(t, p, d, "released-selection")
	prior, err := d.InsertRun(repo.ID, "main", head, head)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(prior.ID, types.RunCompleted); err != nil {
		t.Fatal(err)
	}
	if _, err := NewRunManager(d, p, nil).HandleRerun(context.Background(), repo.ID, "main", "", nil, "", "", false, head, "", nil, agentcfg.RunAgentCodex); err == nil || !strings.Contains(err.Error(), "custody") {
		t.Fatalf("selected rerun guessed custody from an unverified head: %v", err)
	}
	if err := d.UpdateRunStatusWithVerifiedHead(prior.ID, types.RunCompleted, head); err != nil {
		t.Fatal(err)
	}
	selectedCodex := writeSelectedAgentFixture(t, t.TempDir(), "codex", filepath.Join(t.TempDir(), "agents.log"))
	if err := os.WriteFile(p.ConfigFile(), []byte("agent: claude\nagent_path_override:\n  codex: "+selectedCodex+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := d.GetRun(prior.ID)
	if err != nil || before.CustodyReturnedAt != nil {
		t.Fatalf("prior run has unexpected custody stamp: %+v %v", before, err)
	}
	id, err := NewRunManager(d, p, nil).HandleRerun(context.Background(), repo.ID, "main", "", nil, "", "", false, head, "", nil, agentcfg.RunAgentCodex)
	if err != nil {
		t.Fatalf("selected rerun from released head: %v", err)
	}
	run := waitForRunTerminalState(t, d, id)
	if run.Status != types.RunCompleted || run.RunAgent != agentcfg.RunAgentCodex || run.SubmittedHeadSHA == nil || *run.SubmittedHeadSHA != head {
		t.Fatalf("selected rerun = %+v", run)
	}
}

func TestSelectedRerunRequiresCustodyAndExactTerminalHead(t *testing.T) {
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step { return []pipeline.Step{&allRolesProbeStep{}} })
	repo, submitted := setupTestGitRepo(t, p, d, "terminal-selection")
	prior, err := d.InsertRun(repo.ID, "main", submitted, submitted)
	if err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo.WorkingPath, "commit", "--allow-empty", "-m", "review correction")
	preserved := gitOutput(t, repo.WorkingPath, "rev-parse", "HEAD")
	gitCmd(t, repo.WorkingPath, "push", "gate", preserved+":"+custody.RecoveryRef(prior.ID))
	if err := d.UpdateRunStatusWithVerifiedHead(prior.ID, types.RunFailed, preserved); err != nil {
		t.Fatal(err)
	}
	m := NewRunManager(d, p, nil)
	if _, err := m.HandleRerun(context.Background(), repo.ID, "main", "", nil, "", "", false, preserved, "", nil, agentcfg.RunAgentCodex); err == nil || !strings.Contains(err.Error(), "custody") {
		t.Fatalf("selected rerun accepted stale gate head: %v", err)
	}
	runs, err := d.GetRunsByRepo(repo.ID)
	if err != nil || len(runs) != 1 {
		t.Fatalf("refusal created a run: %+v %v", runs, err)
	}
	if got := gitOutput(t, p.RepoDir(repo.ID), "rev-parse", "refs/heads/main"); got != submitted {
		t.Fatalf("refusal moved gate head: %s", got)
	}
	selectedCodex := writeSelectedAgentFixture(t, t.TempDir(), "codex", filepath.Join(t.TempDir(), "agents.log"))
	if err := os.WriteFile(p.ConfigFile(), []byte("agent: claude\nagent_path_override:\n  codex: "+selectedCodex+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	const prURL = "https://example.com/repo/pull/1"
	if err := d.UpdateRunPRURL(prior.ID, prURL); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, repo.WorkingPath, "push", "gate", preserved+":refs/heads/main")
	if _, err := m.HandleRerun(context.Background(), repo.ID, "main", "", nil, "", "", false, preserved, "", nil, agentcfg.RunAgentCodex); err == nil || !strings.Contains(err.Error(), "custody") {
		t.Fatalf("selected rerun accepted outstanding custody: %v", err)
	}
	if _, err := m.startRun(context.Background(), repo, "main", preserved, submitted, "rerun", nil, "selected", "", false, "", nil, agentcfg.RunAgentCodex); err == nil || !strings.Contains(err.Error(), "before launch") {
		t.Fatalf("selected rerun launch accepted outstanding custody: %v", err)
	}
	if runs, err := d.GetRunsByRepo(repo.ID); err != nil || len(runs) != 1 {
		t.Fatalf("refused launch created a run: %+v %v", runs, err)
	}
	if err := d.SetRunCustodyReturned(prior.ID); err != nil {
		t.Fatal(err)
	}
	id, err := m.HandleRerun(context.Background(), repo.ID, "main", "", nil, "", "", false, preserved, "", nil, agentcfg.RunAgentCodex)
	if err != nil {
		t.Fatalf("rerun after exact head adoption: %v", err)
	}
	run := waitForRunTerminalState(t, d, id)
	if run.Status != types.RunCompleted || run.RunAgent != agentcfg.RunAgentCodex || run.SubmittedHeadSHA == nil || *run.SubmittedHeadSHA != preserved || run.PRURL == nil || *run.PRURL != prURL {
		t.Fatalf("selected rerun lost preserved head or PR identity: %+v", run)
	}
}

func TestRecoveredRunRetainsSelectedAgentAcrossConfigChange(t *testing.T) {
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	repo, head := setupTestGitRepo(t, p, d, "recover-selected")
	run, err := d.InsertRunWithAgent(repo.ID, "main", head, head, nil, "", "", "", "", false, nil, nil, agentcfg.RunAgentCodex)
	if err != nil {
		t.Fatal(err)
	}
	workDir := p.WorktreeDir(repo.ID, run.ID)
	gitCmd(t, p.RepoDir(repo.ID), "worktree", "add", "--detach", workDir, head)
	if err := os.WriteFile(p.ConfigFile(), []byte("agent: claude\nreview_agents:\n  reviewer: {agent: claude}\n  fixer: {agent: claude}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	reloaded, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := NewRunManager(d, p, nil).loadRecoveredConfig(context.Background(), reloaded, repo, workDir)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Agent != types.AgentCodex || len(cfg.Agents) != 1 || cfg.Agents[0] != types.AgentCodex || len(cfg.ReviewAgents) != 0 {
		t.Fatalf("recovery changed selected roles: %+v", cfg)
	}
}

type allRolesProbeStep struct{}

func (*allRolesProbeStep) Name() types.StepName { return types.StepReview }
func (*allRolesProbeStep) Execute(sctx *pipeline.StepContext) (*pipeline.StepOutcome, error) {
	for _, purpose := range []string{"review", "review-fix", "test-evidence", "test-repair", "document", "lint", "pr", "rebase", "ci-fix"} {
		if _, err := sctx.RunAgentContext(sctx.Ctx, agent.RunOpts{Purpose: purpose, Prompt: purpose, CWD: sctx.WorkDir}); err != nil {
			return nil, err
		}
	}
	return &pipeline.StepOutcome{}, nil
}

func writeSelectedAgentFixture(t *testing.T, dir, name, capture string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	script := "#!/bin/sh\nprintf '%s\\n' '" + name + "' >> " + shellQuoteForTest(capture) + "\ncat >/dev/null\nsleep 0.05\n"
	if name == "codex" {
		script += "printf '%s\\n' '{\"type\":\"thread.started\",\"thread_id\":\"test-thread\"}' '{\"type\":\"item.completed\",\"item\":{\"id\":\"item_0\",\"type\":\"agent_message\",\"text\":\"ok\"}}' '{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}'\n"
	} else {
		script += "printf '%s\\n' '{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"result\":\"ok\"}'\n"
	}
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is POSIX-only")
	}
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSelectedRunUsesCodexForEveryRoleAndDefaultStaysClaude(t *testing.T) {
	p, d := startTestDaemonWithSteps(t, func() []pipeline.Step { return []pipeline.Step{&allRolesProbeStep{}} })
	dir := t.TempDir()
	capture := filepath.Join(dir, "agents.log")
	claude := writeSelectedAgentFixture(t, dir, "claude", capture)
	codex := writeSelectedAgentFixture(t, dir, "codex", capture)
	config := "agent: claude\nagent_path_override:\n  claude: " + claude + "\n  codex: " + codex + "\nreview_agents:\n  reviewer: {agent: claude}\n  fixer: {agent: claude}\n"
	if err := os.WriteFile(p.ConfigFile(), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	selectedRepo, _ := setupTestGitRepo(t, p, d, "selected-run")
	selectedHead := commitDefaultBranchConfig(t, selectedRepo.WorkingPath, "agent: claude\n")
	defaultRepo, _ := setupTestGitRepo(t, p, d, "default-run")
	defaultHead := commitDefaultBranchConfig(t, defaultRepo.WorkingPath, "agent: claude\n")
	client, err := ipc.Dial(p.Socket())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var selected ipc.StartFreshRunResult
	if err := client.Call(ipc.MethodStartFreshRun, &ipc.StartFreshRunParams{RepoID: selectedRepo.ID, Branch: "main", HeadSHA: selectedHead, Intent: "verify selection", LaunchNonce: "selected-run", ValidationGeneration: "gen", RunAgent: agentcfg.RunAgentCodex}, &selected); err != nil {
		t.Fatal(err)
	}
	if selected.Receipt.RunAgent != agentcfg.RunAgentCodex {
		t.Fatalf("receipt omitted selection: %+v", selected.Receipt)
	}
	if active, err := d.GetActiveRun(selectedRepo.ID, "main"); err != nil || active == nil || active.ID != selected.Receipt.RunID {
		t.Fatalf("selected run was not active as the default run started: %+v %v", active, err)
	}
	var ordinary ipc.StartFreshRunResult
	if err := client.Call(ipc.MethodStartFreshRun, &ipc.StartFreshRunParams{RepoID: defaultRepo.ID, Branch: "main", HeadSHA: defaultHead, Intent: "verify default", LaunchNonce: "default-run", ValidationGeneration: "gen"}, &ordinary); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{selected.Receipt.RunID, ordinary.Receipt.RunID} {
		run := waitForRunTerminalState(t, d, id)
		if run.Status != types.RunCompleted {
			t.Fatalf("run %s: %s %v", id, run.Status, run.Error)
		}
		invocations, err := d.GetAgentInvocationsByRun(id)
		if err != nil {
			t.Fatal(err)
		}
		want := "claude"
		if id == selected.Receipt.RunID {
			want = "codex"
		}
		if len(invocations) != 9 {
			t.Fatalf("run %s recorded %d invocations, want 9", id, len(invocations))
		}
		for _, invocation := range invocations {
			if invocation.Agent != want {
				t.Fatalf("run %s purpose %s used %s, want %s", id, invocation.Purpose, invocation.Agent, want)
			}
		}
	}
	data, err := os.ReadFile(capture)
	if err != nil || !strings.Contains(string(data), "codex") || !strings.Contains(string(data), "claude") {
		t.Fatalf("agent executables were not called: %q %v", data, err)
	}
}

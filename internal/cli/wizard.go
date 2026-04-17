package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/gate"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/wizard"
)

// repoState captures the git state the wizard routing and the wizard itself
// both need to know about.
type repoState struct {
	workDir       string
	currentBranch string
	defaultBranch string
	detached      bool
	dirty         bool
}

// needsBranch reports whether the user has no feature branch to work on —
// either they're on the default branch, or HEAD is detached.
func (s *repoState) needsBranch() bool {
	return s.detached || s.currentBranch == s.defaultBranch
}

// shouldRouteToWizard reports whether the active-run check should be
// bypassed and the wizard invoked directly. Detached HEAD is the only such
// case — pipelines need a real branch, and any "active run" matched here
// couldn't be on our current ref anyway. On any real branch (default or
// feature), we still fall through to strict branch-matched lookup and only
// enter the wizard when no run exists for that branch.
func (s *repoState) shouldRouteToWizard() bool {
	return s.detached
}

// detectRepoState probes the git state for the working directory, falling
// back to the repo's recorded default branch when the origin remote can't
// answer.
func detectRepoState(ctx context.Context, repo *db.Repo) (*repoState, error) {
	workDir, err := git.FindGitRoot(".")
	if err != nil {
		workDir = repo.WorkingPath
	}
	currentBranch, err := git.CurrentBranch(ctx, workDir)
	if err != nil {
		return nil, fmt.Errorf("detect current branch: %w", err)
	}
	detached, err := git.IsDetachedHEAD(ctx, workDir)
	if err != nil {
		return nil, fmt.Errorf("detect detached HEAD: %w", err)
	}
	dirty, err := git.HasUncommittedChanges(ctx, workDir)
	if err != nil {
		return nil, fmt.Errorf("detect working-tree state: %w", err)
	}
	defaultBranch := repo.DefaultBranch
	if defaultBranch == "" {
		defaultBranch = git.DefaultBranch(ctx, workDir, "origin")
	}
	return &repoState{
		workDir:       workDir,
		currentBranch: currentBranch,
		defaultBranch: defaultBranch,
		detached:      detached,
		dirty:         dirty,
	}, nil
}

// runWizard prepares an agent for suggestion calls and runs the interactive
// onboarding wizard against the supplied repo state.
func runWizard(ctx context.Context, p *paths.Paths, state *repoState) (wizard.Result, error) {
	workDir := state.workDir

	globalCfg, err := config.LoadGlobal(p.ConfigFile())
	if err != nil {
		return wizard.Result{}, fmt.Errorf("load global config: %w", err)
	}
	repoCfg, err := config.LoadRepo(workDir)
	if err != nil {
		return wizard.Result{}, fmt.Errorf("load repo config: %w", err)
	}
	cfg := config.Merge(globalCfg, repoCfg)
	if err := cfg.ResolveAgent(ctx, exec.LookPath); err != nil {
		return wizard.Result{}, fmt.Errorf("resolve agent: %w", err)
	}
	// Route agent-server stdout/stderr to a log file so lines don't corrupt
	// the wizard's alt-screen display. Any opencode/rovodev server started
	// during the wizard inherits this sink.
	restoreOutput := captureAgentServerOutput(p)
	defer restoreOutput()

	ag, err := agent.New(cfg.Agent, cfg.AgentPath())
	if err != nil {
		return wizard.Result{}, fmt.Errorf("create agent: %w", err)
	}
	defer ag.Close()

	wizCfg := wizard.Config{
		RepoDir:       workDir,
		CurrentBranch: state.currentBranch,
		DefaultBranch: state.defaultBranch,
		NeedsBranch:   state.needsBranch(),
		IsDirty:       state.dirty,
		GateRemote:    gate.RemoteName,

		CreateBranch: func(ctx context.Context, name string) error {
			return git.CreateBranch(ctx, workDir, name)
		},
		CommitAll: func(ctx context.Context, msg string) error {
			return git.CommitAll(ctx, workDir, msg)
		},
		Push: func(ctx context.Context, branch string) error {
			return git.Push(ctx, workDir, gate.RemoteName, "refs/heads/"+branch, "", false)
		},
		SuggestBranch: func(ctx context.Context) (string, error) {
			return agent.SuggestBranchName(ctx, ag, workDir)
		},
		SuggestCommit: func(ctx context.Context) (string, error) {
			return agent.SuggestCommitMessage(ctx, ag, workDir)
		},
	}

	return wizard.Run(wizCfg)
}

// captureAgentServerOutput routes managed-server logs to a file under
// LogsDir for the duration of the wizard, restoring the previous writer on
// cleanup. Failing to open the log file is not fatal — we fall back to
// io.Discard to avoid corrupting the alt-screen.
func captureAgentServerOutput(p *paths.Paths) func() {
	_ = os.MkdirAll(p.LogsDir(), 0o755)
	logPath := filepath.Join(p.LogsDir(), "wizard-agent.log")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		agent.SetManagedServerOutput(discardWriter{})
		return func() { agent.SetManagedServerOutput(nil) }
	}
	agent.SetManagedServerOutput(f)
	return func() {
		agent.SetManagedServerOutput(nil)
		f.Close()
	}
}

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// waitForActiveRun polls the daemon until an active run appears for the given
// repo/branch, or the deadline elapses. The post-receive hook creates the run
// asynchronously, so a short poll bridges the gap between push and attach.
func waitForActiveRun(client *ipc.Client, repoID, branch string, timeout time.Duration) (*ipc.RunInfo, error) {
	deadline := time.Now().Add(timeout)
	for {
		var result ipc.GetActiveRunResult
		if err := client.Call(ipc.MethodGetActiveRun, &ipc.GetActiveRunParams{RepoID: repoID, Branch: branch}, &result); err != nil {
			return nil, err
		}
		if result.Run != nil {
			return result.Run, nil
		}
		if time.Now().After(deadline) {
			return nil, nil
		}
		time.Sleep(150 * time.Millisecond)
	}
}

package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/gate"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/telemetry"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/kunchenguid/no-mistakes/internal/wizard"
)

var resolveWizardAgent = func(ctx context.Context, cfg *config.Config) error {
	return cfg.ResolveAgent(ctx, exec.LookPath)
}

var newWizardAgent = agent.New
var wizardRun = wizard.Run
var wizardRunAuto = wizard.RunAuto
var runWizardAutoVisible = func(ctx context.Context, p *paths.Paths, state *repoState) (wizard.Result, error) {
	return runWizardWithMode(ctx, p, state, true, true)
}
var runWizardAuto = func(ctx context.Context, p *paths.Paths, state *repoState) (wizard.Result, error) {
	return runWizardWithMode(ctx, p, state, true, false)
}

type wizardAgentSuggester struct {
	cfg     *config.Config
	workDir string
	resolve func(context.Context, *config.Config) error
	new     func(types.AgentName, string) (agent.Agent, error)

	once sync.Once
	ag   agent.Agent
	err  error

	// cachedCommit holds a commit subject returned as a side-effect of the
	// branch-name agent call, so the commit step can consume it without
	// spending another full agent round-trip. Consumed on first read.
	cacheMu      sync.Mutex
	cachedCommit string
}

func newWizardAgentSuggester(cfg *config.Config, workDir string, resolve func(context.Context, *config.Config) error, new func(types.AgentName, string) (agent.Agent, error)) *wizardAgentSuggester {
	if resolve == nil {
		resolve = resolveWizardAgent
	}
	if new == nil {
		new = newWizardAgent
	}
	return &wizardAgentSuggester{cfg: cfg, workDir: workDir, resolve: resolve, new: new}
}

func (s *wizardAgentSuggester) ensure(ctx context.Context) error {
	s.once.Do(func() {
		if err := s.resolve(ctx, s.cfg); err != nil {
			s.err = fmt.Errorf("resolve agent: %w", err)
			return
		}
		ag, err := s.new(s.cfg.Agent, s.cfg.AgentPath())
		if err != nil {
			s.err = fmt.Errorf("create agent: %w", err)
			return
		}
		s.ag = ag
	})
	return s.err
}

func (s *wizardAgentSuggester) suggestBranch(ctx context.Context) (string, error) {
	if err := s.ensure(ctx); err != nil {
		return "", err
	}
	branch, commit, err := agent.SuggestBranchAndCommit(ctx, s.ag, s.workDir)
	if err != nil {
		return "", err
	}
	if commit != "" {
		s.cacheMu.Lock()
		s.cachedCommit = commit
		s.cacheMu.Unlock()
	}
	return branch, nil
}

func (s *wizardAgentSuggester) suggestCommit(ctx context.Context) (string, error) {
	s.cacheMu.Lock()
	cached := s.cachedCommit
	s.cachedCommit = ""
	s.cacheMu.Unlock()
	if cached != "" {
		return cached, nil
	}
	if err := s.ensure(ctx); err != nil {
		return "", err
	}
	return agent.SuggestCommitMessage(ctx, s.ag, s.workDir)
}

func (s *wizardAgentSuggester) Close() error {
	if s.ag == nil {
		return nil
	}
	return s.ag.Close()
}

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

// runWizard prepares optional suggestion hooks and runs the interactive
// onboarding wizard against the supplied repo state.
func runWizard(ctx context.Context, p *paths.Paths, state *repoState) (wizard.Result, error) {
	return runWizardWithMode(ctx, p, state, false, true)
}

func runWizardWithMode(ctx context.Context, p *paths.Paths, state *repoState, auto bool, visible bool) (wizard.Result, error) {
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
	// Route agent-server stdout/stderr to a log file so lines don't corrupt
	// the wizard's alt-screen display. Any opencode/rovodev server started
	// during the wizard inherits this sink.
	restoreOutput := captureAgentServerOutput(p)
	defer restoreOutput()

	suggester := newWizardAgentSuggester(cfg, workDir, nil, nil)
	defer suggester.Close()

	wizCfg := wizard.Config{
		Context:       ctx,
		RepoDir:       workDir,
		CurrentBranch: state.currentBranch,
		DefaultBranch: state.defaultBranch,
		AutoAdvance:   auto && visible,
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
			return suggester.suggestBranch(ctx)
		},
		SuggestCommit: func(ctx context.Context) (string, error) {
			return suggester.suggestCommit(ctx)
		},
		Track: func(action string, fields map[string]any) {
			telemetry.Track("wizard", mergeTelemetryFields(fields, telemetry.Fields{"action": action}))
		},
	}

	telemetry.Pageview("/wizard", telemetry.Fields{
		"entrypoint":          wizardEntrypoint(auto),
		"needs_branch":        state.needsBranch(),
		"is_dirty":            state.dirty,
		"detached":            state.detached,
		"current_branch_role": wizardBranchRole(state.currentBranch, state.defaultBranch, state.detached),
	})

	run := wizardRun
	if auto && !visible {
		run = wizardRunAuto
	}

	res, err := run(wizCfg)
	if err == nil && res.Err != nil {
		err = res.Err
	}
	if err == nil {
		telemetry.Track("wizard", telemetry.Fields{
			"action":         "result",
			"status":         wizardResultStatus(res),
			"branch_created": res.BranchCreated,
			"commit_made":    res.CommitMade,
			"pushed":         res.Pushed,
		})
	}
	return res, err
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
func waitForActiveRun(ctx context.Context, client *ipc.Client, repoID, branch string, timeout time.Duration) (*ipc.RunInfo, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()

	poll := time.NewTicker(150 * time.Millisecond)
	defer poll.Stop()

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var result ipc.GetActiveRunResult
		if err := client.Call(ipc.MethodGetActiveRun, &ipc.GetActiveRunParams{RepoID: repoID, Branch: branch}, &result); err != nil {
			return nil, err
		}
		if result.Run != nil {
			return result.Run, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, nil
		case <-poll.C:
		}
	}
}

func wizardBranchRole(currentBranch, defaultBranch string, detached bool) string {
	if detached {
		return "detached"
	}
	if currentBranch != "" && currentBranch == defaultBranch {
		return "default"
	}
	return "feature"
}

func wizardResultStatus(res wizard.Result) string {
	if res.Success {
		return "completed"
	}
	if res.Aborted {
		return "aborted"
	}
	return "closed"
}

func wizardEntrypoint(auto bool) string {
	if auto {
		return "wizard_auto"
	}
	return "wizard"
}

func mergeTelemetryFields(fields map[string]any, extra telemetry.Fields) telemetry.Fields {
	merged := make(telemetry.Fields, len(fields)+len(extra))
	for k, v := range fields {
		merged[k] = v
	}
	for k, v := range extra {
		merged[k] = v
	}
	return merged
}

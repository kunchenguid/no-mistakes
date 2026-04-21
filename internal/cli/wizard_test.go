package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/telemetry"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/kunchenguid/no-mistakes/internal/wizard"
)

func TestShouldRouteToWizard(t *testing.T) {
	tests := []struct {
		name  string
		state repoState
		want  bool
	}{
		{
			name:  "detached HEAD, clean",
			state: repoState{currentBranch: "HEAD", defaultBranch: "main", detached: true, dirty: false},
			want:  true,
		},
		{
			name:  "detached HEAD, dirty",
			state: repoState{currentBranch: "HEAD", defaultBranch: "main", detached: true, dirty: true},
			want:  true,
		},
		{
			name:  "default branch, dirty — defer to active-run check",
			state: repoState{currentBranch: "main", defaultBranch: "main", dirty: true},
			want:  false,
		},
		{
			name:  "default branch, clean",
			state: repoState{currentBranch: "main", defaultBranch: "main", dirty: false},
			want:  false,
		},
		{
			name:  "feature branch, dirty",
			state: repoState{currentBranch: "feat/x", defaultBranch: "main", dirty: true},
			want:  false,
		},
		{
			name:  "feature branch, clean",
			state: repoState{currentBranch: "feat/x", defaultBranch: "main", dirty: false},
			want:  false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.state.shouldRouteToWizard(); got != tc.want {
				t.Fatalf("shouldRouteToWizard() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestNeedsBranch(t *testing.T) {
	tests := []struct {
		name  string
		state repoState
		want  bool
	}{
		{"default branch", repoState{currentBranch: "main", defaultBranch: "main"}, true},
		{"feature branch", repoState{currentBranch: "feat/x", defaultBranch: "main"}, false},
		{"detached HEAD", repoState{currentBranch: "HEAD", defaultBranch: "main", detached: true}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.state.needsBranch(); got != tc.want {
				t.Fatalf("needsBranch() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestWizardAgentSuggester_IsLazy(t *testing.T) {
	t.Helper()

	lookups := 0
	suggester := newWizardAgentSuggester(&config.Config{Agent: types.AgentAuto}, "/tmp/repo", func(context.Context, *config.Config) error {
		lookups++
		return errors.New("no supported agent found")
	}, nil)
	defer suggester.Close()

	if lookups != 0 {
		t.Fatalf("expected no agent resolution during setup, got %d", lookups)
	}

	if _, err := suggester.suggestBranch(context.Background()); err == nil {
		t.Fatal("expected suggestion to fail when no agent is available")
	}
	if lookups != 1 {
		t.Fatalf("expected one lazy resolution attempt, got %d", lookups)
	}
}

type fakeSuggesterAgent struct {
	mu    sync.Mutex
	calls []agent.RunOpts
}

type fakeSuggesterResponseAgent struct {
	mu              sync.Mutex
	combinedOutputs []string
	commitOutput    string
	calls           []agent.RunOpts
}

func (f *fakeSuggesterAgent) Name() string { return "fake" }
func (f *fakeSuggesterAgent) Run(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
	f.mu.Lock()
	f.calls = append(f.calls, opts)
	f.mu.Unlock()
	if strings.Contains(opts.Prompt, "Branch name rules") {
		return &agent.Result{
			Output: json.RawMessage(`{"branch":"feat/auto","subject":"feat(cli): auto thing"}`),
		}, nil
	}
	if strings.Contains(opts.Prompt, `{"subject":"..."}`) {
		return &agent.Result{
			Output: json.RawMessage(`{"subject":"feat(cli): standalone"}`),
		}, nil
	}
	return &agent.Result{Output: json.RawMessage(`{}`)}, nil
}
func (f *fakeSuggesterAgent) Close() error { return nil }

func (f *fakeSuggesterAgent) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeSuggesterResponseAgent) Name() string { return "fake" }

func (f *fakeSuggesterResponseAgent) Run(_ context.Context, opts agent.RunOpts) (*agent.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, opts)
	if strings.Contains(opts.Prompt, "Branch name rules") {
		if len(f.combinedOutputs) == 0 {
			return &agent.Result{Output: json.RawMessage(`{"branch":"feat/default"}`)}, nil
		}
		output := f.combinedOutputs[0]
		f.combinedOutputs = f.combinedOutputs[1:]
		return &agent.Result{Output: json.RawMessage(output)}, nil
	}
	if strings.Contains(opts.Prompt, `{"subject":"..."}`) {
		return &agent.Result{Output: json.RawMessage(f.commitOutput)}, nil
	}
	return &agent.Result{Output: json.RawMessage(`{}`)}, nil
}

func (f *fakeSuggesterResponseAgent) Close() error { return nil }

func (f *fakeSuggesterResponseAgent) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func newFakeSuggester(t *testing.T, ag *fakeSuggesterAgent) *wizardAgentSuggester {
	t.Helper()
	return newWizardAgentSuggester(
		&config.Config{Agent: types.AgentClaude},
		"/tmp/repo",
		func(context.Context, *config.Config) error { return nil },
		func(types.AgentName, string) (agent.Agent, error) { return ag, nil },
	)
}

func TestWizardAgentSuggester_CachesCommitFromBranchCall(t *testing.T) {
	ag := &fakeSuggesterAgent{}
	s := newFakeSuggester(t, ag)
	defer s.Close()

	branch, err := s.suggestBranch(context.Background())
	if err != nil {
		t.Fatalf("suggestBranch failed: %v", err)
	}
	if branch != "feat/auto" {
		t.Fatalf("unexpected branch: %q", branch)
	}

	commit, err := s.suggestCommit(context.Background())
	if err != nil {
		t.Fatalf("suggestCommit failed: %v", err)
	}
	if commit != "feat(cli): auto thing" {
		t.Fatalf("expected cached commit, got %q", commit)
	}

	if got := ag.callCount(); got != 1 {
		t.Fatalf("expected single combined agent call, got %d", got)
	}
}

func TestWizardAgentSuggester_FallsBackToCommitCall(t *testing.T) {
	// User typed the branch manually, so suggestBranch is never invoked and
	// the cache stays empty — suggestCommit should fall back to its own call.
	ag := &fakeSuggesterAgent{}
	s := newFakeSuggester(t, ag)
	defer s.Close()

	commit, err := s.suggestCommit(context.Background())
	if err != nil {
		t.Fatalf("suggestCommit failed: %v", err)
	}
	if commit != "feat(cli): standalone" {
		t.Fatalf("expected standalone commit, got %q", commit)
	}

	if got := ag.callCount(); got != 1 {
		t.Fatalf("expected one standalone agent call, got %d", got)
	}
}

func TestWizardAgentSuggester_CacheConsumedOnce(t *testing.T) {
	// Consuming the cache should reset it so a subsequent commit call goes
	// back through the agent (guards against stale state if the wizard ever
	// calls suggestCommit twice).
	ag := &fakeSuggesterAgent{}
	s := newFakeSuggester(t, ag)
	defer s.Close()

	if _, err := s.suggestBranch(context.Background()); err != nil {
		t.Fatalf("suggestBranch failed: %v", err)
	}
	if _, err := s.suggestCommit(context.Background()); err != nil {
		t.Fatalf("first suggestCommit failed: %v", err)
	}
	if _, err := s.suggestCommit(context.Background()); err != nil {
		t.Fatalf("second suggestCommit failed: %v", err)
	}
	if got := ag.callCount(); got != 2 {
		t.Fatalf("expected combined+standalone agent calls (2), got %d", got)
	}
}

func TestWizardAgentSuggester_CanceledContextSkipsCachedCommit(t *testing.T) {
	ag := &fakeSuggesterAgent{}
	s := newFakeSuggester(t, ag)
	defer s.Close()

	if _, err := s.suggestBranch(context.Background()); err != nil {
		t.Fatalf("suggestBranch failed: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	commit, err := s.suggestCommit(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("suggestCommit error = %v, want %v", err, context.Canceled)
	}
	if commit != "" {
		t.Fatalf("suggestCommit returned %q, want empty commit", commit)
	}

	if got := ag.callCount(); got != 1 {
		t.Fatalf("expected no extra agent call, got %d calls", got)
	}

	commit, err = s.suggestCommit(context.Background())
	if err != nil {
		t.Fatalf("subsequent suggestCommit failed: %v", err)
	}
	if commit != "feat(cli): auto thing" {
		t.Fatalf("expected cached commit after retry, got %q", commit)
	}

	if got := ag.callCount(); got != 1 {
		t.Fatalf("expected combined agent call only after retry, got %d", got)
	}
}

func TestWizardAgentSuggester_EmptyRetryClearsCachedCommit(t *testing.T) {
	ag := &fakeSuggesterResponseAgent{
		combinedOutputs: []string{
			`{"branch":"feat/first","subject":"feat(cli): first"}`,
			`{"branch":"feat/second"}`,
		},
		commitOutput: `{"subject":"feat(cli): standalone"}`,
	}
	s := newWizardAgentSuggester(
		&config.Config{Agent: types.AgentClaude},
		"/tmp/repo",
		func(context.Context, *config.Config) error { return nil },
		func(types.AgentName, string) (agent.Agent, error) { return ag, nil },
	)
	defer s.Close()

	if _, err := s.suggestBranch(context.Background()); err != nil {
		t.Fatalf("first suggestBranch failed: %v", err)
	}
	if _, err := s.suggestBranch(context.Background()); err != nil {
		t.Fatalf("second suggestBranch failed: %v", err)
	}

	commit, err := s.suggestCommit(context.Background())
	if err != nil {
		t.Fatalf("suggestCommit failed: %v", err)
	}
	if commit != "feat(cli): standalone" {
		t.Fatalf("suggestCommit = %q, want standalone subject", commit)
	}

	if got := ag.callCount(); got != 3 {
		t.Fatalf("expected two branch calls and one commit call, got %d", got)
	}
}

func TestRunWizardTracksPageview(t *testing.T) {
	recorder := &telemetryRecorder{}
	restoreTelemetry := telemetry.SetDefaultForTesting(recorder)
	defer restoreTelemetry()

	prevRun := wizardRun
	wizardRun = func(cfg wizard.Config) (wizard.Result, error) {
		return wizard.Result{Success: true, BranchCreated: true, CommitMade: true, Pushed: true}, nil
	}
	defer func() { wizardRun = prevRun }()

	nmHome := makeSocketSafeTempDir(t)
	t.Setenv("NM_HOME", nmHome)
	p := paths.WithRoot(nmHome)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	repoDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repoDir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	state := &repoState{
		workDir:       repoDir,
		currentBranch: "main",
		defaultBranch: "main",
		detached:      false,
		dirty:         true,
	}

	if _, err := runWizard(context.Background(), p, state, nil); err != nil {
		t.Fatalf("runWizard() error = %v", err)
	}

	event := recorder.find("pageview", "path", "/wizard")
	if event == nil {
		t.Fatal("expected wizard pageview telemetry")
	}
	if got := event.fields["needs_branch"]; got != true {
		t.Fatalf("needs_branch = %v, want true", got)
	}
	if got := event.fields["is_dirty"]; got != true {
		t.Fatalf("is_dirty = %v, want true", got)
	}
	if got := event.fields["detached"]; got != false {
		t.Fatalf("detached = %v, want false", got)
	}
	if got := event.fields["entrypoint"]; got != "wizard" {
		t.Fatalf("entrypoint = %v, want wizard", got)
	}
	if got := event.fields["current_branch_role"]; got != "default" {
		t.Fatalf("current_branch_role = %v, want default", got)
	}
	resultEvent := recorder.find("wizard", "action", "result")
	if resultEvent == nil {
		t.Fatal("expected wizard result telemetry")
	}
	if got := resultEvent.fields["status"]; got != "completed" {
		t.Fatalf("status = %v, want completed", got)
	}
	if got := resultEvent.fields["branch_created"]; got != true {
		t.Fatalf("branch_created = %v, want true", got)
	}
}

func TestRunWizardReturnsTerminalWizardError(t *testing.T) {
	wantErr := errors.New("suggest branch: agent down")

	prevRun := wizardRun
	wizardRun = func(cfg wizard.Config) (wizard.Result, error) {
		return wizard.Result{Err: wantErr}, nil
	}
	defer func() { wizardRun = prevRun }()

	nmHome := makeSocketSafeTempDir(t)
	t.Setenv("NM_HOME", nmHome)
	p := paths.WithRoot(nmHome)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}

	repoDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repoDir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	state := &repoState{
		workDir:       repoDir,
		currentBranch: "main",
		defaultBranch: "main",
		dirty:         true,
	}

	_, err := runWizard(context.Background(), p, state, nil)
	if !errors.Is(err, wantErr) {
		t.Fatalf("runWizard() error = %v, want %v", err, wantErr)
	}
}

// TestRunWizard_ConfiguresServerPIDsDir ensures the wizard points the
// agent package at its PID tracking dir while running, so orphaned
// opencode/rovodev processes left behind by a wizard crash can be reaped
// by the next daemon startup. It also verifies the setting is cleared
// afterwards.
func TestRunWizard_ConfiguresServerPIDsDir(t *testing.T) {
	prevRun := wizardRun
	var observedDir string
	wizardRun = func(cfg wizard.Config) (wizard.Result, error) {
		observedDir = agent.CurrentServerPIDsDir()
		return wizard.Result{Success: true}, nil
	}
	defer func() { wizardRun = prevRun }()

	nmHome := makeSocketSafeTempDir(t)
	t.Setenv("NM_HOME", nmHome)
	p := paths.WithRoot(nmHome)
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	// Start from a known-empty setting so we can tell if the wizard set it.
	agent.SetServerPIDsDir("")
	t.Cleanup(func() { agent.SetServerPIDsDir("") })

	repoDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repoDir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	state := &repoState{
		workDir:       repoDir,
		currentBranch: "main",
		defaultBranch: "main",
		dirty:         true,
	}

	if _, err := runWizard(context.Background(), p, state); err != nil {
		t.Fatalf("runWizard() error = %v", err)
	}

	if want := p.ServerPIDsDir(); observedDir != want {
		t.Fatalf("during wizard, CurrentServerPIDsDir = %q, want %q", observedDir, want)
	}
	if got := agent.CurrentServerPIDsDir(); got != "" {
		t.Fatalf("after wizard, CurrentServerPIDsDir = %q, want empty", got)
	}
}

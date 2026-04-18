package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

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

	if _, err := runWizard(context.Background(), p, state); err != nil {
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

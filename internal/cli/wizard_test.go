package cli

import (
	"context"
	"errors"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/types"
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

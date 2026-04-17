package cli

import "testing"

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

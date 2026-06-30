package pipeline

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
)

func TestResolveIntegrationBranch_Precedence(t *testing.T) {
	tests := []struct {
		name        string
		runOverride string
		repo        *db.Repo
		want        string
	}{
		{
			name: "no override falls back to default branch",
			repo: &db.Repo{DefaultBranch: "main"},
			want: "main",
		},
		{
			name: "persisted base branch wins over default",
			repo: &db.Repo{BaseBranch: "develop", DefaultBranch: "main"},
			want: "develop",
		},
		{
			name:        "run override wins over persisted base",
			runOverride: "release/1.4",
			repo:        &db.Repo{BaseBranch: "develop", DefaultBranch: "main"},
			want:        "release/1.4",
		},
		{
			name: "empty default falls back to main",
			repo: &db.Repo{},
			want: "main",
		},
		{
			name:        "whitespace override is ignored",
			runOverride: "   ",
			repo:        &db.Repo{BaseBranch: "develop", DefaultBranch: "main"},
			want:        "develop",
		},
		{
			name: "nil repo with no override falls back to main",
			want: "main",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ResolveIntegrationBranch(tt.runOverride, tt.repo); got != tt.want {
				t.Fatalf("ResolveIntegrationBranch(%q, %+v) = %q, want %q", tt.runOverride, tt.repo, got, tt.want)
			}
		})
	}
}

func TestStepContext_IntegrationBranch(t *testing.T) {
	sctx := &StepContext{
		Run:  &db.Run{BaseBranch: "develop"},
		Repo: &db.Repo{BaseBranch: "staging", DefaultBranch: "main"},
	}
	if got := sctx.IntegrationBranch(); got != "develop" {
		t.Fatalf("IntegrationBranch with run override = %q, want develop", got)
	}

	sctx.Run.BaseBranch = ""
	if got := sctx.IntegrationBranch(); got != "staging" {
		t.Fatalf("IntegrationBranch with repo base = %q, want staging", got)
	}
}

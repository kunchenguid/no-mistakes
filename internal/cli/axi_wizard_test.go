package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestAxiWizardShowsRoutedStandaloneHistory proves the operator surface renders
// a routed Wizard suggestion attempt recorded under a standalone utility scope,
// and that surfacing that history fabricates no pipeline gate rows.
func TestAxiWizardShowsRoutedStandaloneHistory(t *testing.T) {
	repoDir := t.TempDir()
	nmHome := t.TempDir()
	t.Setenv("NM_HOME", nmHome)
	run(t, repoDir, "git", "init")
	run(t, repoDir, "git", "config", "user.email", "test@test.com")
	run(t, repoDir, "git", "config", "user.name", "Test")
	run(t, repoDir, "git", "commit", "--allow-empty", "-m", "initial")
	rawRoot, err := filepath.EvalSymlinks(repoDir)
	if err != nil {
		rawRoot = repoDir
	}
	chdir(t, rawRoot)

	p := paths.WithRoot(nmHome)
	if err := p.EnsureDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer database.Close()
	repo, err := database.InsertRepoWithID("repo-1", rawRoot, "origin", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}

	// Record a routed Wizard suggestion attempt under a standalone utility scope
	// via the same utility routing invoker the Wizard uses in production.
	scope, err := database.InsertUtilityScope(types.UtilityScopeWizard, os.Getpid())
	if err != nil {
		t.Fatalf("insert utility scope: %v", err)
	}
	invoker := pipeline.NewUtilityRoutingInvoker(nil, config.DefaultRoutingConfig(), database,
		func(types.AgentName, string) (agent.Agent, error) { return &fakeSuggesterAgent{}, nil })
	if _, err := invoker.Invoke(context.Background(), agent.InvocationRequest{
		Purpose: types.PurposeBranchCommitSuggestion,
		Scope:   types.InvocationScope{Kind: types.InvocationScopeUtility, UtilityScopeID: scope.ID},
		Payload: agent.RunOpts{Prompt: "Branch name rules apply"},
	}); err != nil {
		t.Fatalf("record routed wizard attempt: %v", err)
	}

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(&out)
	if err := runAxiWizard(cmd, 10); err != nil {
		t.Fatalf("axi wizard: %v\n%s", err, out.String())
	}
	got := out.String()
	for _, want := range []string{
		"surface: wizard-suggestion-history",
		"sessions: 1",
		"branch_commit_suggestion",
		"prose_fast",
		"gpt-5.6-luna",
		"succeeded",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("axi wizard missing %q in:\n%s", want, got)
		}
	}

	// The standalone history must never fabricate pipeline gate rows.
	runs, err := database.GetRunsByRepo(repo.ID)
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(runs) != 0 {
		t.Fatalf("wizard history fabricated %d pipeline run rows, want 0", len(runs))
	}
}

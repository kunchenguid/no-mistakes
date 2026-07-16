package cli

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/routing"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"github.com/spf13/cobra"
)

func TestAxiRouteEvidenceRendersStableDecisionAndPostResultArrays(t *testing.T) {
	setupTestRepo(t)
	p := paths.WithRoot(os.Getenv("NM_HOME"))
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	gitRoot, err := git.FindGitRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	repo, err := database.InsertRepoWithID("repo-route", gitRoot, "origin", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "route-canary", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.InsertRouteDecision(db.RouteDecision{
		RunID: run.ID, StepName: string(types.StepReview), Round: 1,
		RequestedHarness: "codex", EffectiveHarness: "codex",
		RequestedModel: routing.ModelLuna, EffectiveModel: routing.ModelLuna,
		RequestedEffort: routing.EffortXHigh, EffectiveEffort: routing.EffortXHigh,
		PolicyVersion: routing.PolicyVersion, Phase: "review", Risk: "unknown",
		Reason: "default initial/default work", SourceConfiguration: "cfg",
		ConfigurationGeneration: "gen-1", Repository: "github.com/example/repo",
	}); err != nil {
		t.Fatal(err)
	}
	if err := database.InsertRouteResult(db.RouteResult{
		RunID: run.ID, StepName: string(types.StepReview), Round: 1,
		Phase: "review", Risk: string(routing.RiskMedium),
	}); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(&out)
	if _, err := runAxiRouteEvidence(cmd, run.ID); err != nil {
		t.Fatalf("route evidence: %v\n%s", err, out.String())
	}
	got := out.String()
	for _, want := range []string{
		"run: \"" + run.ID + "\"",
		"route_decisions[1]{id,step,round,requested_harness,effective_harness,requested_model,effective_model,requested_effort,effective_effort,policy_version,phase,risk,reason,source_configuration,configuration_generation,repository,prompt_transport,created_at}:",
		"review_results[1]{id,step,round,phase,risk,append_seq,created_at}:",
		"cfg",
		"gen-1",
		"medium",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("route evidence missing %q:\n%s", want, got)
		}
	}
}

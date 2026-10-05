package cli

import (
	"context"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// A verified tag delivery completes the PR phase without a PR; axi must report
// it as passed, not as passed-with-skips (missing publication evidence).
func TestAxiOutcomeVerifiedTagDeliveryIsPassed(t *testing.T) {
	dir, p, database, repo := setupAxiQueryRepo(t)
	head, err := git.HeadSHA(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	upstream := t.TempDir()
	run(t, upstream, "git", "init", "--bare", "-q")
	run(t, dir, "git", "tag", "-a", "v1", "-m", "release", head)
	run(t, dir, "git", "remote", "add", "origin", "https://github.com/up/repo")
	run(t, dir, "git", "config", "url."+upstream+".insteadOf", "https://github.com/up/repo")
	run(t, dir, "git", "push", "-q", "origin", "refs/tags/v1")
	repo.UpstreamURL = "https://github.com/up/repo"
	r, err := database.InsertRun(repo.ID, "refs/tags/v1", head, head)
	if err != nil {
		t.Fatal(err)
	}
	executor := pipeline.NewExecutor(database, p, nil, nil, []pipeline.Step{&steps.PRStep{}}, nil)
	if err := executor.Execute(context.Background(), r, repo, dir); err != nil {
		t.Fatal(err)
	}
	r, err = database.GetRun(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	results, err := database.GetStepsByRun(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != types.RunCompleted || len(results) != 1 || results[0].Status != types.StepStatusCompleted {
		t.Fatalf("run = %+v, steps = %+v", r, results)
	}
	if got := outcomeForRun(runViewFromDB(r, results, database)); got != "passed" {
		t.Fatalf("verified tag delivery: outcome = %q, want passed", got)
	}
}

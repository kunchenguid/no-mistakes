package pipeline

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func coverageFixtureOutcome(t *testing.T, sctx *StepContext, reviewed, reviewable []string) *StepOutcome {
	t.Helper()
	f := types.Findings{Items: []types.Finding{}, ReviewedPaths: reviewed, ReviewCoverageScope: &types.ReviewCoverageScope{Version: 1, RunID: sctx.Run.ID, StepResultID: sctx.StepResultID, HeadSHA: sctx.Run.HeadSHA, RunBaseSHA: sctx.Run.BaseSHA, BaseSHA: sctx.Run.BaseSHA, TrustedConfigSHA: "fixture-trusted", Paths: reviewable}}
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	return &StepOutcome{NeedsApproval: true, Findings: string(b), ReviewedPaths: reviewed, ReviewablePaths: reviewable, ReviewApprovedHeadSHA: sctx.Run.HeadSHA}
}

func coverageWorkdir(t *testing.T, database *db.DB, run *db.Run) string {
	t.Helper()
	dir := t.TempDir()
	ctx := context.Background()
	if _, err := git.Run(ctx, dir, "init"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"first.go", "second.go"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("package fixture\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, ".gitignore"), []byte(".tmp-evidence/\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := git.Run(ctx, dir, "add", "first.go", "second.go", ".gitignore"); err != nil {
		t.Fatal(err)
	}
	if _, err := git.Run(ctx, dir, "-c", "core.hooksPath=/dev/null", "-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "fixture"); err != nil {
		t.Fatal(err)
	}
	head, err := git.HeadSHA(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunHeadSHA(run.ID, head); err != nil {
		t.Fatal(err)
	}
	run.HeadSHA = head
	return dir
}

func coverageBinding(t *testing.T, database *db.DB, runID string) *ipc.CoverageBinding {
	t.Helper()
	run, err := database.GetRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	steps, err := database.GetStepsByRun(runID)
	if err != nil || len(steps) != 1 {
		t.Fatalf("read disposable review step: %v", err)
	}
	rounds, err := database.GetRoundsByStep(steps[0].ID)
	if err != nil || len(rounds) == 0 {
		t.Fatalf("read disposable review round: %v", err)
	}
	return &ipc.CoverageBinding{HeadSHA: run.HeadSHA, StepResultID: steps[0].ID, RoundID: rounds[len(rounds)-1].ID}
}

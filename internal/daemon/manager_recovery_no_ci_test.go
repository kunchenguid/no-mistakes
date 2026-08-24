package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestRecoveredRunPreservesRecordedNoCITopology(t *testing.T) {
	tests := []struct {
		name        string
		recordedCI  bool
		currentNoCI bool
		wantCI      bool
	}{
		{name: "legacy CI record under trusted no_ci", recordedCI: true, currentNoCI: true, wantCI: true},
		{name: "omitted CI after policy is disabled", recordedCI: false, currentNoCI: false, wantCI: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			database, run := parkedRecoveryRun(t, tt.recordedCI)
			cfg := &config.Config{NoCI: tt.currentNoCI}
			manager := NewRunManager(database, nil, nil)

			execSteps, err := manager.stepsForRecoveredRun(cfg, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := pipeline.ValidateRecoveredRun(database, run, execSteps); err != nil {
				t.Fatalf("recorded topology was not recoverable: %v", err)
			}

			hasCI := false
			for _, step := range execSteps {
				if step.Name() == types.StepCI {
					hasCI = true
				}
			}
			if hasCI != tt.wantCI {
				t.Fatalf("recovered CI step = %v, want %v", hasCI, tt.wantCI)
			}
		})
	}
}

func TestRecoveredLegacyCIUnderTrustedNoCIDoesNotCallForge(t *testing.T) {
	database, run := parkedRecoveryRun(t, true)
	manager := NewRunManager(database, nil, nil)
	execSteps, err := manager.stepsForRecoveredRun(&config.Config{NoCI: true}, run.ID)
	if err != nil {
		t.Fatal(err)
	}

	ghDir, ghLog := writeMockGHState(t, t.TempDir(), "OPEN")
	t.Setenv("PATH", ghDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	executedCI := false
	for _, step := range execSteps {
		if step.Name() != types.StepCI {
			continue
		}
		executedCI = true
		outcome, err := step.Execute(&pipeline.StepContext{Ctx: context.Background()})
		if err != nil {
			t.Fatal(err)
		}
		if outcome == nil || !outcome.Skipped {
			t.Fatalf("recovered legacy CI outcome = %#v, want skipped", outcome)
		}
	}
	if !executedCI {
		t.Fatal("recorded CI topology did not produce a recovery step")
	}
	forgeCalls, err := os.ReadFile(ghLog)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if len(forgeCalls) != 0 {
		t.Fatalf("trusted no_ci recovery called forge: %q", forgeCalls)
	}
}

func parkedRecoveryRun(t *testing.T, includeCI bool) (*db.DB, *db.Run) {
	t.Helper()
	database, err := db.Open(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	repo, err := database.InsertRepoWithID("recovery-no-ci", t.TempDir(), "https://github.com/test/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := database.InsertRun(repo.ID, "feature/no-ci", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}

	names := types.AllSteps()
	if !includeCI {
		names = names[:len(names)-1]
	}
	findings := `{"findings":[{"id":"review-1","severity":"warning","description":"needs approval","action":"ask-user"}],"summary":"needs approval"}`
	for _, name := range names {
		result, err := database.InsertStepResult(run.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		switch name {
		case types.StepIntent, types.StepRebase:
			if err := database.CompleteStep(result.ID, 0, 1, ""); err != nil {
				t.Fatal(err)
			}
		case types.StepReview:
			if err := database.StartStep(result.ID); err != nil {
				t.Fatal(err)
			}
			if err := database.SetStepFindings(result.ID, findings); err != nil {
				t.Fatal(err)
			}
			if _, err := database.InsertReviewStepRound(result.ID, 1, "initial", &findings, nil, run.HeadSHA, 1); err != nil {
				t.Fatal(err)
			}
			if err := database.UpdateStepStatusWithDuration(result.ID, types.StepStatusAwaitingApproval, 1); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := database.SetRunAwaitingAgent(run.ID); err != nil {
		t.Fatal(err)
	}
	stored, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	return database, stored
}

package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
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

			execSteps, err := manager.stepsForRecoveredRun(cfg, run)
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

func TestRecoveredLegacyOmittedCISurvivesThreeRestartsWithoutForge(t *testing.T) {
	database, run := parkedRecoveryRunAt(t, true, types.StepCI)
	manager := NewRunManager(database, nil, nil)
	var execSteps []pipeline.Step
	for restart, noCI := range []bool{true, false, true} {
		var err error
		execSteps, err = manager.stepsForRecoveredRun(&config.Config{NoCI: noCI}, run)
		if err != nil {
			t.Fatalf("restart %d: %v", restart+1, err)
		}
		scheduled := scheduledStepNames(execSteps)
		if got := scheduled[len(scheduled)-1]; got != types.StepLegacyOmittedCI {
			t.Fatalf("restart %d schedule ended in %q, want %q", restart+1, got, types.StepLegacyOmittedCI)
		}
		if err := database.SetRunScheduledSteps(run.ID, scheduled); err != nil {
			t.Fatalf("restart %d publish schedule: %v", restart+1, err)
		}
		run, err = database.GetRun(run.ID)
		if err != nil {
			t.Fatalf("restart %d reload run: %v", restart+1, err)
		}
		if got := run.ScheduledSteps[len(run.ScheduledSteps)-1]; got != types.StepLegacyOmittedCI {
			t.Fatalf("restart %d persisted schedule ended in %q, want %q", restart+1, got, types.StepLegacyOmittedCI)
		}
	}

	ghDir, ghLog := writeMockGHState(t, t.TempDir(), "OPEN")
	t.Setenv("PATH", ghDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	repo, err := database.GetRepo(run.RepoID)
	if err != nil {
		t.Fatal(err)
	}
	executor := pipeline.NewExecutor(database, p, &config.Config{NoCI: false}, nil, execSteps, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := executor.Resume(ctx, run, repo, ""); err != nil {
		t.Fatalf("resume recovered CI gate: %v", err)
	}
	stored, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Status != types.RunCompleted {
		t.Fatalf("recovered run status = %s, want completed", stored.Status)
	}
	results, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if results[len(results)-1].StepName != types.StepCI || results[len(results)-1].Status != types.StepStatusCompleted {
		t.Fatalf("recovered CI gate = %s %s, want completed", results[len(results)-1].StepName, results[len(results)-1].Status)
	}
	forgeCalls, err := os.ReadFile(ghLog)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if len(forgeCalls) != 0 {
		t.Fatalf("legacy omitted-CI recovery called forge: %q", forgeCalls)
	}
}

func TestLegacyOmittedCIScheduleSnapshotPresentsCI(t *testing.T) {
	database, run := parkedRecoveryRunAt(t, true, types.StepReview)
	manager := NewRunManager(database, nil, nil)
	execSteps, err := manager.stepsForRecoveredRun(&config.Config{NoCI: true}, run)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetRunScheduledSteps(run.ID, scheduledStepNames(execSteps)); err != nil {
		t.Fatal(err)
	}
	run, err = database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := run.ScheduledSteps[len(run.ScheduledSteps)-1]; got != types.StepLegacyOmittedCI {
		t.Fatalf("persisted schedule ended in %q, want %q", got, types.StepLegacyOmittedCI)
	}
	results, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	info := runToInfo(database, run, results)
	if got := info.ScheduledSteps[len(info.ScheduledSteps)-1]; got != types.StepCI {
		t.Fatalf("snapshot schedule ended in %q, want user-facing %q", got, types.StepCI)
	}
	for _, name := range info.ScheduledSteps {
		if name == types.StepLegacyOmittedCI {
			t.Fatalf("snapshot exposed internal schedule identity %q", name)
		}
	}
}

func TestRecoveredFinalizedCIScheduleIsNotSkippedByLaterNoCIPolicy(t *testing.T) {
	database, run := parkedRecoveryRunAt(t, true, types.StepCI)
	if err := database.SetRunScheduledSteps(run.ID, types.AllSteps()); err != nil {
		t.Fatal(err)
	}
	run, err := database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	// This test isolates recovered topology selection from the independent git
	// head-continuity guard; the parked run has no real worktree.
	run.HeadSHA = ""

	manager := NewRunManager(database, nil, nil)
	var execSteps []pipeline.Step
	for restart, noCI := range []bool{false, true} {
		execSteps, err = manager.stepsForRecoveredRun(&config.Config{NoCI: noCI}, run)
		if err != nil {
			t.Fatalf("restart %d: %v", restart+1, err)
		}
		if got := scheduledStepNames(execSteps)[len(execSteps)-1]; got != types.StepCI {
			t.Fatalf("restart %d schedule ended in %q, want %q", restart+1, got, types.StepCI)
		}
	}

	ghDir, ghLog := writeMockGHState(t, t.TempDir(), "OPEN")
	t.Setenv("PATH", ghDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	p := paths.WithRoot(t.TempDir())
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	repo, err := database.GetRepo(run.RepoID)
	if err != nil {
		t.Fatal(err)
	}
	executor := pipeline.NewExecutor(database, p, &config.Config{NoCI: true}, nil, execSteps, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := executor.Resume(ctx, run, repo, ""); err == nil {
		t.Fatal("resume unexpectedly skipped finalized CI gate")
	}
	forgeCalls, err := os.ReadFile(ghLog)
	if err != nil {
		t.Fatal(err)
	}
	if len(forgeCalls) == 0 {
		t.Fatal("finalized CI schedule performed no forge call; later no_ci policy reinterpreted durable topology")
	}
}

func parkedRecoveryRun(t *testing.T, includeCI bool) (*db.DB, *db.Run) {
	return parkedRecoveryRunAt(t, includeCI, types.StepReview)
}

func parkedRecoveryRunAt(t *testing.T, includeCI bool, gateStep types.StepName) (*db.DB, *db.Run) {
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
	gateReached := false
	for _, name := range names {
		result, err := database.InsertStepResult(run.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		if name == gateStep {
			gateReached = true
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
		} else if !gateReached {
			if err := database.CompleteStep(result.ID, 0, 1, ""); err != nil {
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

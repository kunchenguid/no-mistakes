package db

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestGetStatsAggregatesReportedFixesAndRescueRuns(t *testing.T) {
	d := openTestDB(t)
	repoA, _ := d.InsertRepo("/repo/a", "git@example.com:a.git", "main")
	repoB, _ := d.InsertRepo("/repo/b", "git@example.com:b.git", "main")

	runA, _ := d.InsertRun(repoA.ID, "feature-a", "head-a", "base-a")
	reviewA, _ := d.InsertStepResult(runA.ID, types.StepReview)
	reviewAInitial := `{"findings":[{"id":"r1","severity":"warning","description":"one","action":"auto-fix"},{"id":"r2","severity":"warning","description":"two","action":"auto-fix"},{"id":"r3","severity":"warning","description":"three","action":"auto-fix"}],"summary":"three","risk_level":"medium","risk_rationale":"test"}`
	reviewAFinal := `{"findings":[{"id":"r3","severity":"warning","description":"three","action":"ask-user"}],"summary":"one left","risk_level":"medium","risk_rationale":"test"}`
	if _, err := d.InsertStepRound(reviewA.ID, 1, "initial", &reviewAInitial, nil, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := d.InsertStepRound(reviewA.ID, 2, "auto_fix", &reviewAFinal, nil, 100); err != nil {
		t.Fatal(err)
	}

	lintA, _ := d.InsertStepResult(runA.ID, types.StepLint)
	lintAInitial := `{"findings":[{"id":"l1","severity":"error","description":"lint","action":"auto-fix"}],"summary":"one","risk_level":"low","risk_rationale":"test"}`
	if _, err := d.InsertStepRound(lintA.ID, 1, "initial", &lintAInitial, nil, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := d.InsertStepRound(lintA.ID, 2, "auto_fix", nil, nil, 100); err != nil {
		t.Fatal(err)
	}

	runB, _ := d.InsertRun(repoB.ID, "feature-b", "head-b", "base-b")
	testB, _ := d.InsertStepResult(runB.ID, types.StepTest)
	testBInitial := `{"findings":[{"id":"t1","severity":"error","description":"test","action":"ask-user"}],"summary":"one","risk_level":"low","risk_rationale":"test"}`
	if _, err := d.InsertStepRound(testB.ID, 1, "initial", &testBInitial, nil, 100); err != nil {
		t.Fatal(err)
	}

	stats, err := d.GetStats()
	if err != nil {
		t.Fatalf("get stats: %v", err)
	}

	if stats.TotalRuns != 2 {
		t.Fatalf("TotalRuns = %d, want 2", stats.TotalRuns)
	}
	if stats.RescueRuns != 1 {
		t.Fatalf("RescueRuns = %d, want 1", stats.RescueRuns)
	}
	if stats.ReportedFindings != 5 {
		t.Fatalf("ReportedFindings = %d, want 5", stats.ReportedFindings)
	}
	if stats.FixedFindings != 3 {
		t.Fatalf("FixedFindings = %d, want 3", stats.FixedFindings)
	}

	assertStepStat(t, stats.StepStats, types.StepReview, 3, 2)
	assertStepStat(t, stats.StepStats, types.StepLint, 1, 1)
	assertStepStat(t, stats.StepStats, types.StepTest, 1, 0)

	if len(stats.RepoStats) != 2 {
		t.Fatalf("len(RepoStats) = %d, want 2", len(stats.RepoStats))
	}
	if stats.RepoStats[0].WorkingPath != "/repo/a" {
		t.Fatalf("top repo = %q, want /repo/a", stats.RepoStats[0].WorkingPath)
	}
	if stats.RepoStats[0].RescueRuns != 1 || stats.RepoStats[0].FixedFindings != 3 {
		t.Fatalf("top repo stats = %#v, want rescue 1 fixes 3", stats.RepoStats[0])
	}
}

func TestGetStatsFallsBackToStepFindingsWhenRoundsAreMissing(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/repo/legacy", "git@example.com:legacy.git", "main")
	run, _ := d.InsertRun(repo.ID, "legacy", "head", "base")
	step, _ := d.InsertStepResult(run.ID, types.StepReview)
	findings := `{"findings":[{"id":"legacy-1","severity":"warning","description":"legacy","action":"ask-user"}],"summary":"one","risk_level":"low","risk_rationale":"test"}`
	if err := d.SetStepFindings(step.ID, findings); err != nil {
		t.Fatal(err)
	}

	stats, err := d.GetStats()
	if err != nil {
		t.Fatalf("get stats: %v", err)
	}

	if stats.ReportedFindings != 1 {
		t.Fatalf("ReportedFindings = %d, want 1", stats.ReportedFindings)
	}
	if stats.FixedFindings != 0 {
		t.Fatalf("FixedFindings = %d, want 0", stats.FixedFindings)
	}
	if stats.RescueRuns != 0 {
		t.Fatalf("RescueRuns = %d, want 0", stats.RescueRuns)
	}
}

func assertStepStat(t *testing.T, stats []StepStats, step types.StepName, reported int, fixes int) {
	t.Helper()
	for _, got := range stats {
		if got.StepName != step {
			continue
		}
		if got.ReportedFindings != reported || got.FixedFindings != fixes {
			t.Fatalf("step %s = reported %d fixes %d, want reported %d fixes %d", step, got.ReportedFindings, got.FixedFindings, reported, fixes)
		}
		return
	}
	t.Fatalf("missing stats for step %s", step)
}

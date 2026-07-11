package steps

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func ciRepairPersistenceContext(t *testing.T) *pipeline.StepContext {
	t.Helper()
	dir, baseSHA, headSHA := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, baseSHA, headSHA, config.Commands{})
	stepResult, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	round, err := sctx.DB.ReserveStepRound(stepResult.ID, 1, "initial")
	if err != nil {
		t.Fatal(err)
	}
	sctx.StepResultID = stepResult.ID
	sctx.CurrentRound = round
	return sctx
}

func persistCIPlan(t *testing.T, step *CIStep, sctx *pipeline.StepContext, plan ciRepairPlan) {
	t.Helper()
	ids, err := step.beginCIRepairs(sctx, plan, ciRepairBudget)
	if err != nil {
		t.Fatal(err)
	}
	if err := finishCIRepairs(sctx, ids, db.RepairVerdictUnresolved, "hosted check still failing", db.RepairStatusUnresolved); err != nil {
		t.Fatal(err)
	}
}

func TestCIStep_RestartRetainsHostedFailureLineageTier(t *testing.T) {
	sctx := ciRepairPersistenceContext(t)
	pr := &scm.PR{Number: "42", URL: "https://github.com/test/repo/pull/42"}
	firstStep := &CIStep{}
	first, err := firstStep.planCIRepair(sctx, pr, []string{"build"}, false, ciRepairBudget)
	if err != nil {
		t.Fatal(err)
	}
	if first.Tier != 0 || len(first.Issues) != 1 {
		t.Fatalf("first plan = %+v, want one tier-0 hosted failure", first)
	}
	persistCIPlan(t, firstStep, sctx, first)
	repairs, err := sctx.DB.GetFindingRepairsByLineage(first.Issues[0].LineageID)
	if err != nil {
		t.Fatal(err)
	}
	if len(repairs) != 1 || repairs[0].Tier != 0 || repairs[0].RemainingBudget != ciRepairBudget-1 {
		t.Fatalf("durable first repair = %+v, want tier 0 with %d remaining", repairs, ciRepairBudget-1)
	}

	restarted := &CIStep{}
	second, err := restarted.planCIRepair(sctx, pr, []string{"build"}, false, ciRepairBudget)
	if err != nil {
		t.Fatal(err)
	}
	if second.Tier != 1 || len(second.Issues) != 1 || second.Issues[0].LineageID != first.Issues[0].LineageID {
		t.Fatalf("restart plan = %+v, want same lineage at tier 1 after %+v", second, first)
	}
	if unresolved, err := sctx.DB.HasUnresolvedBlockingRepair(sctx.Run.ID); err != nil || !unresolved {
		t.Fatalf("unattended unresolved = %v, %v; want true for hosted CI lineage", unresolved, err)
	}
}

func TestCIStep_DistinctHostedFailuresHaveDistinctBudgets(t *testing.T) {
	sctx := ciRepairPersistenceContext(t)
	pr := &scm.PR{Number: "42", URL: "https://github.com/test/repo/pull/42"}
	step := &CIStep{}
	var buildLineage string
	for tier := range ciRepairBudget {
		plan, err := step.planCIRepair(sctx, pr, []string{"build"}, false, ciRepairBudget)
		if err != nil {
			t.Fatal(err)
		}
		if plan.Tier != tier || len(plan.Issues) != 1 {
			t.Fatalf("build plan %d = %+v", tier, plan)
		}
		if buildLineage == "" {
			buildLineage = plan.Issues[0].LineageID
		}
		persistCIPlan(t, step, sctx, plan)
	}
	exhausted, err := (&CIStep{}).planCIRepair(sctx, pr, []string{"build"}, false, ciRepairBudget)
	if err != nil {
		t.Fatal(err)
	}
	if !exhausted.Exhausted || len(exhausted.Issues) != 0 {
		t.Fatalf("exhausted build plan = %+v, want no further quality tier", exhausted)
	}
	if unresolved, err := sctx.DB.HasUnresolvedBlockingRepair(sctx.Run.ID); err != nil || !unresolved {
		t.Fatalf("unattended exhausted lineage = %v, %v; want fail closed", unresolved, err)
	}

	plan, err := (&CIStep{}).planCIRepair(sctx, pr, []string{"build", "test"}, false, ciRepairBudget)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Exhausted != true || plan.Tier != 0 || len(plan.Issues) != 1 || plan.Issues[0].Name != "test" {
		t.Fatalf("mixed exhausted/new plan = %+v, want fresh test failure at tier 0", plan)
	}
	if plan.Issues[0].LineageID == buildLineage {
		t.Fatal("distinct hosted failures shared a lineage and budget")
	}
}

func TestCIStep_HostedFailureJournalReadFailureIsFatal(t *testing.T) {
	sctx := ciRepairPersistenceContext(t)
	pr := &scm.PR{Number: "42", URL: "https://github.com/test/repo/pull/42"}
	if err := sctx.DB.Close(); err != nil {
		t.Fatal(err)
	}
	_, err := (&CIStep{}).planCIRepair(sctx, pr, []string{"build"}, false, ciRepairBudget)
	if err == nil {
		t.Fatal("expected closed repair journal to fail")
	}
	if !isCIJournalFailure(err) {
		t.Fatalf("error %v is not a fatal CI journal failure", err)
	}
}

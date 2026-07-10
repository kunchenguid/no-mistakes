package db

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func startScopedAttempt(t *testing.T, d *DB, scope types.InvocationScope, purpose types.Purpose, role types.InvocationRole, profile string, runner types.Runner) string {
	t.Helper()
	id, err := d.StartInvocationAttempt(types.InvocationAttemptStart{
		Purpose:      purpose,
		Role:         role,
		Scope:        scope,
		CandidateKey: profile + ":0:" + string(runner),
		Candidate:    types.InvocationCandidate{Profile: profile, Tier: 0, CandidateIndex: 0, Runner: runner, Model: "m", Effort: types.EffortMedium},
	})
	if err != nil {
		t.Fatalf("start %s attempt: %v", purpose, err)
	}
	return id
}

func TestFindingRepairLifecycle(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/repair", "git@github.com:user/repair.git", "main")
	run, _ := d.InsertRun(repo.ID, "feature", "abc", "def")
	step, _ := d.InsertStepResult(run.ID, types.StepReview)
	round, _ := d.ReserveStepRound(step.ID, 1, "initial")
	scope := types.InvocationScope{Kind: types.InvocationScopePipeline, RunID: run.ID, StepResultID: step.ID, StepRoundID: round.ID}

	reviewAttempt := startRoutedReviewAttempt(t, d, run, step, round)
	_ = d.FinishInvocationAttempt(reviewAttempt, types.InvocationAttemptTerminal{Outcome: types.InvocationOutcomeSucceeded})
	lineages, err := d.CreateFindingLineages(run.ID, reviewAttempt, []string{"review-1"})
	if err != nil || len(lineages) != 1 {
		t.Fatalf("create lineages: %v (%v)", lineages, err)
	}
	lineage := lineages[0]

	repairID, err := d.StartFindingRepair(FindingRepairStart{
		RunID: run.ID, LineageID: lineage.ID, StepResultID: step.ID, StepRoundID: round.ID,
		Severity: "error", Action: "auto-fix", Description: "nil dereference in parser", File: "parser.go", Line: 42,
		Tier: 0, RemainingBudget: 3,
	})
	if err != nil {
		t.Fatalf("start finding repair: %v", err)
	}

	fixerID := startScopedAttempt(t, d, scope, types.PurposeStructuredFindingRepair, types.InvocationRoleFixer, "fix_fast", types.RunnerCodex)
	_ = d.FinishInvocationAttempt(fixerID, types.InvocationAttemptTerminal{Outcome: types.InvocationOutcomeSucceeded})
	if err := d.SetFindingRepairFixer(repairID, fixerID); err != nil {
		t.Fatalf("link fixer: %v", err)
	}
	if err := d.RecordFindingRepairCheck(repairID, "make test", true, 0, "PASS"); err != nil {
		t.Fatalf("record check: %v", err)
	}
	verifierID := startScopedAttempt(t, d, scope, types.PurposeNormalAggregateVerification, types.InvocationRoleVerifier, "review_strong", types.RunnerCodex)
	_ = d.FinishInvocationAttempt(verifierID, types.InvocationAttemptTerminal{Outcome: types.InvocationOutcomeSucceeded})
	if err := d.SetFindingRepairVerifier(repairID, verifierID); err != nil {
		t.Fatalf("link verifier: %v", err)
	}
	if err := d.ResolveFindingRepair(repairID, RepairVerdictResolved, "the nil deref is now guarded", RepairStatusResolved); err != nil {
		t.Fatalf("resolve repair: %v", err)
	}

	repairs, err := d.GetFindingRepairsByRun(run.ID)
	if err != nil || len(repairs) != 1 {
		t.Fatalf("repairs by run = %+v (%v)", repairs, err)
	}
	r := repairs[0]
	if r.LineageID != lineage.ID || r.Severity != "error" || r.Action != "auto-fix" || r.Description != "nil dereference in parser" || r.File != "parser.go" || r.Line != 42 || r.Tier != 0 || r.RemainingBudget != 3 {
		t.Fatalf("immutable content wrong: %+v", r)
	}
	if r.FixerAttemptID != fixerID || r.VerifierAttemptID != verifierID {
		t.Fatalf("links fixer=%q verifier=%q, want %q/%q", r.FixerAttemptID, r.VerifierAttemptID, fixerID, verifierID)
	}
	if r.Verdict != RepairVerdictResolved || r.VerdictRationale == "" || r.Status != RepairStatusResolved {
		t.Fatalf("verdict/rationale/status = %q/%q/%q", r.Verdict, r.VerdictRationale, r.Status)
	}

	byLineage, err := d.GetFindingRepairsByLineage(lineage.ID)
	if err != nil || len(byLineage) != 1 {
		t.Fatalf("repairs by lineage = %+v (%v)", byLineage, err)
	}
	checks, err := d.GetFindingRepairChecks(repairID)
	if err != nil || len(checks) != 1 {
		t.Fatalf("checks = %+v (%v)", checks, err)
	}
	if !checks[0].Applicable || checks[0].ExitCode != 0 || checks[0].Command != "make test" {
		t.Fatalf("check = %+v", checks[0])
	}
}

func TestHasUnresolvedBlockingRepair(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/home/user/unresolved", "git@github.com:user/unresolved.git", "main")
	run, _ := d.InsertRun(repo.ID, "feature", "abc", "def")
	step, _ := d.InsertStepResult(run.ID, types.StepReview)
	round, _ := d.ReserveStepRound(step.ID, 1, "initial")

	repair := func(lineage, severity string, tier int, status string) {
		id, err := d.StartFindingRepair(FindingRepairStart{
			RunID: run.ID, LineageID: lineage, StepResultID: step.ID, StepRoundID: round.ID,
			Severity: severity, Action: "auto-fix", Description: "d", Tier: tier, RemainingBudget: 2 - tier,
		})
		if err != nil {
			t.Fatalf("start repair: %v", err)
		}
		verdict := RepairVerdictUnresolved
		if status == RepairStatusResolved {
			verdict = RepairVerdictResolved
		}
		_ = d.ResolveFindingRepair(id, verdict, "r", status)
	}

	// A blocking lineage whose latest repair is unresolved.
	repair("lin-A", "error", 0, RepairStatusUnresolved)
	if got, _ := d.HasUnresolvedBlockingRepair(run.ID); !got {
		t.Fatal("want true for an unresolved blocking lineage")
	}
	// Its higher tier resolves it: latest disposition wins.
	repair("lin-A", "error", 1, RepairStatusResolved)
	if got, _ := d.HasUnresolvedBlockingRepair(run.ID); got {
		t.Fatal("want false once the lineage resolves at a higher tier")
	}
	// An unresolved informational lineage is non-blocking and must not count.
	repair("lin-B", "info", 0, RepairStatusUnresolved)
	if got, _ := d.HasUnresolvedBlockingRepair(run.ID); got {
		t.Fatal("an unresolved info lineage must not count as blocking")
	}
}

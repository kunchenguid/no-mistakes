package pipeline

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// fakeRepairInvoker journals fixer/verifier attempts like the real routed
// invoker (so the coordinator's attempt-linking query works) and, for the
// fixer, performs an optional worktree edit so there is something to commit.
type fakeRepairInvoker struct {
	db        *db.DB
	verdict   string
	fixEdit   func()
	fixErr    error
	verifyErr error
	fixCalls  int
	verCalls  int
}

func (f *fakeRepairInvoker) Invoke(_ context.Context, req agent.InvocationRequest) (*agent.Result, error) {
	def, _ := types.PurposeDefinitionFor(req.Purpose)
	profile := "fix_fast"
	if req.Purpose == verifierPurpose {
		profile = "review_strong"
	}
	attemptID, err := f.db.StartInvocationAttempt(types.InvocationAttemptStart{
		Purpose:      req.Purpose,
		Role:         def.Role,
		Scope:        req.Scope,
		CandidateKey: profile + ":0:codex",
		Candidate:    types.InvocationCandidate{Profile: profile, Tier: 0, CandidateIndex: 0, Runner: types.RunnerCodex, Model: "m", Effort: types.EffortMedium},
	})
	if err != nil {
		return nil, err
	}
	if req.Purpose == fixerPurpose {
		f.fixCalls++
		if f.fixErr != nil {
			_ = f.db.FinishInvocationAttempt(attemptID, types.InvocationAttemptTerminal{Outcome: types.InvocationOutcomeFailed})
			return nil, f.fixErr
		}
		if f.fixEdit != nil {
			f.fixEdit()
		}
		_ = f.db.FinishInvocationAttempt(attemptID, types.InvocationAttemptTerminal{Outcome: types.InvocationOutcomeSucceeded})
		return &agent.Result{Output: []byte(`{"summary":"guard the nil dereference"}`)}, nil
	}
	f.verCalls++
	if f.verifyErr != nil {
		_ = f.db.FinishInvocationAttempt(attemptID, types.InvocationAttemptTerminal{Outcome: types.InvocationOutcomeFailed})
		return nil, f.verifyErr
	}
	_ = f.db.FinishInvocationAttempt(attemptID, types.InvocationAttemptTerminal{Outcome: types.InvocationOutcomeSucceeded})
	return &agent.Result{Output: []byte(f.verdict)}, nil
}

// repairFixture wires a git worktree, a routed review attempt + lineage, and a
// coordinator whose reserveRound closure hands out repair rounds after the
// review round.
func repairFixture(t *testing.T, fake *fakeRepairInvoker, checks []repairCheck) (*repairCoordinator, repairTarget) {
	t.Helper()
	database, _, run, _ := setupTest(t)
	dir := t.TempDir()
	initGitRepo(t, dir)
	baseSHA := gitOut(t, dir, "rev-parse", "HEAD")
	gitOut(t, dir, "checkout", "-b", "feature")
	writeTestFile(t, dir, "app.go", "package app\n\nfunc F(p *int) int { return *p }\n")
	gitOut(t, dir, "add", ".")
	gitOut(t, dir, "commit", "-m", "feature")
	run.Branch = "feature"

	step, _ := database.InsertStepResult(run.ID, types.StepReview)
	round1, _ := database.ReserveStepRound(step.ID, 1, "initial")
	reviewScope := types.InvocationScope{Kind: types.InvocationScopePipeline, RunID: run.ID, StepResultID: step.ID, StepRoundID: round1.ID}
	reviewAttempt, err := database.StartInvocationAttempt(types.InvocationAttemptStart{
		Purpose: types.PurposeInitialReview, Role: types.InvocationRoleVerifier, Scope: reviewScope,
		CandidateKey: "review_strong:0:codex",
		Candidate:    types.InvocationCandidate{Profile: "review_strong", Runner: types.RunnerCodex, Model: "gpt-5.6-sol", Effort: types.EffortHigh},
	})
	if err != nil {
		t.Fatalf("start review attempt: %v", err)
	}
	_ = database.FinishInvocationAttempt(reviewAttempt, types.InvocationAttemptTerminal{Outcome: types.InvocationOutcomeSucceeded})
	lineages, err := database.CreateFindingLineages(run.ID, reviewAttempt, []string{"review-1"})
	if err != nil || len(lineages) != 1 {
		t.Fatalf("create lineage: %v (%v)", lineages, err)
	}
	_ = database.CompleteReservedStepRound(round1.ID, nil, nil, 0)

	fake.db = database
	roundNum := 1
	rc := &repairCoordinator{
		invoker:      fake,
		db:           database,
		run:          run,
		stepResultID: step.ID,
		workDir:      dir,
		branch:       run.Branch,
		checks:       checks,
		log:          func(string) {},
		logChunk:     func(string) {},
		reserveRound: func(trigger string) (*db.StepRound, error) {
			roundNum++
			return database.ReserveStepRound(step.ID, roundNum, trigger)
		},
	}
	target := repairTarget{
		LineageID:       lineages[0].ID,
		Finding:         types.Finding{ID: "review-1", Severity: "error", Action: "auto-fix", Description: "nil dereference of p", File: "app.go", Line: 3},
		Intent:          "add F",
		BaseSHA:         baseSHA,
		Tier:            0,
		RemainingBudget: 3,
	}
	// The fixer edit lands a real change so there is a commit to verify.
	fake.fixEdit = func() { writeTestFile(t, dir, "app.go", "package app\n\nfunc F(p *int) int { if p == nil { return 0 }; return *p }\n") }
	return rc, target
}

func resolvedVerdict(lineageID string) string {
	return fmt.Sprintf(`{"lineage_id":%q,"status":"resolved","rationale":"the nil dereference is now guarded"}`, lineageID)
}

func TestRepairCoordinatorResolvesWithVerifiedFix(t *testing.T) {
	fake := &fakeRepairInvoker{}
	rc, target := repairFixture(t, fake, nil)
	fake.verdict = resolvedVerdict(target.LineageID)

	res, err := rc.attemptRepair(context.Background(), target)
	if err != nil {
		t.Fatalf("attemptRepair: %v", err)
	}
	if !res.Resolved || res.Verdict != db.RepairVerdictResolved {
		t.Fatalf("result = %+v, want resolved", res)
	}
	if fake.fixCalls != 1 || fake.verCalls != 1 {
		t.Fatalf("calls fix=%d verify=%d, want 1 and 1", fake.fixCalls, fake.verCalls)
	}
	repairs, _ := rc.db.GetFindingRepairsByLineage(target.LineageID)
	if len(repairs) != 1 {
		t.Fatalf("repairs = %d, want 1", len(repairs))
	}
	r := repairs[0]
	if r.Status != db.RepairStatusResolved || r.Verdict != db.RepairVerdictResolved || r.VerdictRationale == "" {
		t.Fatalf("repair = %+v, want resolved with rationale", r)
	}
	if r.FixerAttemptID == "" || r.VerifierAttemptID == "" {
		t.Fatalf("repair links missing: fixer=%q verifier=%q", r.FixerAttemptID, r.VerifierAttemptID)
	}
	if r.Severity != "error" || r.Action != "auto-fix" || r.File != "app.go" || r.Line != 3 {
		t.Fatalf("immutable finding content wrong: %+v", r)
	}
}

func TestRepairCoordinatorUnresolvedVerdictFailsSafe(t *testing.T) {
	fake := &fakeRepairInvoker{}
	rc, target := repairFixture(t, fake, nil)
	fake.verdict = fmt.Sprintf(`{"lineage_id":%q,"status":"unresolved","rationale":"still dereferences nil"}`, target.LineageID)

	res, err := rc.attemptRepair(context.Background(), target)
	if err != nil {
		t.Fatalf("attemptRepair: %v", err)
	}
	if res.Resolved {
		t.Fatal("an unresolved verdict must not succeed")
	}
	repairs, _ := rc.db.GetFindingRepairsByLineage(target.LineageID)
	if repairs[0].Status != db.RepairStatusUnresolved {
		t.Fatalf("status = %q, want unresolved", repairs[0].Status)
	}
}

func TestRepairCoordinatorWrongLineageFailsSafe(t *testing.T) {
	fake := &fakeRepairInvoker{}
	rc, target := repairFixture(t, fake, nil)
	fake.verdict = `{"lineage_id":"some-other-lineage","status":"resolved","rationale":"looks fine"}`

	res, err := rc.attemptRepair(context.Background(), target)
	if err != nil {
		t.Fatalf("attemptRepair: %v", err)
	}
	if res.Resolved {
		t.Fatal("a resolved verdict for the wrong lineage must fail safe")
	}
}

func TestRepairCoordinatorMalformedVerdictFailsSafe(t *testing.T) {
	fake := &fakeRepairInvoker{}
	rc, target := repairFixture(t, fake, nil)
	fake.verdict = `not json at all`

	res, err := rc.attemptRepair(context.Background(), target)
	if err != nil {
		t.Fatalf("attemptRepair: %v", err)
	}
	if res.Resolved {
		t.Fatal("a malformed adjudication must fail safe")
	}
}

func TestRepairCoordinatorEmptyRationaleFailsSafe(t *testing.T) {
	fake := &fakeRepairInvoker{}
	rc, target := repairFixture(t, fake, nil)
	fake.verdict = fmt.Sprintf(`{"lineage_id":%q,"status":"resolved","rationale":""}`, target.LineageID)

	res, err := rc.attemptRepair(context.Background(), target)
	if err != nil {
		t.Fatalf("attemptRepair: %v", err)
	}
	if res.Resolved {
		t.Fatal("a resolved verdict without a rationale must fail safe")
	}
}

func TestRepairCoordinatorFailedCheckSkipsVerifier(t *testing.T) {
	fake := &fakeRepairInvoker{}
	failingCheck := repairCheck{
		Command: "make test",
		Run:     func(context.Context) (bool, int, string) { return true, 1, "FAIL: TestF" },
	}
	rc, target := repairFixture(t, fake, []repairCheck{failingCheck})
	fake.verdict = resolvedVerdict(target.LineageID)

	res, err := rc.attemptRepair(context.Background(), target)
	if err != nil {
		t.Fatalf("attemptRepair: %v", err)
	}
	if res.Resolved {
		t.Fatal("a failed deterministic check must leave the finding unresolved")
	}
	if fake.verCalls != 0 {
		t.Fatalf("verifier ran %d times; a failed check must skip the verifier", fake.verCalls)
	}
	repairs, _ := rc.db.GetFindingRepairsByLineage(target.LineageID)
	checks, _ := rc.db.GetFindingRepairChecks(repairs[0].ID)
	if len(checks) != 1 || checks[0].ExitCode != 1 || !checks[0].Applicable {
		t.Fatalf("recorded checks = %+v, want one applicable failing check", checks)
	}
}

func TestRepairCoordinatorInapplicableCheckProceedsToVerifier(t *testing.T) {
	fake := &fakeRepairInvoker{}
	skipCheck := repairCheck{
		Command: "make test",
		Run:     func(context.Context) (bool, int, string) { return false, 0, "" },
	}
	rc, target := repairFixture(t, fake, []repairCheck{skipCheck})
	fake.verdict = resolvedVerdict(target.LineageID)

	res, err := rc.attemptRepair(context.Background(), target)
	if err != nil {
		t.Fatalf("attemptRepair: %v", err)
	}
	if !res.Resolved {
		t.Fatal("an inapplicable check must not block a verified resolution")
	}
	if fake.verCalls != 1 {
		t.Fatalf("verifier ran %d times, want 1", fake.verCalls)
	}
	repairs, _ := rc.db.GetFindingRepairsByLineage(target.LineageID)
	checks, _ := rc.db.GetFindingRepairChecks(repairs[0].ID)
	if len(checks) != 1 || checks[0].Applicable {
		t.Fatalf("recorded checks = %+v, want one inapplicable check", checks)
	}
}

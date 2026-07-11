package steps

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// TestVerifyStep_SkipsWhenUnchanged proves Verify skips fresh verification when
// the sealed candidate exactly matches the latest strong-reviewed candidate.
func TestVerifyStep_SkipsWhenUnchanged(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			t.Fatal("verify must not invoke the agent when the candidate is unchanged")
			return nil, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, "base", "head", config.Commands{})
	if _, err := sctx.DB.CreateSeal(sctx.Run.ID, "same-sha", "reviewed"); err != nil {
		t.Fatal(err)
	}
	if _, err := sctx.DB.CreateSeal(sctx.Run.ID, "same-sha", "pre_verify"); err != nil {
		t.Fatal(err)
	}

	step := &VerifyStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatal("expected skipped verification to not gate")
	}
	if len(ag.calls) != 0 {
		t.Fatalf("expected no agent calls on skip, got %d", len(ag.calls))
	}
}

// TestVerifyStep_FreshVerificationPasses proves a changed candidate triggers a
// fresh aggregate verification that passes on empty findings.
func TestVerifyStep_FreshVerificationPasses(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"risk_level":"low","risk_rationale":"candidate verified"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	// Only a pre-verify seal (no matching reviewed seal) forces fresh verification.
	if _, err := sctx.DB.CreateSeal(sctx.Run.ID, headSHA, "pre_verify"); err != nil {
		t.Fatal(err)
	}

	step := &VerifyStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatal("expected clean fresh verification to pass")
	}
	if len(ag.calls) != 1 {
		t.Fatalf("expected 1 aggregate verification call, got %d", len(ag.calls))
	}
	if !strings.Contains(ag.calls[0].Prompt, "final aggregate verification") {
		t.Fatalf("expected the aggregate verification prompt, got: %s", ag.calls[0].Prompt)
	}
	reviewed, err := sctx.DB.LatestSealByReason(sctx.Run.ID, "reviewed")
	if err != nil {
		t.Fatal(err)
	}
	if reviewed == nil || reviewed.SHA != headSHA {
		t.Fatalf("reviewed seal = %+v, want exact verified SHA %s", reviewed, headSHA)
	}
}

func TestVerifyStep_VerifierCommitCannotBlessMutatedCandidate(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			if err := os.WriteFile(filepath.Join(dir, "verifier-mutation.txt"), []byte("not verified\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, dir, "add", "--", "verifier-mutation.txt")
			gitCmd(t, dir, "commit", "-m", "verifier mutation")
			return &agent.Result{Output: json.RawMessage(`{"findings":[],"risk_level":"low","risk_rationale":"candidate verified"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	if _, err := sctx.DB.CreateSeal(sctx.Run.ID, headSHA, "pre_verify"); err != nil {
		t.Fatal(err)
	}

	_, err := (&VerifyStep{}).Execute(sctx)
	if err == nil {
		t.Fatal("expected verifier mutation to invalidate the verification")
	}
	if !strings.Contains(err.Error(), "changed HEAD") {
		t.Fatalf("error = %q, want changed HEAD", err)
	}
	reviewed, err := sctx.DB.LatestSealByReason(sctx.Run.ID, "reviewed")
	if err != nil {
		t.Fatal(err)
	}
	if reviewed != nil {
		t.Fatalf("verifier mutation produced reviewed seal %+v", reviewed)
	}
}

// TestVerifyStep_RejectsIncompleteVerdict proves schema-incomplete verification
// cannot create a reviewed seal.
func TestVerifyStep_RejectsIncompleteVerdict(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		output string
	}{
		{name: "missing findings", output: `{"risk_level":"low","risk_rationale":"bounded change"}`},
		{name: "missing risk", output: `{"findings":[],"risk_rationale":"bounded change"}`},
		{name: "empty rationale", output: `{"findings":[],"risk_level":"low","risk_rationale":"   "}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, baseSHA, headSHA := setupGitRepo(t)
			ag := &mockAgent{
				name: "test",
				runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
					return &agent.Result{Output: json.RawMessage(tc.output)}, nil
				},
			}
			sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
			if _, err := sctx.DB.CreateSeal(sctx.Run.ID, headSHA, "pre_verify"); err != nil {
				t.Fatal(err)
			}

			if _, err := (&VerifyStep{}).Execute(sctx); err == nil {
				t.Fatal("expected incomplete verifier verdict to fail closed")
			}
			reviewed, err := sctx.DB.LatestSealByReason(sctx.Run.ID, "reviewed")
			if err != nil {
				t.Fatal(err)
			}
			if reviewed != nil {
				t.Fatalf("incomplete verdict produced reviewed seal %+v", reviewed)
			}
		})
	}
}

// TestVerifyStep_GatesOnBlockingFindings proves a blocking verification finding
// gates the pipeline before Push.
func TestVerifyStep_GatesOnBlockingFindings(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{
		name: "test",
		runFn: func(ctx context.Context, opts agent.RunOpts) (*agent.Result, error) {
			return &agent.Result{Output: json.RawMessage(`{"findings":[{"severity":"error","file":"main.go","description":"regression introduced after review","action":"auto-fix"}],"risk_level":"high","risk_rationale":"unreviewed regression"}`)}, nil
		},
	}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	if _, err := sctx.DB.CreateSeal(sctx.Run.ID, headSHA, "pre_verify"); err != nil {
		t.Fatal(err)
	}

	step := &VerifyStep{}
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval {
		t.Fatal("expected a blocking verification finding to gate")
	}
}

// TestVerifyStep_FailsClosedWithoutSeal proves Verify fails closed when nothing
// has been sealed.
func TestVerifyStep_FailsClosedWithoutSeal(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := setupGitRepo(t)
	ag := &mockAgent{name: "test"}
	sctx := newTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	step := &VerifyStep{}
	if _, err := step.Execute(sctx); err == nil {
		t.Fatal("expected verify to fail closed with no sealed candidate")
	}
}

// TestVerifyPurpose_Escalates proves normal verification uses review_strong while
// intent-sensitive and post-repair verification escalate to authority_strong.
func TestVerifyPurpose_Escalates(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, "base", "head", config.Commands{})

	if got := verifyPurpose(sctx); got != types.PurposeNormalAggregateVerification {
		t.Fatalf("normal verify purpose = %q, want normal aggregate verification", got)
	}

	sctx.UserIntent = "ship the extracted intent safely"
	if got := verifyPurpose(sctx); got != types.PurposeEscalatedAggregateVerification {
		t.Fatalf("intent-sensitive verify purpose = %q, want escalated aggregate verification", got)
	}

	sctx.UserIntent = ""
	sctx.Fixing = true
	if got := verifyPurpose(sctx); got != types.PurposeEscalatedAggregateVerification {
		t.Fatalf("post-repair verify purpose = %q, want escalated aggregate verification", got)
	}
}

func TestVerifyPurpose_AuthorityHistoryEscalates(t *testing.T) {
	t.Parallel()
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, t.TempDir(), "base", "head", config.Commands{})
	step, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	round, err := sctx.DB.ReserveStepRound(step.ID, 1, "initial")
	if err != nil {
		t.Fatal(err)
	}
	attemptID, err := sctx.DB.StartInvocationAttempt(types.InvocationAttemptStart{
		Purpose: types.PurposeUnstructuredTestRepair,
		Role:    types.InvocationRoleFixer,
		Scope: types.InvocationScope{
			Kind:         types.InvocationScopePipeline,
			RunID:        sctx.Run.ID,
			StepResultID: step.ID,
			StepRoundID:  round.ID,
		},
		CandidateKey: "authority-strong-test",
		Candidate: types.InvocationCandidate{
			Profile:        string(config.ProfileAuthorityStrong),
			Tier:           1,
			CandidateIndex: 0,
			Runner:         types.RunnerCodex,
			Model:          "gpt-5.6-sol",
			Effort:         types.EffortXHigh,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.FinishInvocationAttempt(attemptID, types.InvocationAttemptTerminal{Outcome: types.InvocationOutcomeSucceeded}); err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.CompleteReservedStepRound(round.ID, nil, nil, 1); err != nil {
		t.Fatal(err)
	}

	if got := verifyPurpose(sctx); got != types.PurposeEscalatedAggregateVerification {
		t.Fatalf("verify purpose after authority-tier work = %q, want escalated aggregate verification", got)
	}
}

func TestVerifyPurpose_RepairHistoryEscalates(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		profile config.ProfileName
		verdict string
		status  string
	}{
		{
			name:    "blocking finding survived balanced repair",
			profile: config.ProfileFixBalanced,
			verdict: db.RepairVerdictUnresolved,
			status:  db.RepairStatusUnresolved,
		},
		{
			name:    "repair evidence was inconclusive",
			profile: config.ProfileFixFast,
			verdict: db.RepairVerdictInconclusive,
			status:  db.RepairStatusUnresolved,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, t.TempDir(), "base", "head", config.Commands{})
			recordRepairHistory(t, sctx, tc.profile, tc.verdict, tc.status)
			if got := verifyPurpose(sctx); got != types.PurposeEscalatedAggregateVerification {
				t.Fatalf("verify purpose = %q, want escalated aggregate verification", got)
			}
		})
	}
}

func TestVerifyPurpose_UsesImmutableInitialReviewRisk(t *testing.T) {
	t.Parallel()
	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, t.TempDir(), "base", "head", config.Commands{})
	step, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	initial := `{"findings":[],"risk_level":"high","risk_rationale":"fundamental change"}`
	if _, err := sctx.DB.InsertStepRound(step.ID, 1, "initial", &initial, nil, 1); err != nil {
		t.Fatal(err)
	}
	later := `{"findings":[],"risk_level":"low","risk_rationale":"repair appears bounded"}`
	if _, err := sctx.DB.InsertStepRound(step.ID, 2, "auto_fix", &later, nil, 1); err != nil {
		t.Fatal(err)
	}

	if got := verifyPurpose(sctx); got != types.PurposeEscalatedAggregateVerification {
		t.Fatalf("verify purpose after initially high risk = %q, want escalated aggregate verification", got)
	}
}

func recordRepairHistory(t *testing.T, sctx *pipeline.StepContext, profile config.ProfileName, verdict, status string) {
	t.Helper()
	step, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	round, err := sctx.DB.ReserveStepRound(step.ID, 1, "initial")
	if err != nil {
		t.Fatal(err)
	}
	scope := types.InvocationScope{
		Kind:         types.InvocationScopePipeline,
		RunID:        sctx.Run.ID,
		StepResultID: step.ID,
		StepRoundID:  round.ID,
	}
	reviewAttempt, err := sctx.DB.StartInvocationAttempt(types.InvocationAttemptStart{
		Purpose:      types.PurposeInitialReview,
		Role:         types.InvocationRoleVerifier,
		Scope:        scope,
		CandidateKey: "review-strong",
		Candidate: types.InvocationCandidate{
			Profile: string(config.ProfileReviewStrong), Runner: types.RunnerCodex,
			Model: "gpt-5.6-sol", Effort: types.EffortHigh,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.FinishInvocationAttempt(reviewAttempt, types.InvocationAttemptTerminal{Outcome: types.InvocationOutcomeSucceeded}); err != nil {
		t.Fatal(err)
	}
	lineages, err := sctx.DB.CreateFindingLineages(sctx.Run.ID, reviewAttempt, []string{"review-1"})
	if err != nil || len(lineages) != 1 {
		t.Fatalf("create repair lineage: %v (%d lineages)", err, len(lineages))
	}
	fixerAttempt, err := sctx.DB.StartInvocationAttempt(types.InvocationAttemptStart{
		Purpose:      types.PurposeStructuredFindingRepair,
		Role:         types.InvocationRoleFixer,
		Scope:        scope,
		CandidateKey: string(profile),
		Candidate: types.InvocationCandidate{
			Profile: string(profile), Runner: types.RunnerCodex,
			Model: "gpt-5.6-terra", Effort: types.EffortHigh,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.FinishInvocationAttempt(fixerAttempt, types.InvocationAttemptTerminal{Outcome: types.InvocationOutcomeSucceeded}); err != nil {
		t.Fatal(err)
	}
	repairID, err := sctx.DB.StartFindingRepair(db.FindingRepairStart{
		RunID: sctx.Run.ID, LineageID: lineages[0].ID, StepResultID: step.ID, StepRoundID: round.ID,
		Severity: "warning", Action: types.ActionAutoFix, Description: "blocking defect", Tier: 1, RemainingBudget: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.SetFindingRepairFixer(repairID, fixerAttempt); err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.ResolveFindingRepair(repairID, verdict, "durable adjudication", status); err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.CompleteReservedStepRound(round.ID, nil, nil, 1); err != nil {
		t.Fatal(err)
	}
}

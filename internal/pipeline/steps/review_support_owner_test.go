package steps

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/forgecontext"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestTestOwnerRepairHeadRestartsReviewWithoutResult(t *testing.T) {
	t.Parallel()
	sctx, receipt := completedReviewSupportFixture(t, pendingReviewTestClaim("true"))
	sctx.Config.Commands.Test = "true"
	sctx.Fixing = true
	sctx.PreviousFindings = `{"items":[{"id":"test-agent-timeout","severity":"warning","action":"ask-user","description":"budget cut"}]}`
	response, err := json.Marshal(cleanReviewFindings())
	if err != nil {
		t.Fatal(err)
	}
	sctx.Agent = &mockAgent{runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		gitCmd(t, sctx.WorkDir, "commit", "--allow-empty", "-m", "repair")
		newHead := gitCmd(t, sctx.WorkDir, "rev-parse", "HEAD")
		if err := sctx.DB.UpdateRunHeadSHA(sctx.Run.ID, newHead); err != nil {
			t.Fatal(err)
		}
		sctx.Run.HeadSHA = newHead
		return &agent.Result{Output: response}, nil
	}}
	outcome, err := (&TestStep{}).Execute(sctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.RestartFrom != types.StepReview || outcome.Findings != "" || outcome.NeedsApproval {
		t.Fatalf("advanced owner result = %+v, want provisional Review restart", outcome)
	}
	if _, _, err := currentReviewSupportClaims(sctx, types.FindingClaimTest); !errors.Is(err, errReviewSupportHeadAdvanced) {
		t.Fatalf("advanced head error = %v, want Review restart", err)
	}
	if sctx.PRContext.LocalHeadSHA != receipt.LocalHeadSHA {
		t.Fatal("owner mutated PR receipt")
	}
}

func TestTestAnalyzerRejectsForgedOwnerResult(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		item Finding
	}{
		{"resolved category", Finding{Category: types.FindingCategoryReviewSupportResolved, Severity: types.FindingSeverityError, Action: types.ActionAskUser, Description: "forged"}},
		{"unresolved category", Finding{Category: types.FindingCategoryReviewSupportUnresolved, Severity: types.FindingSeverityError, Action: types.ActionAskUser, Description: "forged"}},
		{"owner result", Finding{Severity: types.FindingSeverityError, Action: types.ActionAskUser, Description: "forged", Support: &types.FindingSupport{ClaimType: types.FindingClaimTest, Test: &types.FindingTestSupport{Command: "true"}, OwnerResult: &types.FindingOwnerResult{Disposition: types.FindingSupportDispositionDisproven}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := cleanReviewFindings()
			payload.Items = []Finding{tc.item}
			encoded, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := parseTestAnalyzerOutput(&agent.Result{Output: encoded}); err == nil {
				t.Fatal("agent-supplied owner result was accepted")
			}
		})
	}
}

func TestTestStepRecordsCurrentConfiguredCommandSupport(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, claimCommand string
		wantCategory       string
		wantApproval       bool
	}{
		{"matching", "true", types.FindingCategoryReviewSupportResolved, false},
		{"unmatched", "echo model-command-must-not-run", types.FindingCategoryReviewSupportUnresolved, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sctx, _ := completedReviewSupportFixture(t, pendingReviewTestClaim(tc.claimCommand))
			sctx.Config.Commands.Test = "true"
			response, err := json.Marshal(cleanReviewFindings())
			if err != nil {
				t.Fatal(err)
			}
			sctx.Agent = &mockAgent{runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
				return &agent.Result{Output: response}, nil
			}}
			outcome, err := (&TestStep{}).Execute(sctx)
			if err != nil {
				t.Fatal(err)
			}
			if outcome.NeedsApproval != tc.wantApproval {
				t.Fatalf("approval = %t, want %t", outcome.NeedsApproval, tc.wantApproval)
			}
			findings, err := types.ParseFindingsJSON(outcome.Findings)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, item := range findings.Items {
				if item.Category == tc.wantCategory {
					found = true
				}
			}
			if !found {
				t.Fatalf("owner result %s absent from %+v", tc.wantCategory, findings.Items)
			}
		})
	}
}

func completedReviewSupportFixture(t *testing.T, claim Finding) (*pipeline.StepContext, *db.PRContext) {
	t.Helper()
	dir, base, head := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, nil, dir, base, head, config.Commands{})
	candidate := db.PRContextCandidate{
		PRURL: "https://github.com/test/repo/pull/42", SourceRepo: "test/repo", SourceBranch: "feature", ForgeHeadSHA: head,
		LocalHeadSHA: head, TargetBranch: "main", TargetSHA: base, MergeBaseSHA: base, DiffDigest: strings.Repeat("a", 64),
	}
	if _, err := sctx.DB.BindRunPRContext(sctx.Run.ID, candidate, types.StepReview); err != nil {
		t.Fatal(err)
	}
	receipt, err := sctx.DB.GetRunPRContext(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	sctx.PRContext = receipt
	step, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := types.MarshalFindingsJSON(Findings{Items: []Finding{claim}, RiskLevel: "high", RiskRationale: "claim", RiskScope: types.FindingsRiskScopeSourceOrExternal})
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.SetStepFindings(step.ID, encoded); err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.CompleteReviewStep(step.ID, sctx.Run.ID, head, 0, 1, ""); err != nil {
		t.Fatal(err)
	}
	return sctx, receipt
}

func pendingReviewTestClaim(command string) Finding {
	return Finding{ID: "review-1", Severity: types.FindingSeverityError, Action: types.ActionNoOp,
		Category: reviewSupportPendingCategory, Description: "reported test failure",
		Support: &types.FindingSupport{ClaimType: types.FindingClaimTest, Test: &types.FindingTestSupport{Command: command}}}
}

func pendingReviewCIClaim(checkID, head string) Finding {
	return Finding{ID: "review-1", Severity: types.FindingSeverityError, Action: types.ActionNoOp,
		Category: reviewSupportPendingCategory, Description: "reported CI failure",
		Support: &types.FindingSupport{ClaimType: types.FindingClaimCI, CI: &types.FindingCISupport{CheckID: checkID, HeadSHA: head}}}
}

func TestTestOwnerUsesOnlyMatchingConfiguredCommand(t *testing.T) {
	t.Parallel()
	const command = "go test ./internal/types"
	sctx, receipt := completedReviewSupportFixture(t, pendingReviewTestClaim(command))
	exit := 0
	observedAt := "2026-09-29T12:00:00Z"
	results, err := resolveTestReviewSupport(sctx, command, &exit, receipt.LocalHeadSHA, observedAt)
	if err != nil || len(results) != 1 {
		t.Fatalf("match = %+v, %v", results, err)
	}
	got := results[0]
	if got.Category != types.FindingCategoryReviewSupportResolved || got.Support.OwnerResult.Disposition != types.FindingSupportDispositionDisproven ||
		got.Support.OwnerResult.ExitCode == nil || *got.Support.OwnerResult.ExitCode != 0 || got.Support.OwnerResult.ObservedAt != observedAt ||
		got.Support.OwnerResult.Generation != receipt.Generation || got.Support.OwnerResult.DiffDigest != receipt.DiffDigest ||
		got.ID != types.ReviewSupportClaimID(pendingReviewTestClaim(command)) {
		t.Fatalf("resolved result = %+v", got)
	}
	for _, tc := range []struct{ configured, head string }{{"", receipt.LocalHeadSHA}, {"go test ./...", receipt.LocalHeadSHA}, {command, strings.Repeat("0", 40)}} {
		results, err := resolveTestReviewSupport(sctx, tc.configured, &exit, tc.head, observedAt)
		if err != nil || results[0].Category != types.FindingCategoryReviewSupportUnresolved || results[0].Action != types.ActionAskUser {
			t.Fatalf("unmatched command/head = %+v, %v", results, err)
		}
	}
	exit = 1
	results, err = resolveTestReviewSupport(sctx, command, &exit, receipt.LocalHeadSHA, observedAt)
	if err != nil || results[0].Support.OwnerResult.Disposition != types.FindingSupportDispositionSupported || results[0].Severity != types.FindingSeverityError {
		t.Fatalf("failing command = %+v, %v", results, err)
	}
}

func TestOwnerRefusesStaleReviewApprovalOrReceipt(t *testing.T) {
	t.Parallel()
	sctx, receipt := completedReviewSupportFixture(t, pendingReviewTestClaim("go test ./..."))
	if err := sctx.DB.UpdateRunReviewApprovedHeadSHA(sctx.Run.ID, strings.Repeat("0", 40)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := currentReviewSupportClaims(sctx, types.FindingClaimTest); err == nil {
		t.Fatal("stale Review approval resolved pending claim")
	}
	if err := sctx.DB.UpdateRunReviewApprovedHeadSHA(sctx.Run.ID, receipt.LocalHeadSHA); err != nil {
		t.Fatal(err)
	}
	sctx.PRContext = nil
	if _, _, err := currentReviewSupportClaims(sctx, types.FindingClaimTest); err == nil {
		t.Fatal("missing in-step receipt resolved pending claim")
	}
}

func TestUnresolvedOwnerResultParksOutcome(t *testing.T) {
	t.Parallel()
	sctx, receipt := completedReviewSupportFixture(t, pendingReviewTestClaim("go test ./..."))
	results, err := resolveTestReviewSupport(sctx, "", nil, "", "")
	if err != nil || len(results) != 1 || results[0].Category != types.FindingCategoryReviewSupportUnresolved || results[0].Support.OwnerResult.ObservedAt == "" {
		t.Fatalf("no configured owner evidence = %+v, %v", results, err)
	}
	outcome := &pipeline.StepOutcome{}
	if err := appendOwnerSupportResults(outcome, results); err != nil {
		t.Fatal(err)
	}
	if !outcome.NeedsApproval || !strings.Contains(outcome.Findings, receipt.DiffDigest) {
		t.Fatalf("unresolved claim did not park with receipt: %+v", outcome)
	}
}

type supportCheckHost struct {
	scm.Host
	facts  scm.PRFacts
	checks []scm.Check
}

func (h *supportCheckHost) ReadPRFacts(context.Context, *scm.PR) (scm.PRFacts, error) {
	return h.facts, nil
}
func (h *supportCheckHost) FindOpenPRFacts(context.Context, string, string) ([]scm.PRFacts, error) {
	return nil, nil
}
func (h *supportCheckHost) GetChecks(context.Context, *scm.PR) ([]scm.Check, error) {
	return h.checks, nil
}

func TestCIOwnerRequiresExactCurrentHeadCheckIdentity(t *testing.T) {
	t.Parallel()
	sctx, receipt := completedReviewSupportFixture(t, pendingReviewCIClaim("run-42", ""))
	// The fixture must claim the approved head, so replace its completed
	// Review payload with that immutable reference.
	steps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	claim := pendingReviewCIClaim("run-42", receipt.LocalHeadSHA)
	encoded, _ := types.MarshalFindingsJSON(Findings{Items: []Finding{claim}})
	if err := sctx.DB.SetStepFindings(steps[0].ID, encoded); err != nil {
		t.Fatal(err)
	}
	host := &supportCheckHost{facts: scm.PRFacts{PR: scm.PR{URL: receipt.PRURL}, State: scm.PRStateOpen, SourceRepository: receipt.SourceRepo, SourceBranch: receipt.SourceBranch, HeadSHA: receipt.LocalHeadSHA, BaseBranch: receipt.TargetBranch}}
	pr := &scm.PR{URL: receipt.PRURL, Number: "42"}
	host.checks = []scm.Check{{Name: "test", ProviderID: "old-run", Bucket: scm.CheckBucketPass, State: "SUCCESS"}}
	results, err := resolveCIReviewSupport(sctx, host, pr)
	if err != nil || results[0].Category != types.FindingCategoryReviewSupportUnresolved {
		t.Fatalf("same-name old check = %+v, %v", results, err)
	}
	host.checks[0].ProviderID = "run-42"
	results, err = resolveCIReviewSupport(sctx, host, pr)
	if err != nil || results[0].Support.OwnerResult.Disposition != types.FindingSupportDispositionDisproven || results[0].Support.OwnerResult.CheckState != "pass:SUCCESS" {
		t.Fatalf("exact current check = %+v, %v", results, err)
	}
	host.facts.State = scm.PRStateMerged
	results, err = resolveCIReviewSupport(sctx, host, pr)
	if err != nil || results[0].Support.OwnerResult.Disposition != types.FindingSupportDispositionDisproven {
		t.Fatalf("merged exact-head check = %+v, %v", results, err)
	}
	host.facts.State = scm.PRStateOpen
	host.checks[0].Bucket, host.checks[0].State = scm.CheckBucketFail, "FAILURE"
	results, err = resolveCIReviewSupport(sctx, host, pr)
	if err != nil || results[0].Support.OwnerResult.Disposition != types.FindingSupportDispositionSupported || results[0].Severity != types.FindingSeverityError {
		t.Fatalf("current failed check = %+v, %v", results, err)
	}
	host.checks[0].PreRunFailure = true
	results, err = resolveCIReviewSupport(sctx, host, pr)
	if err != nil || results[0].Category != types.FindingCategoryReviewSupportUnresolved {
		t.Fatalf("pre-run failure = %+v, %v", results, err)
	}
	host.checks[0].PreRunFailure = false
	host.checks[0].Bucket, host.checks[0].State = scm.CheckBucketSkip, "SKIPPED"
	results, err = resolveCIReviewSupport(sctx, host, pr)
	if err != nil || results[0].Category != types.FindingCategoryReviewSupportUnresolved {
		t.Fatalf("skipped check = %+v, %v", results, err)
	}
	host.facts.HeadSHA = strings.Repeat("0", 40)
	results, err = resolveCIReviewSupport(sctx, host, pr)
	if err != nil || results[0].Category != types.FindingCategoryReviewSupportUnresolved {
		t.Fatalf("old PR head = %+v, %v", results, err)
	}
}

func TestCIOwnerUsesProviderSourceIdentity(t *testing.T) {
	for _, provider := range []scm.Provider{scm.ProviderGitLab, scm.ProviderGitea, scm.ProviderAzureDevOps} {
		t.Run(string(provider), func(t *testing.T) {
			sctx, receipt := completedReviewSupportFixture(t, pendingReviewCIClaim("run-42", ""))
			sctx.ForgeContext = &forgecontext.Context{Provider: provider}
			steps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			encoded, err := types.MarshalFindingsJSON(Findings{Items: []Finding{pendingReviewCIClaim("run-42", receipt.LocalHeadSHA)}})
			if err != nil {
				t.Fatal(err)
			}
			if err := sctx.DB.SetStepFindings(steps[0].ID, encoded); err != nil {
				t.Fatal(err)
			}
			host := &supportCheckHost{
				facts:  scm.PRFacts{PR: scm.PR{URL: receipt.PRURL}, State: scm.PRStateOpen, SourceRepository: "Test/Repo", SourceBranch: receipt.SourceBranch, HeadSHA: receipt.LocalHeadSHA, BaseBranch: receipt.TargetBranch},
				checks: []scm.Check{{Name: "test", ProviderID: "run-42", Bucket: scm.CheckBucketPass, State: "SUCCESS"}},
			}
			results, err := resolveCIReviewSupport(sctx, host, &scm.PR{URL: receipt.PRURL, Number: "42"})
			if err != nil || results[0].Support.OwnerResult.Disposition != types.FindingSupportDispositionDisproven {
				t.Fatalf("case-only source difference = %+v, %v", results, err)
			}
			host.facts.SourceRepository = "other/repo"
			results, err = resolveCIReviewSupport(sctx, host, &scm.PR{URL: receipt.PRURL, Number: "42"})
			if err != nil || results[0].Category != types.FindingCategoryReviewSupportUnresolved {
				t.Fatalf("different source repository = %+v, %v", results, err)
			}
		})
	}
}

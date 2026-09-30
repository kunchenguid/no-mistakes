package steps

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/bitbucket"
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
	candidate, err := readPRComparison(sctx, pipeline.PRTargetSelection{TargetBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	candidate.PRURL = "https://github.com/test/repo/pull/42"
	candidate.SourceRepo = "test/repo"
	candidate.SourceBranch = "feature"
	candidate.ForgeHeadSHA = head
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

func TestOwnerUsesCurrentReceiptWithEarlierReviewApproval(t *testing.T) {
	t.Parallel()
	sctx, _ := completedReviewSupportFixture(t, pendingReviewTestClaim("go test ./..."))
	if err := sctx.DB.UpdateRunReviewApprovedHeadSHA(sctx.Run.ID, strings.Repeat("0", 40)); err != nil {
		t.Fatal(err)
	}
	previous, err := sctx.DB.GetRunPRContext(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	advanced := previous.PRContextCandidate
	advanced.LocalHeadSHA = strings.Repeat("b", 40)
	if _, err := sctx.DB.AdvanceRunPRContext(sctx.Run.ID, advanced); err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.UpdateRunHeadSHA(sctx.Run.ID, advanced.LocalHeadSHA); err != nil {
		t.Fatal(err)
	}
	current, err := sctx.DB.GetRunPRContext(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	sctx.Run.HeadSHA = advanced.LocalHeadSHA
	sctx.PRContext = current
	if _, receipt, err := currentReviewSupportClaims(sctx, types.FindingClaimTest); err != nil || receipt.LocalHeadSHA != advanced.LocalHeadSHA {
		t.Fatalf("earlier Review approval prevented current owner evidence: %v", err)
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

func (h *supportCheckHost) Capabilities() scm.Capabilities { return scm.Capabilities{} }

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
	host.checks = []scm.Check{{Name: "test", ProviderID: "old-run", HeadSHA: receipt.LocalHeadSHA, Bucket: scm.CheckBucketPass, State: "SUCCESS"}}
	results, err := resolveCIReviewSupport(sctx, host, pr)
	if err != nil || results[0].Category != types.FindingCategoryReviewSupportUnresolved {
		t.Fatalf("same-name old check = %+v, %v", results, err)
	}
	host.checks[0].ProviderID = "run-42"
	host.checks[0].HeadSHA = strings.Repeat("0", 40)
	results, err = resolveCIReviewSupport(sctx, host, pr)
	if err != nil || results[0].Category != types.FindingCategoryReviewSupportUnresolved {
		t.Fatalf("named check from another commit = %+v, %v", results, err)
	}
	host.checks[0].HeadSHA = receipt.LocalHeadSHA
	results, err = resolveCIReviewSupport(sctx, host, pr)
	if err != nil || results[0].Support.OwnerResult.Disposition != types.FindingSupportDispositionDisproven || results[0].Support.OwnerResult.CheckState != "pass:SUCCESS" {
		t.Fatalf("exact current check = %+v, %v", results, err)
	}
	host.facts.State = scm.PRStateMerged
	results, err = resolveCIReviewSupport(sctx, host, pr)
	if err != nil || results[0].Category != types.FindingCategoryReviewSupportUnresolved {
		t.Fatalf("merged check without merge evidence = %+v, %v", results, err)
	}
	host.facts.State = scm.PRStateClosed
	results, err = resolveCIReviewSupport(sctx, host, pr)
	if err != nil || results[0].Support.OwnerResult.Disposition != types.FindingSupportDispositionDisproven {
		t.Fatalf("closed exact-head check = %+v, %v", results, err)
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
				checks: []scm.Check{{Name: "test", ProviderID: "run-42", HeadSHA: receipt.LocalHeadSHA, Bucket: scm.CheckBucketPass, State: "SUCCESS"}},
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

func TestCIOwnerRejectsTargetCommitMovement(t *testing.T) {
	sctx, receipt := completedReviewSupportFixture(t, pendingReviewCIClaim("run-42", ""))
	steps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := types.MarshalFindingsJSON(Findings{Items: []Finding{pendingReviewCIClaim("run-42", receipt.LocalHeadSHA)}})
	if err := sctx.DB.SetStepFindings(steps[0].ID, encoded); err != nil {
		t.Fatal(err)
	}
	host := &supportCheckHost{facts: scm.PRFacts{PR: scm.PR{URL: receipt.PRURL}, State: scm.PRStateOpen, SourceRepository: receipt.SourceRepo, SourceBranch: receipt.SourceBranch, HeadSHA: receipt.LocalHeadSHA, BaseBranch: receipt.TargetBranch}, checks: []scm.Check{{ProviderID: "run-42", Bucket: scm.CheckBucketPass}}}
	gitCmd(t, sctx.WorkDir, "checkout", "main")
	gitCmd(t, sctx.WorkDir, "commit", "--allow-empty", "-m", "move target")
	gitCmd(t, sctx.WorkDir, "checkout", "feature")
	results, err := resolveCIReviewSupport(sctx, host, &scm.PR{URL: receipt.PRURL, Number: "42"})
	if err != nil || len(results) != 1 || results[0].Category != types.FindingCategoryReviewSupportUnresolved {
		t.Fatalf("changed target support = %+v, %v; want unresolved", results, err)
	}
	host.facts.State = scm.PRStateClosed
	results, err = resolveCIReviewSupport(sctx, host, &scm.PR{URL: receipt.PRURL, Number: "42"})
	if err != nil || len(results) != 1 || results[0].Category != types.FindingCategoryReviewSupportUnresolved {
		t.Fatalf("closed changed-target support = %+v, %v; want unresolved", results, err)
	}

}

type supportMergedProofHost struct {
	*supportCheckHost
	proof scm.MergedProof
}

func (h *supportMergedProofHost) GetMergedProof(context.Context, *scm.PR, string) (scm.MergedProof, error) {
	return h.proof, nil
}

func TestCIOwnerProvesMergedResultOnAdvancedTarget(t *testing.T) {
	sctx, receipt := completedReviewSupportFixture(t, pendingReviewCIClaim("run-42", ""))
	steps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := types.MarshalFindingsJSON(Findings{Items: []Finding{pendingReviewCIClaim("run-42", receipt.LocalHeadSHA)}})
	if err := sctx.DB.SetStepFindings(steps[0].ID, encoded); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, sctx.WorkDir, "checkout", "main")
	gitCmd(t, sctx.WorkDir, "merge", "--squash", "feature")
	gitCmd(t, sctx.WorkDir, "commit", "-m", "squash reviewed comparison")
	mergeSHA := gitCmd(t, sctx.WorkDir, "rev-parse", "HEAD")
	gitCmd(t, sctx.WorkDir, "commit", "--allow-empty", "-m", "advance after merge")
	gitCmd(t, sctx.WorkDir, "checkout", "feature")
	host := &supportCheckHost{facts: scm.PRFacts{PR: scm.PR{URL: receipt.PRURL}, State: scm.PRStateMerged, MergeCommitSHA: mergeSHA, SourceRepository: receipt.SourceRepo, SourceBranch: receipt.SourceBranch, HeadSHA: receipt.LocalHeadSHA, BaseBranch: receipt.TargetBranch}, checks: []scm.Check{{ProviderID: "run-42", HeadSHA: receipt.LocalHeadSHA, Bucket: scm.CheckBucketPass}}}
	results, err := resolveCIReviewSupport(sctx, host, &scm.PR{URL: receipt.PRURL, Number: "42"})
	if err != nil || len(results) != 1 || results[0].Support.OwnerResult.Disposition != types.FindingSupportDispositionDisproven {
		t.Fatalf("merged current support = %+v, %v", results, err)
	}
	if results[0].Support.OwnerResult.TargetSHA != receipt.TargetSHA || results[0].Support.OwnerResult.DiffDigest != receipt.DiffDigest {
		t.Fatalf("merged support replaced original comparison: %+v", results[0].Support.OwnerResult)
	}
	proofHost := &supportMergedProofHost{supportCheckHost: host, proof: scm.MergedProof{Merged: true, Number: "42", URL: receipt.PRURL, HeadSHA: receipt.LocalHeadSHA, MergeCommitSHA: mergeSHA, MergedAt: time.Now(), MergedBy: "merge-bot"}}
	results, err = resolveCIReviewSupport(sctx, proofHost, &scm.PR{URL: receipt.PRURL, Number: "42"})
	if err != nil || len(results) != 1 || results[0].Support.OwnerResult.Disposition != types.FindingSupportDispositionDisproven {
		t.Fatalf("provider merge proof = %+v, %v", results, err)
	}
	proofHost.proof.HeadSHA = receipt.TargetSHA
	results, err = resolveCIReviewSupport(sctx, proofHost, &scm.PR{URL: receipt.PRURL, Number: "42"})
	if err != nil || len(results) != 1 || results[0].Category != types.FindingCategoryReviewSupportUnresolved {
		t.Fatalf("different provider merged head = %+v, %v", results, err)
	}

}

func TestCIOwnerRechecksHistoricalClaimAfterForwardEdit(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		checks                   []scm.Check
		unknownHead, mutableHead bool
	}{
		{name: "unrelated docs checks pass", checks: []scm.Check{{Name: "docs", ProviderID: "docs-run-1", Bucket: scm.CheckBucketPass}, {Name: "markdown", ProviderID: "docs-run-2", Bucket: scm.CheckBucketPass}}},
		{name: "historical identity returned on new head", checks: []scm.Check{{ProviderID: "old-check", Bucket: scm.CheckBucketPass}}},
		{name: "new check fails", checks: []scm.Check{{ProviderID: "new-check", Bucket: scm.CheckBucketFail}}},
		{name: "check pending", checks: []scm.Check{{ProviderID: "new-check", Bucket: scm.CheckBucketPending}}},
		{name: "check skipped", checks: []scm.Check{{ProviderID: "new-check", Bucket: scm.CheckBucketSkip}}},
		{name: "missing checks"},
		{name: "unknown provider identity", checks: []scm.Check{{Bucket: scm.CheckBucketPass}}},
		{name: "ambiguous provider identity", checks: []scm.Check{{ProviderID: "new-check", Bucket: scm.CheckBucketPass}, {ProviderID: "new-check", Bucket: scm.CheckBucketPass}}},
		{name: "setup failed", checks: []scm.Check{{ProviderID: "new-check", Bucket: scm.CheckBucketPass, PreRunFailure: true}}},
		{name: "approval pending", checks: []scm.Check{{ProviderID: "new-check", Bucket: scm.CheckBucketPass, AwaitingApproval: true}}},
		{name: "mutable historical head", checks: []scm.Check{{ProviderID: "new-check", Bucket: scm.CheckBucketPass}}, mutableHead: true},
		{name: "unknown historical head", checks: []scm.Check{{ProviderID: "new-check", Bucket: scm.CheckBucketPass}}, unknownHead: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sctx, original := completedReviewSupportFixture(t, pendingReviewCIClaim("old-check", ""))
			steps, err := sctx.DB.GetStepsByRun(sctx.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			historicalHead := original.LocalHeadSHA
			if tc.unknownHead {
				historicalHead = strings.Repeat("0", 40)
			}
			if tc.mutableHead {
				historicalHead = "HEAD"
			}
			claim := pendingReviewCIClaim("old-check", historicalHead)
			encoded, err := types.MarshalFindingsJSON(Findings{Items: []Finding{claim}})
			if err != nil {
				t.Fatal(err)
			}
			if err := sctx.DB.SetStepFindings(steps[0].ID, encoded); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, sctx.WorkDir, "commit", "--allow-empty", "-m", "forward pipeline edit")
			head := gitCmd(t, sctx.WorkDir, "rev-parse", "HEAD")
			if err := sctx.DB.UpdateRunHeadSHA(sctx.Run.ID, head); err != nil {
				t.Fatal(err)
			}
			sctx.Run.HeadSHA = head
			candidate, err := readPRComparison(sctx, pipeline.PRTargetSelection{TargetBranch: original.TargetBranch})
			if err != nil {
				t.Fatal(err)
			}
			candidate.PRURL, candidate.SourceRepo, candidate.SourceBranch, candidate.ForgeHeadSHA = original.PRURL, original.SourceRepo, original.SourceBranch, head
			if _, err := sctx.DB.AdvanceRunPRContext(sctx.Run.ID, candidate); err != nil {
				t.Fatal(err)
			}
			receipt, err := sctx.DB.GetRunPRContext(sctx.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			sctx.PRContext = receipt
			host := &supportCheckHost{facts: scm.PRFacts{PR: scm.PR{URL: receipt.PRURL}, State: scm.PRStateOpen, HeadSHA: head, SourceRepository: receipt.SourceRepo, SourceBranch: receipt.SourceBranch, BaseBranch: receipt.TargetBranch}, checks: tc.checks}
			results, err := resolveCIReviewSupport(sctx, host, &scm.PR{URL: receipt.PRURL, Number: "42"})
			if err != nil || len(results) != 1 {
				t.Fatalf("forward claim result = %+v, %v", results, err)
			}
			want := types.FindingSupportDispositionUnresolved
			if results[0].Support.OwnerResult.Disposition != want {
				t.Fatalf("forward claim result = %+v; want %s", results[0], want)
			}
			if results[0].Support.CI.CheckID != "old-check" || results[0].Support.CI.HeadSHA != historicalHead || results[0].Support.OwnerResult.HeadSHA != head || results[0].Support.OwnerResult.Generation != receipt.Generation {
				t.Fatalf("historical claim relabeled or owner proof stale: %+v", results[0].Support)
			}
		})
	}
}

func TestCIOwnerResolvesPublishedForwardClaimWithoutRelabeling(t *testing.T) {
	dir, base, reviewed := setupGitRepo(t)
	sctx := newTestContextWithDBRecords(t, nil, dir, base, reviewed, config.Commands{})
	sctx.Repo.UpstreamURL = dir
	sctx.ForgeContext = &forgecontext.Context{Provider: scm.ProviderBitbucket}
	candidate, err := readPRComparison(sctx, pipeline.PRTargetSelection{TargetBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	candidate.PRURL = "https://bitbucket.org/test/repo/pull-requests/42"
	candidate.SourceRepo = "test/repo"
	candidate.SourceBranch = "feature"
	candidate.ForgeHeadSHA = reviewed
	if _, err := sctx.DB.BindRunPRContext(sctx.Run.ID, candidate, types.StepReview); err != nil {
		t.Fatal(err)
	}
	original, err := sctx.DB.GetRunPRContext(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	sctx.PRContext = original
	step, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	claim := pendingReviewCIClaim("bitbucket-status:build", original.LocalHeadSHA)
	encoded, err := types.MarshalFindingsJSON(Findings{Items: []Finding{claim}})
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.SetStepFindings(step.ID, encoded); err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.CompleteReviewStep(step.ID, sctx.Run.ID, reviewed, 0, 1, ""); err != nil {
		t.Fatal(err)
	}
	claimID := types.ReviewSupportClaimID(claim)
	gitCmd(t, sctx.WorkDir, "commit", "--allow-empty", "-m", "document correction")
	head := gitCmd(t, sctx.WorkDir, "rev-parse", "HEAD")
	if err := sctx.DB.UpdateRunHeadSHA(sctx.Run.ID, head); err != nil {
		t.Fatal(err)
	}
	sctx.Run.HeadSHA = head
	sctx.PRContextAfterStep = true
	selection := pipeline.PRTargetSelection{PRURL: original.PRURL, SourceRepo: original.SourceRepo, SourceBranch: original.SourceBranch,
		ForgeHeadSHA: original.ForgeHeadSHA, TargetBranch: original.TargetBranch}
	if decision, err := guardPRContextWithSelection(sctx, types.StepDocument, selection, false); err != nil || decision.RestartFrom != "" {
		t.Fatalf("document forward guard = %+v, %v", decision, err)
	}
	if err := sctx.DB.UpdateRunPublication(sctx.Run.ID, db.PushBinding{HeadSHA: head}); err != nil {
		t.Fatal(err)
	}
	selection.ForgeHeadSHA = head
	if decision, err := guardPRContextWithSelection(sctx, types.StepPush, selection, false); err != nil || decision.RestartFrom != "" {
		t.Fatalf("publication guard = %+v, %v", decision, err)
	}
	sctx.PRContext, err = sctx.DB.GetRunPRContext(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	api := newFakeBitbucketCIAPI(t, "OPEN", fmt.Sprintf(`{"values":[{"name":"build","key":"build","state":"SUCCESSFUL","links":{"commit":{"href":"https://api.bitbucket.org/2.0/repositories/test/repo/commit/%s"}}}]}`, head), head)
	client, err := bitbucket.NewClientFromEnv(fakeBitbucketEnv(api.server.URL))
	if err != nil {
		t.Fatal(err)
	}
	host := bitbucket.NewHost(client, bitbucket.RepoRef{Workspace: "test", RepoSlug: "repo"}, false)
	results, err := resolveCIReviewSupport(sctx, host, &scm.PR{URL: original.PRURL, Number: "42"})
	if err != nil || len(results) != 1 || results[0].Support.OwnerResult.Disposition != types.FindingSupportDispositionDisproven {
		t.Fatalf("forward CI proof = %+v, %v", results, err)
	}
	if results[0].ID != claimID || results[0].Support.CI.HeadSHA != original.LocalHeadSHA ||
		results[0].Support.OwnerResult.HeadSHA != head || results[0].Support.OwnerResult.Generation != sctx.PRContext.Generation {
		t.Fatalf("claim origin or final owner proof changed: %+v", results[0])
	}
	for _, tc := range []struct {
		name, key, state, checkHead string
		want                        string
	}{
		{"old check commit", "build", "SUCCESSFUL", original.LocalHeadSHA, types.FindingSupportDispositionUnresolved},
		{"missing commit link", "build", "SUCCESSFUL", "", types.FindingSupportDispositionUnresolved},
		{"different check ID", "other", "SUCCESSFUL", head, types.FindingSupportDispositionUnresolved},
		{"pending", "build", "INPROGRESS", head, types.FindingSupportDispositionUnresolved},
		{"skipped", "build", "STOPPED", head, types.FindingSupportDispositionUnresolved},
		{"failed", "build", "FAILED", head, types.FindingSupportDispositionSupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			link := ""
			if tc.checkHead != "" {
				link = fmt.Sprintf(`,"links":{"commit":{"href":"https://api.bitbucket.org/2.0/repositories/test/repo/commit/%s"}}`, tc.checkHead)
			}
			api.statusesJSON = fmt.Sprintf(`{"values":[{"name":"build","key":%q,"state":%q%s}]}`, tc.key, tc.state, link)
			got, err := resolveCIReviewSupport(sctx, host, &scm.PR{URL: original.PRURL, Number: "42"})
			if err != nil || len(got) != 1 || got[0].Support.OwnerResult.Disposition != tc.want {
				t.Fatalf("%s result = %+v, %v", tc.name, got, err)
			}
		})
	}
	stub := &supportCheckHost{facts: scm.PRFacts{PR: scm.PR{URL: original.PRURL}, State: scm.PRStateOpen,
		SourceRepository: original.SourceRepo, SourceBranch: original.SourceBranch, HeadSHA: head, BaseBranch: original.TargetBranch}}
	for _, tc := range []struct {
		name   string
		checks []scm.Check
	}{
		{"duplicate identity", []scm.Check{{ProviderID: "bitbucket-status:build", HeadSHA: head, Bucket: scm.CheckBucketPass}, {ProviderID: "bitbucket-status:build", HeadSHA: head, Bucket: scm.CheckBucketPass}}},
		{"awaiting approval", []scm.Check{{ProviderID: "bitbucket-status:build", HeadSHA: head, Bucket: scm.CheckBucketPass, AwaitingApproval: true}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub.checks = tc.checks
			got, err := resolveCIReviewSupport(sctx, stub, &scm.PR{URL: original.PRURL, Number: "42"})
			if err != nil || len(got) != 1 || got[0].Category != types.FindingCategoryReviewSupportUnresolved {
				t.Fatalf("%s result = %+v, %v", tc.name, got, err)
			}
		})
	}
	api.statusesJSON = fmt.Sprintf(`{"values":[{"name":"build","key":"build","state":"SUCCESSFUL","links":{"commit":{"href":"https://api.bitbucket.org/2.0/repositories/test/repo/commit/%s"}}}]}`, head)
	if err := sctx.DB.UpdateRunPushBinding(sctx.Run.ID, db.PushBinding{HeadSHA: original.LocalHeadSHA}); err != nil {
		t.Fatal(err)
	}
	if got, err := resolveCIReviewSupport(sctx, host, &scm.PR{URL: original.PRURL, Number: "42"}); err != nil || len(got) != 1 || got[0].Category != types.FindingCategoryReviewSupportUnresolved {
		t.Fatalf("unpublished forward head = %+v, %v", got, err)
	}
	if err := sctx.DB.UpdateRunPushBinding(sctx.Run.ID, db.PushBinding{HeadSHA: head}); err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.UpdateRunReviewApprovedHeadSHA(sctx.Run.ID, base); err != nil {
		t.Fatal(err)
	}
	if got, err := resolveCIReviewSupport(sctx, host, &scm.PR{URL: original.PRURL, Number: "42"}); err != nil || len(got) != 1 || got[0].Category != types.FindingCategoryReviewSupportUnresolved {
		t.Fatalf("unapproved historical head = %+v, %v", got, err)
	}
	if err := sctx.DB.UpdateRunReviewApprovedHeadSHA(sctx.Run.ID, original.LocalHeadSHA); err != nil {
		t.Fatal(err)
	}
	api.prSourceSHA = original.LocalHeadSHA
	if got, err := resolveCIReviewSupport(sctx, host, &scm.PR{URL: original.PRURL, Number: "42"}); err != nil || len(got) != 1 || got[0].Category != types.FindingCategoryReviewSupportUnresolved {
		t.Fatalf("externally moved PR = %+v, %v", got, err)
	}
	api.prSourceSHA = head
	sctx.PRContext = original
	if _, err := resolveCIReviewSupport(sctx, host, &scm.PR{URL: original.PRURL, Number: "42"}); err == nil {
		t.Fatal("stale in-step comparison accepted")
	}
	sctx.PRContext, err = sctx.DB.GetRunPRContext(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	gitCmd(t, sctx.WorkDir, "checkout", "main")
	gitCmd(t, sctx.WorkDir, "commit", "--allow-empty", "-m", "move target")
	gitCmd(t, sctx.WorkDir, "checkout", "feature")
	if got, err := resolveCIReviewSupport(sctx, host, &scm.PR{URL: original.PRURL, Number: "42"}); err != nil || len(got) != 1 || got[0].Category != types.FindingCategoryReviewSupportUnresolved {
		t.Fatalf("moved target = %+v, %v", got, err)
	}
}

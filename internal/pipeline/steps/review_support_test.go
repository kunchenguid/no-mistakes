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

func TestReviewSupportSourceUsesPinnedHead(t *testing.T) {
	t.Parallel()
	dir, base, head := setupGitRepo(t)
	ctx := newTestContext(t, nil, dir, base, head, config.Commands{})
	ctx.PRContext = &db.PRContext{PRContextCandidate: db.PRContextCandidate{LocalHeadSHA: head}}
	items := Findings{Items: []Finding{{Action: types.ActionAutoFix, Support: &types.FindingSupport{
		ClaimType: types.FindingClaimSource,
		Source:    &types.FindingSourceSupport{Path: "feature.txt", Line: 1, Quote: "feature"},
	}}}}
	got, err := validateReviewFindingSupport(ctx, items, head)
	if err != nil {
		t.Fatal(err)
	}
	if got.Items[0].Action != types.ActionAutoFix {
		t.Fatalf("source action = %q, want auto-fix", got.Items[0].Action)
	}
	items.Items[0].Support.Source.Quote = "never existed"
	if _, err := validateReviewFindingSupport(ctx, items, head); err == nil {
		t.Fatal("fabricated quote passed")
	}
	if _, err := validateReviewFindingSupport(ctx, items, base); err == nil {
		t.Fatal("review head differing from pinned PR context passed")
	}
}

func TestReviewSupportSourceCanQuoteDeletedLineAtPinnedMergeBase(t *testing.T) {
	dir, base, unchangedHead := setupGitRepo(t)
	gitCmd(t, dir, "rm", "base.txt")
	gitCmd(t, dir, "commit", "-m", "remove base line")
	head := gitCmd(t, dir, "rev-parse", "HEAD")
	ctx := newTestContext(t, nil, dir, base, head, config.Commands{})
	ctx.PRContext = &db.PRContext{PRContextCandidate: db.PRContextCandidate{LocalHeadSHA: head, MergeBaseSHA: base}}
	items := Findings{Items: []Finding{{Action: types.ActionAutoFix, Support: &types.FindingSupport{
		ClaimType: types.FindingClaimSource,
		Source:    &types.FindingSourceSupport{Path: "base.txt", Line: 1, Quote: "base content", HeadSHA: base},
	}}}}
	if _, err := validateReviewFindingSupport(ctx, items, head); err != nil {
		t.Fatalf("pinned merge-base source refused: %v", err)
	}
	ctx.PRContext.LocalHeadSHA = unchangedHead
	if _, err := validateReviewFindingSupport(ctx, items, unchangedHead); err == nil {
		t.Fatal("unchanged merge-base line accepted as removed source")
	}
}

func TestReviewSupportDeletedLineUsesLiteralPathAndCRLF(t *testing.T) {
	for _, tc := range []struct {
		name, path, content string
	}{
		{name: "CRLF", path: "crlf.txt", content: "broken\r\n"},
		{name: "literal pathspec", path: "a[1].txt", content: "broken\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir, _, _ := setupGitRepo(t)
			if err := os.WriteFile(filepath.Join(dir, tc.path), []byte(tc.content), 0o644); err != nil {
				t.Fatal(err)
			}
			gitCmd(t, dir, "add", "--", ":(literal)"+tc.path)
			gitCmd(t, dir, "commit", "-m", "add source")
			base := gitCmd(t, dir, "rev-parse", "HEAD")
			gitCmd(t, dir, "rm", "--", ":(literal)"+tc.path)
			gitCmd(t, dir, "commit", "-m", "remove source")
			head := gitCmd(t, dir, "rev-parse", "HEAD")
			ctx := newTestContext(t, nil, dir, base, head, config.Commands{})
			ctx.PRContext = &db.PRContext{PRContextCandidate: db.PRContextCandidate{LocalHeadSHA: head, MergeBaseSHA: base}}
			items := Findings{Items: []Finding{{Action: types.ActionAutoFix, Support: &types.FindingSupport{
				ClaimType: types.FindingClaimSource,
				Source:    &types.FindingSourceSupport{Path: tc.path, Line: 1, Quote: "broken", HeadSHA: base},
			}}}}
			if _, err := validateReviewFindingSupport(ctx, items, head); err != nil {
				t.Fatalf("deleted source support: %v", err)
			}
		})
	}
}

func TestReviewSupportFixRoundUsesExactNewLocalHeadProvisionally(t *testing.T) {
	t.Parallel()
	ctx := newReviewSupportContext(t)
	oldHead := ctx.PRContext.LocalHeadSHA
	gitCmd(t, ctx.WorkDir, "commit", "--allow-empty", "-m", "fix round")
	newHead := gitCmd(t, ctx.WorkDir, "rev-parse", "HEAD")
	ctx.Fixing = true
	ctx.Run.HeadSHA = newHead
	items := Findings{Items: []Finding{{Action: types.ActionAutoFix, Support: &types.FindingSupport{
		ClaimType: types.FindingClaimSource,
		Source:    &types.FindingSourceSupport{Path: "feature.txt", Line: 1, Quote: "feature"},
	}}}}
	if _, err := validateReviewFindingSupport(ctx, items, newHead); err != nil {
		t.Fatalf("provisional exact fix head refused: %v", err)
	}
	if ctx.PRContext.LocalHeadSHA != oldHead {
		t.Fatalf("Review mutated pinned receipt to %q", ctx.PRContext.LocalHeadSHA)
	}
	ctx.Run.HeadSHA = oldHead
	if _, err := validateReviewFindingSupport(ctx, items, newHead); err == nil {
		t.Fatal("unmatched run head passed")
	}
}

func TestReviewStepPinnedSupportRoutesPendingTestClaim(t *testing.T) {
	t.Parallel()
	dir, base, head := setupGitRepo(t)
	ag := &mockAgent{runFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		return &agent.Result{Output: json.RawMessage(`{"findings":[{"severity":"error","action":"auto-fix","description":"test failed","review_scope":"source","support":{"claim_type":"test","test":{"command":"go test ./..."}}}],"reviewed_paths":["feature.txt"],"risk_level":"high","risk_rationale":"reported test failure","risk_scope":"source-or-external"}`)}, nil
	}}
	ctx := newTestContextWithDBRecords(t, ag, dir, base, head, config.Commands{})
	ctx.PRContext = &db.PRContext{PRContextCandidate: db.PRContextCandidate{LocalHeadSHA: head, TargetBranch: "main", MergeBaseSHA: base}}
	outcome, err := (&ReviewStep{}).Execute(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.NeedsApproval {
		t.Fatalf("pending test claim parked Review: %+v", outcome)
	}
	parsed, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Items) != 1 || parsed.Items[0].Category != reviewSupportPendingCategory {
		t.Fatalf("pending test claim lost: %+v", parsed.Items)
	}
}

func TestReviewPendingCIClaimSurvivesDeliveryFilter(t *testing.T) {
	t.Parallel()
	findings := Findings{Items: []Finding{
		{ReviewScope: types.FindingReviewScopePipelineOwnedDelivery, Category: reviewSupportPendingCategory, Action: types.ActionNoOp, Support: &types.FindingSupport{ClaimType: types.FindingClaimCI, CI: &types.FindingCISupport{CheckID: "42", HeadSHA: strings.Repeat("a", 40)}}},
		{ReviewScope: types.FindingReviewScopePipelineOwnedDelivery, Description: "PR is not open yet"},
	}}
	got, dropped := stripDeferredPipelineOwnedDeliveryFindings(findings)
	if dropped != 1 || len(got.Items) != 1 || got.Items[0].Category != reviewSupportPendingCategory {
		t.Fatalf("filtered = %+v, dropped = %d; want pending CI retained", got.Items, dropped)
	}
}

func newReviewSupportContext(t *testing.T) *pipeline.StepContext {
	t.Helper()
	dir, base, head := setupGitRepo(t)
	ctx := newTestContext(t, nil, dir, base, head, config.Commands{})
	ctx.PRContext = &db.PRContext{PRContextCandidate: db.PRContextCandidate{LocalHeadSHA: head}}
	return ctx
}

func TestReviewSupportPendingClaimsDoNotBlockReview(t *testing.T) {
	t.Parallel()
	ctx := newReviewSupportContext(t)
	items := Findings{Items: []Finding{
		{Action: types.ActionAutoFix, Support: &types.FindingSupport{ClaimType: types.FindingClaimTest, Test: &types.FindingTestSupport{Command: "go test ./..."}}},
		{Action: types.ActionAskUser, Support: &types.FindingSupport{ClaimType: types.FindingClaimCI, CI: &types.FindingCISupport{CheckID: "42", HeadSHA: ctx.PRContext.LocalHeadSHA}}},
	}}
	got, err := validateReviewFindingSupport(ctx, items, ctx.PRContext.LocalHeadSHA)
	if err != nil {
		t.Fatal(err)
	}
	if hasBlockingReviewFindings(got.Items) {
		t.Fatalf("pending owner claims blocked Review: %+v", got.Items)
	}
	for _, item := range got.Items {
		if item.Category != reviewSupportPendingCategory || item.Action != types.ActionNoOp {
			t.Fatalf("pending finding = %+v", item)
		}
	}
}

func TestReviewSupportMissingAndHistoricalClaimsFailClosed(t *testing.T) {
	t.Parallel()
	ctx := newReviewSupportContext(t)
	_, err := validateReviewFindingSupport(ctx, Findings{Items: []Finding{{Action: types.ActionAutoFix}}}, ctx.PRContext.LocalHeadSHA)
	if err == nil || !strings.Contains(err.Error(), "support") {
		t.Fatalf("missing support error = %v", err)
	}
	items := Findings{Items: []Finding{{Action: types.ActionAutoFix, Support: &types.FindingSupport{
		ClaimType: types.FindingClaimSource,
		Source:    &types.FindingSourceSupport{Path: "feature.txt", Line: 1, Quote: "feature", HeadSHA: "old-head"},
	}}}}
	got, err := validateReviewFindingSupport(ctx, items, ctx.PRContext.LocalHeadSHA)
	if err != nil {
		t.Fatal(err)
	}
	if got.Items[0].Action != types.ActionAskUser || got.Items[0].Category != reviewSupportPendingCategory {
		t.Fatalf("historical claim = %+v, want pending human decision", got.Items[0])
	}
	items.Items[0].Support.OwnerResult = &types.FindingOwnerResult{Disposition: types.FindingSupportDispositionDisproven}
	if _, err := validateReviewFindingSupport(ctx, items, ctx.PRContext.LocalHeadSHA); err == nil {
		t.Fatal("agent-supplied owner result passed Review")
	}
}

package db

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestExternalCIOwnerPersistsAndRerunDefaultIsEmpty(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo(filepath.Join(t.TempDir(), "repo"), "https://example.com/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	owner := types.ExternalCIOwnerControllerShipPR
	run, err := d.InsertRunWithExternalCIOwner(repo.ID, "feature", "head", "base", nil, "", "", "", "", false, false, owner, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := d.GetRun(run.ID)
	if err != nil || got.ExternalCIOwner != owner {
		t.Fatalf("persisted owner = %+v, %v", got, err)
	}
	ordinary, err := d.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil || ordinary.ExternalCIOwner != "" {
		t.Fatalf("ordinary owner = %+v, %v", ordinary, err)
	}
	if _, err := d.InsertRunWithExternalCIOwner(repo.ID, "feature", "head", "base", nil, "", "", "", "", false, false, "unknown", nil); err == nil {
		t.Fatal("unknown owner accepted")
	}
}

func TestExternalCIOwnerLaunchReceiptConflictDoesNotConsumeCreated(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo(filepath.Join(t.TempDir(), "repo"), "https://example.com/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	owner := types.ExternalCIOwnerControllerShipPR
	run, err := d.InsertRunWithExternalCIOwner(repo.ID, "feature", "head", "base", nil, "nonce", "generation", "digest", "", false, false, owner, nil)
	if err != nil {
		t.Fatal(err)
	}
	wrong, claimed, err := d.ClaimLaunchReceiptWithExternalOwner(repo.ID, "feature", "nonce", "head", "generation", "digest", "", false, "")
	if err != nil || claimed || wrong == nil || wrong.ID != run.ID {
		t.Fatalf("wrong claim = %+v, %v, %v", wrong, claimed, err)
	}
	correct, claimed, err := d.ClaimLaunchReceiptWithExternalOwner(repo.ID, "feature", "nonce", "head", "generation", "digest", "", false, owner)
	if err != nil || !claimed || correct == nil || correct.ID != run.ID {
		t.Fatalf("correct claim = %+v, %v, %v", correct, claimed, err)
	}
}

func TestPendingExternalCISupportBindsCurrentReviewAndReceipt(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo(filepath.Join(t.TempDir(), "repo"), "https://example.com/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	head := strings.Repeat("a", 40)
	run, err := d.InsertRunWithExternalCIOwner(repo.ID, "feature", head, head, nil, "", "", "", "", false, false, types.ExternalCIOwnerControllerShipPR, nil)
	if err != nil {
		t.Fatal(err)
	}
	review, err := d.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	claim := types.Finding{ID: "review-1", Severity: types.FindingSeverityInfo, Action: types.ActionNoOp, Description: "historical CI check", Category: types.FindingCategoryReviewSupportPending, Support: &types.FindingSupport{ClaimType: types.FindingClaimCI, CI: &types.FindingCISupport{CheckID: "check-17", HeadSHA: strings.Repeat("e", 40)}}}
	encoded, err := types.MarshalFindingsJSON(types.Findings{Items: []types.Finding{claim}})
	if err != nil {
		t.Fatal(err)
	}
	if err := d.SetStepFindings(review.ID, encoded); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateStepStatus(review.ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}
	if _, err := d.InsertReviewStepRoundWithProvenance(review.ID, 1, "initial", nil, nil, head, head, "", nil, nil, 1); err != nil {
		t.Fatal(err)
	}
	receipt := testPRContextCandidate()
	receipt.LocalHeadSHA = head
	receipt.PRURL = "https://github.com/acme/repo/pull/1"
	receipt.SourceRepo = "acme/repo"
	receipt.SourceBranch = "feature"
	receipt.ForgeHeadSHA = head
	if _, err := d.BindRunPRContext(run.ID, receipt, types.StepRebase); err != nil {
		t.Fatal(err)
	}
	if err := d.SetStepFindings(review.ID, encoded); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateStepStatus(review.ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunReviewApprovedHeadSHA(run.ID, head); err != nil {
		t.Fatal(err)
	}
	run, err = d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	items, err := d.PendingExternalCISupport(run)
	if err != nil || len(items) != 1 {
		t.Fatalf("pending = %+v, %v", items, err)
	}
	item := items[0]
	if item.ClaimID != types.ReviewSupportClaimID(claim) || item.OriginalFindingID != claim.ID || item.HistoricalCheckID != "check-17" || item.HistoricalHeadSHA != strings.Repeat("e", 40) || item.SourceHeadSHA != head || item.TargetSHA != receipt.TargetSHA || item.TargetBranch != receipt.TargetBranch || item.DiffDigest != receipt.DiffDigest || item.ReceiptGeneration != 1 || item.RunID != run.ID || item.PRURL != receipt.PRURL || item.Owner != types.ExternalCIOwnerControllerShipPR {
		t.Fatalf("handoff = %+v", item)
	}
	advancedHead := strings.Repeat("b", 40)
	if err := d.UpdateRunHeadSHA(run.ID, advancedHead); err != nil {
		t.Fatal(err)
	}
	receipt.LocalHeadSHA = advancedHead
	receipt.ForgeHeadSHA = advancedHead
	if _, err := d.AdvanceRunPRContext(run.ID, receipt); err != nil {
		t.Fatal(err)
	}
	run, err = d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.PendingExternalCISupport(run); err == nil {
		t.Fatal("handoff accepted a receipt advanced after Review approval")
	}
}

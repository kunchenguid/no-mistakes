package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestAxiCIHandoffRequiresRunAndEmitsJSONError(t *testing.T) {
	cmd := newAxiCIHandoffCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs(nil)
	err := cmd.Execute()
	var exit *exitError
	if !errors.As(err, &exit) || exit.code != 1 {
		t.Fatalf("exit = %v", err)
	}
	var payload map[string]string
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil || payload["error"] != "--run is required" {
		t.Fatalf("JSON error = %q, %v", out.String(), err)
	}
}

func TestExternalCIOwnerPushOptionAndOutcome(t *testing.T) {
	owner := types.ExternalCIOwnerControllerShipPR
	option := formatExternalCIOwnerPushOption(owner)
	if option != "no-mistakes.external-ci-owner=controller-ship-pr" {
		t.Fatalf("option = %q", option)
	}
	got, err := parseExternalCIOwnerPushOptions([]string{"no-mistakes.skip=push", option})
	if err != nil || got != owner {
		t.Fatalf("parsed owner = %q, %v", got, err)
	}
	for _, options := range [][]string{{option, option}, {"no-mistakes.external-ci-owner=other"}} {
		if _, err := parseExternalCIOwnerPushOptions(options); err == nil {
			t.Fatalf("accepted %v", options)
		}
	}
	if got := outcomeForRun(runView{Status: string(types.RunCompleted), ExternalCIOwner: owner}); got != "passed" {
		t.Fatalf("claim-free outcome = %q", got)
	}
	if got := outcomeForRun(runView{Status: string(types.RunCompleted), ExternalCIOwner: owner, PendingCISupport: []types.PendingCISupport{{ClaimID: "claim-1"}}}); got != "pending-external-ci" {
		t.Fatalf("outcome = %q", got)
	}
	if got := outcomeForRun(runView{Status: string(types.RunCompleted), ExternalCIOwner: owner, PendingCISupportError: "stale receipt"}); got != "external-ci-handoff-unavailable" {
		t.Fatalf("unavailable outcome = %q", got)
	}
}

func TestBuildExternalCIHandoffRequiresTerminalSkippedDeliveryAndCurrentClaim(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	repo, err := database.InsertRepoWithFork(filepath.Join(t.TempDir(), "repo"), "https://github.com/acme/upstream.git", "https://github.com/contributor/source-a.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	head := strings.Repeat("a", 40)
	run, err := database.InsertRunWithExternalCIOwner(repo.ID, "feature", head, head, nil, "", "", "", "", false, false, types.ExternalCIOwnerControllerShipPR, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := buildExternalCIHandoff(database, run); err == nil {
		t.Fatal("pending run accepted")
	}
	receipt := db.PRContextCandidate{SourceRepo: "contributor/source-a", SourceBranch: "feature", LocalHeadSHA: head, TargetBranch: "main", TargetSHA: strings.Repeat("b", 40), MergeBaseSHA: strings.Repeat("c", 40), DiffDigest: strings.Repeat("d", 64)}
	if _, err := database.BindRunPRContext(run.ID, receipt, types.StepRebase); err != nil {
		t.Fatal(err)
	}
	review, err := database.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	claim := types.Finding{ID: "review-ci-1", Severity: types.FindingSeverityInfo, Action: types.ActionNoOp, Category: types.FindingCategoryReviewSupportPending, Description: "CI claim", Support: &types.FindingSupport{ClaimType: types.FindingClaimCI, CI: &types.FindingCISupport{CheckID: "check-1", HeadSHA: strings.Repeat("e", 40)}}}
	findings, err := types.MarshalFindingsJSON(types.Findings{Items: []types.Finding{claim}})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.SetStepFindings(review.ID, findings); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateStepStatus(review.ID, types.StepStatusCompleted); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunReviewApprovedHeadSHA(run.ID, head); err != nil {
		t.Fatal(err)
	}
	if _, err := database.InsertReviewStepRoundWithProvenance(review.ID, 1, "initial", nil, nil, head, head, "", nil, nil, 1); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(run.ID, types.RunCompleted); err != nil {
		t.Fatal(err)
	}
	run, err = database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := buildExternalCIHandoff(database, run); err == nil {
		t.Fatal("unskipped delivery accepted")
	}
	for _, stepName := range []types.StepName{types.StepPush, types.StepPR, types.StepCI} {
		step, err := database.InsertStepResult(run.ID, stepName)
		if err != nil {
			t.Fatal(err)
		}
		if err := database.UpdateStepStatus(step.ID, types.StepStatusSkipped); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := database.UpdateRepoForkURL(repo.ID, "https://github.com/contributor/source-b.git"); err != nil {
		t.Fatal(err)
	}
	handoff, err := buildExternalCIHandoff(database, run)
	if err != nil {
		t.Fatal(err)
	}
	if handoff.Outcome != "pending-external-ci" || handoff.RunID != run.ID ||
		handoff.SourceRepo != "contributor/source-a" || handoff.SourceBranch != "feature" ||
		len(handoff.PendingCISupport) != 1 || handoff.PendingCISupport[0].HistoricalCheckID != "check-1" {
		t.Fatalf("handoff = %+v", handoff)
	}
	if err := database.UpdateRunReviewApprovedHeadSHA(run.ID, strings.Repeat("f", 40)); err != nil {
		t.Fatal(err)
	}
	run, err = database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := buildExternalCIHandoff(database, run); err == nil {
		t.Fatal("stale approval accepted")
	}
}

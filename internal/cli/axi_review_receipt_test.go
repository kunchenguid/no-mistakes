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

func TestAxiReviewReceiptRequiresExplicitRun(t *testing.T) {
	cmd := newAxiReviewReceiptCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs(nil)
	var exit *exitError
	if err := cmd.Execute(); !errors.As(err, &exit) || exit.code != 1 {
		t.Fatalf("exit = %v", err)
	}
	var payload map[string]string
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil || payload["error"] != "--run is required" {
		t.Fatalf("JSON error = %q, %v", out.String(), err)
	}
}

func TestBuildReviewReceiptRequiresVerifiedTerminalComparison(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	repo, err := database.InsertRepo(filepath.Join(t.TempDir(), "repo"), "https://github.com/acme/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	head := strings.Repeat("a", 40)
	target := strings.Repeat("b", 40)
	run, err := database.InsertRun(repo.ID, "feature", head, target)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := buildReviewReceipt(database, run); err == nil {
		t.Fatal("pending run accepted")
	}
	if err := database.UpdateRunStatusWithVerifiedHead(run.ID, types.RunCompleted, head); err != nil {
		t.Fatal(err)
	}
	run, err = database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := buildReviewReceipt(database, run); err == nil {
		t.Fatal("completed run without comparison accepted")
	}
	candidate := db.PRContextCandidate{LocalHeadSHA: head, TargetBranch: "main", TargetSHA: target,
		MergeBaseSHA: strings.Repeat("c", 40), DiffDigest: strings.Repeat("d", 64)}
	if _, err := database.BindRunPRContext(run.ID, candidate, types.StepRebase); err != nil {
		t.Fatal(err)
	}
	receipt, err := buildReviewReceipt(database, run)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.RunID != run.ID || receipt.SourceRepo != "acme/repo" || receipt.SourceBranch != "feature" ||
		receipt.LocalHeadSHA != head || receipt.TargetSHA != target || receipt.DiffDigest != candidate.DiffDigest {
		t.Fatalf("receipt = %+v", receipt)
	}
	status := axiDoc(runObjectField(runViewFromDB(run, nil, database)))
	if !strings.Contains(status, "review_generation: 1\n") {
		t.Fatalf("status does not expose the receipt generation: %s", status)
	}
	candidate.LocalHeadSHA = strings.Repeat("e", 40)
	if _, err := database.BindRunPRContext(run.ID, candidate, types.StepReview); err != nil {
		t.Fatal(err)
	}
	if _, err := buildReviewReceipt(database, run); err == nil {
		t.Fatal("receipt for a different head accepted")
	}
}

func TestBuildReviewReceiptPreservesAzureCanonicalSourceIdentity(t *testing.T) {
	database, err := db.Open(filepath.Join(t.TempDir(), "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	remote := "https://dev.azure.com/acme/trading/_git/controller"
	repo, err := database.InsertRepo(filepath.Join(t.TempDir(), "repo"), remote, "main")
	if err != nil {
		t.Fatal(err)
	}
	head := strings.Repeat("a", 40)
	target := strings.Repeat("b", 40)
	run, err := database.InsertRun(repo.ID, "feature", head, target)
	if err != nil {
		t.Fatal(err)
	}
	candidate := db.PRContextCandidate{PRURL: remote + "/pullrequest/1", SourceRepo: remote,
		SourceBranch: "feature", ForgeHeadSHA: head, LocalHeadSHA: head,
		TargetBranch: "main", TargetSHA: target, MergeBaseSHA: strings.Repeat("c", 40),
		DiffDigest: strings.Repeat("d", 64)}
	if _, err := database.BindRunPRContext(run.ID, candidate, types.StepRebase); err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatusWithVerifiedHead(run.ID, types.RunCompleted, head); err != nil {
		t.Fatal(err)
	}
	run, err = database.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := buildReviewReceipt(database, run)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.SourceRepo != remote || receipt.SourceBranch != "feature" || receipt.PRURL != candidate.PRURL {
		t.Fatalf("receipt source = %+v", receipt)
	}
}
